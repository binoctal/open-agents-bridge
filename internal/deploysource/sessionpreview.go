// Session preview packing (session-output-online-preview task 3.1): the
// session tier's variant of RunSourceUpload. Same PackSourceTree (exclusion
// list, sensitive strip, 25MB/300MB ceilings -- honest errors, no
// truncation), same two-phase protocol, different destination namespace and
// one extra dimension: the snapshot id. A user clicking "preview" again
// mid-pack supersedes the snapshot; declare then answers SNAPSHOT_STALE and
// this side simply drops the work (the next poll offers the fresh id).
package deploysource

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// SessionPreviewUploader is the subset of *api.Client this path needs.
type SessionPreviewUploader interface {
	DeclareSessionPreviewSource(instanceID, snapshotID string, files []api.PreviewFile) (*api.SessionPreviewDeclareResponse, error)
	CompleteSessionPreviewSource(instanceID, snapshotID string) error
	FailSessionPreviewSource(instanceID, snapshotID, detail string) error
	UploadPreviewFile(url string, data []byte) error
}

// isSnapshotStale detects the superseded-snapshot 409 from the parsed
// PreviewAPIError code (the platform's declare/complete bodies carry it).
func isSnapshotStale(err error) bool {
	if e, ok := err.(*api.PreviewAPIError); ok {
		return e.Code == "SNAPSHOT_STALE"
	}
	return false
}

// RunSessionPreviewUpload packs the session's work_dir and runs the two-phase
// upload for one pending preview instance. Best-effort like every preview
// path: failures are logged, never propagated; the platform keeps offering
// the row (declare's skip-list means a retry only uploads what is missing).
func RunSessionPreviewUpload(client SessionPreviewUploader, work api.SessionPreviewWork, logf Logf) {
	if logf == nil {
		logf = noopLogf
	}

	pack, err := PackSourceTree(work.WorkDir)
	if err != nil {
		// Deterministic failure (size ceilings, unreadable tree): report it so
		// the platform fails the row -- otherwise every poll tick re-offers the
		// same un-packable tree and the user sees pending forever. SNAPSHOT_STALE
		// drops silently: the swap that superseded this pack owns the row now.
		if failErr := client.FailSessionPreviewSource(work.InstanceID, work.SnapshotID, err.Error()); failErr != nil && !isSnapshotStale(failErr) {
			logf("[%s] pack-failure report failed for preview %s: %v", logger.ModDeploy, work.InstanceID, failErr)
		}
		logf("[%s] work_dir pack failed for preview %s: %v", logger.ModDeploy, work.InstanceID, err)
		return
	}
	if len(pack.Stripped) > 0 {
		logf("[%s] stripped %d sensitive file(s) from preview %s source: %s",
			logger.ModDeploy, len(pack.Stripped), work.InstanceID, strings.Join(pack.Stripped, ", "))
	}

	decl, err := client.DeclareSessionPreviewSource(work.InstanceID, work.SnapshotID, toAPIFiles(pack.Files))
	if err != nil {
		if isSnapshotStale(err) {
			// The user requested a newer snapshot; this pack is dead. Not an
			// error -- the next poll delivers the fresh id.
			logf("[%s] snapshot %s superseded for preview %s; dropping this pack",
				logger.ModDeploy, work.SnapshotID, work.InstanceID)
			return
		}
		logf("[%s] declare failed for preview %s: %v", logger.ModDeploy, work.InstanceID, err)
		return
	}

	blob := func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(work.WorkDir, filepath.FromSlash(path)))
	}
	if err := uploadAll(client, decl.Uploads, blob); err != nil {
		logf("[%s] upload failed for preview %s: %v", logger.ModDeploy, work.InstanceID, err)
		return
	}

	if err := client.CompleteSessionPreviewSource(work.InstanceID, work.SnapshotID); err != nil {
		if isSnapshotStale(err) {
			logf("[%s] snapshot %s superseded for preview %s during complete; dropping",
				logger.ModDeploy, work.SnapshotID, work.InstanceID)
			return
		}
		logf("[%s] complete failed for preview %s: %v", logger.ModDeploy, work.InstanceID, err)
		return
	}
	logger.Info("[%s] session work_dir uploaded for preview %s (session %s): %d files, %d uploaded",
		logger.ModDeploy, work.InstanceID, work.SessionID, len(pack.Files), len(decl.Uploads))
}
