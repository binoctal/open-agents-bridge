package workflows

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const WorktreesDir = ".open-agents-bridge-worktrees"

// WorktreeManager manages git worktrees for parallel task execution
type WorktreeManager struct {
	projectDir string // Base project directory
}

// NewWorktreeManager creates a new worktree manager for the given project directory
func NewWorktreeManager(projectDir string) *WorktreeManager {
	return &WorktreeManager{projectDir: projectDir}
}

// IsGitRepo checks if the project directory is a git repository
func (w *WorktreeManager) IsGitRepo() bool {
	gitDir := filepath.Join(w.projectDir, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return false
	}
	// .git can be a directory or a file (git worktree reference)
	return info.IsDir() || !info.IsDir()
}

// CreateWorktree creates a new git worktree for the given task branch
// Returns the worktree path or an error
func (w *WorktreeManager) CreateWorktree(jobID, taskID string) (string, error) {
	branchName := fmt.Sprintf("task-%s-%s", jobID, taskID)
	worktreePath := filepath.Join(w.projectDir, WorktreesDir, branchName)

	// Ensure worktrees directory exists
	worktreesDir := filepath.Join(w.projectDir, WorktreesDir)
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create worktrees directory: %w", err)
	}

	// Create worktree with new branch from HEAD
	cmd := exec.Command("git", "worktree", "add", worktreePath, "-b", branchName)
	cmd.Dir = w.projectDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add failed: %s: %w", string(output), err)
	}

	return worktreePath, nil
}

// CommitAll commits all changes in the worktree
// Returns the commit hash or an error
func (w *WorktreeManager) CommitAll(worktreePath, taskID, taskTitle string) (string, error) {
	// Check if there are changes to commit
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git status failed: %w", err)
	}

	if len(strings.TrimSpace(string(output))) == 0 {
		// No changes to commit
		return "", nil
	}

	// Stage all changes
	cmd = exec.Command("git", "add", "-A")
	cmd.Dir = worktreePath
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add failed: %s: %w", string(output), err)
	}

	// Commit
	commitMsg := fmt.Sprintf("task(%s): %s", taskID, taskTitle)
	cmd = exec.Command("git", "commit", "-m", commitMsg)
	cmd.Dir = worktreePath
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit failed: %s: %w", string(output), err)
	}

	// Get commit hash
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	hashOutput, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse failed: %w", err)
	}

	return strings.TrimSpace(string(hashOutput)), nil
}

// RemoveWorktree removes a worktree and its branch
func (w *WorktreeManager) RemoveWorktree(jobID, taskID string) error {
	branchName := fmt.Sprintf("task-%s-%s", jobID, taskID)
	worktreePath := filepath.Join(w.projectDir, WorktreesDir, branchName)

	// Remove the worktree
	cmd := exec.Command("git", "worktree", "remove", worktreePath, "--force")
	cmd.Dir = w.projectDir
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove failed: %s: %w", string(output), err)
	}

	// Delete the branch
	cmd = exec.Command("git", "branch", "-D", branchName)
	cmd.Dir = w.projectDir
	if output, err := cmd.CombinedOutput(); err != nil {
		// Branch deletion failure is non-critical, log but don't error
		_ = output
	}

	return nil
}

// MergeBranch merges a task branch into the current branch (main)
// Returns conflict files if merge conflict occurs
func (w *WorktreeManager) MergeBranch(jobID, taskID string) (conflictFiles []string, err error) {
	branchName := fmt.Sprintf("task-%s-%s", jobID, taskID)

	cmd := exec.Command("git", "merge", branchName)
	cmd.Dir = w.projectDir
	output, mergeErr := cmd.CombinedOutput()

	if mergeErr != nil {
		// Check if it's a merge conflict
		outputStr := string(output)
		if strings.Contains(outputStr, "CONFLICT") {
			conflicts := parseConflictFiles(outputStr)
			return conflicts, nil
		}
		return nil, fmt.Errorf("git merge failed: %s: %w", outputStr, mergeErr)
	}

	return nil, nil
}

