package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

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

	if err := a.handleSandboxUp(context.Background(), sandboxItem()); err != nil {
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

	if err := a.handleSandboxUp(context.Background(), sandboxItem()); err != nil {
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

	if err := a.handleSandboxUp(context.Background(), sandboxItem()); err != nil {
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
			if err := a.handleSandboxUp(context.Background(), item); err != nil {
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
		if err := a.handleSandboxUp(context.Background(), item); err != nil {
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
		if err := a.handleSandboxDestroy(context.Background(), item); err != nil {
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
	}{"rm -f": {"", errors.New("docker rm: device or resource busy")}}}
	a := &Agent{Client: client, dockerRun: dk.run}
	item := api.WorkItem{Action: api.ActionSandboxDestroy, SandboxID: "sbx-1", ContainerName: "sbx-abc123"}
	if err := a.handleSandboxDestroy(context.Background(), item); err != nil {
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
