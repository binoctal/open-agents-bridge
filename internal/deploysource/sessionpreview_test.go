package deploysource

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/api"
)

// --- RunSessionPreviewUpload (session-output-online-preview task 6.2) -----
//
// Same seams as the hosted-deploy tests above: a temp file tree as the
// session's work_dir and a fake client recording every boundary call. The one
// dimension this tier adds is the snapshot id -- declare/complete carry it and
// SNAPSHOT_STALE (the user re-clicked preview mid-pack) must silently drop the
// pack instead of failing or uploading stale bytes.

// fakeSessionClient stubs SessionPreviewUploader.
type fakeSessionClient struct {
	declareFiles []api.PreviewFile
	declareResp  *api.SessionPreviewDeclareResponse
	declareErr   error
	completeErr  error
	uploadErr    error
	failErr      error

	declared   int
	declareI   string
	declareS   string
	completed  int
	completedI string
	completedS string
	failed     int
	failedI    string
	failedS    string
	failedD    string
	uploaded   map[string][]byte // url -> bytes
}

func (f *fakeSessionClient) DeclareSessionPreviewSource(instanceID, snapshotID string, files []api.PreviewFile) (*api.SessionPreviewDeclareResponse, error) {
	f.declared++
	f.declareI = instanceID
	f.declareS = snapshotID
	f.declareFiles = files
	if f.declareErr != nil {
		return nil, f.declareErr
	}
	return f.declareResp, nil
}

func (f *fakeSessionClient) CompleteSessionPreviewSource(instanceID, snapshotID string) error {
	f.completed++
	f.completedI = instanceID
	f.completedS = snapshotID
	return f.completeErr
}

func (f *fakeSessionClient) FailSessionPreviewSource(instanceID, snapshotID, detail string) error {
	f.failed++
	f.failedI = instanceID
	f.failedS = snapshotID
	f.failedD = detail
	return f.failErr
}

func (f *fakeSessionClient) UploadPreviewFile(url string, data []byte) error {
	if f.uploaded == nil {
		f.uploaded = map[string][]byte{}
	}
	f.uploaded[url] = data
	return f.uploadErr
}

func TestRunSessionPreviewUpload_HappyPath(t *testing.T) {
	root := writeTree(t, map[string]string{"index.html": "<h1>hi</h1>"})
	fake := &fakeSessionClient{
		declareResp: &api.SessionPreviewDeclareResponse{
			InstanceID: "sp-1",
			Uploads:    []api.PreviewUpload{{Path: "index.html", URL: "https://r2/put/index"}},
		},
	}

	RunSessionPreviewUpload(fake, api.SessionPreviewWork{
		InstanceID: "sp-1", SessionID: "sess-9", SnapshotID: "snap-7", WorkDir: root,
	}, nil)

	if fake.declared != 1 || fake.completed != 1 {
		t.Fatalf("expected declare+complete once, got %d/%d", fake.declared, fake.completed)
	}
	if fake.declareI != "sp-1" || fake.declareS != "snap-7" {
		t.Errorf("declare called with %s/%s", fake.declareI, fake.declareS)
	}
	if fake.completedI != "sp-1" || fake.completedS != "snap-7" {
		t.Errorf("complete called with %s/%s", fake.completedI, fake.completedS)
	}
	if len(fake.declareFiles) != 1 || fake.declareFiles[0].SHA256 != hash("<h1>hi</h1>") {
		t.Errorf("declare manifest = %v", fake.declareFiles)
	}
	if got := fake.uploaded["https://r2/put/index"]; string(got) != "<h1>hi</h1>" {
		t.Errorf("uploaded bytes = %q", got)
	}
}

func TestRunSessionPreviewUpload_DeclareStaleDropsPack(t *testing.T) {
	// The user re-clicked preview before this pack landed: the row moved to a
	// new snapshot and declare answers 409 SNAPSHOT_STALE. Dropping quietly is
	// correct -- the next poll delivers the fresh snapshot id.
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeSessionClient{
		declareErr: &api.PreviewAPIError{StatusCode: 409, Code: "SNAPSHOT_STALE"},
	}

	RunSessionPreviewUpload(fake, api.SessionPreviewWork{
		InstanceID: "sp-1", SnapshotID: "snap-old", WorkDir: root,
	}, nil)

	if fake.completed != 0 || len(fake.uploaded) != 0 {
		t.Errorf("stale pack must upload and complete nothing, got completed=%d uploaded=%d",
			fake.completed, len(fake.uploaded))
	}
	if fake.declareS != "snap-old" {
		t.Errorf("declare must carry the pack's own snapshot id, got %s", fake.declareS)
	}
}

