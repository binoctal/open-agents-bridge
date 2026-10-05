package workflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// newRepoWithOrigin creates a user checkout on branch main with one commit and
// a bare origin it is pushed to.
func newRepoWithOrigin(t *testing.T) (repo, origin string) {
	t.Helper()
	origin = filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, filepath.Dir(origin), "init", "--bare", "-b", "main", origin)
	repo = t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	gitIn(t, repo, "config", "user.email", "t@t")
	gitIn(t, repo, "config", "user.name", "t")
	gitIn(t, repo, "remote", "add", "origin", origin)
	writeFile(t, repo, "base.txt", "base\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-m", "init")
	gitIn(t, repo, "push", "origin", "main")
	return repo, origin
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func head(t *testing.T, dir string) string {
	return strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))
}

func originRefs(t *testing.T, origin string) string {
	return gitIn(t, origin, "for-each-ref")
}

// taskWithCommit creates a task worktree from base and commits one file.
func taskWithCommit(t *testing.T, w *WorktreeManager, job, task, base, file, content string) BranchSpec {
	t.Helper()
	tw, err := w.CreateTaskWorktree(job, task, base)
	if err != nil {
		t.Fatalf("CreateTaskWorktree %s: %v", task, err)
	}
	writeFile(t, tw.Path, file, content)
	gitIn(t, tw.Path, "add", "-A")
	gitIn(t, tw.Path, "commit", "-m", "task "+task)
	return BranchSpec{TaskID: task, BranchName: GetBranchName(job, task), Title: task}
}

func collect(events *[]MergeEvent) func(MergeEvent) {
	return func(e MergeEvent) { *events = append(*events, e) }
}

func TestTaskWorktreeLivesOutsideUserRepo(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	before := gitIn(t, repo, "status", "--porcelain")
	tw, err := w.CreateTaskWorktree("m1", "t1", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(tw.Path, repo) {
		t.Fatalf("worktree %q is inside the user repo %q", tw.Path, repo)
	}
	if _, err := os.Stat(filepath.Join(repo, WorktreesDir)); err == nil {
		t.Fatal("legacy in-repo worktrees dir must not be created")
	}
	if after := gitIn(t, repo, "status", "--porcelain"); after != before {
		t.Fatalf("user checkout status changed: %q -> %q", before, after)
	}
	if tw.BaseBranch != "main" || tw.BaseCommit != head(t, repo) {
		t.Fatalf("base = %q/%q", tw.BaseBranch, tw.BaseCommit)
	}
}

func TestBaseCommitPinnedAcrossUserCommits(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	base := head(t, repo)
	writeFile(t, repo, "later.txt", "x\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-m", "user moves on")
	gitIn(t, repo, "checkout", "-b", "other")

	tw, err := w.CreateTaskWorktree("m1", "t2", base)
	if err != nil {
		t.Fatal(err)
	}
	if got := head(t, tw.Path); got != base {
		t.Fatalf("task cut from %s, want pinned base %s", got, base)
	}
}

func TestUnreachableBaseFailsInsteadOfFallingBackToHead(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	_, err := w.CreateTaskWorktree("m1", "t3", strings.Repeat("a", 40))
	if err == nil || !strings.Contains(err.Error(), "base_unreachable") {
		t.Fatalf("err = %v, want base_unreachable", err)
	}
	if branchExists(t, repo, GetBranchName("m1", "t3")) {
		t.Fatal("no branch may be created for an unreachable base")
	}
}

func TestLegacyInRepoWorktreeIsReused(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	legacy := filepath.Join(repo, WorktreesDir, GetBranchName("m1", "t4"))
	gitIn(t, repo, "worktree", "add", legacy, "-b", GetBranchName("m1", "t4"))
	tw, err := w.CreateTaskWorktree("m1", "t4", "")
	if err != nil {
		t.Fatal(err)
	}
	if tw.Path != legacy {
		t.Fatalf("path = %q, want legacy %q", tw.Path, legacy)
	}
}

func TestIntegrationLeavesDirtyUserCheckoutOnOtherBranchUntouched(t *testing.T) {
	repo, origin := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	base := head(t, repo)
	a := taskWithCommit(t, w, "m1", "a", base, "a.txt", "a\n")
	b := taskWithCommit(t, w, "m1", "b", base, "b.txt", "b\n")

	// The user is on another branch with uncommitted edits.
	gitIn(t, repo, "checkout", "-b", "feature")
	writeFile(t, repo, "base.txt", "dirty edit\n")
	writeFile(t, repo, "scratch.txt", "untracked\n")
	statusBefore := gitIn(t, repo, "status", "--porcelain")
	headBefore := head(t, repo)
	refsBefore := originRefs(t, origin)

	var ev []MergeEvent
	out, err := w.IntegrateBranches("m1", base, []BranchSpec{a, b}, collect(&ev))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Integrated || out.Branch != "oa/mission-m1" || out.LegacyBase {
		t.Fatalf("outcome = %+v", out)
	}
	if len(ev) != 2 || ev[0].Status != "merged" || ev[1].Status != "merged" {
		t.Fatalf("events = %+v", ev)
	}
	if gitIn(t, repo, "status", "--porcelain") != statusBefore || head(t, repo) != headBefore {
		t.Fatal("user checkout changed by integration")
	}
	if originRefs(t, origin) != refsBefore {
		t.Fatal("integration must not push anything")
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(out.Path, f)); err != nil {
			t.Fatalf("integration tree missing %s", f)
		}
	}
	// Merged task worktrees were reclaimed in place, with their branches.
	if w.findWorktree(a.BranchName) != "" {
		t.Fatal("merged task worktree should be reclaimed in place")
	}
}

