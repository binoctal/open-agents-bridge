package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/binoctal/open-agents-bridge/node/api"
	"github.com/binoctal/open-agents-bridge/node/store"
)

// --- user-node command gate (byo-hosting-node 2.2, D1 double check) --------------

// The gate refuses every platform-only surface BEFORE dispatch: the sandbox
// fleet, runtime/session preview instances, actions outside the deployment
// enum, and route items wiring a foreign deployment's label onto this node.
// Refusals of deployment-keyed items also REPORT (the owner's audit trail);
// sandbox/instance items carry no deployment key, so the log is their record.
func TestUserNodeGateRefusalMatrix(t *testing.T) {
	sink, client := newReportAPIServer(t)
	fd := &fakeDocker{}
	a := &Agent{Client: client, UserNode: true, dockerRun: fd.run}
	ep := a.endpoints()[0]

	for _, tc := range []struct {
		name       string
		item       api.WorkItem
		wantReason string
		wantReport bool
	}{
		{
			name:       "sandbox item refused",
			item:       api.WorkItem{Action: api.ActionSandboxUp, SandboxID: "sbx-1", ContainerName: "oa-sbx1"},
			wantReason: "platform-only",
		},
		{
			name:       "runtime preview item refused",
			item:       api.WorkItem{Action: "runtime_up", InstanceID: "rt-1"},
			wantReason: "platform-only",
		},
		{
			name:       "action outside the enum refused",
			item:       api.WorkItem{Action: "session_preview_up", DeploymentID: "d1"},
			wantReason: "outside the user-node command enum",
			wantReport: true,
		},
		{
			name:       "foreign-label route refused",
			item:       api.WorkItem{Action: api.ActionRegisterRoute, DeploymentID: "dMine", Host: "dother.deploy.openagents.top"},
			wantReason: "foreign-label dispatch refused",
			wantReport: true,
		},
	} {
		if a.userNodeGate(context.Background(), ep, tc.item) {
			t.Errorf("%s: gate must refuse", tc.name)
		}
		if tc.wantReport {
			detail := sink.lastDetail(api.EventMaterializeFailed)
			if !strings.Contains(detail, "refused on user node") || !strings.Contains(detail, tc.wantReason) {
				t.Errorf("%s: want materialize_failed report naming %q, got %q", tc.name, tc.wantReason, detail)
			}
		}
	}
	if got := sink.count(api.EventMaterializeFailed); got != 2 {
		t.Errorf("want exactly 2 reported refusals (deployment-keyed ones), got %d", got)
	}
	// handle() on a refused item is a clean nil: no dispatch, no docker.
	if err := a.handle(context.Background(), ep, api.WorkItem{Action: api.ActionSandboxUp, SandboxID: "sbx-1", ContainerName: "oa-sbx1"}); err != nil {
		t.Fatalf("handle refused item: %v", err)
	}
	if len(fd.calls) != 0 {
		t.Errorf("a refused item must not reach docker, got %v", fd.calls)
	}
}

// The gate PASSES exactly the deployment enum, and route hosts match their
// deployment case-insensitively (DNS lowercases; ids are mixed-case base62).
func TestUserNodeGateAllowsDeploymentEnum(t *testing.T) {
	sink, client := newReportAPIServer(t)
	a := &Agent{Client: client, UserNode: true}
	ep := a.endpoints()[0]
	for _, item := range []api.WorkItem{
		{Action: api.ActionMaterialize, DeploymentID: "AbC123"},
		{Action: api.ActionRegisterRoute, DeploymentID: "AbC123", Host: "abc123.deploy.openagents.top"},
		{Action: api.ActionAssertRoute, DeploymentID: "AbC123", Host: "AbC123.deploy.openagents.top"},
		{Action: api.ActionRemoveRoute, DeploymentID: "d1"},
		{Action: api.ActionRelease, DeploymentID: "d1"},
		{Action: api.ActionProbe, DeploymentID: "d1", AppPort: 30001},
	} {
		if !a.userNodeGate(context.Background(), ep, item) {
			t.Errorf("action %q must pass the user-node gate", item.Action)
		}
	}
	if sink.count(api.EventMaterializeFailed) != 0 {
		t.Error("passing items must not report failures")
	}
}

