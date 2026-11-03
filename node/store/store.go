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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/binoctal/open-agents-bridge/node/api"
	"github.com/binoctal/open-agents-bridge/node/compose"
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

	// ScratchRoot gates same-machine declare-only manifest entries
	// (byo-hosting-node D3): a localPath outside this prefix refuses the
	// materialize (path-escape guard). Empty = localPath entries refuse --
	// the platform node never configures one, so a stray local manifest
	// there fails closed instead of reading an arbitrary path.
	ScratchRoot string
}

// New builds a store rooted at dir.
func New(dir string, p Presigner) *Store {
	return &Store{
		Dir:       dir,
		Presigner: p,
		// The store's only HTTP use is downloading manifest objects from
		// R2, and image-tier tars run to 4.5 GiB (docker-image-deploy-tier)
		// -- the ceiling is a blob-download budget, not a chatty-API one.
		// 60s was sized for source-tree objects; a 2.4 GiB tar died at the
		// mark mid-body (10-31 staging run). Presigns ride api.Client's own
		// short-timeout transport and are unaffected. Failed downloads
		// re-presign wholesale, so a slow batch never leans on stale URLs.
		HTTP: &http.Client{Timeout: 60 * time.Minute},
	}
}

func (s *Store) objectPath(sha string) string { return filepath.Join(s.Dir, "objects", sha) }

// RepoPath is the bare repo path for a deployment (what githttp serves).
func (s *Store) RepoPath(deploymentID string) string {
	return filepath.Join(s.Dir, "repos", deploymentID+".git")
}

// Remove drops the deployment's repo and worktree (delete-path cleanup; the
// object store is shared and stays).
func (s *Store) Remove(deploymentID string) error {
	if err := os.RemoveAll(s.RepoPath(deploymentID)); err != nil {
		return err
	}
	if err := os.RemoveAll(s.RepoPath(deploymentID) + ".swap"); err != nil {
		return err
	}
	return os.RemoveAll(s.WorktreePath(deploymentID))
}

// WorktreePath is the plain (non-git) tree a user node composes up from
// (byo-hosting-node D3: no Coolify pulls, the agent itself runs compose).
func (s *Store) WorktreePath(deploymentID string) string {
	return filepath.Join(s.Dir, "worktrees", deploymentID)
}

// fixedCommitDate pins author/committer timestamps so the same manifest
// always yields the same commit hash -- repeat materialization is verifiably
// identical (spec: 同一部署重复物化产出内容一致的 repo), and a re-materialize
// that reports a different commit would be a bug, not a new snapshot.
const fixedCommitDate = "1970-01-01T00:00:00Z"

// presignFunc is the batch-presign seam: hosted deployments adapt
// PresignSource, runtime previews adapt RuntimePresign. Both answer with
// R2 keys whose tail segment is the sha, so fetchBatch is prefix-agnostic.
type presignFunc func(ctx context.Context, shas []string) (*api.PresignResponse, error)

// Materialize ensures every manifest object exists locally (verified by
// sha256), then produces repos/{deploymentID}.git: a bare repo whose single
// commit contains exactly the manifest tree. Returns the commit hash.
//
// Presign batches are re-fetched wholesale on any failure (URLs expire as a
// set; partial retry against a stale set is pointless) -- same contract as
// node-runner's puller. Presigns go to the store's configured Presigner; use
// MaterializeVia to route them to a specific endpoint (dual-home D2: each
// environment has its own R2 bucket and credentials).
func (s *Store) Materialize(ctx context.Context, deploymentID, sourcePrefix string, manifest []api.File) (string, error) {
	return s.MaterializeVia(ctx, s.Presigner, deploymentID, sourcePrefix, manifest)
}

// MaterializeVia is Materialize with the presign origin pinned to p -- the
// client of the endpoint that offered the work item.
func (s *Store) MaterializeVia(ctx context.Context, p Presigner, deploymentID, sourcePrefix string, manifest []api.File) (string, error) {
	presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return p.PresignSource(ctx, deploymentID, shas)
	}
	if err := s.FetchMissing(ctx, manifest, presign); err != nil {
		return "", fmt.Errorf("materialize %s: %w", deploymentID, err)
	}
	return s.BuildRepo(ctx, deploymentID, manifest, nil, true)
}

