// Package deploysource implements the bridge side of add-coolify-hosting's
// artifact bridge (tasks 4.1-4.3): pack the mission's working source tree and
// upload it to the platform's deploys/ R2 namespace via the same two-phase
// protocol previews use (declare -> presigned PUTs -> complete).
//
// Unlike the preview package there is no auto path at all — the only trigger
// is a `pending` hosted_deployments row the user created with an explicit
// deploy click, discovered through the bridge's poll. Task completion and
// merges never reach this package, so zero source leaves the machine unless
// the user asked for it (task 4.3's hard requirement).
package deploysource

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/preview"
)

// Uploader is the subset of *api.Client this package needs. Kept as a local
// interface (not preview.Uploader, which demands the preview calls) so tests
// stub exactly the deploy boundary.
type Uploader interface {
	DeclareHostedSource(missionID, deploymentID string, files []api.PreviewFile, composePath string, chunk *api.DeclareChunk) (*api.DeclareSourceResponse, error)
	CompleteHostedSource(missionID, deploymentID string, verify *api.CompleteSourceVerify) error
	UploadPreviewFile(url string, data []byte) error
}

// Logf matches bridge.go's b.logInfo/b.logDebug shape.
type Logf func(format string, args ...interface{})

func noopLogf(string, ...interface{}) {}

// ExcludedDirs are directory names never packed, at any depth. Mirrors the
// platform's SOURCE_EXCLUDED_DIRS (apps/api/src/services/hosted-deployments.ts):
// .git because the platform materializes its own single-commit repo from the
// tree, .open-agents-bridge-worktrees because mission task worktrees under it
// are merge intermediates (their leftover state after a failed merge must not
// ride along — the platform rejects any path hitting the name), the rest
// because they are build outputs or dependency caches the build on the hosting
// node regenerates anyway.
var ExcludedDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"out": true, ".next": true, ".cache": true, ".turbo": true, "coverage": true,
	// Language build/dependency outputs the hosting node regenerates anyway:
	// Maven (target/), Python virtualenvs (.venv/, venv/). Mirrors the
	// platform's SOURCE_EXCLUDED_DIRS.
	"target": true, ".venv": true, "venv": true,
	".open-agents-bridge-worktrees": true,
}

