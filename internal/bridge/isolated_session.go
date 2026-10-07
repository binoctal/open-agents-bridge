package bridge

import (
	"fmt"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// sendSessionStartFailed reports a session:start failure with the generic
// SESSION_START_FAILED code; the error text carries the specific reason.
func (b *Bridge) sendSessionStartFailed(sessionID, reason string) {
	b.sendMessage(Message{
		Type: "session:error",
		Payload: map[string]interface{}{
			"sessionId": sessionID,
			"machineId":  b.config.MachineID,
			"error":     reason,
			"code":      "SESSION_START_FAILED",
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// rememberIsolatedSession records which project an isolated session's
// worktree belongs to, so stop can reclaim it conservatively.
func (b *Bridge) rememberIsolatedSession(sessionID, projectDir string) {
	b.worktreeManagersMu.Lock()
	if b.isolatedSessions == nil {
		b.isolatedSessions = map[string]string{}
	}
	b.isolatedSessions[sessionID] = projectDir
	b.worktreeManagersMu.Unlock()
}

// reclaimIsolatedSession removes the session's worktree only when it is clean
// and carries no unique commits; otherwise it is kept (never forced).
func (b *Bridge) reclaimIsolatedSession(sessionID string) {
	b.worktreeManagersMu.Lock()
	projectDir, ok := b.isolatedSessions[sessionID]
	delete(b.isolatedSessions, sessionID)
	b.worktreeManagersMu.Unlock()
	if !ok {
		return
	}
	if b.getWorktreeManager(projectDir).ReclaimSessionWorktree(sessionID) {
		b.logInfo("[%s] Reclaimed isolated worktree of session %s", logger.ModSession, sessionID)
	} else {
		b.logInfo("[%s] Kept isolated worktree of session %s (has changes or git failure)", logger.ModSession, sessionID)
	}
}

// prepareIsolatedWorkDir creates (or reuses) the session's linked worktree
// under projectPath and returns its path. A non-git project is refused with a
// NOT_A_GIT_REPO-prefixed error.
func (b *Bridge) prepareIsolatedWorkDir(sessionID, projectPath string) (string, error) {
	wm := b.getWorktreeManager(projectPath)
	if !wm.IsGitRepo() {
		return "", fmt.Errorf("NOT_A_GIT_REPO: isolated copy needs a git repository")
	}
	wtPath, err := wm.EnsureSessionWorktree(sessionID)
	if err != nil {
		return "", err
	}
	b.rememberIsolatedSession(sessionID, wm.ProjectDir())
	return wtPath, nil
}
