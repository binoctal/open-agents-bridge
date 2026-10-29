package deploysource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/preview"
)

// --- PackSourceTree ---------------------------------------------------------

// writeTree creates files under a temp root from a path->content map.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func paths(res *PackResult) map[string]bool {
	out := map[string]bool{}
	for _, f := range res.Files {
		out[f.Path] = true
	}
	return out
}

func TestPackSourceTree_ExcludesDirsAndStripsSensitive(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/app.ts":                 "app",
		"package.json":               "{}",
		".git/config":                "git-internal",
		"node_modules/x/i.js":        "dep",
		"apps/web/dist/i.html":       "build-output",
		"a/.next/b.js":               "framework-cache",
		".env":                       "SECRET=1",
		".env.production":            "SECRET=2",
		"certs/server.pem":           "cert",
		"ssh/id_ed25519":             "key",
		"gcp/service-account-x.json": "creds",
	})

	res, err := PackSourceTree(root)
	if err != nil {
		t.Fatal(err)
	}

	got := paths(res)
	if !got["src/app.ts"] || !got["package.json"] {
		t.Errorf("source files missing from manifest: %v", got)
	}
	for _, excluded := range []string{
		".git/config", "node_modules/x/i.js", "apps/web/dist/i.html", "a/.next/b.js",
		".env", ".env.production", "certs/server.pem", "ssh/id_ed25519", "gcp/service-account-x.json",
	} {
		if got[excluded] {
			t.Errorf("excluded/sensitive path packed: %s", excluded)
		}
	}
	if len(res.Stripped) != 5 {
		t.Errorf("expected 5 stripped sensitive files, got %v", res.Stripped)
	}

	// Hash contract: same bytes, same sha256 as the platform computes.
	for _, f := range res.Files {
		want := hash(map[string]string{"src/app.ts": "app", "package.json": "{}"}[f.Path])
		if f.SHA256 != want {
			t.Errorf("hash mismatch for %s", f.Path)
		}
	}
}

// S6 regression (2026-09-08 prod smoke): a leftover mission task worktree made
// the pack declare .open-agents-bridge-worktrees/task-x/t0/.git -- a linked
// worktree's .git is a POINTER FILE, not a directory, so the directory skip
// never fired and the platform rejected the whole declare (SOURCE_EXCLUDED_PATH).
func TestPackSourceTree_SkipsBridgeWorktreesAndDotGitPointerFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"index.html": "hello",
		// The linked-worktree shape: .git as a FILE (gitdir pointer).
		".open-agents-bridge-worktrees/task-job_x-t0/.git":   "gitdir: /repo/.git/worktrees/t0",
		".open-agents-bridge-worktrees/task-job_x-t0/app.ts": "leftover",
		".open-agents-bridge-worktrees/task-job_x-t0/.env":   "SECRET",
		// A bare .git file at the repo root too (submodule-style pointer).
		".git": "gitdir: elsewhere",
	})

	res, err := PackSourceTree(root)
	if err != nil {
		t.Fatal(err)
	}

	got := paths(res)
	if !got["index.html"] {
		t.Errorf("real source file missing from manifest: %v", got)
	}
	for _, excluded := range []string{
		".git",
		".open-agents-bridge-worktrees/task-job_x-t0/.git",
		".open-agents-bridge-worktrees/task-job_x-t0/app.ts",
	} {
		if got[excluded] {
			t.Errorf("worktree residue packed: %s", excluded)
		}
	}
	if len(res.Files) != 1 {
		t.Errorf("expected only index.html, got %v", got)
	}
}

func TestPackSourceTree_DeterministicOrder(t *testing.T) {
	root := writeTree(t, map[string]string{
		"b.txt":   "1",
		"a.txt":   "2",
		"c/d.txt": "3",
	})
	res, err := PackSourceTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files[0].Path != "a.txt" || res.Files[1].Path != "b.txt" || res.Files[2].Path != "c/d.txt" {
		t.Errorf("manifest not sorted: %v", res.Files)
	}
}

func TestPackSourceTree_OversizeFileIsAnErrorNotATruncation(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, MaxFileBytes+1)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PackSourceTree(root); err == nil {
		t.Fatal("oversize file must fail the pack, not upload a truncated tree")
	}
}

