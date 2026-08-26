package workflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Known-issue #20 companion: a re-dispatch calls CreateWorktree for a task
// whose worktree already exists (branch created on the first dispatch).
// `git worktree add -b` fails on the existing branch, and the caller fell
// back to workDir "." — the wrong directory with no isolation. Creating the
// same task worktree twice must be idempotent: return the existing path.
func TestCreateWorktreeIdempotent(t *testing.T) {
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

	w := NewWorktreeManager(".")
	first, err := w.CreateWorktree("job_rd", "task_rd")
	if err != nil {
		t.Fatalf("first CreateWorktree: %v", err)
	}

	second, err := w.CreateWorktree("job_rd", "task_rd")
	if err != nil {
		t.Fatalf("re-dispatch CreateWorktree must reuse, got error: %v", err)
	}
	if first != second {
		t.Fatalf("reuse returned different path: first=%q second=%q", first, second)
	}
	if !filepath.IsAbs(second) {
		t.Fatalf("reused path %q is not absolute", second)
	}
}
