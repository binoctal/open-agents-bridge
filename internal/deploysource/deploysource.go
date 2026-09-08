// Package deploysource implements the bridge side of add-coolify-hosting's
// artifact bridge (tasks 4.1-4.3): pack the mission's working source tree and
// upload it to the platform's deploys/ R2 namespace via the same two-phase
// protocol previews use (declare -> presigned PUTs -> complete).
//
// Unlike the preview package there is no auto path at all — the only trigger
// is a `pending` hosted_deployments row the user created with an explicit
// deploy click, discovered through the bridge's poll. Task completion and
// merges never reach this package, so zero source leaves the device unless
// the user asked for it (task 4.3's hard requirement).
package deploysource

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	DeclareHostedSource(missionID, deploymentID string, files []api.PreviewFile) (*api.DeclareSourceResponse, error)
	CompleteHostedSource(missionID, deploymentID string) error
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

// PackResult carries the manifest plus what was left out, so the caller can
// log the strip decisions — a deploy whose app genuinely needs a stripped
// file fails at the hosting node, and this is the only place the omission is
// visible before that.
type PackResult struct {
	Files    []preview.ManifestFile
	Stripped []string // sensitive basenames removed, for the log line
}

// PackSourceTree walks root and manifests every packable file. Paths are
// POSIX-relative to root, sorted deterministically, hashed with sha256 —
// the same manifest contract as previews. Oversize files/trees are errors,
// not truncations: a partial tree deploys a broken app.
func PackSourceTree(root string) (*PackResult, error) {
	var res PackResult
	var total int64

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

		// A linked git worktree's .git is a POINTER FILE (gitdir: ...), not a
		// directory, so the directory skip above never sees it — and the
		// platform rejects any manifest path ending in a .git segment. Same
		// for the file branch of BuildStaticTreeManifest.
		if info.Name() == ".git" {
			return nil
		}

		base := filepath.Base(path)
		if isSensitive(base) {
			res.Stripped = append(res.Stripped, relSlash)
			return nil
		}

		if info.Size() > MaxFileBytes {
			return fmt.Errorf("file %s is %d bytes, over the %d byte limit", relSlash, info.Size(), MaxFileBytes)
		}
		sum, err := preview.SHA256File(path)
		if err != nil {
			return err
		}
		total += info.Size()
		if total > MaxTotalBytes {
			return fmt.Errorf("source tree exceeds the %d byte limit", MaxTotalBytes)
		}

		res.Files = append(res.Files, preview.ManifestFile{Path: relSlash, SHA256: sum, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(res.Files) == 0 {
		return nil, fmt.Errorf("no packable files under %s", root)
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Path < res.Files[j].Path })
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
func uploadAll(client Uploader, uploads []api.PreviewUpload, blob func(path string) ([]byte, error)) error {
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

// RunSourceUpload packs repoRoot and runs the two-phase upload for one
// pending deployment. Best-effort like every preview path: failures are
// logged and returned, never propagated into mission control flow — and the
// platform keeps offering the row on the next poll, so a transient failure
// self-heals (declare's skip-list means the retry only uploads what is
// missing).
func RunSourceUpload(client Uploader, missionID, deploymentID, repoRoot string, logf Logf) {
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

	decl, err := client.DeclareHostedSource(missionID, deploymentID, toAPIFiles(pack.Files))
	if err != nil {
		logf("[%s] declare failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}

	blob := func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
	}
	if err := uploadAll(client, decl.Uploads, blob); err != nil {
		logf("[%s] upload failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}

	if err := client.CompleteHostedSource(missionID, deploymentID); err != nil {
		logf("[%s] complete failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
		return
	}
	logger.Info("[%s] source tree uploaded for deployment %s (mission %s): %d files, %d uploaded",
		logger.ModDeploy, deploymentID, missionID, len(pack.Files), len(decl.Uploads))
}
