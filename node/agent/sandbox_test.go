package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/binoctal/open-agents-bridge/node/api"
)

// Tests must never touch the real /etc/hosting-agent path: stub the ensure
// seam package-wide; focused tests below override it deliberately.
func TestMain(m *testing.M) {
	orig := ensureSandboxResolvConf
	ensureSandboxResolvConf = func() error { return nil }
	code := m.Run()
	ensureSandboxResolvConf = orig
	os.Exit(code)
}

// dockerCall is one recorded docker invocation.
type dockerCall struct {
	args []string
}

// fakeDocker answers scripted outcomes per command head and records every
// call. Outcomes map the joined args to (stdout, err).
type fakeDocker struct {
	calls    []dockerCall
	outcomes map[string]struct {
		stdout string
		err    error
	}
}

func (f *fakeDocker) run(ctx context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, dockerCall{args: append([]string(nil), args...)})
	key := strings.Join(args[:min(2, len(args))], " ")
	if o, ok := f.outcomes[key]; ok {
		return o.stdout, o.err
	}
	return "", nil
}

// A 64-hex config digest fixture; every up item must pin one (supply chain).
const runnerName = "open-agents-runner:2026-09-09"

var pinnedDigest = strings.Repeat("ab", 32)
var runnerRef = runnerName + "@sha256:" + pinnedDigest

func sandboxItem() api.WorkItem {
	return api.WorkItem{
		Action:         api.ActionSandboxUp,
		SandboxID:      "sbx-1",
		MissionID:      "m-1",
		TaskID:         "t-1",
		ContainerName:  "sbx-abc123",
		ImageRef:       runnerRef,
		TaskToken:      "stt_plaintext",
		APIBaseURL:     "https://api.openagents.top",
		TimeoutSeconds: 1800,
		TimeoutAt:      "2026-09-10T12:30:00Z",
		TaskPrompt:     "# T\n\n\ndo the thing",
		Attempt:        2,
		SizeClass: &api.SizeClass{
			ID: "sandbox-s1", VCPU: 0.5, MemoryMiB: 512, StorageMiB: 1024,
		},
	}
}

// The up path must generate exactly the security-baseline docker invocation:
// runsc runtime, locked-down network, tier-derived cgroup limits, pids
// ceiling, noexec tmpfs, token only in env, and no floating image pulls. The
// image runs by its digest-VERIFIED name (inspect precedes run).
func TestHandleSandboxUpArgs(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{
		"image inspect": {"sha256:" + pinnedDigest + "\n", nil},
		"run -d":        {"container-full-id\n", nil},
	}}
	a := &Agent{Client: client, dockerRun: dk.run}

	if err := a.handleSandboxUp(context.Background(), a.endpoints()[0], sandboxItem()); err != nil {
		t.Fatalf("handleSandboxUp: %v", err)
	}
	if len(dk.calls) != 2 {
		t.Fatalf("want 2 docker calls (image inspect + run), got %d", len(dk.calls))
	}
	if insp := dk.calls[0].args; insp[0] != "image" || insp[1] != "inspect" || insp[len(insp)-1] != runnerName {
		t.Fatalf("first call must inspect the pinned image by name, got %v", insp)
	}
	args := dk.calls[1].args
	assertHas(t, args, "--runtime", "runsc")
	assertHas(t, args, "--network", "sbx-net")
	assertHas(t, args, "--memory", "512m")
	assertHas(t, args, "--cpus", "0.5")
	assertHas(t, args, "--pids-limit", "64")
	if i := indexOf(args, "--tmpfs"); i < 0 || !strings.Contains(args[i+1], "size=1024m") || !strings.Contains(args[i+1], "noexec") {
		t.Errorf("tmpfs args wrong: %v", args)
	}
	// The runner is the unprivileged node user: a non-1777 tmpfs mode would
	// make HOME=/tmp state writes fail at CLI startup.
	if i := indexOf(args, "--tmpfs"); !strings.Contains(args[i+1], "mode=1777") {
		t.Errorf("tmpfs must be world-writable (mode=1777): %v", args)
	}
	if i := indexOf(args, "--pull"); i < 0 || args[i+1] != "never" {
		t.Errorf("--pull never missing: %v", args)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "TASK_TOKEN=stt_plaintext") {
		t.Errorf("task token not passed via env: %s", joined)
	}
	if !strings.Contains(joined, "API_BASE_URL=https://api.openagents.top") {
		t.Errorf("api base url not passed via env: %s", joined)
	}
	// The task payload rides env too (5.2): the runner refuses to start
	// without TASK_PROMPT, and ATTEMPT is the dispatch generation the runner
	// echoes back for G19 attempt pairing.
	if !strings.Contains(joined, "TASK_PROMPT=# T") {
		t.Errorf("task prompt not passed via env: %s", joined)
	}
	if !strings.Contains(joined, "ATTEMPT=2") {
		t.Errorf("dispatch attempt not passed via env: %s", joined)
	}
	if !strings.Contains(joined, " "+runnerName) {
		t.Errorf("verified image name missing from run: %s", joined)
	}
	if strings.Contains(joined, "@sha256:") {
		t.Errorf("run must use the digest-stripped name, not the pin: %s", joined)
	}
	if sink.count(api.EventSandboxUp) != 1 {
		t.Fatalf("want one sandbox_up report, got %d", sink.count(api.EventSandboxUp))
	}
	if sink.lastContainerID(api.EventSandboxUp) != "container-full-id" {
		t.Errorf("reported container id = %q", sink.lastContainerID(api.EventSandboxUp))
	}
}