// SensitivePatterns match file basenames stripped from the pack (not uploaded
// — the hosting node gets a clean tree and injects its own env). Mirrors the
// platform's SOURCE_SENSITIVE_PATTERNS; the platform additionally REJECTS a
// manifest containing any of these (SOURCE_SENSITIVE_FILE), so the strip here
// is for the honest path and the rejection there is the backstop against a
// buggy or modified packer.
var SensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\.env(\..+)?$`),
	regexp.MustCompile(`\.(pem|key|p12|pfx)$`),
	regexp.MustCompile(`^id_rsa`),
	regexp.MustCompile(`^id_ed25519`),
	regexp.MustCompile(`^id_ecdsa`),
	regexp.MustCompile(`^service-account.+\.json$`),
	regexp.MustCompile(`^credentials.*\.json$`),
}

func isSensitive(base string) bool {
	for _, re := range SensitivePatterns {
		if re.MatchString(base) {
			return true
		}
	}
	return false
}

// Same ceilings as the platform's defaults (hosted-deployments.ts). Checked
// at pack time to fail fast before any bytes move; the platform re-checks at
// declare, so a config drift between the two sides surfaces as a declare
// rejection rather than a truncated upload.
const (
	MaxFileBytes  = 26_214_400  // 25MB per file
	MaxTotalBytes = 314_572_800 // 300MB per source tree
)

// composeConventionPaths mirrors the platform's COMPOSE_DIR_CANDIDATES x
// COMPOSE_FILE_NAMES (apps/api/src/services/compose-policy.ts) in the same
// order: root first, then deploy/, docker-compose spelling before compose,
// .yml before .yaml. Detection order is a contract — the two sides must pick
// the same file when a tree carries several.
var composeConventionPaths = []string{
	"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml",
	"deploy/docker-compose.yml", "deploy/docker-compose.yaml", "deploy/compose.yml", "deploy/compose.yaml",
}

// DetectComposePath reports the first convention-path compose file present in
// a packed manifest, or "" when the tree is single-container shape. Detection
// rides the MANIFEST, so a reported path is by construction part of the
// declared tree (the platform fail-louds paths outside the manifest anyway).
func DetectComposePath(files []preview.ManifestFile) string {
	set := make(map[string]bool, len(files))
	for _, f := range files {
		set[f.Path] = true
	}
	for _, p := range composeConventionPaths {
		if set[p] {
			return p
		}
	}
	return ""
}

// PackResult carries the manifest plus what was left out, so the caller can
// log the strip decisions — a deploy whose app genuinely needs a stripped
// file fails at the hosting node, and this is the only place the omission is
// visible before that.
type PackResult struct {
	Files       []preview.ManifestFile
	Stripped    []string // sensitive basenames removed, for the log line
	ComposePath string   // first convention-path compose file, "" when none
}

// PackSourceTree walks root and manifests every packable file. Paths are
// POSIX-relative to root, sorted deterministically, hashed with sha256 —
// the same manifest contract as previews. Oversize files/trees are errors,
// not truncations: a partial tree deploys a broken app.
//
// Git-aware: when root is a git work tree the file set comes from
// `git ls-files --cached --others --exclude-standard` — exactly the tree a
// commit would carry. Untracked build output and local scratch that
// .gitignore already excludes (virtualenvs, vendored caches, research
// dumps...) never belong in a deploy source, and honoring gitignore keeps
// real projects under the size ceiling without asking users to clean their
// workdir first. Non-git roots keep the plain walk (ExcludedDirs et al).
func PackSourceTree(root string) (*PackResult, error) {
	if paths := gitPackPaths(root); paths != nil {
		return packPaths(root, paths)
	}
	return packWalk(root)
}

// gitPackPaths returns the repo-relative POSIX paths git would commit
// (tracked + untracked-but-not-ignored). nil when root is not a git work
// tree, or when git cannot enumerate it (unresolvable linked-worktree
// pointer, corrupted repo) — the caller then falls back to the plain walk,
// which keeps today's behavior for such trees.
func gitPackPaths(root string) []string {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, p := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// packWalk is the non-git packing path: a plain directory walk.
func packWalk(root string) (*PackResult, error) {
	var paths []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relSlash := filepath.ToSlash(rel)
		if info.IsDir() {
			if relSlash != "." && ExcludedDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, relSlash)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return packPaths(root, paths)
}

// packPaths manifests the given repo-relative paths (strip, size-check,
// hash) into a PackResult.
func packPaths(root string, paths []string) (*PackResult, error) {
	var res PackResult
	var total int64

	for _, relSlash := range paths {
		// The platform rejects any manifest path hitting these segments;
		// skip them here so a committed build dir does not fail the declare.
		if segs := strings.Split(relSlash, "/"); segs[len(segs)-1] == ".git" || slices.ContainsFunc(segs, func(s string) bool { return ExcludedDirs[s] }) {
			continue
		}

		path := filepath.Join(root, filepath.FromSlash(relSlash))
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue // submodule dir entries, symlinks
		}

		base := filepath.Base(path)
		if isSensitive(base) {
			res.Stripped = append(res.Stripped, relSlash)
			continue
		}

		if info.Size() > MaxFileBytes {
			return nil, fmt.Errorf("file %s is %d bytes, over the %d byte limit", relSlash, info.Size(), MaxFileBytes)
		}
		sum, err := preview.SHA256File(path)
		if err != nil {
			return nil, err
		}
		total += info.Size()
		if total > MaxTotalBytes {
			return nil, fmt.Errorf("source tree exceeds the %d byte limit", MaxTotalBytes)
		}

		res.Files = append(res.Files, preview.ManifestFile{Path: relSlash, SHA256: sum, Size: info.Size()})
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("no packable files under %s", root)
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path })
	res.ComposePath = DetectComposePath(res.Files)
	return &res, nil
}

// toAPIFiles converts to the wire type. No .map filtering here (that rule is
// about source maps leaking original source from BUILD OUTPUTS — a deploy
// tree's sources are the point of the upload).
func toAPIFiles(files []preview.ManifestFile) []api.PreviewFile {
	out := make([]api.PreviewFile, 0, len(files))
	for _, f := range files {
		out = append(out, api.PreviewFile{Path: f.Path, SHA256: f.SHA256, Size: f.Size})
	}
	return out
}

// uploadAll PUTs every presigned upload with bounded parallelism — same shape
// and rationale as preview.uploadAll (a source tree carries thousands of
// files and a sequential loop cannot finish inside the presigned-URL TTL).
// First error wins; blob closures only read from disk.
func uploadAll(client interface {
	UploadPreviewFile(url string, data []byte) error
}, uploads []api.PreviewUpload, blob func(path string) ([]byte, error)) error {
	const workers = 8
	jobs := make(chan api.PreviewUpload)
	errs := make(chan error, workers)
	done := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				data, err := blob(u.Path)
				if err != nil {
					errs <- fmt.Errorf("read %s for upload: %w", u.Path, err)
					return
				}
				if err := client.UploadPreviewFile(u.URL, data); err != nil {
					errs <- fmt.Errorf("upload %s: %w", u.Path, err)
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, u := range uploads {
			select {
			case jobs <- u:
			case <-done:
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(errs)
	}()

	var firstErr error
	for err := range errs {
		if firstErr == nil {
			firstErr = err
			close(done)
		}
	}
	return firstErr
}

// declareChunkFiles slices a chunked declare. ~800 files keeps one request's
// manifest JSON around 200KB, an order of magnitude under what blew the API
// worker's CPU budget in a single shot (CF 1102 on a real 7.5k-file tree,
// 2026-10-30).
const declareChunkFiles = 800

func featureEnabled(features []string, name string) bool {
	for _, f := range features {
		if f == name {
			return true
		}
	}
	return false
}

// RunSourceUpload packs repoRoot and runs the two-phase upload for one
// pending deployment. Best-effort like every preview path: failures are
// logged and returned, never propagated into mission control flow — and the
// platform keeps offering the row on the next poll, so a transient failure
// self-heals (declare's skip-list means the retry only uploads what is
// missing).
//
// features is the platform's advertised protocol set from the pending poll.
// Large trees (>declareChunkFiles files) take the chunked declare protocol
// ONLY when the platform advertises chunked_declare: against an old API the
// chunked wire would silently declare the last slice as the whole tree, so
// the fallback keeps failing loudly (503) instead of corrupting quietly.
func RunSourceUpload(client Uploader, missionID, deploymentID, repoRoot string, logf Logf, features []string) {
	if logf == nil {
		logf = noopLogf
	}

	pack, err := PackSourceTree(repoRoot)
	if err != nil {
		logf("[%s] source pack failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}
	if len(pack.Stripped) > 0 {
		logf("[%s] stripped %d sensitive file(s) from deployment %s source: %s",
			logger.ModDeploy, len(pack.Stripped), deploymentID, strings.Join(pack.Stripped, ", "))
	}

	shaByPath := make(map[string]string, len(pack.Files))
	for _, f := range pack.Files {
		shaByPath[f.Path] = f.SHA256
	}
	blob := func(path string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		// The pack hashed this file at walk time; verify the exact bytes we are
		// about to PUT. A workspace being edited concurrently (live-project
		// deploys are the norm) otherwise uploads new bytes under the old
		// content-addressed key and the hosting node fails materialize on the
		// sha mismatch. Abort this cycle instead; the row stays pending and the
		// next poll re-packs a fresh snapshot.
		if sum := preview.SHA256Bytes(data); sum != shaByPath[path] {
			return nil, fmt.Errorf("%s changed during upload (packed %s, read %s); source tree is being modified concurrently",
				path, shaByPath[path][:12], sum[:12])
		}
		return data, nil
	}

	if featureEnabled(features, "chunked_declare") && len(pack.Files) > declareChunkFiles {
		runChunkedSourceUpload(client, missionID, deploymentID, pack, blob, logf)
		return
	}

	decl, err := client.DeclareHostedSource(missionID, deploymentID, toAPIFiles(pack.Files), pack.ComposePath, nil)
	if err != nil {
		logf("[%s] declare failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}

	if err := uploadAll(client, decl.Uploads, blob); err != nil {
		logf("[%s] upload failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}

	if err := client.CompleteHostedSource(missionID, deploymentID, nil); err != nil {
		logf("[%s] complete failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}
	if pack.ComposePath != "" {
		logf("[%s] compose stack detected for deployment %s: %s", logger.ModDeploy, deploymentID, pack.ComposePath)
	}
	logger.Info("[%s] source tree uploaded for deployment %s (mission %s): %d files, %d uploaded",
		logger.ModDeploy, deploymentID, missionID, len(pack.Files), len(decl.Uploads))
}

// runChunkedSourceUpload is the large-tree upload: declare the pack slice by
// slice, upload each slice's presigned objects, then complete with the full
// {path, sha256} projection so the platform never has to parse the assembled
// manifest itself. The compose path rides the FINAL declare (the platform
// stamps it at assembly); every part carries the whole-tree TotalBytes so
// the platform's quota gate sees the same total on every part and retries
// stay idempotent. A failure mid-sequence returns; the row stays pending and
// the next poll restarts at part 0, where declare's skip-list means only the
// objects that never landed get re-PUT.
func runChunkedSourceUpload(client Uploader, missionID, deploymentID string, pack *PackResult, blob func(string) ([]byte, error), logf Logf) {
	totalParts := (len(pack.Files) + declareChunkFiles - 1) / declareChunkFiles
	var totalBytes int64
	for _, f := range pack.Files {
		totalBytes += f.Size
	}

	verify := &api.CompleteSourceVerify{Files: make([]api.VerifyFile, 0, len(pack.Files))}
	for part := 0; part < totalParts; part++ {
		slice := pack.Files[part*declareChunkFiles : min((part+1)*declareChunkFiles, len(pack.Files))]
		chunk := &api.DeclareChunk{Part: part, TotalParts: totalParts, TotalBytes: totalBytes}
		// compose_path rides the final part: assembly stamps it then, and the
		// whole-tree membership check moved to complete (verify.Files).
		composePath := ""
		if part == totalParts-1 {
			composePath = pack.ComposePath
		}
		decl, err := client.DeclareHostedSource(missionID, deploymentID, toAPIFiles(slice), composePath, chunk)
		if err != nil {
			logf("[%s] declare failed for deployment %s (part %d/%d): %v",
				logger.ModDeploy, deploymentID, part+1, totalParts, err)
			return
		}
		if err := uploadAll(client, decl.Uploads, blob); err != nil {
			logf("[%s] upload failed for deployment %s (part %d/%d): %v",
				logger.ModDeploy, deploymentID, part+1, totalParts, err)
			return
		}
		for _, f := range slice {
			verify.Files = append(verify.Files, api.VerifyFile{Path: f.Path, SHA256: f.SHA256})
		}
		logf("[%s] declared part %d/%d for deployment %s: %d files, %d uploaded",
			logger.ModDeploy, part+1, totalParts, deploymentID, len(slice), len(decl.Uploads))
	}

	if err := client.CompleteHostedSource(missionID, deploymentID, verify); err != nil {
		logf("[%s] complete failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}
	if pack.ComposePath != "" {
		logf("[%s] compose stack detected for deployment %s: %s", logger.ModDeploy, deploymentID, pack.ComposePath)
	}
	logger.Info("[%s] source tree uploaded for deployment %s (mission %s): %d files in %d parts",
		logger.ModDeploy, deploymentID, missionID, len(pack.Files), totalParts)
}
