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
	calls     []dockerCall
	outcomes  map[string]struct {
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

func sandboxItem() api.WorkItem {
	return api.WorkItem{
		Action:        api.ActionSandboxUp,
		SandboxID:     "sbx-1",
		MissionID:     "m-1",
		TaskID:        "t-1",
		ContainerName: "sbx-abc123",
		ImageRef:      "ghcr.io/open-agents/runner@sha256:deadbeef",
		TaskToken:     "stt_plaintext",
		APIBaseURL:    "https://api.openagents.top",
		TimeoutSeconds: 1800,
		SizeClass: &api.SizeClass{
			ID: "sandbox-s1", VCPU: 0.5, MemoryMiB: 512, StorageMiB: 1024,
		},
	}
}

// The up path must generate exactly the security-baseline docker invocation:
// runsc runtime, locked-down network, tier-derived cgroup limits, pids
// ceiling, noexec tmpfs, token only in env, and no floating image pulls.
func TestHandleSandboxUpArgs(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{"run -d": {"container-full-id\n", nil}}}
	a := &Agent{Client: client, dockerRun: dk.run}

	if err := a.handleSandboxUp(context.Background(), sandboxItem()); err != nil {
		t.Fatalf("handleSandboxUp: %v", err)
	}
	if len(dk.calls) != 1 {
		t.Fatalf("want 1 docker call, got %d", len(dk.calls))
	}
	args := dk.calls[0].args
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
	if !strings.Contains(joined, "ghcr.io/open-agents/runner@sha256:deadbeef") {
		t.Errorf("image ref missing: %s", joined)
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
		"run -d":     {"", errors.New(`docker run -d: exit: Error response from daemon: Conflict. The container name "/sbx-abc123" is already in use`)},
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
	if len(dk.calls) != 2 || dk.calls[1].args[0] != "inspect" {
		t.Fatalf("want run+inspect, got %v", dk.calls)
	}
}

// A plain run failure (image missing, cgroup refused) is reported
// sandbox_up_failed and never crashes the poll loop.
func TestHandleSandboxUpFailureReports(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{"run -d": {"", errors.New("docker run: No such image")}}}
	a := &Agent{Client: client, dockerRun: dk.run}

	if err := a.handleSandboxUp(context.Background(), sandboxItem()); err != nil {
		t.Fatalf("handleSandboxUp: %v", err)
	}
	if sink.count(api.EventSandboxUpFailed) != 1 {
		t.Fatalf("want one sandbox_up_failed, got %d", sink.count(api.EventSandboxUpFailed))
	}
	if !strings.Contains(sink.lastDetail(api.EventSandboxUpFailed), "No such image") {
		t.Errorf("detail should carry the docker error, got %q", sink.lastDetail(api.EventSandboxUpFailed))
	}
}

// An up item missing its payload (tier, image, token) is rejected before any
// docker call -- a sandbox started without its credential is a brick that
// still burns node resources.
func TestHandleSandboxUpMalformed(t *testing.T) {
	sink, client := newReportAPIServer(t)
	dk := &fakeDocker{}
	a := &Agent{Client: client, dockerRun: dk.run}

	item := sandboxItem()
	item.TaskToken = ""
	if err := a.handleSandboxUp(context.Background(), item); err != nil {
		t.Fatalf("handleSandboxUp: %v", err)
	}
	if len(dk.calls) != 0 {
		t.Fatalf("malformed item must not reach docker, got %v", dk.calls)
	}
	if sink.count(api.EventSandboxUpFailed) != 1 {
		t.Errorf("want sandbox_up_failed report")
	}
}

// Destroy is success when the container is already gone ("No such") -- the
// action promises the box is torn down, and it is.
func TestHandleSandboxDestroyIdempotent(t *testing.T) {
	sink, client := newReportAPIServer(t)
	for _, tc := range []struct {
		name    string
		rmErr   error
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
