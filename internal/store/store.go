// Package store materializes deployment source trees from R2 into bare git
// repos (design D1: R2 tree -> single-commit bare repo -> git http on the
// loopback for Coolify to pull).
//
// Layout under the cache dir:
//
//	objects/{sha256}        content-addressed, sha-verified object store
//	repos/{deploymentID}.git  the bare repos githttp serves
//	repos/{deploymentID}.tmp  staging scratch; never survives a failed run
//
// Materialization is a pure function of the manifest: every run rebuilds the
// work tree from scratch in a temp dir and swaps atomically, so a crash or a
// retry never leaves a half-built {id}.git behind (spec: 失败可重入，不残留
// 半成品 repo 状态).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

// Presigner is the seam to the API (implemented by api.Client; faked in
// tests). Keeping it an interface keeps this package's heavy logic testable
// without a network boundary.
type Presigner interface {
	PresignSource(ctx context.Context, deploymentID string, shas []string) (*api.PresignResponse, error)
}

// Store materializes trees and owns the on-disk footprint per deployment.
type Store struct {
	Dir       string
	Presigner Presigner
	HTTP      *http.Client
}

// New builds a store rooted at dir.
func New(dir string, p Presigner) *Store {
	return &Store{
		Dir:       dir,
		Presigner: p,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *Store) objectPath(sha string) string { return filepath.Join(s.Dir, "objects", sha) }

// RepoPath is the bare repo path for a deployment (what githttp serves).
func (s *Store) RepoPath(deploymentID string) string {
	return filepath.Join(s.Dir, "repos", deploymentID+".git")
}

// Remove drops the deployment's repo (delete-path cleanup; the object store
// is shared and stays).
func (s *Store) Remove(deploymentID string) error {
	if err := os.RemoveAll(s.RepoPath(deploymentID)); err != nil {
		return err
	}
	return os.RemoveAll(s.RepoPath(deploymentID) + ".swap")
}

// fixedCommitDate pins author/committer timestamps so the same manifest
// always yields the same commit hash -- repeat materialization is verifiably
// identical (spec: 同一部署重复物化产出内容一致的 repo), and a re-materialize
// that reports a different commit would be a bug, not a new snapshot.
const fixedCommitDate = "1970-01-01T00:00:00Z"

// Materialize ensures every manifest object exists locally (verified by
// sha256), then produces repos/{deploymentID}.git: a bare repo whose single
// commit contains exactly the manifest tree. Returns the commit hash.
//
// Presign batches are re-fetched wholesale on any failure (URLs expire as a
// set; partial retry against a stale set is pointless) -- same contract as
// node-runner's puller.
func (s *Store) Materialize(ctx context.Context, deploymentID, sourcePrefix string, manifest []api.File) (string, error) {
	if len(manifest) == 0 {
		return "", fmt.Errorf("materialize %s: empty manifest", deploymentID)
	}

	// 1. Content store: which objects are missing?
	var missing []string
	for _, f := range manifest {
		if !s.hasObject(f.SHA256) {
			missing = append(missing, f.SHA256)
		}
	}
	for i := 0; i < len(missing); i += 500 {
		end := min(i+500, len(missing))
		if err := s.fetchBatch(ctx, deploymentID, sourcePrefix, missing[i:end]); err != nil {
			return "", fmt.Errorf("materialize %s: %w", deploymentID, err)
		}
	}

	// 2. Assemble the work tree in scratch space.
	work := filepath.Join(s.Dir, "repos", deploymentID+".work")
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	for _, f := range manifest {
		dst := filepath.Join(work, filepath.FromSlash(f.Path))
		if filepath.Clean(dst) != dst || !strings.HasPrefix(dst, work+string(os.PathSeparator)) {
			return "", fmt.Errorf("materialize %s: manifest path escapes root: %s", deploymentID, f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := hardlinkOrCopy(s.objectPath(f.SHA256), dst); err != nil {
			return "", fmt.Errorf("materialize %s: place %s: %w", deploymentID, f.Path, err)
		}
	}

	// 3. Single-commit repo, then bare-copy and atomic swap.
	if err := runGit(ctx, work, "init", "--quiet", "--initial-branch=main"); err != nil {
		return "", fmt.Errorf("materialize %s: git init: %w", deploymentID, err)
	}
	if err := runGit(ctx, work, "add", "-A"); err != nil {
		return "", fmt.Errorf("materialize %s: git add: %w", deploymentID, err)
	}
	gitEnv := []string{
		"GIT_AUTHOR_NAME=Open Agents Deploy",
		"GIT_AUTHOR_EMAIL=deploy@openagents.top",
		"GIT_COMMITTER_NAME=Open Agents Deploy",
		"GIT_COMMITTER_EMAIL=deploy@openagents.top",
		"GIT_AUTHOR_DATE=" + fixedCommitDate,
		"GIT_COMMITTER_DATE=" + fixedCommitDate,
	}
	if err := runGitEnv(ctx, gitEnv, work, "commit", "--quiet", "-m", "deployment snapshot"); err != nil {
		return "", fmt.Errorf("materialize %s: git commit: %w", deploymentID, err)
	}
	out, err := outputGit(ctx, work, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("materialize %s: rev-parse: %w", deploymentID, err)
	}
	commit := strings.TrimSpace(out)

	final := s.RepoPath(deploymentID)
	swap := final + ".swap"
	if err := os.RemoveAll(swap); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "clone", "--quiet", "--bare", work, swap); err != nil {
		return "", fmt.Errorf("materialize %s: bare clone: %w", deploymentID, err)
	}
	// A concurrent http-backend may hold the old repo open mid-pull; removing
	// the previous final first means the swap is rename-atomic for new opens.
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(swap, final); err != nil {
		return "", fmt.Errorf("materialize %s: swap repo in: %w", deploymentID, err)
	}
	return commit, nil
}

// hasObject reports whether a verified copy of sha exists in the store.
func (s *Store) hasObject(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	_, err := os.Stat(s.objectPath(sha))
	return err == nil
}

// fetchBatch presigns one batch and downloads + verifies each object. Any
// failure fails the whole batch so the caller re-signs (fresh 15-min URLs).
func (s *Store) fetchBatch(ctx context.Context, deploymentID, sourcePrefix string, shas []string) error {
	resp, err := s.Presigner.PresignSource(ctx, deploymentID, shas)
	if err != nil {
		return fmt.Errorf("presign: %w", err)
	}
	byKey := map[string]string{}
	for _, obj := range resp.URLs {
		// key = deploys/{deploymentId}/{sha} -- the sha is the tail
		if i := strings.LastIndex(obj.Key, "/"); i >= 0 {
			byKey[obj.Key[i+1:]] = obj.URL
		}
	}
	for _, sha := range shas {
		url, ok := byKey[sha]
		if !ok {
			return fmt.Errorf("server did not presign %s", sha)
		}
		if err := s.downloadVerified(ctx, url, sha); err != nil {
			return err
		}
	}
	return nil
}

// downloadVerified streams url to the content store only if the body hashes
// to sha; a mismatch removes the partial file and fails the batch.
func (s *Store) downloadVerified(ctx context.Context, url, sha string) error {
	if err := os.MkdirAll(filepath.Dir(s.objectPath(sha)), 0o755); err != nil {
		return err
	}
	tmp := s.objectPath(sha) + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		f.Close()
		return fmt.Errorf("get %s: %w", sha[:12], err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.Close()
		return fmt.Errorf("get %s: status %d", sha[:12], resp.StatusCode)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("stream %s: %w", sha[:12], err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return fmt.Errorf("sha256 mismatch for %s: got %s", sha[:12], got[:12])
	}
	return os.Rename(tmp, s.objectPath(sha))
}

// hardlinkOrCopy links the verified object into the tree (dedupe without a
// second copy on disk); filesystems that refuse links get a copy. See
// node-runner's puller for the EEXIST dance this mirrors.
func hardlinkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return err
		}
		if err := os.Link(src, dst); err == nil {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// runGit / runGitEnv / outputGit wrap the git binary. Env vars come first so
// a hostile inherited GIT_DIR cannot hijack the command.
func runGit(ctx context.Context, dir string, args ...string) error {
	return runGitEnv(ctx, nil, dir, args...)
}

func runGitEnv(ctx context.Context, env []string, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(env, "PATH="+os.Getenv("PATH"), "HOME="+dir)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func outputGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