func TestConflictLeavesCleanIntegrationTreeAndKeepsEarlierMerges(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	base := head(t, repo)
	a := taskWithCommit(t, w, "m2", "a", base, "x.txt", "from a\n")
	b := taskWithCommit(t, w, "m2", "b", base, "x.txt", "from b\n")
	c := taskWithCommit(t, w, "m2", "c", base, "c.txt", "c\n")

	var ev []MergeEvent
	out, err := w.IntegrateBranches("m2", base, []BranchSpec{a, b, c}, collect(&ev))
	if err != nil {
		t.Fatal(err)
	}
	if out.Integrated {
		t.Fatal("conflicting mission must not be integrated")
	}
	if len(ev) != 2 || ev[1].Status != "conflict" || len(ev[1].ConflictFiles) != 1 || ev[1].ConflictFiles[0] != "x.txt" {
		t.Fatalf("events = %+v (the run must stop at the conflict)", ev)
	}
	if st := gitIn(t, out.Path, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("integration tree not clean: %q", st)
	}
	if _, err := os.Stat(filepath.Join(out.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(out.Path, "x.txt")); string(got) != "from a\n" {
		t.Fatalf("earlier merge lost: x.txt = %q", got)
	}
	// c was never attempted.
	if _, err := os.Stat(filepath.Join(out.Path, "c.txt")); err == nil {
		t.Fatal("run must stop at the first conflict")
	}
}

func TestIntegrationRerunIsIdempotent(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	base := head(t, repo)
	a := taskWithCommit(t, w, "m3", "a", base, "a.txt", "a\n")
	b := taskWithCommit(t, w, "m3", "b", base, "b.txt", "b\n")

	var ev1, ev2 []MergeEvent
	out1, err := w.IntegrateBranches("m3", base, []BranchSpec{a, b}, collect(&ev1))
	if err != nil || !out1.Integrated {
		t.Fatalf("first run: %+v %v", out1, err)
	}
	out2, err := w.IntegrateBranches("m3", base, []BranchSpec{a, b}, collect(&ev2))
	if err != nil || !out2.Integrated {
		t.Fatalf("rerun: %+v %v", out2, err)
	}
	for _, e := range ev2 {
		if e.Status != "merged" {
			t.Fatalf("rerun events = %+v", ev2)
		}
	}
	if out2.HeadCommit != out1.HeadCommit {
		t.Fatalf("rerun moved the integration head: %s -> %s", out1.HeadCommit, out2.HeadCommit)
	}
	// Merged task worktrees are gone, branches stay.
	if w.findWorktree(a.BranchName) != "" || !branchExists(t, repo, a.BranchName) {
		t.Fatal("expected worktree reclaimed and branch kept")
	}
}

func TestLegacyBaseFlaggedWhenNoBaseCommit(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	a := taskWithCommit(t, w, "m4", "a", "", "a.txt", "a\n")
	out, err := w.IntegrateBranches("m4", "", []BranchSpec{a}, func(MergeEvent) {})
	if err != nil || !out.LegacyBase || !out.Integrated {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func integrated(t *testing.T, w *WorktreeManager, repo, mission string) {
	t.Helper()
	base := head(t, repo)
	a := taskWithCommit(t, w, mission, "a", base, "a.txt", "a\n")
	out, err := w.IntegrateBranches(mission, base, []BranchSpec{a}, func(MergeEvent) {})
	if err != nil || !out.Integrated {
		t.Fatalf("setup integration failed: %+v %v", out, err)
	}
}

func TestDeliverFastForwardRefusals(t *testing.T) {
	repo, _ := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	integrated(t, w, repo, "m5")

	if r := w.Deliver("m5", "fast_forward", "oa/mission-m5", ""); r.Reason != ReasonNoBaseBranch {
		t.Fatalf("empty base: %+v", r)
	}
	if r := w.Deliver("m5", "fast_forward", "oa/mission-m5", "develop"); r.Reason != ReasonWrongBranch {
		t.Fatalf("wrong branch: %+v", r)
	}
	writeFile(t, repo, "base.txt", "dirty\n")
	if r := w.Deliver("m5", "fast_forward", "oa/mission-m5", "main"); r.Reason != ReasonDirtyWorktree {
		t.Fatalf("dirty: %+v", r)
	}
	gitIn(t, repo, "checkout", "base.txt")
	writeFile(t, repo, "user.txt", "u\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-m", "user commit")
	headBefore := head(t, repo)
	if r := w.Deliver("m5", "fast_forward", "oa/mission-m5", "main"); r.Reason != ReasonNotFastForward {
		t.Fatalf("diverged: %+v", r)
	}
	if head(t, repo) != headBefore {
		t.Fatal("refused delivery must not move HEAD")
	}
}

func TestDeliverFastForwardSucceedsAndOriginUntouched(t *testing.T) {
	repo, origin := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	integrated(t, w, repo, "m6")
	refs := originRefs(t, origin)

	r := w.Deliver("m6", "fast_forward", "oa/mission-m6", "main")
	if !r.OK || r.HeadCommit == "" {
		t.Fatalf("ff: %+v", r)
	}
	if head(t, repo) != r.HeadCommit {
		t.Fatal("main did not fast-forward")
	}
	if _, err := os.Stat(filepath.Join(repo, "a.txt")); err != nil {
		t.Fatal("delivered file missing")
	}
	if originRefs(t, origin) != refs {
		t.Fatal("fast_forward must not push")
	}
	if w.IntegrationWorktreePath("m6") != "" {
		t.Fatal("integration checkout should be reclaimed after fast-forward")
	}
	if !branchExists(t, repo, "oa/mission-m6") {
		t.Fatal("integration branch must be kept")
	}
}

func TestDeliverPushSendsOnlyIntegrationBranch(t *testing.T) {
	repo, origin := newRepoWithOrigin(t)
	w := NewWorktreeManager(repo)
	integrated(t, w, repo, "m7")
	mainBefore := strings.TrimSpace(gitIn(t, origin, "rev-parse", "main"))

	if r := w.Deliver("m7", "push", "oa/mission-m7", "main"); !r.OK {
		t.Fatalf("push: %+v", r)
	}
	if got := strings.TrimSpace(gitIn(t, origin, "rev-parse", "main")); got != mainBefore {
		t.Fatal("origin main moved")
	}
	if !strings.Contains(originRefs(t, origin), "refs/heads/oa/mission-m7") {
		t.Fatal("integration branch not on origin")
	}
	if r := w.Deliver("m7", "push", "main", "main"); r.Reason != ReasonInvalidBranch {
		t.Fatalf("pushing a non-integration branch must be refused: %+v", r)
	}
}

// Delivery safety: nothing in the shipped bridge may push HEAD.
func TestNoPushHeadInBridgeSource(t *testing.T) {
	bad := regexp.MustCompile(`"push"[^\n]*"HEAD"|PushMain|push origin HEAD`)
	root := filepath.Join("..", "..")
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if loc := bad.Find(b); loc != nil {
			t.Errorf("%s contains a push-HEAD call: %s", p, loc)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
