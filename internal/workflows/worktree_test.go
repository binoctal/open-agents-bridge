package workflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Known-issue #10: the bridge constructs WorktreeManager with a literal
// relative base ("."), and CreateWorktree returned filepath.Join of it — a
// relative path like ./.open-agents-bridge-worktrees/task-…. The ACP adapter
// rejects a relative cwd ("cwd must be an absolute path") → 60s hang → PTY
// fallback. The manager must hand out absolute paths regardless of how its
// base was given.
func TestCreateWorktreeReturnsAbsolutePath(t *testing.T) {
	// Real git repo fixture (git worktree needs one).
	repo := t.TempDir()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir("/") }()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// Relative base, exactly how bridge.go constructs the manager.
	w := NewWorktreeManager(".")
	path, err := w.CreateWorktree("job_x", "task_y")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("worktree path %q is not absolute — ACP session/new would reject it", path)
	}
}