// A name conflict is a re-offer after a lost report, not a failure: recover
// the existing container id by name and report up.
func TestHandleSandboxUpNameConflictRecovers(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{
		"image inspect":    {"sha256:" + pinnedDigest + "\n", nil},
		"run -d":           {"", errors.New(`docker run -d: exit: Error response from daemon: Conflict. The container name "/sbx-abc123" is already in use`)},
		"inspect --format": {"existing-id\n", nil},
	}}
	a := &Agent{Client: client, dockerRun: dk.run}

	if err := a.handleSandboxUp(context.Background(), a.endpoints()[0], sandboxItem()); err != nil {
		t.Fatalf("handleSandboxUp: %v", err)
	}
	if sink.count(api.EventSandboxUp) != 1 || sink.lastContainerID(api.EventSandboxUp) != "existing-id" {
		t.Fatalf("want recovered up report with existing-id, got %d reports", sink.count(api.EventSandboxUp))
	}
	if sink.count(api.EventSandboxUpFailed) != 0 {
		t.Errorf("unexpected sandbox_up_failed")
	}
	if len(dk.calls) != 3 || dk.calls[1].args[0] != "run" || dk.calls[2].args[0] != "inspect" {
		t.Fatalf("want inspect+run+inspect, got %v", dk.calls)
	}
}

// A plain run failure (image missing, cgroup refused) is reported
// sandbox_up_failed and never crashes the poll loop.
func TestHandleSandboxUpFailureReports(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{
		"image inspect": {"sha256:" + pinnedDigest + "\n", nil},
		"run -d":        {"", errors.New("docker run: cgroup refused")},
	}}
	a := &Agent{Client: client, dockerRun: dk.run}

	if err := a.handleSandboxUp(context.Background(), a.endpoints()[0], sandboxItem()); err != nil {
		t.Fatalf("handleSandboxUp: %v", err)
	}
	if sink.count(api.EventSandboxUpFailed) != 1 {
		t.Fatalf("want one sandbox_up_failed, got %d", sink.count(api.EventSandboxUpFailed))
	}
	if !strings.Contains(sink.lastDetail(api.EventSandboxUpFailed), "cgroup refused") {
		t.Errorf("detail should carry the docker error, got %q", sink.lastDetail(api.EventSandboxUpFailed))
	}
}