// parseConflictFiles extracts conflict file paths from git merge output
func parseConflictFiles(mergeOutput string) []string {
	var files []string
	lines := strings.Split(mergeOutput, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "CONFLICT") {
			// Extract file path from lines like:
			// CONFLICT (content): Merge conflict in path/to/file
			parts := strings.SplitN(line, " in ", 2)
			if len(parts) == 2 {
				files = append(files, strings.TrimSpace(parts[1]))
			} else {
				// CONFLICT (add/add): Merge conflict in path
				parts = strings.SplitN(line, "conflict in ", 2)
				if len(parts) == 2 {
					files = append(files, strings.TrimSpace(parts[1]))
				}
			}
		}
	}
	return files
}

// CleanupStaleWorktrees scans and removes worktrees not belonging to active tasks
func (w *WorktreeManager) CleanupStaleWorktrees(activeTaskIDs map[string]bool) ([]string, error) {
	worktreesDir := filepath.Join(w.projectDir, WorktreesDir)

	entries, err := os.ReadDir(worktreesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read worktrees directory: %w", err)
	}

	var cleaned []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dirName := entry.Name()
		// Directory name format: task-{jobId}-{taskId}
		// Extract taskId from the directory name
		parts := strings.SplitN(dirName, "-", 3)
		if len(parts) < 3 {
			continue
		}

		taskID := strings.Join(parts[2:], "-")

		// If this task is still active, skip
		if activeTaskIDs[taskID] {
			continue
		}

		// Remove stale worktree
		worktreePath := filepath.Join(worktreesDir, dirName)
		cmd := exec.Command("git", "worktree", "remove", worktreePath, "--force")
		cmd.Dir = w.projectDir
		if err := cmd.Run(); err != nil {
			// If git worktree remove fails, try force directory removal
			os.RemoveAll(worktreePath)
		}

		// Delete the branch
		cmd = exec.Command("git", "branch", "-D", dirName)
		cmd.Dir = w.projectDir
		cmd.Run()

		cleaned = append(cleaned, dirName)
	}

	return cleaned, nil
}

// GetBranchName returns the branch name for a given job and task
func GetBranchName(jobID, taskID string) string {
	return fmt.Sprintf("task-%s-%s", jobID, taskID)
}

// BranchSpec defines a branch to merge in multi-device workflows
type BranchSpec struct {
	TaskID     string
	BranchName string
}

// PushBranch pushes a branch from a worktree to the remote origin
func (w *WorktreeManager) PushBranch(worktreePath, branchName string) error {
	cmd := exec.Command("git", "push", "origin", branchName)
	cmd.Dir = worktreePath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push failed: %s: %w", string(output), err)
	}
	return nil
}

// FetchBranch fetches a specific branch from origin
func (w *WorktreeManager) FetchBranch(branchName string) error {
	cmd := exec.Command("git", "fetch", "origin", branchName)
	cmd.Dir = w.projectDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch failed: %s: %w", string(output), err)
	}
	return nil
}

// MergeBranchByRef merges a branch (already fetched) into the current branch
func (w *WorktreeManager) MergeBranchByRef(branchName string) (conflictFiles []string, err error) {
	// Use FETCH_HEAD reference (from fetch) or the branch name directly
	cmd := exec.Command("git", "merge", fmt.Sprintf("origin/%s", branchName))
	cmd.Dir = w.projectDir
	output, mergeErr := cmd.CombinedOutput()

	if mergeErr != nil {
		outputStr := string(output)
		if strings.Contains(outputStr, "CONFLICT") {
			conflicts := parseConflictFiles(outputStr)
			return conflicts, nil
		}
		return nil, fmt.Errorf("git merge failed: %s: %w", outputStr, mergeErr)
	}

	return nil, nil
}

// PushMain pushes the main branch to origin
func (w *WorktreeManager) PushMain() error {
	cmd := exec.Command("git", "push", "origin", "HEAD")
	cmd.Dir = w.projectDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git push main failed: %s: %w", string(output), err)
	}
	return nil
}
