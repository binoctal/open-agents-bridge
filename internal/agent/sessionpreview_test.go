package agent

// Session preview agent-half tests (session-output-online-preview task 6.3).
// Mirrors runtimepreview_test's fake trio (API/Coolify/ingress) over the real
// Store/GitHTTP in temp dirs. The Coolify ownership is inverted on this tier:
// up only materializes + serves git-http and reports session_materialized --
// no application create/deploy may happen agent-side; register_route wires
// the ingress once the API-driven build flips the row ready.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

// ─── fakes ──────────────────────────────────────────────────────────────────

type spReport struct {
	instanceID, event, appID string
	repoPort                 int
	idle                     bool
	detail                   string
}

// fakeSPAPI is the session-tier API seam; the presign object server is the
// runtime fake's sha-keyed httptest server, reused as-is.
type fakeSPAPI struct {
	mu        sync.Mutex
	manifests map[string]*api.RuntimeSnapshotManifest
	reports   []spReport
	objects   *fakeRTAPI
}

func newFakeSPAPI(t *testing.T, objects map[string][]byte) *fakeSPAPI {
	t.Helper()
	return &fakeSPAPI{
		manifests: map[string]*api.RuntimeSnapshotManifest{},
		objects:   newFakeRTAPI(t, objects),
	}
}

func (f *fakeSPAPI) FetchSessionSnapshot(ctx context.Context, instanceID string) (*api.RuntimeSnapshotManifest, error) {
	m, ok := f.manifests[instanceID]
	if !ok {
		return nil, fmt.Errorf("no manifest for %s", instanceID)
	}
	return m, nil
}

func (f *fakeSPAPI) SessionPresign(ctx context.Context, instanceID string, shas []string) (*api.PresignResponse, error) {
	out := &api.PresignResponse{}
	for _, sha := range shas {
		out.URLs = append(out.URLs, api.PresignedObject{
			Key: fmt.Sprintf("session-previews/%s/%s", instanceID, sha),
			URL: f.objects.objects.URL + "/" + sha,
		})
	}
	return out, nil
}

func (f *fakeSPAPI) ReportSessionPreview(ctx context.Context, instanceID, event string, repoPort int, coolifyAppID string, idle bool, detail string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, spReport{instanceID, event, coolifyAppID, repoPort, idle, detail})
	return nil
}

func (f *fakeSPAPI) reportsOf(instanceID string) []spReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []spReport
	for _, r := range f.reports {
		if r.instanceID == instanceID {
			out = append(out, r)
		}
	}
	return out
}

// fakeSPCoolify records Stop calls only (the API owns create/deploy here).
type fakeSPCoolify struct {
	mu      sync.Mutex
	stopped []string
}

func (f *fakeSPCoolify) Stop(ctx context.Context, uuid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, uuid)
	return nil
}