// Supply-chain gate (spec: digest mismatch refuses start): a re-pointed tag,
// a truncated digest, a floating ref, or a missing local image all refuse
// BEFORE docker run -- nothing unvetted ever executes.
func TestHandleSandboxUpDigestGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ref     string
		inspOut string
		inspErr error
		want    string
	}{
		{
			name:    "digest mismatch (tag re-pointed by rebuild)",
			ref:     runnerRef,
			inspOut: "sha256:" + strings.Repeat("cd", 32) + "\n",
			want:    "digest mismatch",
		},
		{
			name:    "image not present locally",
			ref:     runnerRef,
			inspErr: errors.New("docker image inspect: Error: No such image"),
			want:    "not present locally",
		},
		{
			name: "floating tag refused",
			ref:  "open-agents-runner:latest",
			want: "digest-pinned",
		},
		{
			name: "truncated digest refused",
			ref:  runnerName + "@sha256:deadbeef",
			want: "digest-pinned",
		},
		{
			name: "uppercase digest refused",
			ref:  runnerName + "@sha256:" + strings.ToUpper(strings.Repeat("ab", 32)),
			want: "digest-pinned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, client := newReportAPIServer(t)
			dk := &fakeDocker{outcomes: map[string]struct {
				stdout string
				err    error
			}{"image inspect": {tc.inspOut, tc.inspErr}}}
			a := &Agent{Client: client, dockerRun: dk.run}

			item := sandboxItem()
			item.ImageRef = tc.ref
			if err := a.handleSandboxUp(context.Background(), a.endpoints()[0], item); err != nil {
				t.Fatalf("handleSandboxUp: %v", err)
			}
			if sink.count(api.EventSandboxUpFailed) != 1 {
				t.Fatalf("want one sandbox_up_failed, got %d", sink.count(api.EventSandboxUpFailed))
			}
			detail := sink.lastDetail(api.EventSandboxUpFailed)
			if !strings.Contains(detail, tc.want) {
				t.Errorf("detail %q should mention %q", detail, tc.want)
			}
			for _, call := range dk.calls {
				if call.args[0] == "run" {
					t.Fatalf("docker run must never execute on a digest-gate failure, got %v", call.args)
				}
			}
		})
	}
}

// An up item missing its payload (tier, image, token, prompt) is rejected
// before any docker call -- a sandbox started without its credential or its
// prompt is a brick that still burns node resources.
func TestHandleSandboxUpMalformed(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{}
	a := &Agent{Client: client, dockerRun: dk.run}

	want := 1
	for name, mutate := range map[string]func(*api.WorkItem){
		"no token":  func(i *api.WorkItem) { i.TaskToken = "" },
		"no prompt": func(i *api.WorkItem) { i.TaskPrompt = "" },
	} {
		item := sandboxItem()
		mutate(&item)
		if err := a.handleSandboxUp(context.Background(), a.endpoints()[0], item); err != nil {
			t.Fatalf("%s: handleSandboxUp: %v", name, err)
		}
		if len(dk.calls) != 0 {
			t.Fatalf("%s: malformed item must not reach docker, got %v", name, dk.calls)
		}
		if got := sink.count(api.EventSandboxUpFailed); got != want {
			t.Fatalf("%s: want sandbox_up_failed report #%d, got %d", name, want, got)
		}
		want++
	}
}

// Attempt 0 (payload-less item) omits the ATTEMPT env entirely: the runner
// then soft-passes attempt on its callbacks, exactly like pre-attempt bridge
// runners -- never a forged attempt 0.
func TestSandboxUpArgsOmitAttemptWhenZero(t *testing.T) {
	item := sandboxItem()
	item.Attempt = 0
	joined := strings.Join(sandboxUpArgs(item, runnerName), " ")
	if strings.Contains(joined, "ATTEMPT=") {
		t.Errorf("attempt 0 must not inject ATTEMPT env: %s", joined)
	}
	if !strings.Contains(joined, "TASK_PROMPT=") {
		t.Errorf("task prompt env missing: %s", joined)
	}
}

