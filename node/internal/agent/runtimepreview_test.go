package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/coolify"
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
}

// ─── fakes ──────────────────────────────────────────────────────────────────

type rtReport struct {
	instanceID, event, appID string
	idle                     bool
	detail                   string
}

type fakeRTAPI struct {
	mu        sync.Mutex
	manifests map[string]*api.RuntimeSnapshotManifest
	reports   []rtReport
	objects   *httptest.Server // serves sha-keyed bytes for presign
}

func newFakeRTAPI(t *testing.T, objects map[string][]byte) *fakeRTAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := objects[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &fakeRTAPI{manifests: map[string]*api.RuntimeSnapshotManifest{}, objects: srv}
}

func (f *fakeRTAPI) FetchRuntimeSnapshot(ctx context.Context, instanceID string) (*api.RuntimeSnapshotManifest, error) {
	m, ok := f.manifests[instanceID]
	if !ok {
		return nil, fmt.Errorf("no manifest for %s", instanceID)
	}
	return m, nil
}

func (f *fakeRTAPI) RuntimePresign(ctx context.Context, instanceID string, shas []string) (*api.PresignResponse, error) {
	out := &api.PresignResponse{}
	for _, sha := range shas {
		out.URLs = append(out.URLs, api.PresignedObject{
			Key: fmt.Sprintf("previews/pv/%s", sha),
			URL: f.objects.URL + "/" + sha,
		})
	}
	return out, nil
}

func (f *fakeRTAPI) ReportRuntimePreview(ctx context.Context, instanceID, event, appID string, idle bool, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, rtReport{instanceID, event, appID, idle, detail})
	return nil
}

func (f *fakeRTAPI) reportsOf(instanceID string) []rtReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rtReport
	for _, r := range f.reports {
		if r.instanceID == instanceID {
			out = append(out, r)
		}
	}
	return out
}

type fakeRTCoolify struct {
	mu       sync.Mutex
	apps     map[string]string // uuid -> name
	nextUUID int
	lastUpd  coolify.ApplicationInput
	stopped  []string
	depState string // deployment status the poll sees
	createIn coolify.ApplicationInput

	// Per-user project seam (coolify-project-per-user): projects maps name ->
	// uuid (pre-seeded = already exists); ensureErr forces the D5 failure
	// path; ensureCalls counts EnsureProject invocations.
	projects    map[string]string
	ensureCalls int
	ensureErr   error
}

func newFakeRTCoolify() *fakeRTCoolify {
	return &fakeRTCoolify{
		apps:     map[string]string{},
		depState: "finished",
		projects: map[string]string{},
	}
}

func (f *fakeRTCoolify) ListApplications(ctx context.Context) ([]coolify.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []coolify.Application
	for uuid, name := range f.apps {
		out = append(out, coolify.Application{UUID: uuid, Name: name})
	}
	return out, nil
}

func (f *fakeRTCoolify) CreateApplication(ctx context.Context, in coolify.ApplicationInput) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUUID++
	uuid := fmt.Sprintf("app-%d", f.nextUUID)
	f.apps[uuid] = strings.TrimSuffix(filepath.Base(in.GitRepository), ".git")
	f.createIn = in
	return uuid, nil
}

func (f *fakeRTCoolify) EnsureProject(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	if uuid, ok := f.projects[name]; ok {
		return uuid, nil
	}
	uuid := fmt.Sprintf("proj-%d", len(f.projects)+1)
	f.projects[name] = uuid
	return uuid, nil
}

func (f *fakeRTCoolify) GetApplication(ctx context.Context, uuid string) (*coolify.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.apps[uuid]
	if !ok {
		return nil, &coolify.Error{Status: 404}
	}
	return &coolify.Application{UUID: uuid, Name: name}, nil
}

func (f *fakeRTCoolify) UpdateApplication(ctx context.Context, uuid string, in coolify.ApplicationInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.apps[uuid]; !ok {
		return &coolify.Error{Status: 404}
	}
	f.lastUpd = in
	return nil
}

func (f *fakeRTCoolify) Deploy(ctx context.Context, uuid string, force bool) (string, error) {
	return "dep-1", nil
}

