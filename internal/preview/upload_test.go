package preview

import (
	"fmt"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/api"
)

// fakeUploader stubs the HTTP boundary (api.Client) so upload orchestration
// logic can be tested without a network call, following the same seam
// approach as api/client_test.go's httptest servers but one layer up.
type fakeUploader struct {
	createResp  *api.DeclarePreviewResponse
	createErr   error
	completeErr error
	uploadErr   error
	kindErr     error

	createCalls  int
	uploadedURLs []string
	uploadedBytes map[string][]byte

	createMeta    *api.DeclarePreviewMeta
	completedID   string
	completedBody api.CompletePreviewBody
	reportedKinds []string
}

func (f *fakeUploader) CreatePreview(jobID string, files []api.PreviewFile, meta *api.DeclarePreviewMeta) (*api.DeclarePreviewResponse, error) {
	f.createCalls++
	f.createMeta = meta
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createResp, nil
}

func (f *fakeUploader) CompletePreview(jobID, previewID string, body api.CompletePreviewBody) error {
	f.completedID = previewID
	f.completedBody = body
	return f.completeErr
}

func (f *fakeUploader) ReportArtifactKind(jobID, kind string) error {
	f.reportedKinds = append(f.reportedKinds, kind)
	return f.kindErr
}

func (f *fakeUploader) UploadPreviewFile(url string, data []byte) error {
	f.uploadedURLs = append(f.uploadedURLs, url)
	if f.uploadedBytes == nil {
		f.uploadedBytes = map[string][]byte{}
	}
	f.uploadedBytes[url] = data
	return f.uploadErr
}

func testFiles() []ManifestFile {
	return []ManifestFile{
		{Path: "index.html", SHA256: "aaa", Size: 3},
		{Path: "assets/app.js", SHA256: "bbb", Size: 4},
	}
}

func testBlob(path string) ([]byte, error) {
	switch path {
	case "index.html":
		return []byte("<h>"), nil
	case "assets/app.js":
		return []byte("code"), nil
	default:
		return nil, fmt.Errorf("no such file: %s", path)
	}
}

func TestUpload_HappyPath(t *testing.T) {
	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			URL:       "https://preview.openagents.top/abc/",
			Uploads: []api.PreviewUpload{
				{Path: "index.html", URL: "https://r2.example.com/put-index"},
				{Path: "assets/app.js", URL: "https://r2.example.com/put-app"},
			},
		},
	}

	body := api.CompletePreviewBody{TaskID: "task-1", Kind: KindStatic}
	meta := &api.DeclarePreviewMeta{HTMLRewrites: 2, FileCount: 2}
	err := Upload(fake, "mission-1", testFiles(), testBlob, body, meta)
	if err != nil {
		t.Fatal(err)
	}
	if fake.createCalls != 1 {
		t.Errorf("expected exactly one CreatePreview call, got %d", fake.createCalls)
	}
	if len(fake.uploadedURLs) != 2 {
		t.Fatalf("expected 2 uploads, got %d", len(fake.uploadedURLs))
	}
	if fake.completedID != "p1" {
		t.Errorf("expected complete to be called with previewId p1, got %s", fake.completedID)
	}
	if fake.completedBody != body {
		t.Errorf("complete body = %+v, want %+v", fake.completedBody, body)
	}
	if fake.createMeta == nil || *fake.createMeta != *meta {
		t.Errorf("declare meta = %+v, want %+v", fake.createMeta, meta)
	}
}

// The revive path completes with a zero body: no taskId (the snapshot is
// already registered and the platform's soft-compat skips re-registration),
// no kind (detection never ran on a re-upload).
func TestUpload_ReviveCompletesWithEmptyBody(t *testing.T) {
	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads:   nil,
		},
	}

	if err := Upload(fake, "mission-1", testFiles(), testBlob, api.CompletePreviewBody{}, nil); err != nil {
		t.Fatal(err)
	}
	if fake.completedBody != (api.CompletePreviewBody{}) {
		t.Errorf("revive complete body must be zero-valued, got %+v", fake.completedBody)
	}
	if fake.createMeta != nil {
		t.Errorf("revive declare must carry no meta, got %+v", fake.createMeta)
	}
}

func TestUpload_SkipListHonored(t *testing.T) {
	// A revive where every file is already in the bucket: uploads is empty,
	// and Upload must still call complete without attempting any PUT.
	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads:   nil,
		},
	}

	err := Upload(fake, "mission-1", testFiles(), testBlob, api.CompletePreviewBody{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.uploadedURLs) != 0 {
		t.Errorf("expected no uploads when skip-list covers everything, got %v", fake.uploadedURLs)
	}
	if fake.completedID != "p1" {
		t.Error("expected complete to still be called")
	}
}