// fix-runner-etimedout-long-turns: the container must learn the sandbox's
// AUTHORITATIVE wall-clock deadline as absolute epoch ms, so the runner
// derives its CLI kill time from the real window instead of a hardcoded 25min
// decoupled from it. 2026-09-10T12:30:00Z = 1789043400000 ms.
func TestSandboxUpArgsDeadlineEnv(t *testing.T) {
	joined := strings.Join(sandboxUpArgs(sandboxItem(), runnerName), " ")
	if !strings.Contains(joined, "SANDBOX_DEADLINE_EPOCH_MS=1789043400000") {
		t.Errorf("deadline env must carry timeout_at as epoch ms: %s", joined)
	}
}

// No TimeoutAt on the item (older API): the env is omitted entirely and the
// runner falls back to its 25min default -- never a zero/negative deadline.
func TestSandboxUpArgsOmitDeadlineWhenAbsent(t *testing.T) {
	item := sandboxItem()
	item.TimeoutAt = ""
	joined := strings.Join(sandboxUpArgs(item, runnerName), " ")
	if strings.Contains(joined, "SANDBOX_DEADLINE_EPOCH_MS=") {
		t.Errorf("missing timeoutAt must not inject a deadline env: %s", joined)
	}
	if !strings.Contains(joined, "TASK_PROMPT=") {
		t.Errorf("task prompt env missing: %s", joined)
	}
}

// A malformed TimeoutAt (not RFC3339) must not kill the launch -- the env is
// skipped and the runner's 25min fallback applies, same as an absent value.
func TestSandboxUpArgsSkipMalformedDeadline(t *testing.T) {
	item := sandboxItem()
	item.TimeoutAt = "not-a-timestamp"
	joined := strings.Join(sandboxUpArgs(item, runnerName), " ")
	if strings.Contains(joined, "SANDBOX_DEADLINE_EPOCH_MS=") {
		t.Errorf("malformed timeoutAt must not inject a deadline env: %s", joined)
	}
}

// The container must run with a mounted resolv.conf naming REAL resolvers:
// on user-defined networks docker forces 127.0.0.11 into resolv.conf, and
// gVisor's netstack cannot reach that embedded DNS proxy -- every lookup
// inside the box then fails with EAI_AGAIN (live finding, 2026-09-09 e2e).
func TestSandboxUpArgsMountResolvConf(t *testing.T) {
	args := sandboxUpArgs(sandboxItem(), runnerName)
	if i := indexOf(args, "-v"); i < 0 || args[i+1] != sandboxResolvConf+":/etc/resolv.conf:ro" {
		t.Errorf("run must bind-mount the sandbox resolv.conf: %v", args)
	}
}

