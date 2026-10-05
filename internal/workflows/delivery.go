package workflows

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

// Mission branch delivery (mission-branch-delivery): a mission never writes
// the user's main checkout. Task branches merge into an integration branch
// (oa/mission-<m>) checked out in a dedicated worktree outside the repo;
// the user later delivers it explicitly (push that one branch, or fast-forward
// their own branch).

// ErrBaseUnreachable means the mission baseline commit exists neither locally
// nor after `git fetch origin`. The task must fail instead of silently
// starting from HEAD (which would diverge from its siblings).
var ErrBaseUnreachable = errors.New("base_unreachable")

// Delivery failure reasons reported in workflow:delivery_result.
const (
	ReasonNoBaseBranch   = "no_base_branch"
	ReasonWrongBranch    = "wrong_branch"
	ReasonDirtyWorktree  = "dirty_worktree"
	ReasonNotFastForward = "not_fast_forward"
	ReasonPushFailed     = "push_failed"
	ReasonFFFailed       = "ff_failed"
	ReasonInvalidBranch  = "invalid_branch"
)

// IntegrationBranch is the per-mission integration branch name.
func IntegrationBranch(missionID string) string { return "oa/mission-" + missionID }

// TaskWorktree describes a created/reused task worktree and its baseline.
type TaskWorktree struct {
	Path string
	// BaseBranch is the main checkout's branch at creation ("" when detached).
	BaseBranch string
	// BaseCommit is the commit the task branch was cut from.
	BaseCommit string
}

// repoKey identifies the repository (shared by all its linked worktrees):
// sha256 of the absolute git common dir, first 12 hex chars.
func (w *WorktreeManager) repoKey() string {
	out, err := w.git(w.projectDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	common := strings.TrimSpace(out)
	if err != nil || common == "" {
		common = filepath.Join(w.projectDir, ".git")
	}
	sum := sha256.Sum256([]byte(common))
	return hex.EncodeToString(sum[:])[:12]
}

// worktreeRoot is where new worktrees are created — outside the user's repo.
func (w *WorktreeManager) worktreeRoot() string {
	return filepath.Join(config.ConfigDir(), "worktrees", w.repoKey())
}

func (w *WorktreeManager) worktreePath(name string) string {
	return filepath.Join(w.worktreeRoot(), name)
}

// worktreeDirs lists every directory worktrees may live in (new, then legacy).
func (w *WorktreeManager) worktreeDirs() []string {
	return []string{w.worktreeRoot(), filepath.Join(w.projectDir, WorktreesDir)}
}

// findWorktree returns the path of an existing linked worktree called name
// (legacy location first, so an in-flight task keeps its checkout), or "".
func (w *WorktreeManager) findWorktree(name string) string {
	for _, p := range []string{
		filepath.Join(w.projectDir, WorktreesDir, name),
		w.worktreePath(name),
	} {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p
		}
	}
	return ""
}

