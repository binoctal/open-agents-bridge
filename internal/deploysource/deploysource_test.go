package deploysource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	declared      int
	completed     int
	completedM    string
	completedD    string
	completedBody *api.CompleteSourceVerify
	uploaded      map[string][]byte // url -> bytes

	// chunked-call recording: every declare's (composePath, chunk) in order.
	declareCalls []struct {
		compose string
		chunk   *api.DeclareChunk
		files   int
	}

	// declareHook runs after a successful declare record — tests use it to
	// mutate the tree between the pack walk and the upload reads.
	declareHook func()
}

func (f *fakeUploader) DeclareHostedSource(missionID, deploymentID string, files []api.PreviewFile, composePath string, chunk *api.DeclareChunk) (*api.DeclareSourceResponse, error) {
	f.declared++
	f.declareFiles = files
	f.declareCompose = composePath
	f.declareCalls = append(f.declareCalls, struct {
		compose string
		chunk   *api.DeclareChunk
		files   int
	}{composePath, chunk, len(files)})
	if f.declareErr != nil {
		return nil, f.declareErr
	}
	if f.declareHook != nil {
		f.declareHook()
	}
	return f.declareResp, nil
}

func (f *fakeUploader) CompleteHostedSource(missionID, deploymentID string, verify *api.CompleteSourceVerify) error {
	f.completed++
	f.completedM = missionID
	f.completedD = deploymentID
	f.completedBody = verify
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
	}, nil)

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

func TestRunSourceUpload_ConcurrentEditAbortsInsteadOfCorrupting(t *testing.T) {
	// The pack hashed the file at walk time; a workspace edit between declare
	// and the PUT read would otherwise upload new bytes under the old
	// content-addressed key and the hosting node fails materialize on the sha
	// mismatch (2026-10-31 vnivni round 6: an active session kept editing the
	// tree mid-upload). The cycle must abort — never complete, never PUT.
	root := writeTree(t, map[string]string{"src/app.ts": "app"})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{
			DeploymentID: "dep1",
			Uploads:      []api.PreviewUpload{{Path: "src/app.ts", URL: "https://r2/put/app"}},
		},
	}
	fake.declareHook = func() {
		if err := os.WriteFile(filepath.Join(root, "src/app.ts"), []byte("edited"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var logs []string
	RunSourceUpload(fake, "m1", "dep1", root, func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}, nil)

	if fake.completed != 0 {
		t.Fatalf("complete must not fire on a concurrent-edit abort, got %d", fake.completed)
	}
	if len(fake.uploaded) != 0 {
		t.Errorf("no bytes should be PUT, got %v", fake.uploaded)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "changed during upload") {
		t.Errorf("abort log should name the concurrent edit, got: %s", joined)
	}
}

func TestRunSourceUpload_SkipListMeansZeroUploads(t *testing.T) {
	// Everything already in the bucket: declare returns no uploads, complete
	// still fires (the platform verifies via LIST on its side).
	root := writeTree(t, map[string]string{"a.ts": "a"})
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"},
	}

	RunSourceUpload(fake, "m1", "dep1", root, nil, nil)

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

	RunSourceUpload(fake, "m1", "dep1", root, nil, nil)

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

	RunSourceUpload(fake, "m1", "dep1", root, nil, nil)

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
	RunSourceUpload(fake, "m1", "dep1", root, logf, nil)

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
	}, nil)

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
	RunSourceUpload(fake, "m1", "dep1", root, nil, nil)
	if fake.declareCompose != "" {
		t.Errorf("single-container tree must declare empty compose_path, got %q", fake.declareCompose)
	}
}

// Git-aware packing: in a real git work tree the file set is exactly what a
// commit would carry — ignored material (virtualenvs, dumps) stays out even
// without matching ExcludedDirs, while untracked-but-not-ignored files (a
// freshly written compose file) ride along.
func TestPackSourceTree_GitRepoHonorsGitignoreAndKeepsUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	for p, content := range map[string]string{
		"README.md":               "tracked",
		"docker-compose.yml":      "untracked but packable",
		"scratch/vendor-blob.bin": "ignored material",
	} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "t@t"},
		{"git", "config", "user.name", "t"},
		{"git", "add", "README.md"},
		{"git", "commit", "-qm", "init"},
	} {
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("scratch/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := PackSourceTree(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range res.Files {
		got[f.Path] = true
	}
	if !got["README.md"] || !got["docker-compose.yml"] {
		t.Errorf("tracked file or untracked compose missing: %v", got)
	}
	if got["scratch/vendor-blob.bin"] {
		t.Errorf("gitignored material must not ride the pack: %v", got)
	}
	if res.ComposePath != "docker-compose.yml" {
		t.Errorf("compose detection should see the untracked compose file, got %q", res.ComposePath)
	}
}