// writeSandboxResolvConf seeds the mount target from SANDBOX_DNS_SERVERS and
// never overwrites an existing file (an operator edit must win).
func TestWriteSandboxResolvConf(t *testing.T) {
	t.Setenv("SANDBOX_DNS_SERVERS", "1.1.1.1, 100.100.2.136 ,,9.9.9.9")
	path := filepath.Join(t.TempDir(), "nested", "sbx-resolv.conf")
	if err := writeSandboxResolvConf(path); err != nil {
		t.Fatalf("writeSandboxResolvConf: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	want := "nameserver 1.1.1.1\nnameserver 100.100.2.136\nnameserver 9.9.9.9\n"
	if string(got) != want {
		t.Errorf("resolv.conf = %q, want %q", got, want)
	}
	// Second run is a no-op for an operator-edited file.
	if err := os.WriteFile(path, []byte("nameserver 10.0.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeSandboxResolvConf(path); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "nameserver 10.0.0.1\n" {
		t.Errorf("existing file must not be overwritten, got %q", got)
	}
}

// A resolv.conf that cannot be ensured refuses the start loudly: docker run
// would fail on the missing mount anyway, but with a reason the API can't
// attribute. Nothing may execute.
func TestHandleSandboxUpResolvConfFailure(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{}
	orig := ensureSandboxResolvConf
	ensureSandboxResolvConf = func() error { return errors.New("read-only filesystem") }
	t.Cleanup(func() { ensureSandboxResolvConf = orig })
	a := &Agent{Client: client, dockerRun: dk.run}

	if err := a.handleSandboxUp(context.Background(), a.endpoints()[0], sandboxItem()); err != nil {
		t.Fatalf("handleSandboxUp: %v", err)
	}
	// The read-only digest inspect may precede the refusal; only execution
	// (docker run) is forbidden.
	for _, c := range dk.calls {
		if c.args[0] == "run" {
			t.Fatalf("docker run must never execute when resolv.conf is unusable, got %v", c.args)
		}
	}
	if got := sink.count(api.EventSandboxUpFailed); got != 1 {
		t.Fatalf("want one sandbox_up_failed report, got %d", got)
	}
}

// Destroy is success when the container is already gone ("No such") -- the
// action promises the box is torn down, and it is.
func TestHandleSandboxDestroyIdempotent(t *testing.T) {
	sink, client := newReportAPIServer(t)
	for _, tc := range []struct {
		name  string
		rmErr error
	}{
		{"removed", nil},
		{"already gone", errors.New("docker rm: Error: No such container: sbx-abc123")},
	} {
		dk := &fakeDocker{outcomes: map[string]struct {
			stdout string
			err    error
		}{"rm -f": {"", tc.rmErr}}}
		a := &Agent{Client: client, dockerRun: dk.run}
		item := api.WorkItem{Action: api.ActionSandboxDestroy, SandboxID: "sbx-1", ContainerName: "sbx-abc123"}
		if err := a.handleSandboxDestroy(context.Background(), a.endpoints()[0], item); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if sink.count(api.EventSandboxDestroy) == 0 {
			t.Errorf("%s: want sandbox_destroy report", tc.name)
		}
		if sink.count(api.EventSandboxDestroyFailed) != 0 {
			t.Errorf("%s: unexpected destroy_failed", tc.name)
		}
		// Reset per case; sink carries all reports but counts here are cumulative-safe
		// only if both cases report success exactly once each.
	}
	if len(sink.reportsFor()) != 2 {
		t.Errorf("want 2 success reports total, got %d", len(sink.reportsFor()))
	}
}

func TestHandleSandboxDestroyFailure(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{"rm -f": {"", errors.New("docker rm: machine or resource busy")}}}
	a := &Agent{Client: client, dockerRun: dk.run}
	item := api.WorkItem{Action: api.ActionSandboxDestroy, SandboxID: "sbx-1", ContainerName: "sbx-abc123"}
	if err := a.handleSandboxDestroy(context.Background(), a.endpoints()[0], item); err != nil {
		t.Fatalf("handleSandboxDestroy: %v", err)
	}
	if sink.count(api.EventSandboxDestroyFailed) != 1 {
		t.Fatalf("want sandbox_destroy_failed, got %d", sink.count(api.EventSandboxDestroyFailed))
	}
}

func assertHas(t *testing.T, args []string, flag, want string) {
	t.Helper()
	if i := indexOf(args, flag); i < 0 || i+1 >= len(args) || args[i+1] != want {
		t.Errorf("%s %s missing or wrong (got args %v)", flag, want, args)
	}
}

func indexOf(xs []string, needle string) int {
	for i, x := range xs {
		if x == needle {
			return i
		}
	}
	return -1
}

// Sandbox-specific reads on the probe suite's reportSink.
func (s *reportSink) lastContainerID(event string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.reports) - 1; i >= 0; i-- {
		if s.reports[i]["event"] == event {
			id, _ := s.reports[i]["containerId"].(string)
			return id
		}
	}
	return ""
}

func (s *reportSink) reportsFor() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.reports...)
}