func TestPackSourceTree_EmptyTreeFails(t *testing.T) {
	root := writeTree(t, map[string]string{
		"node_modules/only-this.js": "excluded",
	})
	if _, err := PackSourceTree(root); err == nil {
		t.Fatal("a tree with nothing packable must fail, not declare an empty manifest")
	}
}

// --- RunSourceUpload --------------------------------------------------------

// fakeUploader stubs the deploy boundary (same seam as preview's fakeUploader).
type fakeUploader struct {
	declareFiles   []api.PreviewFile
	declareCompose string
	declareResp    *api.DeclareSourceResponse
	declareErr     error
	completeErr    error
	uploadErr      error

	declared   int
	completed  int
	completedM string
	completedD string
	uploaded   map[string][]byte // url -> bytes
}

func (f *fakeUploader) DeclareHostedSource(missionID, deploymentID string, files []api.PreviewFile, composePath string) (*api.DeclareSourceResponse, error) {
	f.declared++
	f.declareFiles = files
	f.declareCompose = composePath
	if f.declareErr != nil {
		return nil, f.declareErr
	}
	return f.declareResp, nil
}

func (f *fakeUploader) CompleteHostedSource(missionID, deploymentID string) error {
	f.completed++
	f.completedM = missionID
	f.completedD = deploymentID
	return f.completeErr
}

func (f *fakeUploader) UploadPreviewFile(url string, data []byte) error {
	if f.uploaded == nil {
		f.uploaded = map[string][]byte{}
	}
	f.uploaded[url] = data
	return f.uploadErr
}

func TestRunSourceUpload_HappyPath(t *testing.T) {
	root := writeTree(t, map[string]string{"src/app.ts": "app"})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{
			DeploymentID: "dep1",
			Uploads:      []api.PreviewUpload{{Path: "src/app.ts", URL: "https://r2/put/app"}},
		},
	}

	var logs []string
	RunSourceUpload(fake, "m1", "dep1", root, func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	if fake.declared != 1 || fake.completed != 1 {
		t.Fatalf("expected declare+complete once, got %d/%d", fake.declared, fake.completed)
	}
	if fake.completedM != "m1" || fake.completedD != "dep1" {
		t.Errorf("complete called with %s/%s", fake.completedM, fake.completedD)
	}
	if got := fake.uploaded["https://r2/put/app"]; string(got) != "app" {
		t.Errorf("uploaded bytes = %q", got)
	}
	if len(fake.declareFiles) != 1 || fake.declareFiles[0].SHA256 != hash("app") {
		t.Errorf("declare manifest = %v", fake.declareFiles)
	}
}

func TestRunSourceUpload_SkipListMeansZeroUploads(t *testing.T) {
	// Everything already in the bucket: declare returns no uploads, complete
	// still fires (the platform verifies via LIST on its side).
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"},
	}

	RunSourceUpload(fake, "m1", "dep1", root, nil)

	if fake.declared != 1 || fake.completed != 1 {
		t.Fatalf("expected declare+complete once, got %d/%d", fake.declared, fake.completed)
	}
	if len(fake.uploaded) != 0 {
		t.Errorf("skip-listed pack must upload nothing, uploaded %v", fake.uploaded)
	}
}

func TestRunSourceUpload_DeclareFailureSkipsUploadAndComplete(t *testing.T) {
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeUploader{
		declareErr: &api.PreviewAPIError{StatusCode: 400, Code: "SOURCE_SENSITIVE_FILE"},
	}

	RunSourceUpload(fake, "m1", "dep1", root, nil)

	if fake.completed != 0 || len(fake.uploaded) != 0 {
		t.Errorf("declare failure must stop the pipeline, got completed=%d uploaded=%d",
			fake.completed, len(fake.uploaded))
	}
}