// --- chunked declare (large trees vs the API worker CPU budget) -------------

// writeMany writes n tiny numbered files under root (deterministic paths).
func writeMany(t *testing.T, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%05d.txt", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("content-%d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// declareChunkFiles+1 files plus a compose file: two parts (800 + remainder),
// compose detection on, feature advertised.
func TestRunSourceUpload_ChunkedSlicesLargeTrees(t *testing.T) {
	root := t.TempDir()
	writeMany(t, root, declareChunkFiles+1)
	if err := os.WriteFile(filepath.Join(root, "docker-compose.yml"), []byte("services: {web: {image: nginx}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeUploader{declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"}}

	var logs []string
	RunSourceUpload(fake, "m1", "dep1", root, func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}, []string{"chunked_declare"})

	total := declareChunkFiles + 2 // numbered files + compose
	if len(fake.declareCalls) != 2 {
		t.Fatalf("expected 2 declares, got %d", len(fake.declareCalls))
	}
	first, last := fake.declareCalls[0], fake.declareCalls[1]
	if first.chunk == nil || last.chunk == nil {
		t.Fatal("both parts must carry chunk metadata")
	}
	if first.chunk.Part != 0 || last.chunk.Part != 1 || first.chunk.TotalParts != 2 || last.chunk.TotalParts != 2 {
		t.Errorf("chunk parts wrong: %+v %+v", first.chunk, last.chunk)
	}
	if first.chunk.TotalBytes == 0 || first.chunk.TotalBytes != last.chunk.TotalBytes {
		t.Errorf("whole-tree total must ride every part identically: %d vs %d", first.chunk.TotalBytes, last.chunk.TotalBytes)
	}
	if first.files != declareChunkFiles || last.files != total-declareChunkFiles {
		t.Errorf("slice sizes wrong: %d then %d (total %d)", first.files, last.files, total)
	}
	// "docker-compose.yml" sorts inside part 0, but the wire contract sends it
	// on the FINAL part only (the platform stamps at assembly).
	if first.compose != "" || last.compose != "docker-compose.yml" {
		t.Errorf("compose path must ride the final part only: %q then %q", first.compose, last.compose)
	}
	if fake.completedBody == nil || len(fake.completedBody.Files) != total {
		t.Fatalf("complete verify must carry all %d path+sha pairs, got %+v", total, fake.completedBody)
	}
	if fake.completedBody.Files[0].Path != "docker-compose.yml" {
		t.Errorf("verify projection must keep manifest order, first = %q", fake.completedBody.Files[0].Path)
	}
}

// Version-skew guard: the same large tree against an API that did not
// advertise chunked_declare must take the single legacy declare (which fails
// loudly server-side on oversize) instead of half-declaring a chunked tree.
func TestRunSourceUpload_FallsBackToSingleDeclareWithoutFeature(t *testing.T) {
	root := t.TempDir()
	writeMany(t, root, declareChunkFiles+50)
	fake := &fakeUploader{declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"}}

	RunSourceUpload(fake, "m1", "dep1", root, nil, nil)

	if len(fake.declareCalls) != 1 || fake.declareCalls[0].chunk != nil {
		t.Fatalf("old API must get one legacy declare, got %+v", fake.declareCalls)
	}
	if fake.completedBody != nil {
		t.Errorf("legacy complete carries no verify body, got %+v", fake.completedBody)
	}
}

// A failure anywhere in the part loop stops the upload without completing --
// the row stays pending and the next poll restarts at part 0 (skip-listed).
func TestRunSourceUpload_ChunkedDeclareFailureNeverCompletes(t *testing.T) {
	root := t.TempDir()
	writeMany(t, root, declareChunkFiles+1)
	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{DeploymentID: "dep1"},
		declareErr:  errors.New("CF 1102"),
	}

	RunSourceUpload(fake, "m1", "dep1", root, nil, []string{"chunked_declare"})

	if fake.completed != 0 {
		t.Errorf("declare failure must not complete, got completed=%d", fake.completed)
	}
}
