package workflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return string(out)
}

func branchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = repo
	return cmd.Run() == nil
}

// The startup sweep (CleanupStaleWorktrees(nil)) must only reclaim task
// worktrees that hold nothing: a bridge restart used to force-remove every
// worktree and `branch -D` it, destroying unmerged task output and the
// uncommitted state of an interrupted task.
func TestCleanupStaleWorktreesKeepsUnfinishedWork(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init")
	gitIn(t, repo, "config", "user.email", "t@t")
	gitIn(t, repo, "config", "user.name", "t")
	gitIn(t, repo, "commit", "--allow-empty", "-m", "init")

	w := NewWorktreeManager(repo)
	mk := func(task string) string {
		p, err := w.CreateWorktree("job", task)
		if err != nil {
			t.Fatalf("CreateWorktree %s: %v", task, err)
		}
		return p
	}
	write := func(dir, name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Completed but not merged: a commit the main checkout does not have.
	unmerged := mk("unmerged")
	write(unmerged, "a.txt")
	gitIn(t, unmerged, "add", "-A")
	gitIn(t, unmerged, "commit", "-m", "task output")

	// Interrupted mid-run: uncommitted edits.
	dirty := mk("dirty")
	write(dirty, "b.txt")

	// Completed and merged into the main checkout.
	merged := mk("merged")
	write(merged, "c.txt")
	gitIn(t, merged, "add", "-A")
	gitIn(t, merged, "commit", "-m", "merged output")
	gitIn(t, repo, "merge", "task-job-merged")

	// Ran but produced nothing.
	empty := mk("empty")

	cleaned, err := w.CleanupStaleWorktrees(nil)
	if err != nil {
		t.Fatalf("CleanupStaleWorktrees: %v", err)
	}

	got := map[string]bool{}
	for _, c := range cleaned {
		got[c] = true
	}
	if len(cleaned) != 2 || !got["task-job-merged"] || !got["task-job-empty"] {
		t.Fatalf("cleaned = %v, want exactly task-job-merged and task-job-empty", cleaned)
	}

	for _, kept := range []struct{ path, branch string }{
		{unmerged, "task-job-unmerged"},
		{dirty, "task-job-dirty"},
	} {
		if _, err := os.Stat(kept.path); err != nil {
			t.Errorf("worktree %s was removed: %v", kept.path, err)
		}
		if !branchExists(t, repo, kept.branch) {
			t.Errorf("branch %s was deleted", kept.branch)
		}
	}
	if _, err := os.Stat(filepath.Join(dirty, "b.txt")); err != nil {
		t.Errorf("uncommitted file in dirty worktree lost: %v", err)
	}

	for _, gone := range []struct{ path, branch string }{
		{merged, "task-job-merged"},
		{empty, "task-job-empty"},
	} {
		if _, err := os.Stat(gone.path); !os.IsNotExist(err) {
			t.Errorf("disposable worktree %s still present (err=%v)", gone.path, err)
		}
		if branchExists(t, repo, gone.branch) {
			t.Errorf("disposable branch %s still present", gone.branch)
		}
	}
}

func TestCleanupStaleWorktreesSkipsActiveTasks(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init")
	gitIn(t, repo, "config", "user.email", "t@t")
	gitIn(t, repo, "config", "user.name", "t")
	gitIn(t, repo, "commit", "--allow-empty", "-m", "init")

	w := NewWorktreeManager(repo)
	p, err := w.CreateWorktree("job", "live")
	if err != nil {
		t.Fatal(err)
	}

	cleaned, err := w.CleanupStaleWorktrees(map[string]bool{"live": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cleaned) != 0 {
		t.Fatalf("active task worktree reclaimed: %v", cleaned)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("active worktree removed: %v", err)
	}
}