// MaterializeCompose is the compose-stack variant (hosted-compose-stacks
// 5.2): after fetching the tree it reads the ORIGINAL compose file from the
// object store, re-validates it against the rule table and rewrites it
// (limits injected, non-ingress host ports stripped, ingress bound to the
// negotiated loopback slot, profiles activated). The rewritten bytes ride
// BuildRepo's extra channel, which overwrites the manifest copy at the same
// path -- Coolify pulls a repo whose compose file is the deployable one.
// staticFallback is off: compose trees never reach the nixpacks heuristic,
// Coolify builds them as build_pack=dockercompose.
//
// Image tier narrows the TREE, not the fetch (image-tier-compose-only-
// materialize): every manifest object still lands in the store (docker load
// reads tars from ObjectPath, never from the repo), but tar entries are
// dropped from BuildRepo's manifest so the commit Coolify clones carries
// only the compose file and the small files -- a multi-GiB tar in the tree
// bought nothing but a clone+checkout that times out on tight disks
// (2026-11-01 vnivni: 4min loadComposeFile failures; compose-only trees
// deploy in 1-2.5min end to end). The .tar suffix filter is deliberately
// the same rule loadImageTars uses to FIND the tars, so the two sides
// cannot drift on what counts as an image tar. Source tier keeps the full
// manifest in the tree (Coolify builds it on the node).
func (s *Store) MaterializeCompose(ctx context.Context, p Presigner, deploymentID, sourcePrefix string, manifest []api.File, spec api.ComposeSpec) (string, error) {
	presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return p.PresignSource(ctx, deploymentID, shas)
	}
	if err := s.FetchMissing(ctx, manifest, presign); err != nil {
		return "", fmt.Errorf("materialize %s: %w", deploymentID, err)
	}
	var composeSHA string
	for _, f := range manifest {
		if f.Path == spec.ComposePath {
			composeSHA = f.SHA256
			break
		}
	}
	if composeSHA == "" {
		return "", fmt.Errorf("materialize %s: compose path %s not in manifest", deploymentID, spec.ComposePath)
	}
	raw, err := s.ObjectBytes(composeSHA)
	if err != nil {
		return "", fmt.Errorf("materialize %s: read compose source: %w", deploymentID, err)
	}
	rewritten, err := compose.Rewrite(raw, spec)
	if err != nil {
		return "", fmt.Errorf("materialize %s: %w", deploymentID, err)
	}
	treeManifest := manifest
	if spec.Tier == "image" {
		treeManifest = make([]api.File, 0, len(manifest))
		for _, f := range manifest {
			if strings.HasSuffix(f.Path, ".tar") {
				continue
			}
			treeManifest = append(treeManifest, f)
		}
	}
	return s.BuildRepo(ctx, deploymentID, treeManifest, map[string][]byte{spec.ComposePath: rewritten}, false)
}

