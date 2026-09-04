package bridge

import (
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/preview"
)

func boolPtr(v bool) *bool { return &v }

// triggeredBridge builds a minimal Bridge whose preview launches go through
// a recording hook instead of the real build+upload. launched receives one
// {jobID, repoRoot} per launch.
func triggeredBridge(cfg *config.Config) (b *Bridge, launched chan [2]string) {
	launched = make(chan [2]string, 8)
	b = &Bridge{config: cfg}
	b.previewBuildRun = func(_ preview.Uploader, _ *preview.Cache, jobID, repoRoot string, _ preview.Logf) {
		launched <- [2]string{jobID, repoRoot}
	}
	return b, launched
}

func worktreeMeta() *taskMeta {
	return &taskMeta{
		JobID:    "mission-1",
		TaskID:   "task-1",
		Title:    "t",
		WorkDir:  "/tmp/wt-mission-1",
		Worktree: true,
	}
}

// The trigger matrix (preview-hosting-ux-parity task 3.4): the toggle's
// three states crossed with worktree/non-worktree and exit code. A launch is
// expected ONLY for effective-ON + worktree + exitCode 0.
func TestMaybeBuildPreviewFromWorktreeTriggerMatrix(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *config.Config
		meta       *taskMeta
		exitCode   int
		wantLaunch bool
	}{
		{"unconfigured + worktree + success launches (default ON)", &config.Config{}, worktreeMeta(), 0, true},
		{"explicit true launches", &config.Config{PreviewBuildEnabled: boolPtr(true)}, worktreeMeta(), 0, true},
		{"explicit false never launches", &config.Config{PreviewBuildEnabled: boolPtr(false)}, worktreeMeta(), 0, false},
		{"failed exit code skips", &config.Config{}, worktreeMeta(), 1, false},
		{"non-worktree task skips (would race the live repo)", &config.Config{}, &taskMeta{JobID: "mission-1", WorkDir: ".", Worktree: false}, 0, false},
		{"nil meta skips", &config.Config{}, nil, 0, false},
		{"empty WorkDir skips", &config.Config{}, &taskMeta{JobID: "m", Worktree: true}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, launched := triggeredBridge(tc.cfg)
			b.maybeBuildPreviewFromWorktree(tc.meta, tc.exitCode)
			if !tc.wantLaunch {
				select {
				case got := <-launched:
					t.Fatalf("unexpected launch: %v", got)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
			select {
			case got := <-launched:
				if got[0] != tc.meta.JobID || got[1] != tc.meta.WorkDir {
					t.Fatalf("launched with %v, want {%s %s}", got, tc.meta.JobID, tc.meta.WorkDir)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("expected a launch, none arrived")
			}
		})
	}
}

// In-flight mutex: while a mission's build is running, a second trigger for
// the same mission skips; a different mission's trigger is unaffected; and
// once the build finishes the slot frees up again.
func TestPreviewInFlightMutex(t *testing.T) {
	b, launched := triggeredBridge(&config.Config{})

	// Block the first build until we release it.
	release := make(chan struct{})
	done := make(chan struct{})
	b.previewBuildRun = func(_ preview.Uploader, _ *preview.Cache, jobID, _ string, _ preview.Logf) {
		launched <- [2]string{jobID, ""}
		if jobID == "mission-1" {
			<-release
			close(done)
		}
	}

	meta := worktreeMeta()
	b.maybeBuildPreviewFromWorktree(meta, 0)
	<-launched // first build is now in flight (blocked)

	// Second trigger for the SAME mission must skip.
	b.maybeBuildPreviewFromWorktree(meta, 0)
	select {
	case got := <-launched:
		t.Fatalf("second trigger for same mission should skip, launched %v", got)
	case <-time.After(100 * time.Millisecond):
	}

	// A different mission is not blocked by mission-1's build.
	other := &taskMeta{JobID: "mission-2", TaskID: "t2", WorkDir: "/tmp/wt-2", Worktree: true}
	b.maybeBuildPreviewFromWorktree(other, 0)
	select {
	case got := <-launched:
		if got[0] != "mission-2" {
			t.Fatalf("expected mission-2 launch, got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("different mission should not be mutexed")
	}

	// The skip must not have stolen the slot: after release, mission-1 can
	// build again.
	close(release)
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.previewBuildStart("mission-1") {
			b.previewBuildDone("mission-1")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("mission-1 slot never freed after build completed")
}

// Fire-and-forget contract: the trigger returns immediately even while the
// build is still running — the task callback path must never wait on it.
func TestMaybeBuildPreviewFromWorktreeDoesNotBlock(t *testing.T) {
	b, _ := triggeredBridge(&config.Config{})
	block := make(chan struct{})
	b.previewBuildRun = func(_ preview.Uploader, _ *preview.Cache, _, _ string, _ preview.Logf) {
		<-block
	}

	meta := worktreeMeta()
	start := time.Now()
	b.maybeBuildPreviewFromWorktree(meta, 0)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("trigger blocked for %v on a running build; must be fire-and-forget", elapsed)
	}
	close(block)
}