func TestUpload_FiltersMapFiles(t *testing.T) {
	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{PreviewID: "p1"},
	}
	files := append(testFiles(), ManifestFile{Path: "assets/app.js.map", SHA256: "ccc", Size: 10})

	var sentFiles []api.PreviewFile
	captureUploader := &capturingUploader{fakeUploader: fake, onCreate: func(f []api.PreviewFile) { sentFiles = f }}

	if err := Upload(captureUploader, "mission-1", files, testBlob, api.CompletePreviewBody{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, f := range sentFiles {
		if f.Path == "assets/app.js.map" {
			t.Fatal(".map file must never reach CreatePreview, even as a defense-in-depth check")
		}
	}
}

type capturingUploader struct {
	*fakeUploader
	onCreate func([]api.PreviewFile)
}

func (c *capturingUploader) CreatePreview(jobID string, files []api.PreviewFile, meta *api.DeclarePreviewMeta) (*api.DeclarePreviewResponse, error) {
	if c.onCreate != nil {
		c.onCreate(files)
	}
	return c.fakeUploader.CreatePreview(jobID, files, meta)
}

func TestUpload_MissingRootIndexRejected(t *testing.T) {
	fake := &fakeUploader{createResp: &api.DeclarePreviewResponse{PreviewID: "p1"}}
	files := []ManifestFile{{Path: "assets/app.js", SHA256: "bbb", Size: 4}}

	err := Upload(fake, "mission-1", files, testBlob, api.CompletePreviewBody{}, nil)
	if err == nil {
		t.Fatal("expected error when manifest has no root index.html")
	}
	if fake.createCalls != 0 {
		t.Error("must not call CreatePreview when the manifest is invalid")
	}
}

func TestUpload_CreateErrorStopsEarly(t *testing.T) {
	fake := &fakeUploader{createErr: fmt.Errorf("PREVIEW_QUOTA_EXCEEDED")}

	err := Upload(fake, "mission-1", testFiles(), testBlob, api.CompletePreviewBody{}, nil)
	if err == nil {
		t.Fatal("expected error to propagate")
	}
	if len(fake.uploadedURLs) != 0 {
		t.Error("must not attempt uploads when declare failed")
	}
}

func TestUpload_PutFailureAbortsBeforeComplete(t *testing.T) {
	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads: []api.PreviewUpload{
				{Path: "index.html", URL: "https://r2.example.com/put-index"},
			},
		},
		uploadErr: fmt.Errorf("connection reset"),
	}

	err := Upload(fake, "mission-1", testFiles(), testBlob, api.CompletePreviewBody{}, nil)
	if err == nil {
		t.Fatal("expected error when a PUT fails")
	}
	if fake.completedID != "" {
		t.Error("complete must not be called when an upload failed")
	}
}

func TestRunAndUpload_NoBuildScriptSkipsSilently(t *testing.T) {
	dir := t.TempDir()
	// No package.json at all.
	fake := &fakeUploader{}
	var logs []string
	RunAndUpload(fake, nil, "mission-1", dir, "task-1", func(f string, a ...interface{}) {
		logs = append(logs, fmt.Sprintf(f, a...))
	})
	if fake.createCalls != 0 {
		t.Error("must not call the API when there is no build script")
	}
	if len(fake.reportedKinds) != 0 {
		t.Error("must not report artifact kind without a build")
	}
	if len(logs) == 0 {
		t.Error("expected a log line explaining the skip")
	}
}

func TestRunRevive_NothingCachedSkipsSilently(t *testing.T) {
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeUploader{}
	var logged bool
	RunRevive(fake, cache, "never-built", func(f string, a ...interface{}) { logged = true })
	if fake.createCalls != 0 {
		t.Error("must not call the API when nothing is cached for the mission")
	}
	if !logged {
		t.Error("expected a log line explaining the skip")
	}
}

func TestRunRevive_UsesCachedArtifact(t *testing.T) {
	outputDir := t.TempDir()
	writeFile(t, outputDir, "index.html", "<html></html>")

	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, err := BuildManifest(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Store("mission-1", outputDir, files); err != nil {
		t.Fatal(err)
	}

	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads: []api.PreviewUpload{
				{Path: "index.html", URL: "https://r2.example.com/put-index"},
			},
		},
	}

	RunRevive(fake, cache, "mission-1", nil)

	if fake.createCalls != 1 {
		t.Fatalf("expected CreatePreview to be called, calls=%d", fake.createCalls)
	}
	if fake.completedID != "p1" {
		t.Error("expected complete to be called on successful revive")
	}
	if fake.completedBody != (api.CompletePreviewBody{}) {
		t.Errorf("revive complete must carry no taskId/kind, got %+v", fake.completedBody)
	}
	if len(fake.reportedKinds) != 0 {
		t.Error("revive must not re-report artifact kind; it never rebuilt")
	}
	if string(fake.uploadedBytes["https://r2.example.com/put-index"]) != "<html></html>" {
		t.Errorf("expected cached bytes to be re-uploaded, got %q", fake.uploadedBytes["https://r2.example.com/put-index"])
	}
}
