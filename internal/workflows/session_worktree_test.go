package workflows

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initSessionRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func TestSessionWorktreeCreateReuseReclaim(t *testing.T) {
	repo := initSessionRepo(t)
	w := NewWorktreeManager(repo)

	p, err := w.EnsureSessionWorktree("s1")
	if err != nil {
		t.Fatal(err)
	}
	if p == repo {
		t.Fatal("isolated checkout must not be the main checkout")
	}
	if again, _ := w.EnsureSessionWorktree("s1"); again != p {
		t.Errorf("reuse: got %q want %q", again, p)
	}
	if !w.ReclaimSessionWorktree("s1") {
		t.Error("clean worktree without unique commits must be reclaimed")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("worktree dir should be gone after reclaim")
	}
}

func TestSessionWorktreeKeptWhenDirtyOrAhead(t *testing.T) {
	repo := initSessionRepo(t)
	w := NewWorktreeManager(repo)

	p, _ := w.EnsureSessionWorktree("dirty")
	if err := os.WriteFile(filepath.Join(p, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w.ReclaimSessionWorktree("dirty") {
		t.Error("dirty worktree must be kept")
	}

	p2, _ := w.EnsureSessionWorktree("ahead")
	for _, args := range [][]string{{"commit", "-q", "--allow-empty", "-m", "work"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = p2
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s", out)
		}
	}
	if w.ReclaimSessionWorktree("ahead") {
		t.Error("worktree with unique commits must be kept")
	}
}
