package preview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// Uploader is the subset of *api.Client that this package needs. Declaring
// it here (instead of depending on *api.Client directly) gives unit tests a
// seam to stub the HTTP boundary, matching how api/client_test.go isolates
// transport from logic.
type Uploader interface {
	CreatePreview(jobID string, files []api.PreviewFile, meta *api.DeclarePreviewMeta) (*api.DeclarePreviewResponse, error)
	CompletePreview(jobID, previewID string, body api.CompletePreviewBody) error
	ReportArtifactKind(jobID, kind string) error
	UploadPreviewFile(url string, data []byte) error
}

// Logf matches bridge.go's b.logInfo/b.logDebug shape, so callers can pass
// those methods straight through without an adapter.
type Logf func(format string, args ...interface{})

func noopLogf(string, ...interface{}) {}

// toAPIFiles converts the pure-logic ManifestFile into the wire type,
// filtering out anything ending in .map as a second line of defense —
// BuildManifest already excludes them, but this holds even if a manifest
// somehow arrives from a stale on-disk cache built by an older version.
func toAPIFiles(files []ManifestFile) []api.PreviewFile {
	out := make([]api.PreviewFile, 0, len(files))
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".map") {
			continue
		}
		out = append(out, api.PreviewFile{Path: f.Path, SHA256: f.SHA256, Size: f.Size})
	}
	return out
}

// Upload runs the two-phase upload protocol (task 4.3) for an already-built
// manifest: declare -> PUT each presigned upload -> complete. blob must
// return the raw bytes for a manifest path (read from the build output
// directory on a fresh build, or from the on-disk artifact cache on a
// revive). complete rides the snapshot key + detection kind; meta carries
// rewrite telemetry on the declare. Both may be zero-valued (revive path).
//
// Every failure is returned as a plain error. Nothing here retries — the
// caller (RunAndUpload / RunRevive) logs and stops, exactly like every other
// preview-hosting failure mode in this package.
func Upload(client Uploader, jobID string, files []ManifestFile, blob func(path string) ([]byte, error), completeBody api.CompletePreviewBody, meta *api.DeclarePreviewMeta) error {
	apiFiles := toAPIFiles(files)
	if len(apiFiles) == 0 {
		return fmt.Errorf("no files to upload after filtering")
	}
	if !HasRootIndexHTML(files) {
		return fmt.Errorf("manifest missing root index.html")
	}

	decl, err := client.CreatePreview(jobID, apiFiles, meta)
	if err != nil {
		return fmt.Errorf("declare preview: %w", err)
	}

	for _, u := range decl.Uploads {
		data, err := blob(u.Path)
		if err != nil {
			return fmt.Errorf("read %s for upload: %w", u.Path, err)
		}
		if err := client.UploadPreviewFile(u.URL, data); err != nil {
			return fmt.Errorf("upload %s: %w", u.Path, err)
		}
	}

	if err := client.CompletePreview(jobID, decl.PreviewID, completeBody); err != nil {
		return fmt.Errorf("complete preview: %w", err)
	}

	logger.Info("[%s] preview ready for mission %s: %s", logger.ModPreview, jobID, decl.URL)
	return nil
}