// newTestSP assembles a fully-faked SessionPreviews (real Store/GitHTTP in
// temp dirs; dial stubbed to always-ready).
func newTestSP(t *testing.T, objects map[string][]byte) (*SessionPreviews, *fakeSPAPI, *fakeSPCoolify, *fakeRTIngress) {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	fapi := newFakeSPAPI(t, objects)
	fc := &fakeSPCoolify{}
	fi := newFakeRTIngress()
	sp := &SessionPreviews{
		Coolify: fc,
		API:     fapi,
		Ingress: fi,
		Store:   store.New(dir, nil),
		GitHTTP: githttp.New(dir+"/repos", "127.0.0.1", 34200, 34299),
		Cfg: SessionPreviewConfig{
			IdleTTL:      30 * time.Minute,
			LogDir:       filepath.Join(dir, "logs"),
			DomainSuffix: "deploy.openagents.top",
			StateDir:     filepath.Join(dir, "session-previews"),
		},
		dial: func(ctx context.Context, port int) error { return nil },
	}
	if err := os.MkdirAll(sp.Cfg.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return sp, fapi, fc, fi
}

func spUpItem() api.WorkItem {
	return api.WorkItem{
		InstanceID: "sp1",
		Action:     api.ActionSessionPreviewUp,
		Host:       "sp1.deploy.openagents.top",
	}
}

const testIndexHTML = `<html>session output</html>`

func seedSPManifest(t *testing.T, fapi *fakeSPAPI) {
	t.Helper()
	fapi.manifests["sp1"] = &api.RuntimeSnapshotManifest{
		PreviewID: "sp1",
		Files: map[string]string{
			"index.html": storeSha(t, testIndexHTML),
			"Dockerfile": storeSha(t, "FROM node:22-slim\n"),
		},
	}
}

func spObjects(t *testing.T) map[string][]byte {
	return map[string][]byte{
		storeSha(t, testIndexHTML):         []byte(testIndexHTML),
		storeSha(t, "FROM node:22-slim\n"): []byte("FROM node:22-slim\n"),
	}
}

// ─── up: materialize + git-http, no Coolify ─────────────────────────────────

func TestSPUpMaterializesRepoAndReportsPort(t *testing.T) {
	sp, fapi, fc, _ := newTestSP(t, spObjects(t))
	seedSPManifest(t, fapi)

	if err := sp.up(context.Background(), spUpItem(), SpEndpoint{API: fapi}); err != nil {
		t.Fatalf("up: %v", err)
	}
	reports := fapi.reportsOf("sp1")
	if len(reports) != 1 || reports[0].event != api.EventSessionMaterialized {
		t.Fatalf("must report session_materialized once, got %+v", reports)
	}
	if reports[0].repoPort <= 0 {
		t.Fatalf("report must carry the git-http port, got %d", reports[0].repoPort)
	}
	// The API owns the build: the agent side must not touch Coolify on up.
	if len(fc.stopped) != 0 {
		t.Fatalf("up must not drive Coolify, got %v", fc.stopped)
	}
	// The committed tree carries the snapshot verbatim -- no Dockerfile
	// injection, no runtime.json (the tree's own Dockerfile rides along).
	out, err := exec.Command("git", "--git-dir", sp.Store.RepoPath("oa-session-preview-sp1"),
		"ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := string(out)
	if !strings.Contains(tree, "index.html") || !strings.Contains(tree, "Dockerfile") {
		t.Fatalf("tree must carry the packed snapshot, got:\n%s", tree)
	}
	if strings.Contains(tree, "runtime.json") {
		t.Fatalf("session tier must not inject runtime manifests, got:\n%s", tree)
	}
}

func TestSPUpFailureReportsMaterializeFailed(t *testing.T) {
	// No manifest seeded: the fetch fails. dispatch runs up async, so wait for
	// the report BY CONTENT, never on a clock guess.
	sp, fapi, _, _ := newTestSP(t, nil)
	sp.dispatch(spUpItem(), SpEndpoint{API: fapi})

	deadline := time.Now().Add(2 * time.Second)
	var got *spReport
	for time.Now().Before(deadline) {
		if rs := fapi.reportsOf("sp1"); len(rs) > 0 {
			got = &rs[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil || got.event != api.EventSessionMaterializeFail {
		t.Fatalf("must report session_materialize_failed, got %+v", got)
	}
	if !strings.Contains(got.detail, "fetch snapshot") {
		t.Fatalf("failure detail must name the failed step, got %q", got.detail)
	}
}

// ─── register_route / stop ──────────────────────────────────────────────────

func spReadyItem() api.WorkItem {
	item := spUpItem()
	item.Action = api.ActionSessionPreviewRegisterRoute
	item.AppPort = 30500
	item.CoolifyAppID = "app-9"
	return item
}

func TestSPRegisterRouteWiresIngressAndTracksState(t *testing.T) {
	sp, fapi, _, fi := newTestSP(t, nil)

	if err := sp.registerRoute(context.Background(), spReadyItem(), SpEndpoint{API: fapi, BaseURL: "https://api.test"}); err != nil {
		t.Fatalf("registerRoute: %v", err)
	}
	if fi.routes["sp1"] != "127.0.0.1:30500" {
		t.Fatalf("route must be wired to the app port, got %+v", fi.routes)
	}
	if fi.logged["sp1"] != "sp1.deploy.openagents.top" {
		t.Fatalf("access log must be attached for the host, got %+v", fi.logged)
	}
	reports := fapi.reportsOf("sp1")
	if len(reports) != 1 || reports[0].event != api.EventSessionRouteRegistered || reports[0].appID != "app-9" {
		t.Fatalf("must report session_route_registered with the app, got %+v", reports)
	}
	st := sp.activeFor("sp1")
	if st == nil || st.Port != 30500 || st.Label != "sp1" || st.APIBaseURL != "https://api.test" {
		t.Fatalf("state must be tracked for the sweep, got %+v", st)
	}
	// Persisted for restart rediscovery.
	if _, err := os.Stat(sp.statePath("sp1")); err != nil {
		t.Fatalf("state must be persisted: %v", err)
	}
}

func TestSPRegisterRouteRejectsForeignHost(t *testing.T) {
	sp, fapi, _, _ := newTestSP(t, nil)
	item := spReadyItem()
	item.Host = "sp1.elsewhere.example"

	if err := sp.registerRoute(context.Background(), item, SpEndpoint{API: fapi}); err == nil {
		t.Fatal("a host outside the configured domain must fail honestly")
	}
	// dispatch maps the failure to session_failed (the row leaves ready).
	sp2, fapi2, _, _ := newTestSP(t, nil)
	sp2.dispatch(item, SpEndpoint{API: fapi2})
	deadline := time.Now().Add(2 * time.Second)
	var got *spReport
	for time.Now().Before(deadline) {
		if rs := fapi2.reportsOf("sp1"); len(rs) > 0 {
			got = &rs[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil || got.event != api.EventSessionFailed {
		t.Fatalf("register_route failure must report session_failed, got %+v", got)
	}
}

func TestSPStopTearsDownAndReports(t *testing.T) {
	sp, fapi, fc, fi := newTestSP(t, nil)
	if err := sp.registerRoute(context.Background(), spReadyItem(), SpEndpoint{API: fapi}); err != nil {
		t.Fatal(err)
	}

	item := spUpItem()
	item.Action = api.ActionSessionPreviewStop
	item.CoolifyAppID = "app-9"
	if err := sp.stop(context.Background(), item, SpEndpoint{API: fapi}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(fc.stopped) != 1 || fc.stopped[0] != "app-9" {
		t.Fatalf("container must be stopped via the app uuid, got %v", fc.stopped)
	}
	if fi.routes["sp1"] != "" {
		t.Fatal("route must be detached")
	}
	if _, still := fi.logged["sp1"]; still {
		t.Fatal("access log must be detached")
	}
	if sp.activeFor("sp1") != nil {
		t.Fatal("state must be cleared")
	}
	if _, err := os.Stat(sp.statePath("sp1")); !os.IsNotExist(err) {
		t.Fatal("persisted state must be dropped on stop")
	}
	reports := fapi.reportsOf("sp1")
	if len(reports) != 2 || reports[1].event != api.EventSessionStopped || reports[1].idle {
		t.Fatalf("stop must report session_stopped (idle=false), got %+v", reports)
	}
}

func TestSPStopWithoutStateStillTearsDownFromItem(t *testing.T) {
	// State lost to a restart: the item's own fields cover teardown.
	sp, fapi, fc, fi := newTestSP(t, nil)
	item := spUpItem()
	item.Action = api.ActionSessionPreviewStop
	item.CoolifyAppID = "app-7"

	if err := sp.stop(context.Background(), item, SpEndpoint{API: fapi}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(fc.stopped) != 1 || fc.stopped[0] != "app-7" {
		t.Fatalf("container must be stopped from the item, got %v", fc.stopped)
	}
	if fi.routes["sp1"] != "" || len(fi.logged) != 0 {
		t.Fatalf("wiring must come off via the item host, got %+v %+v", fi.routes, fi.logged)
	}
}

// ─── sweep: route re-assert, idle stop, crash-loop ──────────────────────────

func TestSPSweepReassertsRouteAfterCaddyReload(t *testing.T) {
	sp, _, _, fi := newTestSP(t, nil)
	if err := sp.registerRoute(context.Background(), spReadyItem(), SpEndpoint{API: sp.API}); err != nil {
		t.Fatal(err)
	}
	// A Caddyfile reload wipes admin config: the route vanishes externally.
	delete(fi.routes, "sp1")

	sp.SweepIdle(context.Background())
	if fi.routes["sp1"] != "127.0.0.1:30500" {
		t.Fatalf("sweep must re-assert the route, got %+v", fi.routes)
	}
	if sp.activeFor("sp1") == nil {
		t.Fatal("a live instance must survive the sweep")
	}
}

func TestSPSweepIdleStopsAfterTTL(t *testing.T) {
	sp, fapi, fc, fi := newTestSP(t, nil)
	if err := sp.registerRoute(context.Background(), spReadyItem(), SpEndpoint{API: fapi}); err != nil {
		t.Fatal(err)
	}

	// Fresh traffic resets the window.
	if err := os.WriteFile(sp.logPath("sp1"), []byte("hit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sp.SweepIdle(context.Background())
	if sp.activeFor("sp1") == nil {
		t.Fatal("an instance with fresh traffic must survive the sweep")
	}

	// Stale lastAccess past the TTL with no new bytes -> idle stop.
	st := sp.activeFor("sp1")
	st.LastAccess = time.Now().Add(-31 * time.Minute)
	sp.SweepIdle(context.Background())
	if sp.activeFor("sp1") != nil {
		t.Fatal("an idle instance must be stopped by the sweep")
	}
	if len(fc.stopped) != 1 || fc.stopped[0] != "app-9" {
		t.Fatalf("container must be stopped, got %v", fc.stopped)
	}
	reports := fapi.reportsOf("sp1")
	var stoppedIdle int
	for _, r := range reports {
		if r.event == api.EventSessionStopped && r.idle {
			stoppedIdle++
		}
	}
	if stoppedIdle != 1 {
		t.Fatalf("idle stop must report session_stopped idle=true exactly once, got %+v", reports)
	}
	if fi.routes["sp1"] != "" {
		t.Fatal("route must be detached on idle stop")
	}
}

func TestSPSweepCrashLoopReportsFailed(t *testing.T) {
	sp, fapi, _, _ := newTestSP(t, nil)
	if err := sp.registerRoute(context.Background(), spReadyItem(), SpEndpoint{API: fapi}); err != nil {
		t.Fatal(err)
	}
	sp.dial = func(ctx context.Context, port int) error { return fmt.Errorf("refused") }

	sp.SweepIdle(context.Background())
	sp.SweepIdle(context.Background())
	if sp.activeFor("sp1") == nil {
		t.Fatal("two probe failures must not fail the instance yet")
	}
	sp.SweepIdle(context.Background())
	if sp.activeFor("sp1") != nil {
		t.Fatal("a 3-fail crash-loop streak must fail the instance")
	}
	var failed int
	for _, r := range fapi.reportsOf("sp1") {
		if r.event == api.EventSessionFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("crash-loop must report session_failed exactly once, got %d", failed)
	}
}

// ─── restart recovery ───────────────────────────────────────────────────────

func TestSPRecoverReAdoptsPersistedState(t *testing.T) {
	sp1, fapi, _, _ := newTestSP(t, nil)
	if err := sp1.registerRoute(context.Background(), spReadyItem(), SpEndpoint{API: fapi}); err != nil {
		t.Fatal(err)
	}
	// Traffic happened before the restart; the log holds 10 bytes.
	if err := os.WriteFile(sp1.logPath("sp1"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A fresh process (empty maps) over the same dirs.
	sp2 := &SessionPreviews{Cfg: sp1.Cfg}
	sp2.Recover()
	st := sp2.activeFor("sp1")
	if st == nil || st.Port != 30500 || st.CoolifyAppID != "app-9" {
		t.Fatalf("persisted instance must be re-adopted, got %+v", st)
	}
	if st.LastOffset != 10 {
		t.Fatalf("offset must re-baseline at the current log size, got %d", st.LastOffset)
	}
	if time.Since(st.LastAccess) > time.Minute {
		t.Fatal("lastAccess must re-baseline to now (conservative early-stop)")
	}
}