func TestRunSessionPreviewUpload_CompleteStaleIsQuiet(t *testing.T) {
	// Stale can also surface at complete (the swap landed between upload and
	// complete). The bytes are already in the bucket under the dead snapshot's
	// prefix; nothing to retry, nothing to log as an error.
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeSessionClient{
		declareResp: &api.SessionPreviewDeclareResponse{
			Uploads: []api.PreviewUpload{{Path: "a.ts", URL: "https://r2/put/a"}},
		},
		completeErr: &api.PreviewAPIError{StatusCode: 409, Code: "SNAPSHOT_STALE"},
	}

	RunSessionPreviewUpload(fake, api.SessionPreviewWork{
		InstanceID: "sp-1", SnapshotID: "snap-old", WorkDir: root,
	}, nil)

	if fake.declared != 1 || fake.completed != 1 {
		t.Fatalf("expected declare+complete to both fire once, got %d/%d", fake.declared, fake.completed)
	}
}

func TestRunSessionPreviewUpload_UploadFailureSkipsComplete(t *testing.T) {
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeSessionClient{
		declareResp: &api.SessionPreviewDeclareResponse{
			Uploads: []api.PreviewUpload{{Path: "a.ts", URL: "https://r2/put/a"}},
		},
		uploadErr: errors.New("ExpiredRequest"),
	}

	RunSessionPreviewUpload(fake, api.SessionPreviewWork{
		InstanceID: "sp-1", SnapshotID: "snap-7", WorkDir: root,
	}, nil)

	if fake.completed != 0 {
		t.Errorf("upload failure must not call complete (row stays pending, poll retries)")
	}
}

func TestRunSessionPreviewUpload_DeclareFailureIsBestEffort(t *testing.T) {
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeSessionClient{
		declareErr: &api.PreviewAPIError{StatusCode: 503, Code: "SESSION_PREVIEW_PLATFORM_BUSY"},
	}

	RunSessionPreviewUpload(fake, api.SessionPreviewWork{
		InstanceID: "sp-1", SnapshotID: "snap-7", WorkDir: root,
	}, nil)

	if fake.completed != 0 || len(fake.uploaded) != 0 {
		t.Errorf("declare failure must stop the pipeline, got completed=%d uploaded=%d",
			fake.completed, len(fake.uploaded))
	}
}

func TestRunSessionPreviewUpload_MissingWorkDirReportsPackFailure(t *testing.T) {
	// A vanished work_dir (session wiped mid-poll) is a DETERMINISTIC pack
	// failure: the row must be failed via the report endpoint (otherwise the
	// poll re-offers the same dead tree forever) and declare/complete never run.
	fake := &fakeSessionClient{}

	RunSessionPreviewUpload(fake, api.SessionPreviewWork{
		InstanceID: "sp-1", SnapshotID: "snap-7", WorkDir: "/nonexistent/workdir",
	}, nil)

	if fake.failed != 1 || fake.failedI != "sp-1" || fake.failedS != "snap-7" {
		t.Errorf("pack failure must be reported once with id/snapshot, got failed=%d %s/%s",
			fake.failed, fake.failedI, fake.failedS)
	}
	if fake.failedD == "" {
		t.Error("pack-failure report must carry the pack error as detail")
	}
	if fake.declared != 0 || fake.completed != 0 {
		t.Errorf("failed pack must not declare/complete, got declared=%d completed=%d",
			fake.declared, fake.completed)
	}
}

func TestRunSessionPreviewUpload_PackFailureStaleReportIsQuiet(t *testing.T) {
	// The user swapped before the failure report landed: the endpoint answers
	// 409 SNAPSHOT_STALE and the report is dropped without noise (the new
	// snapshot's pack owns the row now).
	fake := &fakeSessionClient{
		failErr: &api.PreviewAPIError{StatusCode: 409, Code: "SNAPSHOT_STALE"},
	}

	RunSessionPreviewUpload(fake, api.SessionPreviewWork{
		InstanceID: "sp-1", SnapshotID: "snap-old", WorkDir: "/nonexistent/workdir",
	}, func(format string, args ...interface{}) {
		if strings.Contains(fmt.Sprintf(format, args...), "pack-failure report failed") {
			t.Error("stale failure report must be dropped quietly")
		}
	})

	if fake.failed != 1 {
		t.Errorf("report must still fire once, got %d", fake.failed)
	}
}
