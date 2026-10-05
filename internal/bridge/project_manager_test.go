package bridge

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// workspace-checkout: worktree managers are resolved per project, never from
// the bridge's launch directory when a project is supplied.

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}

func TestGetWorktreeManagerCachesPerRepoRoot(t *testing.T) {
	repo := initRepo(t)
	sub := repo + "/pkg"
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	b := newSendBridge(t)

	a := b.getWorktreeManager(repo)
	if got := b.getWorktreeManager(sub); got != a {
		t.Error("a sub-directory of the same repo must share the manager")
	}
	other := b.getWorktreeManager(initRepo(t))
	if other == a {
		t.Error("different repositories must not share a manager")
	}
}

func TestManagerForPrefersPayloadThenJobThenLegacy(t *testing.T) {
	repoA, repoB := initRepo(t), initRepo(t)
	b := newSendBridge(t)
	b.worktreeManager = b.getWorktreeManager(initRepo(t)) // stands in for the legacy manager
	legacy := b.worktreeManager

	if got := b.managerFor(map[string]interface{}{}, "job-x"); got != legacy {
		t.Error("no payload path and unknown job must fall back to legacy")
	}

	b.rememberJobManager("job-1", b.getWorktreeManager(repoA))
	if got := b.managerFor(map[string]interface{}{}, "job-1"); got != b.getWorktreeManager(repoA) {
		t.Error("a known job must resolve to the manager it was dispatched with")
	}
	if got := b.managerFor(map[string]interface{}{"projectPath": repoB}, "job-1"); got != b.getWorktreeManager(repoB) {
		t.Error("an explicit projectPath must win over the job registry")
	}
}

func TestTaskAssignInNonGitProjectFailsWithNotAGitRepo(t *testing.T) {
	notRepo := t.TempDir()
	b := newSendBridge(t)
	b.handleWorkflowTaskAssign(Message{
		Type: "workflow:task_assign",
		Payload: map[string]interface{}{
			"jobId":          "job-ng",
			"taskId":         "task-ng",
			"agent":          "replay",
			"title":          "t",
			"worktreeBranch": "oa/job-ng/task-ng",
			"projectPath":    notRepo,
		},
		Timestamp: time.Now().UnixMilli(),
	})
	payload, ok := findOffline(t, b, "workflow:task_error")
	if !ok {
		t.Fatal("expected workflow:task_error for a non-git project")
	}
	if et, _ := payload["errorType"].(string); et != "not_a_git_repo" {
		t.Errorf("errorType = %q, want not_a_git_repo", et)
	}
	if sess := b.sessions.Get("task-ng"); sess != nil {
		t.Error("no session may start for a task that failed isolation")
	}
}