func (f *fakeRTCoolify) GetDeployment(ctx context.Context, uuid string) (*coolify.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &coolify.Deployment{UUID: uuid, Status: f.depState}, nil
}

func (f *fakeRTCoolify) Stop(ctx context.Context, uuid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, uuid)
	return nil
}

type fakeRTIngress struct {
	mu      sync.Mutex
	routes  map[string]string // label -> addr
	logged  map[string]string // instanceID -> host
	detachL []string
}

func newFakeRTIngress() *fakeRTIngress {
	return &fakeRTIngress{routes: map[string]string{}, logged: map[string]string{}}
}

func (f *fakeRTIngress) Upsert(ctx context.Context, label, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[label] = addr
	return nil
}

func (f *fakeRTIngress) Remove(ctx context.Context, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.routes, label)
	return nil
}

func (f *fakeRTIngress) AttachAccessLog(ctx context.Context, instanceID, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logged[instanceID] = host
	return nil
}

func (f *fakeRTIngress) DetachAccessLog(ctx context.Context, instanceID, host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.logged, instanceID)
	f.detachL = append(f.detachL, instanceID)
	return nil
}

// newTestRT assembles a fully-faked RuntimePreviews (real Store/GitHTTP, in
// temp dirs; dial seam stubbed to "always ready").
func newTestRT(t *testing.T, objects map[string][]byte) (*RuntimePreviews, *fakeRTAPI, *fakeRTCoolify, *fakeRTIngress) {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	fapi := newFakeRTAPI(t, objects)
	fc := newFakeRTCoolify()
	fi := newFakeRTIngress()
	rp := &RuntimePreviews{
		Coolify: fc,
		API:     fapi,
		Ingress: fi,
		Store:   store.New(dir, nil),
		GitHTTP: githttp.New(dir+"/repos", "127.0.0.1", 34100, 34199),
		Cfg: RuntimePreviewConfig{
			CoolifyProjectUUID:     "proj",
			CoolifyServerUUID:      "srv",
			CoolifyEnvironmentName: "production",
			GitBindHost:            "127.0.0.1",
			PortMin:                34100,
			PortMax:                34199,
			IdleTTL:                30 * time.Minute,
			LogDir:                 filepath.Join(dir, "logs"),
			DomainSuffix:           "deploy.openagents.top",
			MemoryLimit:            "128m",
			StateDir:               filepath.Join(dir, "runtime-previews"),
		},
		dial: func(ctx context.Context, port int) error { return nil },
	}
	if err := os.MkdirAll(rp.Cfg.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return rp, fapi, fc, fi
}

func upItem() api.WorkItem {
	return api.WorkItem{
		InstanceID:     "inst1",
		MissionID:      "m1",
		Action:         api.ActionRuntimeUp,
		Host:           "abc123.deploy.openagents.top",
		PreviewID:      "pv1",
		SnapshotTaskID: "merge",
	}
}

// ─── pure manifest / Dockerfile (task 5.6 honest-fail) ─────────────────────

func TestParseRuntimeManifest(t *testing.T) {
	valid := `{"version":1,"startCommand":["node","server.js"],"port":3000}`
	m, err := parseRuntimeManifest([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if m.Node != 0 {
		t.Fatalf("absent node field must parse as 0 (default 22), got %d", m.Node)
	}
	if _, err := parseRuntimeManifest([]byte(`{"version":1,"startCommand":["node","server.js"],"port":3000,"node":20}`)); err != nil {
		t.Fatal(err)
	}
	// Unknown major fails honestly -- never guess the nearest version (5.6).
	if _, err := parseRuntimeManifest([]byte(`{"version":1,"startCommand":["node","server.js"],"port":3000,"node":21}`)); err == nil {
		t.Fatal("node 21 must fail honestly")
	}
	// v0 contract violations.
	for name, raw := range map[string]string{
		"version":  `{"version":2,"startCommand":["node"],"port":3000}`,
		"no start": `{"version":1,"port":3000}`,
		"port":     `{"version":1,"startCommand":["node"],"port":0}`,
		"garbage":  `{`,
	} {
		if _, err := parseRuntimeManifest([]byte(raw)); err == nil {
			t.Fatalf("%s violation must fail", name)
		}
	}
}

func TestGenerateDockerfile(t *testing.T) {
	m := runtimeManifest{Version: 1, StartCommand: []string{"node", "server.js"}, Port: 3000}
	df := string(generateDockerfile(m))
	if !strings.Contains(df, "FROM node:22-slim\n") {
		t.Fatalf("absent node field must default to node:22-slim:\n%s", df)
	}
	if !strings.Contains(df, `CMD ["node", "server.js"]`) {
		t.Fatalf("start command must ride the CMD verbatim:\n%s", df)
	}
	m.Node = 20
	if !strings.Contains(string(generateDockerfile(m)), "FROM node:20-slim\n") {
		t.Fatal("node 20 must select node:20-slim")
	}
}

// ─── idle judgment (task 5.4, pure) ─────────────────────────────────────────

func TestIdleExpired(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	last := now.Add(-29 * time.Minute)
	if idleExpired(now, last, 30*time.Minute) {
		t.Fatal("29 minutes since access must not be idle")
	}
	last = now.Add(-30 * time.Minute)
	if !idleExpired(now, last, 30*time.Minute) {
		t.Fatal("exactly 30 minutes since access must be idle")
	}
	// Never-idle guards.
	if idleExpired(now, time.Time{}, 30*time.Minute) {
		t.Fatal("zero lastAccess must never trip")
	}
	if idleExpired(now, now, 0) {
		t.Fatal("zero TTL must never trip")
	}
}

func TestNoteAccess(t *testing.T) {
	rp, _, _, _ := newTestRT(t, nil)
	logPath := rp.logPath("instA")
	st := &rtState{InstanceID: "instA", Host: "instA.deploy.example", LastAccess: time.Now().Add(-time.Hour)}

	if rp.noteAccess(st) {
		t.Fatal("absent log file must not count as access")
	}
	// The per-instance file also catches sibling hosts' lines (Caddy include
	// filter); only matching-host lines may count as traffic.
	foreign := `{"request":{"host":"analytics.example"},"status":404}` + "\n"
	own := `{"request":{"host":"instA.deploy.example"},"status":200}` + "\n"
	if err := os.WriteFile(logPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if rp.noteAccess(st) {
		t.Fatal("a sibling host's line must not count as access")
	}
	if err := os.WriteFile(logPath, []byte(foreign+own), 0o644); err != nil {
		t.Fatal(err)
	}
	before := st.LastAccess
	if !rp.noteAccess(st) {
		t.Fatal("an own-host line past the offset must count as access")
	}
	if !st.LastAccess.After(before) {
		t.Fatal("access must refresh lastAccess")
	}
	if rp.noteAccess(st) {
		t.Fatal("no growth must not count as access")
	}
	// Truncated/recreated file re-baselines without counting as access.
	if err := os.WriteFile(logPath, []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if rp.noteAccess(st) {
		t.Fatal("a shrunk file must re-baseline, not count as access")
	}
	if st.LastOffset != int64(len(own)) {
		t.Fatalf("offset must re-baseline to the new size, got %d", st.LastOffset)
	}
}

// ─── up / stop lifecycle (tasks 5.2/5.3) ────────────────────────────────────

const testRuntimeJSON = `{"version":1,"startCommand":["node","server.js"],"port":3000,"healthCheckPath":"/"}`
const testServerJS = `console.log("up")`

func testObjects(t *testing.T) map[string][]byte {
	t.Helper()
	// The shas the manifest below references; computed like store_test's file().
	h := func(b string) string { return storeSha(t, b) }
	return map[string][]byte{
		h(testRuntimeJSON): []byte(testRuntimeJSON),
		h(testServerJS):    []byte(testServerJS),
	}
}

func storeSha(t *testing.T, content string) string {
	t.Helper()
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func seedManifest(t *testing.T, fapi *fakeRTAPI) {
	t.Helper()
	h := func(b string) string { return storeSha(t, b) }
	fapi.manifests["inst1"] = &api.RuntimeSnapshotManifest{
		PreviewID:      "pv1",
		SnapshotTaskID: "merge",
		TaskLabel:      "merge",
		Files: map[string]string{
			"runtime.json": h(testRuntimeJSON),
			"server.js":    h(testServerJS),
		},
	}
}

func TestUpDrivesToRunning(t *testing.T) {
	rp, fapi, fc, fi := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)

	if err := rp.up(context.Background(), upItem(), RtEndpoint{API: rp.API}); err != nil {
		t.Fatalf("up: %v", err)
	}
	reports := fapi.reportsOf("inst1")
	if len(reports) != 1 || reports[0].event != api.EventRuntimeRunning {
		t.Fatalf("must report runtime_running once, got %+v", reports)
	}
	if reports[0].appID != "app-1" || reports[0].idle {
		t.Fatalf("report must carry the coolify app and idle=false, got %+v", reports[0])
	}
	// The app was created with the dockerfile build pack and updated with the
	// runtime preview port + domain + memory limit.
	if fc.createIn.BuildPack != "dockerfile" {
		t.Fatalf("app must build from the tree's Dockerfile, got %q", fc.createIn.BuildPack)
	}
	if fc.lastUpd.Domains != "https://abc123.deploy.openagents.top" {
		t.Fatalf("PATCH must carry the preview domain, got %q", fc.lastUpd.Domains)
	}
	if fc.lastUpd.LimitsMemory != "128m" {
		t.Fatalf("PATCH must carry the 128m preview tier limit, got %q", fc.lastUpd.LimitsMemory)
	}
	if fc.lastUpd.PortsExposes < rp.Cfg.PortMin || fc.lastUpd.PortsExposes > rp.Cfg.PortMax {
		t.Fatalf("port must come from the runtime range, got %d", fc.lastUpd.PortsExposes)
	}
	// Route + access log attached for the full host.
	if fi.routes["abc123"] == "" {
		t.Fatalf("route must be wired for the label, got %+v", fi.routes)
	}
	if fi.logged["inst1"] != "abc123.deploy.openagents.top" {
		t.Fatalf("access log must be attached for the host, got %+v", fi.logged)
	}
	// The generated Dockerfile rides the committed repo tree.
	out, err := exec.Command("git", "--git-dir", rp.Store.RepoPath("oa-preview-m1"),
		"ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := string(out)
	if !strings.Contains(tree, "Dockerfile") || !strings.Contains(tree, "runtime.json") {
		t.Fatalf("tree must carry the Dockerfile and the snapshot, got:\n%s", tree)
	}
	// State persisted for restart rediscovery.
	st := rp.activeFor("m1")
	if st == nil || st.CoolifyAppID != "app-1" {
		t.Fatalf("state must be tracked and persisted, got %+v", st)
	}
}

func TestUpReusesAppByNameScanWhenStateLost(t *testing.T) {
	rp, fapi, fc, _ := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)
	// Pre-existing app under the Coolify-derived name ("<repo>.git:main-<suffix>"),
	// no in-memory/persisted state.
	fc.apps["app-preexisting"] = "oa-preview-m1.git:main-cigzcidikctv24ntoxjmom5y"

	if err := rp.up(context.Background(), upItem(), RtEndpoint{API: rp.API}); err != nil {
		t.Fatalf("up: %v", err)
	}
	if fc.nextUUID != 0 {
		t.Fatal("an app discoverable by name must never be re-created")
	}
	if fapi.reportsOf("inst1")[0].appID != "app-preexisting" {
		t.Fatal("the discovered uuid must ride the running report")
	}
}

func TestUpHonestFailOnUnsupportedNode(t *testing.T) {
	rj := `{"version":1,"startCommand":["node","server.js"],"port":3000,"node":21}`
	objects := map[string][]byte{storeSha(t, rj): []byte(rj), storeSha(t, testServerJS): []byte(testServerJS)}
	rp, fapi, _, _ := newTestRT(t, objects)
	seedManifest(t, fapi)
	// Overwrite the runtime.json content to the node-21 variant.
	fapi.manifests["inst1"].Files["runtime.json"] = storeSha(t, rj)

	err := rp.up(context.Background(), upItem(), RtEndpoint{API: rp.API})
	if err == nil || !strings.Contains(err.Error(), "node major 21") {
		t.Fatalf("unsupported node major must fail honestly, got %v", err)
	}
}

func TestStopTearsDownAndReports(t *testing.T) {
	rp, fapi, fc, fi := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)
	if err := rp.up(context.Background(), upItem(), RtEndpoint{API: rp.API}); err != nil {
		t.Fatal(err)
	}
	item := upItem()
	item.Action = api.ActionRuntimeStop
	if err := rp.stop(context.Background(), item, RtEndpoint{API: rp.API}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(fc.stopped) != 1 || fc.stopped[0] != "app-1" {
		t.Fatalf("container must be stopped via the app uuid, got %v", fc.stopped)
	}
	if fi.routes["abc123"] != "" {
		t.Fatal("route must be detached")
	}
	if _, still := fi.logged["inst1"]; still {
		t.Fatal("access log must be detached")
	}
	if rp.activeFor("m1") != nil {
		t.Fatal("state must be cleared")
	}
	reports := fapi.reportsOf("inst1")
	if len(reports) != 2 || reports[1].event != api.EventRuntimeStopped {
		t.Fatalf("stop must report runtime_stopped, got %+v", reports)
	}
}

// ─── sweep: idle stop + crash-loop (tasks 5.4/5.5) ──────────────────────────

func TestSweepIdleStopsAfterTTL(t *testing.T) {
	rp, fapi, fc, fi := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)
	if err := rp.up(context.Background(), upItem(), RtEndpoint{API: rp.API}); err != nil {
		t.Fatal(err)
	}
	st := rp.activeFor("m1")

	// Fresh traffic resets the window.
	if err := os.WriteFile(rp.logPath("inst1"), []byte("hit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rp.SweepIdle(context.Background())
	if rp.activeFor("m1") == nil {
		t.Fatal("an instance with fresh traffic must survive the sweep")
	}

	// Stale lastAccess past the TTL with no new bytes -> idle stop with the
	// idle marker on the report.
	st.LastAccess = time.Now().Add(-31 * time.Minute)
	rp.SweepIdle(context.Background())
	if rp.activeFor("m1") != nil {
		t.Fatal("an idle instance must be stopped by the sweep")
	}
	if len(fc.stopped) != 1 {
		t.Fatalf("container must be stopped, got %v", fc.stopped)
	}
	reports := fapi.reportsOf("inst1")
	// Exactly one runtime_stopped report, with the idle marker: the API's
	// status-CAS drops replays against a stopped row, so a follow-up report
	// could never correct the flag.
	if len(reports) != 2 || reports[1].event != api.EventRuntimeStopped || !reports[1].idle {
		t.Fatalf("idle stop must be exactly running + stopped(idle=true), got %+v", reports)
	}
	if fi.routes["abc123"] != "" {
		t.Fatal("route must be detached on idle stop")
	}
}

func TestSweepCrashLoopReportsFailed(t *testing.T) {
	rp, fapi, _, _ := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)
	if err := rp.up(context.Background(), upItem(), RtEndpoint{API: rp.API}); err != nil {
		t.Fatal(err)
	}
	// Dead port on every probe.
	rp.dial = func(ctx context.Context, port int) error { return fmt.Errorf("refused") }

	// First two sweeps: within the streak budget, still tracked.
	rp.SweepIdle(context.Background())
	rp.SweepIdle(context.Background())
	if rp.activeFor("m1") == nil {
		t.Fatal("two probe failures must not fail the instance yet")
	}
	// Third: runtime_failed, once.
	rp.SweepIdle(context.Background())
	if rp.activeFor("m1") != nil {
		t.Fatal("a 3-fail crash-loop streak must fail the instance")
	}
	reports := fapi.reportsOf("inst1")
	var failed int
	for _, r := range reports {
		if r.event == api.EventRuntimeFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("crash-loop must report runtime_failed exactly once, got %d (%+v)", failed, reports)
	}
}

// ─── restart recovery (task 5.5) ────────────────────────────────────────────

func TestRecoverReAdoptsPersistedStateWithRebaselinedWindow(t *testing.T) {
	rp1, fapi, _, _ := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)
	if err := rp1.up(context.Background(), upItem(), RtEndpoint{API: rp1.API}); err != nil {
		t.Fatal(err)
	}
	// Traffic happened before the restart; the log holds 10 bytes.
	if err := os.WriteFile(rp1.logPath("inst1"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A fresh process (empty maps) over the same dirs.
	rp2 := &RuntimePreviews{
		Cfg: rp1.Cfg,
	}
	rp2.Recover()
	st := rp2.activeFor("m1")
	if st == nil || st.InstanceID != "inst1" || st.CoolifyAppID != "app-1" {
		t.Fatalf("persisted instance must be re-adopted, got %+v", st)
	}
	if st.LastOffset != 10 {
		t.Fatalf("offset must re-baseline at the current log size, got %d", st.LastOffset)
	}
	if time.Since(st.LastAccess) > time.Minute {
		t.Fatal("lastAccess must re-baseline to now (conservative early-stop)")
	}
	// Rediscovery keeps the idle window alive: pre-restart traffic bytes do
	// not count as post-restart access.
	if rp2.noteAccess(st) {
		t.Fatal("bytes from before the restart must not count as new access")
	}
}

func TestUpAfterRestartReusesPersistedAppUUID(t *testing.T) {
	rp1, fapi, _, _ := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)
	if err := rp1.up(context.Background(), upItem(), RtEndpoint{API: rp1.API}); err != nil {
		t.Fatal(err)
	}

	// Restart: same dirs, fresh maps; the API re-offers the activating row.
	rp2, fapi2, fc2, _ := newTestRT2(t, rp1, fapi)
	seedManifest(t, fapi2)
	rp2.Recover()
	if err := rp2.up(context.Background(), upItem(), RtEndpoint{API: rp2.API}); err != nil {
		t.Fatalf("re-drive after restart: %v", err)
	}
	if fc2.nextUUID != 0 {
		t.Fatal("re-drive must reuse the persisted app uuid, not create")
	}
	if got := fapi2.reportsOf("inst1")[0].appID; got != "app-1" {
		t.Fatalf("running report must cite the reused app, got %q", got)
	}
}

// newTestRT2 rebuilds the fake trio over an existing rp's config/dirs.
func newTestRT2(t *testing.T, rp *RuntimePreviews, _ *fakeRTAPI) (*RuntimePreviews, *fakeRTAPI, *fakeRTCoolify, *fakeRTIngress) {
	t.Helper()
	fapi := newFakeRTAPI(t, map[string][]byte{})
	fc := newFakeRTCoolify()
	// The app created by the first process must be visible to the second.
	fc.apps["app-1"] = "oa-preview-m1"
	fi := newFakeRTIngress()
	rp2 := &RuntimePreviews{
		Coolify: fc,
		API:     fapi,
		Ingress: fi,
		Store:   rp.Store,
		GitHTTP: rp.GitHTTP,
		Cfg:     rp.Cfg,
		dial:    func(ctx context.Context, port int) error { return nil },
	}
	return rp2, fapi, fc, fi
}

// ─── per-user project placement (coolify-project-per-user task 2.4) ────────

func TestUpPlacesNewAppInPerUserProject(t *testing.T) {
	rp, _, fc, _ := newTestRT(t, testObjects(t))
	seedManifest(t, rp.API.(*fakeRTAPI))

	item := upItem()
	item.UserID = "user_42"
	if err := rp.up(context.Background(), item, RtEndpoint{API: rp.API}); err != nil {
		t.Fatalf("up: %v", err)
	}
	if fc.ensureCalls != 1 {
		t.Fatalf("must resolve the per-user project exactly once, got %d", fc.ensureCalls)
	}
	if _, ok := fc.projects["oa-user-user_42"]; !ok {
		t.Fatalf("must ensure project oa-user-user_42, got %+v", fc.projects)
	}
	if fc.createIn.ProjectUUID != "proj-1" {
		t.Fatalf("new app must land in the per-user project uuid, got %q", fc.createIn.ProjectUUID)
	}
}

func TestUpWithoutUserIDFallsBackToStaticProject(t *testing.T) {
	rp, _, fc, _ := newTestRT(t, testObjects(t))
	seedManifest(t, rp.API.(*fakeRTAPI))

	// The static fallback comes from the offering endpoint (dual-home D2),
	// not the global config value.
	ep := RtEndpoint{API: rp.API, BaseURL: "https://api.test", ProjectUUID: "ep-proj"}
	if err := rp.up(context.Background(), upItem(), ep); err != nil {
		t.Fatalf("up: %v", err)
	}
	if fc.ensureCalls != 0 {
		t.Fatalf("no userId on the wire: must not touch the project seam, got %d calls", fc.ensureCalls)
	}
	if fc.createIn.ProjectUUID != "ep-proj" {
		t.Fatalf("must use the endpoint project, got %q", fc.createIn.ProjectUUID)
	}
}

func TestUpEnsureProjectErrorFailsHonest(t *testing.T) {
	rp, _, fc, _ := newTestRT(t, testObjects(t))
	seedManifest(t, rp.API.(*fakeRTAPI))
	fc.ensureErr = &coolify.Error{Status: 502, Endpoint: "GET /api/v1/projects", Body: "bad gateway"}

	item := upItem()
	item.UserID = "user_42"
	if err := rp.up(context.Background(), item, RtEndpoint{API: rp.API}); err == nil {
		t.Fatal("ensureProject failure must fail the up drive, not fall back")
	}
	if fc.nextUUID != 0 {
		t.Fatal("no application may be created into the shared project")
	}
}

func TestUpEnsureProjectErrorReportsRuntimeFailed(t *testing.T) {
	// dispatch runs up in a goroutine, so wait for the report BY CONTENT
	// (poll the report list), never on a clock guess.
	rp, fapi, fc, _ := newTestRT(t, testObjects(t))
	seedManifest(t, fapi)
	fc.ensureErr = &coolify.Error{Status: 502, Endpoint: "GET /api/v1/projects", Body: "bad gateway"}

	item := upItem()
	item.UserID = "user_42"
	rp.dispatch(item, RtEndpoint{API: rp.API})

	deadline := time.Now().Add(2 * time.Second)
	var got *rtReport
	for time.Now().Before(deadline) {
		if rs := fapi.reportsOf("inst1"); len(rs) > 0 {
			got = &rs[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil || got.event != api.EventRuntimeFailed {
		t.Fatalf("must report runtime_failed, got %+v", got)
	}
	if !strings.Contains(got.detail, "per-user coolify project") {
		t.Fatalf("failure detail must name the project resolution, got %q", got.detail)
	}
	if fc.nextUUID != 0 {
		t.Fatal("no application may be created into the shared project")
	}
}

// Dual-home D2 (dual-home-hosting-agent 2.3): runtime preview reports ride the
// offering endpoint, and the sweep -- which holds no dispatch-time endpoint --
// resolves the owning environment through the persisted APIBaseURL.
func TestRuntimePreviewReportsRouteByOwningEndpoint(t *testing.T) {
	rp, fapi, _, _ := newTestRT(t, testObjects(t))
	seedManifest(t, fapi) // unused receiver: the offering endpoint is `offer`

	offer := newFakeRTAPI(t, testObjects(t))
	seedManifest(t, offer)
	ep := RtEndpoint{API: offer, BaseURL: "https://api-env-a", ProjectUUID: "proj-a"}
	if err := rp.up(context.Background(), upItem(), ep); err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(offer.reportsOf("inst1")) == 0 {
		t.Fatal("runtime_running must be reported through the offering endpoint")
	}
	if len(fapi.reportsOf("inst1")) != 0 {
		t.Fatal("the configured API must not see another environment's reports")
	}

	// The persisted state carries the owner; the sweep resolves it via
	// EndpointFor and reports runtime_stopped home.
	st := rp.activeFor("m1")
	if st == nil || st.APIBaseURL != "https://api-env-a" {
		t.Fatalf("state must persist the owning endpoint, got %+v", st)
	}
	home := newFakeRTAPI(t, nil)
	rp.EndpointFor = func(baseURL string) RtEndpoint {
		if baseURL == "https://api-env-a" {
			return RtEndpoint{API: home, BaseURL: baseURL}
		}
		return RtEndpoint{}
	}
	rp.idleStop(context.Background(), st)
	stopped := false
	for _, r := range home.reportsOf("inst1") {
		if r.event == api.EventRuntimeStopped && r.idle {
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("the idle sweep must report runtime_stopped to the owning environment")
	}
	if len(fapi.reportsOf("inst1")) != 0 || len(offer.reportsOf("inst1")) != 1 {
		t.Fatal("no report may leak to a non-owning endpoint")
	}
}