// FetchMissing ensures every manifest object exists locally, presigning via
// fn in batches of 500 (add-dynamic-preview-compute 5.2: the runtime preview
// path reuses the whole pipeline through this seam, only the presign origin
// differs). A failed pass re-derives missing from disk and retries with
// fresh presigns (URLs expire as a set; a multi-GiB image-tier tar can
// outlive a batch's 15-minute signature mid-stream -- 10-31: batch member
// two 403'd after a 2.4 GiB sibling took 14 minutes). Completed objects are
// never re-downloaded: hasObject skips them on the rescan.
func (s *Store) FetchMissing(ctx context.Context, manifest []api.File, fn presignFunc) error {
	if len(manifest) == 0 {
		return fmt.Errorf("empty manifest")
	}
	// Declare-only entries first (byo-hosting-node D3): local objects are
	// imported into the content store (escape-checked + sha-verified) BEFORE
	// the missing scan, so the presign path never sees them -- the platform
	// holds no blob for a local entry and a presign download would 404.
	for _, f := range manifest {
		if f.LocalPath != "" && !s.hasObject(f.SHA256) {
			if err := s.importLocalObject(f); err != nil {
				return err
			}
		}
	}
	const attempts = 3
	var lastErr error
	for try := 0; try < attempts; try++ {
		var missing []string
		for _, f := range manifest {
			if !s.hasObject(f.SHA256) {
				missing = append(missing, f.SHA256)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		lastErr = nil
		for i := 0; i < len(missing); i += 500 {
			end := min(i+500, len(missing))
			if err := s.fetchBatch(ctx, fn, missing[i:end]); err != nil {
				lastErr = err
				break
			}
		}
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// importLocalObject lands one declare-only entry in the content store:
// scratch-prefix escape check, sha256 verification against the declared
// hash, then hardlink (or copy) into objects/{sha}. The escape check is the
// agent-side half of the local-manifest protocol's two-layer guard (design
// D3 risk table): the platform could be coerced into declaring an arbitrary
// path, and this is where the agent refuses to read it.
func (s *Store) importLocalObject(f api.File) error {
	if err := s.checkLocalPath(f.LocalPath); err != nil {
		return fmt.Errorf("local object %s: %w", f.Path, err)
	}
	h := sha256.New()
	in, err := os.Open(f.LocalPath)
	if err != nil {
		return fmt.Errorf("local object %s: open: %w", f.Path, err)
	}
	if _, err := io.Copy(h, in); err != nil {
		in.Close()
		return fmt.Errorf("local object %s: read: %w", f.Path, err)
	}
	in.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("local object %s: sha256 mismatch (declared %s, file hashes %s) -- the declared scratch file changed or the declaration is stale",
			f.Path, f.SHA256[:12], got[:12])
	}
	if err := os.MkdirAll(filepath.Dir(s.objectPath(f.SHA256)), 0o755); err != nil {
		return err
	}
	return hardlinkOrCopy(f.LocalPath, s.objectPath(f.SHA256))
}

// checkLocalPath enforces the scratch-root prefix (path-escape guard). Both
// the lexical path AND its symlink-resolved form must sit under the resolved
// root: a symlink planted inside the scratch tree must not smuggle a read
// from outside it. Errors are worded for the deployment row's failure detail
// (user-visible, byo-hosting-node spec: 逃逸拒绝并审计，失败原因可读).
func (s *Store) checkLocalPath(p string) error {
	if s.ScratchRoot == "" {
		return fmt.Errorf("localPath entries are not allowed on this node (no scratch root configured)")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("local path %q is not absolute", p)
	}
	root, err := filepath.EvalSymlinks(s.ScratchRoot)
	if err != nil {
		return fmt.Errorf("scratch root %s unusable: %w", s.ScratchRoot, err)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fmt.Errorf("local path %s unusable: %w", p, err)
	}
	cleanRoot := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(resolved+string(os.PathSeparator), cleanRoot) {
		return fmt.Errorf("local path %s escapes the scratch root %s (path-escape refused)", p, s.ScratchRoot)
	}
	return nil
}

// Worktree materializes the manifest as a PLAIN directory at
// worktrees/{deploymentID} (user-node compose-up path, byo-hosting-node
// D1/D3: no git repo, no git http -- the agent itself runs docker compose
// from this tree). Fetch/import discipline identical to MaterializeVia; the
// tree assembles in a .work scratch and swaps in atomically, so a crash never
// leaves a half-built worktree behind (same contract as BuildRepo).
func (s *Store) Worktree(ctx context.Context, p Presigner, deploymentID, sourcePrefix string, manifest []api.File, extra map[string][]byte) (string, error) {
	presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return p.PresignSource(ctx, deploymentID, shas)
	}
	if err := s.FetchMissing(ctx, manifest, presign); err != nil {
		return "", fmt.Errorf("materialize %s: %w", deploymentID, err)
	}
	final := s.WorktreePath(deploymentID)
	work := final + ".work"
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	if err := s.assembleTree(work, manifest, extra); err != nil {
		return "", fmt.Errorf("materialize %s: %w", deploymentID, err)
	}
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(work, final); err != nil {
		return "", fmt.Errorf("materialize %s: swap worktree in: %w", deploymentID, err)
	}
	return final, nil
}

// ObjectPath is the on-disk path of a fetched object (sha-addressed, no
// extension). The image tier docker-loads image tars straight from the store
// (docker-image-deploy-tier D6) -- `docker load -i` is content-driven, so
// the sha filename is fine and no multi-GiB copy happens.
func (s *Store) ObjectPath(sha string) (string, error) {
	if !s.hasObject(sha) {
		return "", fmt.Errorf("object %s not in store", sha)
	}
	return s.objectPath(sha), nil
}

// ObjectBytes returns the verified content of one object already in the
// store (the runtime preview path reads runtime.json this way after
// FetchMissing, before generating the Dockerfile that rides the same commit).
func (s *Store) ObjectBytes(sha string) ([]byte, error) {
	if !s.hasObject(sha) {
		return nil, fmt.Errorf("object %s not in store", sha)
	}
	return os.ReadFile(s.objectPath(sha))
}

// BuildRepo produces repos/{repoKey}.git: a bare repo whose single commit
// contains exactly the manifest tree plus any extra files (the generated
// Dockerfile for runtime previews -- deliberately NOT sha-store content, it
// is derived, not fetched). Returns the commit hash. staticFallback governs
// the nixpacks static-site heuristic; runtime trees build from a Dockerfile
// and skip it.
func (s *Store) BuildRepo(ctx context.Context, repoKey string, manifest []api.File, extra map[string][]byte, staticFallback bool) (string, error) {
	// 1. Assemble the work tree in scratch space.
	work := filepath.Join(s.Dir, "repos", repoKey+".work")
	if err := os.RemoveAll(work); err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	if err := s.assembleTree(work, manifest, extra); err != nil {
		return "", fmt.Errorf("build repo %s: %w", repoKey, err)
	}

	// 1b. Static-site fallback (fix-seed-audit-blockers S4). A tree with
	// index.html but no start/build command is a static site; nixpacks picks
	// its node provider off the mere PRESENCE of package.json, builds no CMD,
	// and the container crash-loops while the row reads ready (2026-09-08
	// live incident). Moving the manifest aside makes nixpacks fall through
	// to its static provider: nginx serving the tree on $PORT, zero injected
	// code to maintain. Pure function of the manifest -> idempotent commit.
	if staticFallback {
		if err := applyStaticFallback(work, manifest); err != nil {
			return "", fmt.Errorf("build repo %s: static fallback: %w", repoKey, err)
		}
	}

	// 2. Single-commit repo, then bare-copy and atomic swap.
	if err := runGit(ctx, work, "init", "--quiet", "--initial-branch=main"); err != nil {
		return "", fmt.Errorf("build repo %s: git init: %w", repoKey, err)
	}
	// --force: the manifest is the sanitized source of truth. The tree can
	// carry the project's own .gitignore, and a plain `git add -A` obeys it —
	// silently dropping manifest files from the committed snapshot (a tree
	// whose Dockerfile COPYs a .gitignore'd runtime asset builds broken even
	// though the manifest and R2 both hold the file).
	if err := runGit(ctx, work, "add", "-A", "--force"); err != nil {
		return "", fmt.Errorf("build repo %s: git add: %w", repoKey, err)
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
		return "", fmt.Errorf("build repo %s: git commit: %w", repoKey, err)
	}
	out, err := outputGit(ctx, work, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("build repo %s: rev-parse: %w", repoKey, err)
	}
	commit := strings.TrimSpace(out)

	final := s.RepoPath(repoKey)
	swap := final + ".swap"
	if err := os.RemoveAll(swap); err != nil {
		return "", err
	}
	if err := runGit(ctx, work, "clone", "--quiet", "--bare", work, swap); err != nil {
		return "", fmt.Errorf("build repo %s: bare clone: %w", repoKey, err)
	}
	// A concurrent http-backend may hold the old repo open mid-pull; removing
	// the previous final first means the swap is rename-atomic for new opens.
	if err := os.RemoveAll(final); err != nil {
		return "", err
	}
	if err := os.Rename(swap, final); err != nil {
		return "", fmt.Errorf("build repo %s: swap repo in: %w", repoKey, err)
	}
	return commit, nil
}

// assembleTree places every manifest object (from the content store) plus any
// extra derived files into work, refusing manifest paths that escape the root
// (the same lexical check BuildRepo always enforced; Worktree shares it).
func (s *Store) assembleTree(work string, manifest []api.File, extra map[string][]byte) error {
	place := func(rel string, place func(dst string) error) error {
		dst := filepath.Join(work, filepath.FromSlash(rel))
		if filepath.Clean(dst) != dst || !strings.HasPrefix(dst, work+string(os.PathSeparator)) {
			return fmt.Errorf("manifest path escapes root: %s", rel)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return place(dst)
	}
	for _, f := range manifest {
		if err := place(f.Path, func(dst string) error {
			return hardlinkOrCopy(s.objectPath(f.SHA256), dst)
		}); err != nil {
			return fmt.Errorf("place %s: %w", f.Path, err)
		}
	}
	for path, content := range extra {
		if err := place(path, func(dst string) error {
			return os.WriteFile(dst, content, 0o644)
		}); err != nil {
			return fmt.Errorf("place extra %s: %w", path, err)
		}
	}
	return nil
}

// staticFallbackName is what package.json is moved to when the static
// fallback fires: the file stays in the tree (auditable, and a developer can
// see why nixpacks stopped seeing it) but no longer steers the provider.
const staticFallbackName = "package.json.static-fallback"

// applyStaticFallback moves package.json aside when the tree is a static
// site nixpacks would mis-detect. Firing conditions, all required:
//   - package.json exists at the root but declares no start AND no build
//     script (a build script means a real toolchain -- not our call)
//   - index.html exists at the root (the thing the static provider serves)
//   - no Procfile / nixpacks.toml / railway.json|toml (an explicit start
//     declaration anywhere wins over the heuristic)
//
// A missing package.json with index.html already lands on the static
// provider natively, so there is nothing to do for it here.
func applyStaticFallback(work string, manifest []api.File) error {
	has := func(p string) bool {
		for _, f := range manifest {
			if f.Path == p {
				return true
			}
		}
		return false
	}
	for _, p := range []string{"Procfile", "nixpacks.toml", "railway.json", "railway.toml"} {
		if has(p) {
			return nil
		}
	}
	if !has("index.html") || !has("package.json") {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(work, "package.json"))
	if err != nil {
		return err
	}
	var pkg struct {
		Scripts struct {
			Start string `json:"start"`
			Build string `json:"build"`
		} `json:"scripts"`
	}
	// A malformed package.json moves aside too: nixpacks' provider detection
	// is presence-based, so even unparseable it steers node and no-CMD.
	_ = json.Unmarshal(raw, &pkg)
	if strings.TrimSpace(pkg.Scripts.Start) != "" || strings.TrimSpace(pkg.Scripts.Build) != "" {
		return nil
	}
	return os.Rename(filepath.Join(work, "package.json"), filepath.Join(work, staticFallbackName))
}

// hasObject reports whether a verified copy of sha exists in the store.
func (s *Store) hasObject(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	_, err := os.Stat(s.objectPath(sha))
	return err == nil
}

// fetchBatch presigns one batch via fn and downloads + verifies each object.
// Any failure fails the whole batch so the caller re-signs (fresh 15-min URLs).
func (s *Store) fetchBatch(ctx context.Context, fn presignFunc, shas []string) error {
	resp, err := fn(ctx, shas)
	if err != nil {
		return fmt.Errorf("presign: %w", err)
	}
	byKey := map[string]string{}
	for _, obj := range resp.URLs {
		// key = {prefix}/{sha} (deploys/... or previews/...) -- the sha is
		// the tail segment, so the prefix origin does not matter here.
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

// downloadVerified streams url into the content store only if the resulting
// file hashes to sha. It resumes from a previous attempt's .part when one is
// present: a Range request continues at the partial size (R2 answers 206 with
// the remaining bytes), the already-downloaded prefix feeds the hash, and a
// server that ignores Range (plain 200) restarts from scratch. The .part is
// kept across NETWORK failures -- a multi-hundred-MB image-tier tar over a
// slow cross-border link can drop every ~50min and re-downloading from zero
// each retry means the transfer never finishes (11-03: 141MB of 196MB lost to
// a truncate-on-retry) -- and removed only when the content proves wrong (sha
// mismatch) or the object completes (rename).
func (s *Store) downloadVerified(ctx context.Context, url, sha string) error {
	if err := os.MkdirAll(filepath.Dir(s.objectPath(sha)), 0o755); err != nil {
		return err
	}
	tmp := s.objectPath(sha) + ".part"
	// O_RDWR: the resumed prefix is READ back through this handle to feed
	// the hash before the tail streams in through the same one.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	var offset int64
	if st, err := os.Stat(tmp); err == nil && st.Size() > 0 {
		offset = st.Size()
		// Hash the already-downloaded prefix so the final sum covers the
		// whole object, and verify it before spending bandwidth on the tail:
		// a prefix that does not hash to a plausible position is unprovable
		// (partial writes mid-object are possible), so restart from zero.
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if _, err := io.Copy(h, f); err != nil {
				offset = 0
				h.Reset()
			} else {
				if _, err := f.Seek(offset, io.SeekStart); err != nil {
					offset = 0
					h.Reset()
				}
			}
		}
		if offset == 0 {
			if err := f.Truncate(0); err != nil {
				f.Close()
				return err
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		f.Close()
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		f.Close()
		return fmt.Errorf("get %s: %w", sha[:12], err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		// The server honored the range; the hash already covers [0, offset).
	case resp.StatusCode == http.StatusOK:
		// Full content: any resumed prefix is invalid -- restart the file
		// and the hash from byte zero.
		if offset > 0 {
			if err := f.Truncate(0); err != nil {
				f.Close()
				return err
			}
			h.Reset()
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				f.Close()
				return err
			}
		}
	default:
		f.Close()
		return fmt.Errorf("get %s: status %d", sha[:12], resp.StatusCode)
	}

	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("stream %s: %w", sha[:12], err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		// Wrong content: the .part is poison, not progress. Remove it so the
		// next attempt starts clean instead of resuming onto garbage.
		_ = os.Remove(tmp)
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
