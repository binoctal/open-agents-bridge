package workflows

import (
	"fmt"
	"os"
	"path/filepath"
)

// SessionBranch is the branch an isolated interactive session works on.
func SessionBranch(sessionID string) string { return "oa/session-" + sessionID }

func sessionWorktreeName(sessionID string) string { return "session-" + sessionID }

// EnsureSessionWorktree creates (or reuses) the isolated checkout for an
// interactive session: a linked worktree outside the user's repo, on its own
// branch cut from the main checkout's HEAD. Returns the worktree path.
func (w *WorktreeManager) EnsureSessionWorktree(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("session id required")
	}
	name := sessionWorktreeName(sessionID)
	if existing := w.findWorktree(name); existing != "" {
		return existing, nil
	}
	base, err := w.resolveBase("")
	if err != nil {
		return "", err
	}
	path := w.worktreePath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("failed to create worktrees directory: %w", err)
	}
	if out, err := w.git(w.projectDir, "worktree", "add", path, "-b", SessionBranch(sessionID), base); err != nil {
		return "", fmt.Errorf("git worktree add failed: %s: %w", out, err)
	}
	return path, nil
}

// ReclaimSessionWorktree removes the session's isolated checkout only when it
// carries nothing of value (clean tree, no commits outside HEAD). Returns
// true when reclaimed; false means kept — including on any git failure.
// Never forces and never deletes a branch holding unique commits.
func (w *WorktreeManager) ReclaimSessionWorktree(sessionID string) bool {
	name := sessionWorktreeName(sessionID)
	path := w.findWorktree(name)
	if path == "" {
		return false
	}
	branch := SessionBranch(sessionID)
	if !w.worktreeIsDisposable(path, branch) {
		return false
	}
	if _, err := w.git(w.projectDir, "worktree", "remove", path); err != nil {
		return false
	}
	_, _ = w.git(w.projectDir, "branch", "-D", branch)
	return true
}