func TestSandboxUpArgsInteractiveSessionCarriesOnlySessionCredential(t *testing.T) {
	item := api.WorkItem{
		SandboxID: "sb1", ExecutorID: "cse_abc", ContainerName: "oa-sb1",
		SizeClass:     &api.SizeClass{ID: "sandbox-s1", VCPU: 0.5, MemoryMiB: 512, StorageMiB: 1024},
		APIBaseURL:    "https://api.example",
		LifecycleMode: "interactive-session", SessionID: "sess1", UserID: "u1", SessionToken: "sst_secret",
		IdleReclaimSeconds: 1800, MaxLifetimeSeconds: 14400,
		TaskToken: "must-not-leak", TaskPrompt: "must-not-leak",
	}
	joined := strings.Join(sandboxUpArgs(item, "runner"), "\n")
	for _, want := range []string{"SESSION_TOKEN=sst_secret", "SESSION_ID=sess1", "USER_ID=u1", "SHADOW_MACHINE_ID=cse_abc", "IDLE_RECLAIM_SECONDS=1800", "MAX_LIFETIME_SECONDS=14400"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in args", want)
		}
	}
	for _, bad := range []string{"TASK_TOKEN", "TASK_PROMPT", "MISSION_ID", "TASK_ID", "must-not-leak"} {
		if strings.Contains(joined, bad) {
			t.Errorf("interactive session args must not contain %q", bad)
		}
	}
}

// --- workspace artifact export (cloud-session-shadow-machine 4.5) -------------

// uploadServer accepts artifact PUTs and records what landed.
type uploadServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	puts   []int // body sizes
	bodies []string
	// contentLength[i] is the Content-Length header of puts[i]; -1 = chunked.
	contentLength []int
}