func TestRunSourceUpload_UploadFailureSkipsComplete(t *testing.T) {
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{
			DeploymentID: "dep1",
			Uploads:      []api.PreviewUpload{{Path: "a.ts", URL: "https://r2/put/a"}},
		},
		uploadErr: errors.New("ExpiredRequest"),
	}

	RunSourceUpload(fake, "m1", "dep1", root, nil)

	if fake.completed != 0 {
		t.Errorf("upload failure must not call complete (row stays pending, poll retries)")
	}
}

func TestRunSourceUpload_StrippedFilesAreLogged(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.ts": "a",
		".env": "SECRET",
	})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"},
	}

	var logs []string
	logf := func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	RunSourceUpload(fake, "m1", "dep1", root, logf)

	found := false
	for _, l := range logs {
		if strings.Contains(l, ".env") {
			found = true
		}
	}
	if !found {
		t.Errorf("strip decisions must be visible in logs, got %v", logs)
	}
	if len(fake.declareFiles) != 1 || fake.declareFiles[0].Path != "a.ts" {
		t.Errorf("stripped file must not be declared: %v", fake.declareFiles)
	}
}

// --- compose detection (hosted-compose-stacks 3.1) ---------------------------

// Detection order mirrors the platform's COMPOSE_DIR_CANDIDATES x
// COMPOSE_FILE_NAMES: root docker-compose.yml wins over every other
// candidate; deploy/ ranks below root regardless of spelling.
func TestDetectComposePath_ConventionOrder(t *testing.T) {
	all := map[string]bool{}
	for _, p := range []string{
		"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml",
		"deploy/docker-compose.yml", "deploy/docker-compose.yaml", "deploy/compose.yml", "deploy/compose.yaml",
	} {
		all[p] = true
		files := []preview.ManifestFile{{Path: p}, {Path: "src/app.ts"}}
		if got := DetectComposePath(files); got != p {
			t.Errorf("single candidate %s: detected %q", p, got)
		}
	}
	if got := DetectComposePath([]preview.ManifestFile{{Path: "docker-compose.yml"}, {Path: "compose.yml"}}); got != "docker-compose.yml" {
		t.Errorf("root docker-compose.yml must outrank compose.yml, got %q", got)
	}
	if got := DetectComposePath([]preview.ManifestFile{{Path: "deploy/docker-compose.yml"}, {Path: "compose.yaml"}}); got != "compose.yaml" {
		t.Errorf("root candidates must outrank deploy/, got %q", got)
	}
}

func TestDetectComposePath_NonConventionAndEmpty(t *testing.T) {
	for _, p := range []string{"ops/docker-compose.yml", "docker-compose.prod.yml", "src/compose.yaml"} {
		if got := DetectComposePath([]preview.ManifestFile{{Path: p}}); got != "" {
			t.Errorf("non-convention path %s must not detect, got %q", p, got)
		}
	}
	if got := DetectComposePath([]preview.ManifestFile{{Path: "a.ts"}}); got != "" {
		t.Errorf("plain tree must not detect, got %q", got)
	}
}

func TestRunSourceUpload_DeclaresComposePathWhenDetected(t *testing.T) {
	root := writeTree(t, map[string]string{
		"src/app.ts":                "app",
		"deploy/docker-compose.yml": "services: {web: {image: nginx}}",
	})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"},
	}

	var logs []string
	RunSourceUpload(fake, "m1", "dep1", root, func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	if fake.declareCompose != "deploy/docker-compose.yml" {
		t.Errorf("declare must carry the detected path, got %q", fake.declareCompose)
	}
	found := false
	for _, l := range logs {
		if strings.Contains(l, "deploy/docker-compose.yml") {
			found = true
		}
	}
	if !found {
		t.Errorf("detection must be visible in logs, got %v", logs)
	}
}

// Zero-change guarantee: a tree without a convention-path compose file must
// declare with an EMPTY compose_path (omitempty keeps the wire byte-identical
// to pre-compose bridges).
func TestRunSourceUpload_NoComposeDeclaresEmptyPath(t *testing.T) {
	root := writeTree(t, map[string]string{"src/app.ts": "app"})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"},
	}
	RunSourceUpload(fake, "m1", "dep1", root, nil)
	if fake.declareCompose != "" {
		t.Errorf("single-container tree must declare empty compose_path, got %q", fake.declareCompose)
	}
}