func (w *WorktreeManager) git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitOut is like git but returns stdout only (stderr noise excluded).
func (w *WorktreeManager) gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (w *WorktreeManager) commitExists(commit string) bool {
	_, err := w.git(w.projectDir, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// resolveBase returns the commit a new branch must be cut from. A requested
// baseCommit that is not local triggers `fetch origin`; still missing →
// ErrBaseUnreachable. Without a request the main checkout's HEAD is used.
func (w *WorktreeManager) resolveBase(baseCommit string) (string, error) {
	if baseCommit == "" {
		head, err := w.gitOut(w.projectDir, "rev-parse", "HEAD")
		if err != nil {
			return "", fmt.Errorf("resolve HEAD: %w", err)
		}
		return head, nil
	}
	if !w.commitExists(baseCommit) {
		_, _ = w.git(w.projectDir, "fetch", "origin")
		if !w.commitExists(baseCommit) {
			return "", fmt.Errorf("%w: %s", ErrBaseUnreachable, baseCommit)
		}
	}
	full, err := w.gitOut(w.projectDir, "rev-parse", baseCommit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrBaseUnreachable, baseCommit)
	}
	return full, nil
}

// currentBranch is the main checkout's branch, "" when detached.
func (w *WorktreeManager) currentBranch() string {
	b, err := w.gitOut(w.projectDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || b == "HEAD" {
		return ""
	}
	return b
}

// CreateTaskWorktree creates (or reuses) the task worktree, cut from
// baseCommit when given (fixed mission baseline) or from HEAD otherwise.
// An existing worktree — in either location — is reused in place (known-issue
// #20 re-dispatch semantics).
func (w *WorktreeManager) CreateTaskWorktree(jobID, taskID, baseCommit string) (*TaskWorktree, error) {
	branchName := GetBranchName(jobID, taskID)
	baseBranch := w.currentBranch()

	if existing := w.findWorktree(branchName); existing != "" {
		base := baseCommit
		if base == "" {
			base, _ = w.gitOut(w.projectDir, "merge-base", "HEAD", branchName)
		}
		return &TaskWorktree{Path: existing, BaseBranch: baseBranch, BaseCommit: base}, nil
	}

	base, err := w.resolveBase(baseCommit)
	if err != nil {
		return nil, err
	}

	worktreePath := w.worktreePath(branchName)
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create worktrees directory: %w", err)
	}
	if out, err := w.git(w.projectDir, "worktree", "add", worktreePath, "-b", branchName, base); err != nil {
		return nil, fmt.Errorf("git worktree add failed: %s: %w", out, err)
	}
	return &TaskWorktree{Path: worktreePath, BaseBranch: baseBranch, BaseCommit: base}, nil
}

// MergeEvent is one per-branch outcome of IntegrateBranches. Status is one of
// merged | conflict | error | fetch_failed.
type MergeEvent struct {
	TaskID        string
	Status        string
	ConflictFiles []string
}

// IntegrationOutcome is the end state of an integration run.
type IntegrationOutcome struct {
	Branch     string
	Path       string
	HeadCommit string
	// LegacyBase is true when no baseCommit was supplied and the branch was
	// cut from the current HEAD (pre-baseline API).
	LegacyBase bool
	// Integrated is true only when every branch is in the integration branch.
	Integrated bool
}

// EnsureIntegrationWorktree creates (or reuses) the mission's integration
// worktree. An interrupted merge left in a reused tree is aborted first.
func (w *WorktreeManager) EnsureIntegrationWorktree(missionID, baseCommit string) (path, branch string, legacy bool, err error) {
	branch = IntegrationBranch(missionID)
	name := "mission-" + missionID
	if existing := w.findWorktree(name); existing != "" {
		w.abortMergeIfAny(existing)
		return existing, branch, false, nil
	}
	path = w.worktreePath(name)
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		return "", branch, false, fmt.Errorf("failed to create worktrees directory: %w", mkErr)
	}
	if w.refExists("refs/heads/" + branch) {
		if out, e := w.git(w.projectDir, "worktree", "add", path, branch); e != nil {
			return "", branch, false, fmt.Errorf("git worktree add failed: %s: %w", out, e)
		}
		return path, branch, false, nil
	}
	base, e := w.resolveBase(baseCommit)
	if e != nil {
		return "", branch, false, e
	}
	if out, e := w.git(w.projectDir, "worktree", "add", path, "-b", branch, base); e != nil {
		return "", branch, false, fmt.Errorf("git worktree add failed: %s: %w", out, e)
	}
	return path, branch, baseCommit == "", nil
}

// IntegrationWorktreePath returns the existing integration worktree for a
// mission, or "".
func (w *WorktreeManager) IntegrationWorktreePath(missionID string) string {
	return w.findWorktree("mission-" + missionID)
}

func (w *WorktreeManager) abortMergeIfAny(dir string) {
	if _, err := w.git(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		_, _ = w.git(dir, "merge", "--abort")
	}
}

// IntegrateBranches merges the given task branches, in order, into the
// mission integration branch (never the user's checkout, never pushing). For
// each branch emit is called with its outcome. The run stops at the first
// conflict or error (state stays well-defined; re-running skips what already
// landed). A conflicting merge is aborted, so the integration tree is left
// clean with earlier merges intact.
func (w *WorktreeManager) IntegrateBranches(missionID, baseCommit string, branches []BranchSpec, emit func(MergeEvent)) (*IntegrationOutcome, error) {
	path, branch, legacy, err := w.EnsureIntegrationWorktree(missionID, baseCommit)
	if err != nil {
		return nil, err
	}
	out := &IntegrationOutcome{Branch: branch, Path: path, LegacyBase: legacy}
	all := len(branches) > 0

	for _, br := range branches {
		ref, fetchErr := w.resolveBranchRef(br.BranchName)
		if fetchErr != nil {
			emit(MergeEvent{TaskID: br.TaskID, Status: "fetch_failed"})
			all = false
			break
		}

		// Already contained → idempotent skip.
		if _, e := w.git(path, "merge-base", "--is-ancestor", ref, "HEAD"); e == nil {
			w.reclaimMergedTask(missionID, br)
			emit(MergeEvent{TaskID: br.TaskID, Status: "merged"})
			continue
		}

		msg := "Merge task " + br.TaskID
		if br.Title != "" {
			msg = fmt.Sprintf("Merge task %s (%s)", br.Title, br.TaskID)
		}
		if _, mergeErr := w.git(path, "merge", "--no-ff", "-m", msg, ref); mergeErr != nil {
			files, _ := w.gitOut(path, "diff", "--name-only", "--diff-filter=U")
			w.abortMergeIfAny(path)
			if files != "" {
				emit(MergeEvent{TaskID: br.TaskID, Status: "conflict", ConflictFiles: strings.Split(files, "\n")})
			} else {
				emit(MergeEvent{TaskID: br.TaskID, Status: "error"})
			}
			all = false
			break
		}
		w.reclaimMergedTask(missionID, br)
		emit(MergeEvent{TaskID: br.TaskID, Status: "merged"})
	}

	head, _ := w.gitOut(path, "rev-parse", "HEAD")
	out.HeadCommit = head
	out.Integrated = all
	return out, nil
}