// RunAndUpload builds repoRoot as a static site and, on success, uploads it
// as a preview for jobID (task 4.1-4.3 wired together). taskID is the
// snapshot key for the complete call: the owning task's id from the
// task-completed path, the "merge" sentinel from the merge-final paths, and
// "" from nowhere — RunRevive deliberately completes without one.
//
// Every step's failure is logged via logf and causes an immediate, silent
// return — this is the function bridge.go calls from a goroutine right after
// a merge succeeds or a task completes, and it must never propagate a
// failure back into the mission's control flow (there is nothing to
// propagate it to: the result message has already been sent).
func RunAndUpload(client Uploader, cache *Cache, jobID, repoRoot, taskID string, logf Logf) {
	if logf == nil {
		logf = noopLogf
	}

	hasBuild, err := HasBuildScript(repoRoot)
	if err != nil {
		logf("[%s] preview: could not read package.json for mission %s: %v", logger.ModPreview, jobID, err)
		return
	}
	if !hasBuild {
		logf("[%s] preview: no build script for mission %s, skipping", logger.ModPreview, jobID)
		return
	}

	logf("[%s] preview: building mission %s", logger.ModPreview, jobID)
	if err := RunBuild(repoRoot); err != nil {
		logf("[%s] preview: build failed for mission %s: %v", logger.ModPreview, jobID, err)
		return
	}

	// D3: report the mission-level artifact kind whether or not a preview
	// row follows — the Next non-export path stops right after this.
	kind := DetectAndReport(client, jobID, repoRoot, logf)

	outputDir, ok := ResolveOutputDir(repoRoot)
	if !ok {
		logf("[%s] preview: no build output with index.html for mission %s, skipping", logger.ModPreview, jobID)
		return
	}

	// D4: rewrite root-absolute references to relative BEFORE the manifest
	// so the hashed bytes are the served bytes. Failure here means the
	// output tree is unreadable — BuildManifest would fail the same way.
	rewrites, err := RewriteHTMLAbsolutePaths(outputDir)
	if err != nil {
		logf("[%s] preview: HTML path rewrite failed for mission %s: %v", logger.ModPreview, jobID, err)
		return
	}
	if rewrites > 0 {
		logf("[%s] preview: rewrote %d absolute reference(s) to relative for mission %s", logger.ModPreview, rewrites, jobID)
	}

	files, err := BuildManifest(outputDir)
	if err != nil {
		logf("[%s] preview: manifest build failed for mission %s: %v", logger.ModPreview, jobID, err)
		return
	}
	if !HasRootIndexHTML(files) {
		logf("[%s] preview: build output for mission %s has no root index.html, skipping", logger.ModPreview, jobID)
		return
	}

	if cache != nil {
		if err := cache.Store(jobID, outputDir, files); err != nil {
			// A cache-write failure only affects a future revive (task 4.4);
			// it must not abort an otherwise-good upload attempt.
			logf("[%s] preview: artifact cache store failed for mission %s: %v", logger.ModPreview, jobID, err)
		}
	}

	blob := func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(path)))
	}

	if err := Upload(client, jobID, files, blob, api.CompletePreviewBody{TaskID: taskID, Kind: kind}, &api.DeclarePreviewMeta{
		HTMLRewrites: rewrites,
		FileCount:    len(files),
	}); err != nil {
		logf("[%s] preview: upload failed for mission %s: %v", logger.ModPreview, jobID, err)
	}
}

// RunRevive re-uploads a mission's cached artifact without rebuilding —
// the bridge's response to a GET .../pending-revives entry (task 4.4). If
// nothing is cached (never built on this device, or the cache was cleared),
// it logs and skips: the user gets no preview until the mission runs (and
// merges) again.
//
// The complete call carries no taskId/kind (design): the content is
// unchanged, the snapshot is already registered, and the platform's
// soft-compat rule skips re-registration on an empty key.
func RunRevive(client Uploader, cache *Cache, jobID string, logf Logf) {
	if logf == nil {
		logf = noopLogf
	}
	if cache == nil {
		logf("[%s] preview: revive requested for mission %s but no artifact cache configured", logger.ModPreview, jobID)
		return
	}

	files, ok := cache.Manifest(jobID)
	if !ok {
		logf("[%s] preview: revive requested for mission %s but nothing cached, skipping", logger.ModPreview, jobID)
		return
	}

	blob := func(path string) ([]byte, error) {
		for _, f := range files {
			if f.Path == path {
				return cache.Blob(f.SHA256)
			}
		}
		return nil, fmt.Errorf("no cached file for path %s", path)
	}

	if err := Upload(client, jobID, files, blob, api.CompletePreviewBody{}, nil); err != nil {
		logf("[%s] preview: revive upload failed for mission %s: %v", logger.ModPreview, jobID, err)
	}
}