func newUploadServer(t *testing.T) *uploadServer {
	t.Helper()
	u := &uploadServer{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("artifact endpoint got %s, want PUT", r.Method)
			w.WriteHeader(405)
			return
		}
		b, _ := io.ReadAll(r.Body)
		cl := -1
		if v := r.Header.Get("Content-Length"); v != "" {
			cl, _ = strconv.Atoi(v)
		}
		u.mu.Lock()
		u.puts = append(u.puts, len(b))
		u.bodies = append(u.bodies, string(b))
		u.contentLength = append(u.contentLength, cl)
		u.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func interactiveDestroyItem(uploadURL string) api.WorkItem {
	return api.WorkItem{
		Action:            api.ActionSandboxDestroy,
		SandboxID:         "sb1",
		ExecutorID:        "cse_abc",
		ContainerName:     "oa-sb1",
		LifecycleMode:     "interactive-session",
		SessionID:         "sess1",
		ArtifactUploadURL: uploadURL,
	}
}

func stubCpTar(t *testing.T, tarErr error, content string) {
	t.Helper()
	orig := dockerWorkspaceTar
	dockerWorkspaceTar = func(ctx context.Context, containerName string, w io.Writer) error {
		if tarErr != nil {
			return tarErr
		}
		_, err := w.Write([]byte(content))
		return err
	}
	t.Cleanup(func() { dockerWorkspaceTar = orig })
}

// Destroy of an interactive session with an artifact URL: export BEFORE rm,
// upload gzip(understood as opaque bytes here), and the destroy report carries
// artifactsStatus=available. The rm must happen after the cp.
func TestDestroyExportsWorkspaceArtifact(t *testing.T) {
	sink, client := newReportAPIServer(t)
	up := newUploadServer(t)
	stubCpTar(t, nil, "hello-workspace")

	var order []string
	dk := &fakeDocker{}
	a := &Agent{Client: client, dockerRun: func(ctx context.Context, args ...string) (string, error) {
		order = append(order, strings.Join(args[:2], " "))
		return dk.run(ctx, args...)
	}}
	if err := a.handleSandboxDestroy(context.Background(), a.endpoints()[0], interactiveDestroyItem(up.srv.URL)); err != nil {
		t.Fatalf("handleSandboxDestroy: %v", err)
	}
	if len(up.puts) != 1 {
		t.Fatalf("want 1 artifact PUT, got %d", len(up.puts))
	}
	// gzip magic: the upload must be compressed, not a raw tar copy.
	if !strings.HasPrefix(up.bodies[0], "\x1f\x8b") {
		t.Errorf("uploaded body is not gzip (starts %q)", up.bodies[0][:min(2, len(up.bodies[0]))])
	}
	// R2 rejects a chunked PUT with 411 MissingContentLength: the upload
	// must carry an exact Content-Length matching the body.
	if up.contentLength[0] != len(up.bodies[0]) {
		t.Errorf("Content-Length = %d, want %d (chunked uploads get 411 from R2)", up.contentLength[0], len(up.bodies[0]))
	}
	if sink.lastArtifactsStatus(api.EventSandboxDestroy) != "available" {
		t.Errorf("want artifactsStatus=available on destroy report, got %q", sink.lastArtifactsStatus(api.EventSandboxDestroy))
	}
	// Order: the export (cp) must precede the removal (rm).
	if len(order) != 1 || order[0] != "rm -f" {
		t.Fatalf("docker calls = %v (cp rides its own seam)", order)
	}
}

// A failed export (container already gone, cp error, oversized, upload
// 5xx) must NOT block the destroy: the report still says sandbox_destroy with
// artifactsStatus=failed.
func TestDestroyArtifactFailureDoesNotBlockRemoval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cpErr    error
		cpOutput string
		putCode  int
	}{
		{"cp fails", errors.New("docker cp: No such container: oa-sb1"), "", 200},
		{"upload 5xx", nil, "data", 500},
	} {
		sink, client := newReportAPIServer(t)
		up := newUploadServer(t)
		if tc.putCode != 200 {
			up.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.putCode)
			})
		}
		stubCpTar(t, tc.cpErr, tc.cpOutput)
		dk := &fakeDocker{}
		a := &Agent{Client: client, dockerRun: dk.run}
		if err := a.handleSandboxDestroy(context.Background(), a.endpoints()[0], interactiveDestroyItem(up.srv.URL)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if sink.count(api.EventSandboxDestroy) != 1 {
			t.Errorf("%s: want destroy report", tc.name)
		}
		if got := sink.lastArtifactsStatus(api.EventSandboxDestroy); got != "failed" {
			t.Errorf("%s: want artifactsStatus=failed, got %q", tc.name, got)
		}
		if sink.count(api.EventSandboxDestroyFailed) != 0 {
			t.Errorf("%s: destroy itself must succeed", tc.name)
		}
	}
}

// Task sandboxes and old-API destroys (no artifact URL) never upload and the
// report carries no artifactsStatus.
func TestDestroyWithoutArtifactChannelIsUnchanged(t *testing.T) {
	sink, client := newReportAPIServer(t)
	stubCpTar(t, nil, "should-not-run")
	dk := &fakeDocker{}
	a := &Agent{Client: client, dockerRun: dk.run}
	item := api.WorkItem{Action: api.ActionSandboxDestroy, SandboxID: "sbx-1", ContainerName: "sbx-abc123"}
	if err := a.handleSandboxDestroy(context.Background(), a.endpoints()[0], item); err != nil {
		t.Fatalf("handleSandboxDestroy: %v", err)
	}
	if sink.count(api.EventSandboxDestroy) != 1 {
		t.Fatal("want destroy report")
	}
	if got := sink.lastArtifactsStatus(api.EventSandboxDestroy); got != "" {
		t.Errorf("task destroy must not carry artifactsStatus, got %q", got)
	}
}
