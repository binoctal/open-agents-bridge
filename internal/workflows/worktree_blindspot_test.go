package workflows

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo creates a real git repository at dir with one empty commit and
// returns a cleanup that restores the original working directory.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// workdir-blindspot (2026-09-04): bridges start from monorepo subdirectories
// (apps/api) where `.git` lives at the repo root. IsGitRepo used to check only
// projectDir/.git, silently disabled worktree isolation, and tasks ran in the
// live repo. The manager must resolve its base up to the repository root.
func TestNewWorktreeManagerResolvesRepoRootFromSubdirectory(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	sub := filepath.Join(repo, "apps", "api")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir("/") }()

	// Relative base from inside the subdirectory, exactly how bridge.go
	// constructs the manager.
	w := NewWorktreeManager(".")
	if got := w.ProjectDir(); got != repo {
		t.Fatalf("ProjectDir() = %q, want repo root %q — worktree ops would run in the subdirectory", got, repo)
	}
	if !w.IsGitRepo() {
		t.Fatal("IsGitRepo() = false for subdirectory launch — worktree isolation would be silently skipped")
	}
}

// A linked worktree carries a `.git` FILE, not a directory — root resolution
// and IsGitRepo must accept both shapes.
func TestNewWorktreeManagerResolvesLinkedWorktreeFile(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	w := NewWorktreeManager(repo)
	linked, err := w.CreateWorktree("job_b", "task_rootfile")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	inner := filepath.Join(linked, "nested", "deep")
	if err := os.MkdirAll(inner, 0755); err != nil {
		t.Fatal(err)
	}
	w2 := NewWorktreeManager(inner)
	if got := w2.ProjectDir(); got != linked {
		t.Fatalf("ProjectDir() = %q, want linked worktree root %q", got, linked)
	}
}

// No .git anywhere up the tree: keep the original directory so the existing
// non-git fallbacks (worktree skipped, workDir ".") still apply.
func TestNewWorktreeManagerKeepsDirWithoutGit(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "plain", "dir")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	w := NewWorktreeManager(sub)
	if got := w.ProjectDir(); got != sub {
		t.Fatalf("ProjectDir() = %q, want unchanged %q for non-git dir", got, sub)
	}
	if w.IsGitRepo() {
		t.Fatal("IsGitRepo() = true for non-git dir")
	}
}

// ListChangedFiles is the contract gate's non-LLM evidence source: whatever
// git sees as changed — modified, untracked, renamed (destination path).
func TestListChangedFiles(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	writeFile := func(rel, content string) {
		t.Helper()
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	writeFile("tracked.txt", "v1")
	git("add", "-A")
	git("commit", "-m", "files")

	// One modified, one untracked, one staged rename of a tracked file.
	writeFile("tracked.txt", "v2")
	writeFile("apps/api/new-file.ts", "fresh")
	git("mv", "tracked.txt", "renamed.txt")

	w := NewWorktreeManager(repo)
	got := w.ListChangedFiles(repo)

	joined := "\n" + strings.Join(got, "\n") + "\n"
	for _, want := range []string{"renamed.txt", "apps/api/new-file.ts"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("changed files %v missing %q", got, want)
		}
	}
	if strings.Contains(joined, "tracked.txt") && !strings.Contains(joined, "tracked.txt -> ") {
		// plain occurrence of the old name (not inside a rename arrow) is wrong
		t.Errorf("rename entry should report destination only, got %v", got)
	}

	// Clean tree → nil/empty, so the callback payload omits the field.
	git("add", "-A")
	git("commit", "-m", "all")
	if files := w.ListChangedFiles(repo); len(files) != 0 {
		t.Errorf("clean tree returned %v, want empty", files)
	}

	// Non-git directory → nil (not an error path).
	if files := w.ListChangedFiles(t.TempDir()); files != nil {
		t.Errorf("non-git dir returned %v, want nil", files)
	}
}

// The task_result payload must carry changedFiles only when collected, so
// legacy orchestrators and golden-replay fixtures see an unchanged shape.
func TestSendTaskResultPayloadCarriesChangedFiles(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		bodies = append(bodies, m)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cm := NewCallbackManager(CallbackConfig{APIURL: srv.URL, DeviceID: "dev-1", UserID: "user-1", DeviceToken: "devtok"})

	if err := cm.SendTaskResult(TaskResult{JobID: "j1", TaskID: "t1", Success: true, ChangedFiles: []string{"apps/api/solar.txt"}}); err != nil {
		t.Fatalf("SendTaskResult: %v", err)
	}
	payload := bodies[0]["payload"].(map[string]any)
	files, _ := payload["changedFiles"].([]any)
	if len(files) != 1 || files[0] != "apps/api/solar.txt" {
		t.Errorf("payload.changedFiles = %v, want [apps/api/solar.txt]", payload["changedFiles"])
	}

	if err := cm.SendTaskResult(TaskResult{JobID: "j1", TaskID: "t2", Success: true}); err != nil {
		t.Fatalf("SendTaskResult: %v", err)
	}
	payload = bodies[1]["payload"].(map[string]any)
	if _, present := payload["changedFiles"]; present {
		t.Errorf("payload.changedFiles present for empty result: %v", payload["changedFiles"])
	}
}