// --- user-node materialize (2.3) --------------------------------------------------

// byoAPI scripts every platform face the user-node paths talk to and records
// reports + presign counts: /report, /presign-source, and the object server
// behind the presigned URLs.
type byoAPI struct {
	srv     *httptest.Server
	objects map[string][]byte
	mu      sync.Mutex
	presign int
	reports []map[string]any
}

func newByoAPI(t *testing.T, objects map[string][]byte) *byoAPI {
	t.Helper()
	b := &byoAPI{objects: objects}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/hosting-agent/report", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.mu.Lock()
		b.reports = append(b.reports, body)
		b.mu.Unlock()
		w.WriteHeader(200)
	})
	mux.HandleFunc("/internal/hosting-agent/presign-source", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DeploymentID string   `json:"deploymentId"`
			Shas         []string `json:"shas"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.mu.Lock()
		b.presign++
		b.mu.Unlock()
		resp := api.PresignResponse{}
		for _, s := range req.Shas {
			// Slashed key like the real worker ({prefix}/{sha}); the store
			// matches on the tail segment.
			resp.URLs = append(resp.URLs, api.PresignedObject{Key: "deploys/" + req.DeploymentID + "/" + s, URL: b.srv.URL + "/obj/" + s})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/obj/", func(w http.ResponseWriter, r *http.Request) {
		sha := strings.TrimPrefix(r.URL.Path, "/obj/")
		raw, ok := b.objects[sha]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(raw)
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *byoAPI) count(event string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, r := range b.reports {
		if r["event"] == event {
			n++
		}
	}
	return n
}

func (b *byoAPI) lastField(event, key string) any {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.reports) - 1; i >= 0; i-- {
		if b.reports[i]["event"] == event {
			return b.reports[i][key]
		}
	}
	return nil
}

// manifestServer serves the manifest document (any path).
func manifestServer(t *testing.T, files []api.File) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/manifest.json"
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// The BYO compose materialize: rewritten stack file (limits + loopback
// ingress slot) lands in a PLAIN worktree, docker compose up runs under the
// stable folded project name, and the report carries the ingress host port
// -- no Coolify, no git http, no image loads on a source-tier row.
func TestUserNodeMaterializeCompose(t *testing.T) {
	composeSrc := `
services:
  web:
    image: nginx:1.27
    ports: ["8080:3000"]
`
	files := []api.File{
		{Path: "docker-compose.yml", SHA256: sha256Hex([]byte(composeSrc))},
		{Path: "index.html", SHA256: sha256Hex([]byte("<h1>ok</h1>"))},
	}
	b := newByoAPI(t, map[string][]byte{
		files[0].SHA256: []byte(composeSrc),
		files[1].SHA256: []byte("<h1>ok</h1>"),
	})
	manifestURL := manifestServer(t, files)

	st := store.New(t.TempDir(), nil)
	dk := &fakeDocker{}
	a := &Agent{Store: st, UserNode: true, dockerRun: dk.run}
	item := api.WorkItem{
		Action:       api.ActionMaterialize,
		DeploymentID: "AbC123",
		ManifestURL:  manifestURL,
		Compose: &api.ComposeSpec{
			ComposePath:    "docker-compose.yml",
			IngressService: "web",
			IngressPort:    3000,
			HostPort:       30001,
			Services:       []api.ServiceLimit{{Name: "web", MemoryMiB: 512, CPUs: 1}},
		},
	}
	if err := a.handleMaterialize(context.Background(), APIEndpoint{Client: api.New(b.srv.URL, "node-test", "node-secret")}, item); err != nil {
		t.Fatalf("handleMaterialize: %v", err)
	}
	if b.count(api.EventMaterializeFailed) > 0 {
		t.Fatalf("materialize failed on the node: %v", b.lastField(api.EventMaterializeFailed, "detail"))
	}

	// Exactly one docker call, the compose up, under the folded project name.
	if len(dk.calls) != 1 {
		t.Fatalf("want exactly 1 docker call (compose up), got %d: %v", len(dk.calls), dk.calls)
	}
	args := dk.calls[0].args
	if len(args) < 6 || args[0] != "compose" || args[1] != "-p" || args[2] != "oa-abc123" || args[3] != "-f" {
		t.Fatalf("compose head must be [compose -p oa-abc123 -f <file>], got %v", args)
	}
	if tail := strings.Join(args[5:], " "); tail != "up -d --wait --remove-orphans" {
		t.Fatalf("compose tail must be [up -d --wait --remove-orphans], got %q", tail)
	}
	if i := indexOf(args, "-f"); i < 0 || !strings.HasSuffix(args[i+1], "docker-compose.yml") {
		t.Errorf("compose -f must point at the worktree file, got %v", args)
	} else {
		// The worktree is plain and carries the REWRITTEN compose.
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(args[i+1]), "docker-compose.yml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "127.0.0.1:30001:3000") {
			t.Errorf("rewritten compose missing loopback ingress slot:\n%s", raw)
		}
		if got, err := os.ReadFile(filepath.Join(filepath.Dir(args[i+1]), "index.html")); err != nil || !strings.Contains(string(got), "ok") {
			t.Errorf("source tree missing from worktree (err %v)", err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(args[i+1]), ".git")); !os.IsNotExist(err) {
			t.Error("user-node worktree must be plain (no .git)")
		}
	}
	if b.count(api.EventMaterialized) != 1 {
		t.Fatalf("want one materialized report, got %d", b.count(api.EventMaterialized))
	}
	if port, _ := b.lastField(api.EventMaterialized, "repoPort").(float64); int(port) != 30001 {
		t.Errorf("report repoPort = %v, want the ingress host port 30001", b.lastField(api.EventMaterialized, "repoPort"))
	}
}

// --- user-node stop (byo-hosting-node): stop keeps containers for restart ---------

// stopDocker records calls and answers the two discovery queries by filter
// content (fakeDocker's 2-arg outcome keys cannot tell the label query from
// the exact-name one), with a stopOutcome for the stop calls.
type stopDocker struct {
	calls       []dockerCall
	labelIDs    string
	nameIDs     string
	stopOutcome func(id string) error
}

func (s *stopDocker) run(_ context.Context, args ...string) (string, error) {
	s.calls = append(s.calls, dockerCall{args: append([]string(nil), args...)})
	joined := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(joined, "ps -q --filter label=com.docker.compose.project="):
		return s.labelIDs, nil
	case strings.HasPrefix(joined, "ps -q --filter name="):
		return s.nameIDs, nil
	case args[0] == "stop":
		if s.stopOutcome != nil {
			return "", s.stopOutcome(args[1])
		}
		return "", nil
	}
	return "", nil
}

// Discovery-then-stop converges both shapes: the compose project's label
// query returns the running ids, each is stopped by id, and the report is
// stop_done (the API folds it into stopping -> stopped). The exact-name query
// for the single-container shape returns nothing here -- both shapes ride the
// same item.
func TestUserNodeHandleStopStopsDiscoveredContainers(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &stopDocker{labelIDs: "cid1\ncid2\n"}
	a := &Agent{Client: client, UserNode: true, dockerRun: dk.run}
	if err := a.handleStop(context.Background(), a.endpoints()[0], api.WorkItem{Action: api.ActionStop, DeploymentID: "AbC123"}); err != nil {
		t.Fatalf("handleStop: %v", err)
	}
	var stopped []string
	for _, c := range dk.calls {
		if len(c.args) >= 2 && c.args[0] == "stop" {
			stopped = append(stopped, c.args[1])
		}
	}
	if len(stopped) != 2 || stopped[0] != "cid1" || stopped[1] != "cid2" {
		t.Fatalf("must stop cid1 and cid2 by id, got %v (calls: %v)", stopped, dk.calls)
	}
	if sink.count(api.EventStopDone) != 1 {
		t.Fatalf("want one stop_done report, got %d", sink.count(api.EventStopDone))
	}
}

// Zero discoveries is success -- nothing running is the steady state of a
// re-offered stopping row, and it must still confirm stop_done.
func TestUserNodeHandleStopNothingRunningConfirms(t *testing.T) {
	sink, client := newReportAPIServer(t)
	a := &Agent{Client: client, UserNode: true, dockerRun: (&stopDocker{}).run}
	if err := a.handleStop(context.Background(), a.endpoints()[0], api.WorkItem{Action: api.ActionStop, DeploymentID: "d1"}); err != nil {
		t.Fatalf("handleStop: %v", err)
	}
	if sink.count(api.EventStopDone) != 1 {
		t.Fatalf("empty discovery must report stop_done, got %d", sink.count(api.EventStopDone))
	}
}

// A real stop failure of a discovered container reports NOTHING: the API's
// stopping row is cron-force-completed if unconfirmed, and a failure event
// would flip a recoverable stop into a failed row.
func TestUserNodeHandleStopFailureReportsNothing(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &stopDocker{labelIDs: "cid1\n", stopOutcome: func(string) error { return errors.New("cannot stop container") }}
	a := &Agent{Client: client, UserNode: true, dockerRun: dk.run}
	if err := a.handleStop(context.Background(), a.endpoints()[0], api.WorkItem{Action: api.ActionStop, DeploymentID: "d1"}); err != nil {
		t.Fatalf("handleStop returns nil (log-only failure): %v", err)
	}
	for _, ev := range []string{api.EventStopDone, api.EventMaterializeFailed} {
		if sink.count(ev) != 0 {
			t.Fatalf("failed stop must report no event, got %s x%d", ev, sink.count(ev))
		}
	}
}

// --- user-node release (2.2): converge by name, no compose file needed -----------

// Release items carry no compose spec; teardown converges by the stable
// project name: the single container rm, then the label-keyed purge
// (containers, networks, volumes), then the deployment's local images.
func TestUserNodeTeardownConvergesByName(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{
		// One leftover image to rmi; empty purge lists elsewhere.
		"images --filter": {"oa-deploy-abc123-web:latest\n", nil},
	}}
	st := store.New(t.TempDir(), nil)
	a := &Agent{Client: client, Store: st, UserNode: true, dockerRun: dk.run}
	item := api.WorkItem{Action: api.ActionRelease, DeploymentID: "AbC123"}

	if err := a.handleRelease(context.Background(), a.endpoints()[0], item); err != nil {
		t.Fatalf("handleRelease: %v", err)
	}
	var joined []string
	for _, c := range dk.calls {
		joined = append(joined, strings.Join(c.args, " "))
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{
		"rm -f oa-abc123",
		"label=com.docker.compose.project=oa-abc123",
		"network ls -q --filter",
		"volume ls -q --filter",
		"reference=oa-deploy-abc123-*",
		"rmi oa-deploy-abc123-web:latest",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("teardown must run %q, got:\n%s", want, all)
		}
	}
	if sink.count(api.EventReleased) != 1 {
		t.Errorf("want one released report, got %d", sink.count(api.EventReleased))
	}
}

// A cold node must pull the missing tar blob before docker load: the
// non-compose branch presigns and downloads from the object store instead of
// failing with "object <sha> not in store" (the compose branch already had
// this; the fix levels the single-container path with it).
func TestUserNodeMaterializeSingleFetchesMissing(t *testing.T) {
	tar := []byte("fake-tar-bytes")
	tarSHA := sha256Hex(tar)
	b := newByoAPI(t, map[string][]byte{tarSHA: tar})
	manifestURL := manifestServer(t, []api.File{{Path: "web.tar", SHA256: tarSHA}})

	// Cold store: nothing pre-imported, so the blob can only arrive via the
	// presign download.
	st := store.New(t.TempDir(), nil)
	dk := &fakeDocker{}
	a := &Agent{Store: st, UserNode: true, dockerRun: dk.run}
	item := api.WorkItem{
		Action:       api.ActionMaterialize,
		DeploymentID: "AbC123",
		ManifestURL:  manifestURL,
		AppPort:      31001,
	}
	if err := a.handleMaterialize(context.Background(), APIEndpoint{Client: api.New(b.srv.URL, "node-test", "node-secret")}, item); err != nil {
		t.Fatalf("handleMaterialize: %v", err)
	}
	if b.count(api.EventMaterializeFailed) > 0 {
		t.Fatalf("materialize failed on the node: %v", b.lastField(api.EventMaterializeFailed, "detail"))
	}
	if b.presign == 0 {
		t.Error("the missing blob must be presigned and downloaded before docker load")
	}
	var runArgs []string
	for _, c := range dk.calls {
		if c.args[0] == "run" {
			runArgs = c.args
		}
	}
	if runArgs == nil {
		t.Fatalf("docker run must execute after the fetch, calls: %v", dk.calls)
	}
	if i := indexOf(runArgs, "-p"); i < 0 || runArgs[i+1] != "127.0.0.1:31001:31001" {
		t.Errorf("run must bind the pre-allocated port, got %v", runArgs)
	}
	if got := runArgs[len(runArgs)-1]; got != "oa-deploy-abc123-web" {
		t.Errorf("run must use the folded deploy tag, got %q", got)
	}
}

// --- single-container future-proofing path ---------------------------------------

func TestNodeRunSingleArgs(t *testing.T) {
	tar := []byte("fake-tar-bytes")
	tarSHA := sha256Hex(tar)
	st := store.New(t.TempDir(), nil)
	if err := os.MkdirAll(filepath.Join(st.Dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Dir, "objects", tarSHA), tar, 0o644); err != nil {
		t.Fatal(err)
	}
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{"image inspect": {"amd64\n", nil}}}
	a := &Agent{Store: st, UserNode: true, dockerRun: dk.run}
	item := api.WorkItem{DeploymentID: "AbC123", AppPort: 31001}
	manifest := []api.File{{Path: "web.tar", SHA256: tarSHA}}

	port, err := a.nodeRunSingle(context.Background(), item, manifest)
	if err != nil {
		t.Fatalf("nodeRunSingle: %v", err)
	}
	if port != 31001 {
		t.Errorf("port = %d, want the pre-allocated 31001", port)
	}
	var runArgs []string
	for _, c := range dk.calls {
		if c.args[0] == "run" {
			runArgs = c.args
		}
	}
	if runArgs == nil {
		t.Fatal("docker run never executed")
	}
	joined := strings.Join(runArgs, " ")
	for _, want := range []string{
		"--name oa-abc123",
		"--restart unless-stopped",
		"-p 127.0.0.1:31001:31001",
		"oa-deploy-abc123-web", // lowercase-folded three-end tag
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args missing %q: %s", want, joined)
		}
	}
}

// A compose up failure keeps the last 300 chars of output in the error --
// compose failures bury the real error lines mid-log.
func TestComposeUpErrorTail(t *testing.T) {
	long := strings.Repeat("x", 500)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{"compose -p": {long + "REAL-ERROR-LINE", errors.New("exit status 1")}}}
	a := &Agent{dockerRun: dk.run}
	err := a.composeUp(context.Background(), "d1", "/tmp/docker-compose.yml")
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	// The 300-char tail keeps the real line (which sits at the END of the
	// log) and drops the 200 x's that precede it.
	if !strings.Contains(msg, "REAL-ERROR-LINE") {
		t.Errorf("error must carry the output tail with the real error line, got %q", msg)
	}
	if n := len(msg); n > 400 {
		t.Errorf("error must be tail-truncated to ~300 chars of output, got %d", n)
	}
}

// --- usage sampling stays platform-only (D1/D4) ----------------------------------

func TestUserNodeReportsNoUsage(t *testing.T) {
	sink, client := newReportAPIServer(t)
	fd := &fakeDocker{}
	a := &Agent{Client: client, UserNode: true, dockerRun: fd.run}
	if err := a.reportUsage(context.Background()); err != nil {
		t.Fatalf("reportUsage: %v", err)
	}
	if len(fd.calls) != 0 {
		t.Errorf("user node must not sample usage (flat fee, D4), got %v", fd.calls)
	}
	if len(sink.reportsFor()) != 0 {
		t.Errorf("user node must not report usage rows, got %d", len(sink.reportsFor()))
	}
}
