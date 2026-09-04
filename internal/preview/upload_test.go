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
	createResp    *api.DeclarePreviewResponse
	createErr     error
	completeErr   error
	uploadErr     error
	createCalls   int
	uploadedURLs  []string
	uploadedBytes map[string][]byte
	completedID   string
}

func (f *fakeUploader) CreatePreview(jobID string, files []api.PreviewFile) (*api.DeclarePreviewResponse, error) {
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createResp, nil
}

func (f *fakeUploader) CompletePreview(jobID, previewID string) error {
	f.completedID = previewID
	return f.completeErr
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

	err := Upload(fake, "mission-1", testFiles(), testBlob)
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

	err := Upload(fake, "mission-1", testFiles(), testBlob)
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

	if err := Upload(captureUploader, "mission-1", files, testBlob); err != nil {
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

func (c *capturingUploader) CreatePreview(jobID string, files []api.PreviewFile) (*api.DeclarePreviewResponse, error) {
	if c.onCreate != nil {
		c.onCreate(files)
	}
	return c.fakeUploader.CreatePreview(jobID, files)
}

func TestUpload_MissingRootIndexRejected(t *testing.T) {
	fake := &fakeUploader{createResp: &api.DeclarePreviewResponse{PreviewID: "p1"}}
	files := []ManifestFile{{Path: "assets/app.js", SHA256: "bbb", Size: 4}}

	err := Upload(fake, "mission-1", files, testBlob)
	if err == nil {
		t.Fatal("expected error when manifest has no root index.html")
	}
	if fake.createCalls != 0 {
		t.Error("must not call CreatePreview when the manifest is invalid")
	}
}

func TestUpload_CreateErrorStopsEarly(t *testing.T) {
	fake := &fakeUploader{createErr: fmt.Errorf("PREVIEW_QUOTA_EXCEEDED")}

	err := Upload(fake, "mission-1", testFiles(), testBlob)
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

	err := Upload(fake, "mission-1", testFiles(), testBlob)
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
	RunAndUpload(fake, nil, "mission-1", dir, func(f string, a ...interface{}) {
		logs = append(logs, fmt.Sprintf(f, a...))
	})
	if fake.createCalls != 0 {
		t.Error("must not call the API when there is no build script")
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
	if string(fake.uploadedBytes["https://r2.example.com/put-index"]) != "<html></html>" {
		t.Errorf("expected cached bytes to be re-uploaded, got %q", fake.uploadedBytes["https://r2.example.com/put-index"])
	}
}