// resolveBranchRef prefers the local branch; otherwise fetches it from origin
// and uses origin/<branch>.
func (w *WorktreeManager) resolveBranchRef(branch string) (string, error) {
	if w.refExists("refs/heads/" + branch) {
		return branch, nil
	}
	if err := w.FetchBranch(branch); err != nil {
		return "", err
	}
	return "origin/" + branch, nil
}

// reclaimMergedTask removes a merged task's local worktree in place (no
// reliance on workflow:task_cleanup). Only when disposable. The branch is
// kept: it is a cheap ref, and a re-run of the merge resolves it locally and
// skips it as already contained (idempotence even without a remote).
func (w *WorktreeManager) reclaimMergedTask(missionID string, br BranchSpec) {
	name := br.BranchName
	path := w.findWorktree(name)
	if path == "" || !w.worktreeIsDisposable(path, name) {
		return
	}
	_, _ = w.git(w.projectDir, "worktree", "remove", path)
}

// DeliverResult is the outcome of Deliver.
type DeliverResult struct {
	OK         bool
	Reason     string
	HeadCommit string
	Detail     string
}

// Deliver performs an explicit delivery of the integration branch:
//   - push: pushes only oa/mission-<m> to origin;
//   - fast_forward: fast-forwards the user's baseBranch checkout, only if it
//     is on baseBranch, clean, and an ancestor of the integration branch.
func (w *WorktreeManager) Deliver(missionID, action, integrationBranch, baseBranch string) DeliverResult {
	want := IntegrationBranch(missionID)
	if integrationBranch == "" {
		integrationBranch = want
	}
	if integrationBranch != want {
		return DeliverResult{Reason: ReasonInvalidBranch, Detail: "integration branch must be " + want}
	}
	if !w.refExists("refs/heads/" + integrationBranch) {
		return DeliverResult{Reason: ReasonInvalidBranch, Detail: "integration branch does not exist"}
	}
	head, _ := w.gitOut(w.projectDir, "rev-parse", integrationBranch)

	switch action {
	case "push":
		refspec := "refs/heads/" + integrationBranch + ":refs/heads/" + integrationBranch
		if out, err := w.git(w.projectDir, "push", "origin", refspec); err != nil {
			return DeliverResult{Reason: ReasonPushFailed, Detail: strings.TrimSpace(out)}
		}
		return DeliverResult{OK: true, HeadCommit: head}
	case "fast_forward":
		if baseBranch == "" {
			return DeliverResult{Reason: ReasonNoBaseBranch}
		}
		if w.currentBranch() != baseBranch {
			return DeliverResult{Reason: ReasonWrongBranch}
		}
		if st, err := w.gitOut(w.projectDir, "status", "--porcelain", "--untracked-files=no"); err != nil || st != "" {
			return DeliverResult{Reason: ReasonDirtyWorktree}
		}
		if _, err := w.git(w.projectDir, "merge-base", "--is-ancestor", "HEAD", integrationBranch); err != nil {
			return DeliverResult{Reason: ReasonNotFastForward}
		}
		if out, err := w.git(w.projectDir, "merge", "--ff-only", integrationBranch); err != nil {
			return DeliverResult{Reason: ReasonFFFailed, Detail: strings.TrimSpace(out)}
		}
		// The checkout of the integration branch is no longer needed; the
		// branch itself stays.
		if p := w.IntegrationWorktreePath(missionID); p != "" {
			_, _ = w.git(w.projectDir, "worktree", "remove", p)
		}
		return DeliverResult{OK: true, HeadCommit: head}
	default:
		return DeliverResult{Reason: ReasonInvalidBranch, Detail: "unknown action " + action}
	}
}
