package bridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRepoForIsolated(t *testing.T) string {
	t.Helper()
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

func TestPrepareIsolatedWorkDirCreatesAndReclaims(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepoForIsolated(t)
	b := newSendBridge(t)
	wd, err := b.prepareIsolatedWorkDir("iso1", repo)
	if err != nil {
		t.Fatal(err)
	}
	if wd == repo || !strings.Contains(wd, "session-iso1") {
		t.Errorf("workDir = %q, want an isolated session worktree", wd)
	}
	if _, err := os.Stat(filepath.Join(wd, ".git")); err != nil {
		t.Errorf("worktree missing: %v", err)
	}
	b.reclaimIsolatedSession("iso1")
	if _, err := os.Stat(wd); !os.IsNotExist(err) {
		t.Error("clean isolated worktree should be reclaimed on stop")
	}
}

func TestIsolatedSessionStartNonGitIsRejected(t *testing.T) {
	b := newSendBridge(t)
	b.handleSessionStart(Message{
		Type: "session:start",
		Payload: map[string]interface{}{
			"sessionId": "iso2", "cliType": "replay", "workDir": t.TempDir(), "isolated": true,
		},
		Timestamp: time.Now().UnixMilli(),
	})
	payload, ok := findOffline(t, b, "session:error")
	if !ok {
		t.Fatal("expected session:error")
	}
	if e, _ := payload["error"].(string); !strings.HasPrefix(e, "NOT_A_GIT_REPO") {
		t.Errorf("error = %q", e)
	}
	if _, started := findOffline(t, b, "session:started"); started {
		t.Error("no session:started for a rejected isolated start")
	}
}
