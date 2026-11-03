package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/binoctal/open-agents-bridge/node/api"
)

// fakePresigner hands out URLs against a local object-serving test server and
// counts calls so tests can assert presign-skip on a warm object store.
type fakePresigner struct {
	mu    sync.Mutex
	calls int
	base  string // object server origin
	fail  bool
}

func (f *fakePresigner) PresignSource(ctx context.Context, deploymentID string, shas []string) (*api.PresignResponse, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.fail {
		return nil, fmt.Errorf("presign backend down")
	}
	resp := &api.PresignResponse{}
	for _, sha := range shas {
		resp.URLs = append(resp.URLs, api.PresignedObject{
			Key: fmt.Sprintf("deploys/%s/%s", deploymentID, sha),
			URL: f.base + "/" + sha,
		})
	}
	return resp, nil
}

func (f *fakePresigner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
}

// objectServer serves sha-keyed bytes (the R2 stand-in).
func objectServer(objects map[string][]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := objects[r.URL.Path[1:]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
}

func file(path, content string) api.File {
	return api.File{Path: path, SHA256: sha([]byte(content)), Size: int64(len(content))}
}

func newTestStore(t *testing.T, objects map[string][]byte) (*Store, *fakePresigner) {
	t.Helper()
	srv := objectServer(objects)
	t.Cleanup(srv.Close)
	p := &fakePresigner{base: srv.URL}
	dir := t.TempDir()
	return New(dir, p), p
}

func TestMaterializeIdempotentSameCommit(t *testing.T) {
	requireGit(t)
	manifest := []api.File{
		file("index.html", "<h1>hello</h1>"),
		file("pkg/main.go", "package main\n"),
	}
	st, p := newTestStore(t, map[string][]byte{
		manifest[0].SHA256: []byte("<h1>hello</h1>"),
		manifest[1].SHA256: []byte("package main\n"),
	})

	c1, err := st.Materialize(context.Background(), "dep1", "deploys/dep1", manifest)
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	c2, err := st.Materialize(context.Background(), "dep1", "deploys/dep1", manifest)
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("same manifest must yield the same commit: %s != %s", c1, c2)
	}
	// Warm store: the second run must not presign anything.
	if got := p.callCount(); got != 1 {
		t.Fatalf("expected 1 presign call total (second run served from cache), got %d", got)
	}
}

// listRepoFiles shells out to the bare repo itself: the assertions care about
// the COMMITTED tree, not the scratch directory.
func listRepoFiles(t *testing.T, st *Store, deploymentID string) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", st.RepoPath(deploymentID), "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	files := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files[line] = true
		}
	}
	return files
}

// S4 (fix-seed-audit-blockers): the live incident was a static site whose
// package.json had no scripts -- nixpacks' node provider built it with an
// empty CMD, the container crash-looped, and the row read ready. The static
// fallback must move that package.json aside in the committed tree so
// nixpacks lands on its nginx static provider.
func TestMaterializeStaticFallbackMovesScriptlessPackageJson(t *testing.T) {
	requireGit(t)
	pkg := `{"name":"site","version":"1.0.0"}`
	manifest := []api.File{
		file("package.json", pkg),
		file("index.html", "<h1>static</h1>"),
	}
	st, _ := newTestStore(t, map[string][]byte{
		manifest[0].SHA256: []byte(pkg),
		manifest[1].SHA256: []byte("<h1>static</h1>"),
	})
	if _, err := st.Materialize(context.Background(), "dep-static", "deploys/dep-static", manifest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	files := listRepoFiles(t, st, "dep-static")
	if files["package.json"] {
		t.Fatal("scriptless package.json must be moved aside in the committed tree")
	}
	if !files[staticFallbackName] || !files["index.html"] {
		t.Fatalf("fallback file and index.html must survive, got %v", files)
	}
}

// The fallback is a pure function of the manifest: firing it twice yields the
// same commit (spec idempotence holds on the injected path too).
func TestMaterializeStaticFallbackIdempotentSameCommit(t *testing.T) {
	requireGit(t)
	pkg := `{"name":"site"}`
	manifest := []api.File{
		file("package.json", pkg),
		file("index.html", "<h1>x</h1>"),
	}
	st, _ := newTestStore(t, map[string][]byte{
		manifest[0].SHA256: []byte(pkg),
		manifest[1].SHA256: []byte("<h1>x</h1>"),
	})
	c1, err := st.Materialize(context.Background(), "dep-idem", "deploys/dep-idem", manifest)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	c2, err := st.Materialize(context.Background(), "dep-idem", "deploys/dep-idem", manifest)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if c1 != c2 {
		t.Fatalf("static-fallback materialize must be idempotent: %s != %s", c1, c2)
	}
}

// Real node apps must not be touched: a start or build script, an explicit
// Procfile/nixpacks.toml, or the absence of index.html all disable the
// fallback.
func TestMaterializeStaticFallbackNotAppliedForRealApps(t *testing.T) {
	requireGit(t)
	cases := []struct {
		name  string
		files []struct{ path, content string }
	}{
		{
			name: "start script present",
			files: []struct{ path, content string }{
				{"package.json", `{"scripts":{"start":"node server.js"}}`},
				{"index.html", "<h1>x</h1>"},
				{"server.js", "require('http').createServer(()=>{}).listen(3000)"},
			},
		},
		{
			name: "build script present",
			files: []struct{ path, content string }{
				{"package.json", `{"scripts":{"build":"vite build"}}`},
				{"index.html", "<h1>x</h1>"},
			},
		},
		{
			name: "Procfile wins over the heuristic",
			files: []struct{ path, content string }{
				{"package.json", `{"name":"site"}`},
				{"index.html", "<h1>x</h1>"},
				{"Procfile", "web: node server.js"},
			},
		},
		{
			name: "no index.html at the root",
			files: []struct{ path, content string }{
				{"package.json", `{"name":"lib"}`},
				{"src/index.html", "<h1>nested</h1>"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := make([]api.File, 0, len(tc.files))
			objects := map[string][]byte{}
			for _, f := range tc.files {
				mf := file(f.path, f.content)
				manifest = append(manifest, mf)
				objects[mf.SHA256] = []byte(f.content)
			}
			st, _ := newTestStore(t, objects)
			id := "dep-nofallback"
			if _, err := st.Materialize(context.Background(), id, "deploys/"+id, manifest); err != nil {
				t.Fatalf("materialize: %v", err)
			}
			if got := listRepoFiles(t, st, id); !got["package.json"] {
				t.Fatalf("package.json must stay put for this shape, got %v", got)
			}
		})
	}
}

func TestMaterializeFailedLeavesNoPartialRepo(t *testing.T) {
	requireGit(t)
	manifest := []api.File{file("index.html", "x")}
	st, p := newTestStore(t, map[string][]byte{manifest[0].SHA256: []byte("x")})
	p.fail = true

	if _, err := st.Materialize(context.Background(), "dep2", "deploys/dep2", manifest); err == nil {
		t.Fatal("presign failure must fail materialize")
	}
	if _, err := os.Stat(st.RepoPath("dep2")); !os.IsNotExist(err) {
		t.Fatalf("no repo must exist after failure, stat err = %v", err)
	}
	for _, suffix := range []string{".git.swap", ".work"} {
		if _, err := os.Stat(st.RepoPath("dep2") + suffix); !os.IsNotExist(err) {
			t.Fatalf("no %s must survive a failed run", suffix)
		}
	}
}

// TestMaterializeCommitsGitignoredFiles: the tree can carry the project's own
// .gitignore, and every manifest file must land in the committed snapshot
// regardless (2026-10-20 live incident: a Dockerfile COPY of a .gitignore'd
// runtime db failed the build while the manifest and R2 both held the file).
func TestMaterializeCommitsGitignoredFiles(t *testing.T) {
	requireGit(t)
	gitignore := "runtime/\n*.db\n"
	db := "sqlite bytes"
	manifest := []api.File{
		file(".gitignore", gitignore),
		file("Dockerfile", "FROM scratch\nCOPY runtime/data/xuantie.db /db\n"),
		file("runtime/data/xuantie.db", db),
	}
	st, _ := newTestStore(t, map[string][]byte{
		manifest[0].SHA256: []byte(gitignore),
		manifest[1].SHA256: []byte("FROM scratch\nCOPY runtime/data/xuantie.db /db\n"),
		manifest[2].SHA256: []byte(db),
	})

	if _, err := st.Materialize(context.Background(), "dep-gitignore", "deploys/dep-gitignore", manifest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	out, err := exec.Command("git", "--git-dir", st.RepoPath("dep-gitignore"), "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	for _, want := range []string{".gitignore", "runtime/data/xuantie.db"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("manifest file %s missing from committed tree; tree:\n%s", want, out)
		}
	}
}

func TestMaterializeRejectsPathEscape(t *testing.T) {
	requireGit(t)
	evil := api.File{Path: "../escape.txt", SHA256: sha([]byte("x")), Size: 1}
	st, _ := newTestStore(t, map[string][]byte{evil.SHA256: []byte("x")})
	if _, err := st.Materialize(context.Background(), "dep3", "deploys/dep3", []api.File{evil}); err == nil {
		t.Fatal("manifest path escaping the root must be rejected")
	}
}

func TestMaterializeEmptyManifestRejected(t *testing.T) {
	requireGit(t)
	st, _ := newTestStore(t, nil)
	if _, err := st.Materialize(context.Background(), "dep4", "deploys/dep4", nil); err == nil {
		t.Fatal("empty manifest must be rejected")
	}
}

func TestDownloadVerifiedRejectsCorruptObject(t *testing.T) {
	good := sha([]byte("good"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("evil content")) // wrong bytes for the requested sha
	}))
	defer srv.Close()
	st := New(t.TempDir(), nil)

	err := st.downloadVerified(context.Background(), srv.URL+"/"+good, good)
	if err == nil {
		t.Fatal("sha mismatch must fail the download")
	}
	if _, statErr := os.Stat(st.objectPath(good)); !os.IsNotExist(statErr) {
		t.Fatal("a failed verification must not leave an object behind")
	}
}

// 11-03 (byo-hosting-node): a dropped transfer must KEEP its .part and the
// retry must resume with a Range request at the partial size -- a 196MB tar
// over a link that dies every ~50min never finishes if every retry restarts
// from byte zero. The fake serves the tail as 206 like R2 does.
func TestDownloadVerifiedResumesFromPart(t *testing.T) {
	full := []byte(strings.Repeat("x", 4096) + "tail-marker")
	fullSHA := sha(full)
	cut := 1000
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			ranges = append(ranges, rng)
			var start int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil || start > int64(len(full)) {
				http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[start:])
			return
		}
		// First attempt: serve the prefix, flush it to the client, then drop
		// the connection mid-body (a flush is what makes this a stream error
		// rather than a failed GET).
		_, _ = w.Write(full[:cut])
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()
	st := New(t.TempDir(), nil)
	part := st.objectPath(fullSHA) + ".part"

	if err := st.downloadVerified(context.Background(), srv.URL+"/"+fullSHA, fullSHA); err == nil || !strings.Contains(err.Error(), "stream") {
		t.Fatalf("mid-body drop must fail attempt 1 with a stream error, got %v", err)
	}
	if st, err := os.Stat(part); err != nil || st.Size() != int64(cut) {
		t.Fatalf("dropped transfer must keep %d bytes of .part, got (%v, %v)", cut, st, err)
	}

	if err := st.downloadVerified(context.Background(), srv.URL+"/"+fullSHA, fullSHA); err != nil {
		t.Fatalf("resume attempt: %v", err)
	}
	if len(ranges) != 1 || ranges[0] != fmt.Sprintf("bytes=%d-", cut) {
		t.Fatalf("resume must send exactly one Range bytes=%d-, got %v", cut, ranges)
	}
	got, err := os.ReadFile(st.objectPath(fullSHA))
	if err != nil || string(got) != string(full) {
		t.Fatalf("resumed object must equal the full body (err %v)", err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("completed object must leave no .part behind")
	}
}

// A server that ignores Range and answers plain 200 must produce the exact
// object, not prefix+body -- the resumed prefix is discarded and the file and
// hash restart from zero.
func TestDownloadVerifiedRestartsWhenServerIgnoresRange(t *testing.T) {
	full := []byte("the full object body")
	fullSHA := sha(full)
	var sawRange bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			sawRange = true // sent, but the 200 below ignores it
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(full)
	}))
	defer srv.Close()
	st := New(t.TempDir(), nil)
	part := st.objectPath(fullSHA) + ".part"
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, full[:4], 0o644); err != nil { // "the "
		t.Fatal(err)
	}

	if err := st.downloadVerified(context.Background(), srv.URL+"/"+fullSHA, fullSHA); err != nil {
		t.Fatalf("200-restart attempt: %v", err)
	}
	if !sawRange {
		t.Fatal("a non-empty .part must ask with a Range header")
	}
	got, err := os.ReadFile(st.objectPath(fullSHA))
	if err != nil || string(got) != string(full) {
		t.Fatalf("object must be the exact body (no prefix duplication), got %q (err %v)", got, err)
	}
}

// A .part whose content proves WRONG (sha mismatch) is poison, not progress:
// it must be removed so the next attempt starts clean. A Range-honoring server
// makes this observable -- a kept .part would resume from a wrong offset and
// mismatch forever.
func TestDownloadVerifiedRemovesPoisonPart(t *testing.T) {
	full := []byte("correct content")
	fullSHA := sha(full)
	var sawRange bool
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			sawRange = true
			var start int64
			fmt.Sscanf(rng, "bytes=%d-", &start)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(full)-1, len(full)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(full[start:])
			return
		}
		served++
		if served == 1 {
			_, _ = w.Write([]byte("evil content")) // hashes to nothing useful
			return
		}
		_, _ = w.Write(full) // the server is fixed between attempts
	}))
	defer srv.Close()
	st := New(t.TempDir(), nil)
	part := st.objectPath(fullSHA) + ".part"

	if err := st.downloadVerified(context.Background(), srv.URL+"/"+fullSHA, fullSHA); err == nil {
		t.Fatal("wrong content must fail verification")
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("a sha-mismatch .part must be removed, not kept for resume")
	}
	if err := st.downloadVerified(context.Background(), srv.URL+"/"+fullSHA, fullSHA); err != nil {
		t.Fatalf("clean retry after poison: %v", err)
	}
	if sawRange {
		t.Fatal("retry after poison removal must start from byte zero (no Range)")
	}
}

// --- runtime preview path (add-dynamic-preview-compute 5.2) -----------------

// fakeRuntimePresigner keys objects under previews/{previewId}/ -- the tail
// segment is still the sha, exercising fetchBatch's prefix-agnosticism.
type fakeRuntimePresigner struct {
	base string
}

func (f *fakeRuntimePresigner) RuntimePresign(ctx context.Context, instanceID string, shas []string) (*api.PresignResponse, error) {
	resp := &api.PresignResponse{}
	for _, s := range shas {
		resp.URLs = append(resp.URLs, api.PresignedObject{
			Key: fmt.Sprintf("previews/pv-1/%s", s),
			URL: f.base + "/" + s,
		})
	}
	return resp, nil
}

func TestRuntimePreviewTreeFetchesViaRuntimePresignAndAddsExtraFiles(t *testing.T) {
	requireGit(t)
	rjContent := `{"version":1,"startCommand":["node","server.js"],"port":3000}`
	rj := file("runtime.json", rjContent)
	app := file("server.js", "console.log('up')")
	objects := map[string][]byte{rj.SHA256: []byte(rjContent), app.SHA256: []byte("console.log('up')")}

	srv := objectServer(objects)
	defer srv.Close()
	st := New(t.TempDir(), nil)
	presigner := &fakeRuntimePresigner{base: srv.URL}

	presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return presigner.RuntimePresign(ctx, "inst1", shas)
	}
	manifest := []api.File{rj, app}
	if err := st.FetchMissing(context.Background(), manifest, presign); err != nil {
		t.Fatalf("fetch missing: %v", err)
	}
	got, err := st.ObjectBytes(rj.SHA256)
	if err != nil || string(got) != rjContent {
		t.Fatalf("ObjectBytes must read the verified object, got (%q, %v)", got, err)
	}
	if _, err := st.ObjectBytes(sha([]byte("not fetched"))); err == nil {
		t.Fatal("ObjectBytes of an absent object must fail")
	}

	dockerfile := []byte("FROM node:22-slim\nCOPY . .\n")
	commit1, err := st.BuildRepo(context.Background(), "oa-preview-m1", manifest, map[string][]byte{"Dockerfile": dockerfile}, false)
	if err != nil {
		t.Fatalf("build repo: %v", err)
	}
	// Deterministic like Materialize: same inputs -> same commit (a re-drive
	// after a report hiccup must not look like a new snapshot).
	commit2, err := st.BuildRepo(context.Background(), "oa-preview-m1", manifest, map[string][]byte{"Dockerfile": dockerfile}, false)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if commit1 != commit2 {
		t.Fatalf("same manifest + extra must yield the same commit, got %s then %s", commit1, commit2)
	}
	// The extra file actually landed in the tree.
	out, err := outputGit(context.Background(), st.RepoPath("oa-preview-m1"), "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Dockerfile") || !strings.Contains(out, "runtime.json") {
		t.Fatalf("tree must contain the extra Dockerfile and the manifest tree, got:\n%s", out)
	}
}

func TestBuildRepoRejectsEscapingExtraPath(t *testing.T) {
	requireGit(t)
	app := file("server.js", "x")
	st, _ := newTestStore(t, map[string][]byte{app.SHA256: []byte("x")})
	if err := st.FetchMissing(context.Background(), []api.File{app}, func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return st.Presigner.PresignSource(ctx, "d", shas)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BuildRepo(context.Background(), "k", []api.File{app}, map[string][]byte{"../escape": []byte("x")}, false); err == nil {
		t.Fatal("extra path escaping the root must be rejected")
	}
}

// readRepoFile returns a committed file's bytes (the assertions care about
// the committed tree, not the scratch directory).
func readRepoFile(t *testing.T, st *Store, deploymentID, path string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", st.RepoPath(deploymentID), "show", "HEAD:"+path).Output()
	if err != nil {
		t.Fatalf("git show %s: %v", path, err)
	}
	return string(out)
}

// lsRepoTree lists every path in the committed tree.
func lsRepoTree(t *testing.T, st *Store, deploymentID string) []string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", st.RepoPath(deploymentID), "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	return strings.Fields(string(out))
}

// hosted-compose-stacks 5.2: a compose materialize must land the REWRITTEN
// stack file in the commit -- limits injected, non-ingress host ports gone,
// ingress on the negotiated loopback slot -- while the rest of the tree rides
// through untouched. Determinism carries over too: the rewrite is a pure
// function, so re-materializing yields the same commit.
func TestMaterializeComposeRewritesStackFile(t *testing.T) {
	requireGit(t)
	composeSrc := `
services:
  web:
    image: nginx:1.27
    ports: ["8080:3000"]
  worker:
    image: redis:7
    ports: ["6379:6379"]
`
	manifest := []api.File{
		file("docker-compose.yml", composeSrc),
		file("src/app.ts", "app"),
	}
	st, _ := newTestStore(t, map[string][]byte{
		manifest[0].SHA256: []byte(composeSrc),
		manifest[1].SHA256: []byte("app"),
	})
	spec := api.ComposeSpec{
		ComposePath:    "docker-compose.yml",
		IngressService: "web",
		IngressPort:    3000,
		HostPort:       30001,
		Services: []api.ServiceLimit{
			{Name: "web", MemoryMiB: 512, CPUs: 1},
			{Name: "worker", MemoryMiB: 256, CPUs: 0.5},
		},
	}

	c1, err := st.MaterializeCompose(context.Background(), st.Presigner, "dep1", "deploys/dep1", manifest, spec)
	if err != nil {
		t.Fatal(err)
	}
	committed := readRepoFile(t, st, "dep1", "docker-compose.yml")
	for _, want := range []string{
		`127.0.0.1:30001:3000`, // ingress: negotiated loopback slot
		"cpus: 1",              // web limits injected
		`memory: "536870912"`,  // 512MiB in bytes
		"cpus: 0.5",            // worker limits injected
		`memory: "268435456"`,  // 256MiB in bytes
	} {
		if !strings.Contains(committed, want) {
			t.Errorf("committed compose missing %q:\n%s", want, committed)
		}
	}
	for _, banned := range []string{"8080", "6379"} {
		if strings.Contains(committed, banned) {
			t.Errorf("committed compose must not carry original host ports (%s):\n%s", banned, committed)
		}
	}
	if got := readRepoFile(t, st, "dep1", "src/app.ts"); got != "app" {
		t.Errorf("the rest of the tree must ride through untouched, got %q", got)
	}

	// Deterministic: same manifest + spec -> same commit.
	c2, err := st.MaterializeCompose(context.Background(), st.Presigner, "dep1", "deploys/dep1", manifest, spec)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatalf("compose materialize must be deterministic: %s != %s", c1, c2)
	}
}

// A spec pointing outside the manifest (drift) must fail loudly, never build
// a repo whose rewrite silently no-ops.
func TestMaterializeComposeRejectsUnknownComposePath(t *testing.T) {
	requireGit(t)
	app := file("src/app.ts", "app")
	st, _ := newTestStore(t, map[string][]byte{app.SHA256: []byte("app")})
	if err := st.FetchMissing(context.Background(), []api.File{app}, func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		return st.Presigner.PresignSource(ctx, "d", shas)
	}); err != nil {
		t.Fatal(err)
	}
	spec := api.ComposeSpec{ComposePath: "compose.yaml", IngressService: "web", IngressPort: 80, HostPort: 1,
		Services: []api.ServiceLimit{{Name: "web", MemoryMiB: 256, CPUs: 0.5}}}
	if _, err := st.MaterializeCompose(context.Background(), st.Presigner, "dep1", "deploys/dep1", []api.File{app}, spec); err == nil {
		t.Fatal("compose path outside the manifest must fail the materialize")
	}
}

// imageTierManifest is the image-tier shape: the rewritten compose, the
// per-service image tars, and a small rider file. The objects map carries
// the same bytes file() hashed, so FetchMissing verifies cleanly.
func imageTierManifest(t *testing.T) (manifest []api.File, objects map[string][]byte) {
	t.Helper()
	composeSrc := `
services:
  web:
    image: oa-deploy-dep1-web:latest
    ports: ["8080:3000"]
`
	entries := []struct{ path, content string }{
		{"docker-compose.yml", composeSrc},
		{"web.tar", "pretend docker save tar for web"},
		{"nested/worker.tar", "pretend docker save tar for worker"},
		{"README.md", "readme"},
	}
	objects = map[string][]byte{}
	for _, e := range entries {
		manifest = append(manifest, file(e.path, e.content))
		objects[sha([]byte(e.content))] = []byte(e.content)
	}
	return manifest, objects
}

// image-tier-compose-only-materialize 1.2: the image tier must keep tars OUT
// of the committed tree (Coolify only ever reads the compose) while they
// still land in the object store (docker load reads ObjectPath, not the
// repo), and the committed compose must be the rewritten one.
func TestMaterializeComposeImageTierOmitsTarsFromTree(t *testing.T) {
	requireGit(t)
	manifest, objects := imageTierManifest(t)
	st, _ := newTestStore(t, objects)
	spec := api.ComposeSpec{
		ComposePath:    "docker-compose.yml",
		IngressService: "web",
		IngressPort:    3000,
		HostPort:       30001,
		Tier:           "image",
		Services:       []api.ServiceLimit{{Name: "web", MemoryMiB: 512, CPUs: 1}},
	}

	if _, err := st.MaterializeCompose(context.Background(), st.Presigner, "dep1", "deploys/dep1", manifest, spec); err != nil {
		t.Fatal(err)
	}
	// Tree carries compose + rider, zero tar entries (nested or flat).
	paths := lsRepoTree(t, st, "dep1")
	want := map[string]bool{"docker-compose.yml": false, "README.md": false}
	for _, p := range paths {
		if strings.HasSuffix(p, ".tar") {
			t.Errorf("image-tier tree must not carry tar entries, found %s", p)
		}
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("image-tier tree must keep %s, tree=%v", p, paths)
		}
	}
	// Committed compose is the rewritten one (loopback slot injected).
	if committed := readRepoFile(t, st, "dep1", "docker-compose.yml"); !strings.Contains(committed, "127.0.0.1:30001:3000") {
		t.Errorf("committed compose must be the rewrite:\n%s", committed)
	}
	// The tars still exist in the object store: docker load reads
	// ObjectPath(sha), so dropping them from the tree must not drop them
	// from disk.
	for _, f := range manifest {
		if !strings.HasSuffix(f.Path, ".tar") {
			continue
		}
		if _, err := os.Stat(st.objectPath(f.SHA256)); err != nil {
			t.Errorf("tar %s must remain in the object store for docker load: %v", f.Path, err)
		}
	}
}

// image-tier-compose-only-materialize 1.3: the source tier keeps the full
// manifest in the tree, byte-for-byte with the pre-change behavior.
func TestMaterializeComposeSourceTierKeepsFullTree(t *testing.T) {
	requireGit(t)
	manifest, objects := imageTierManifest(t)
	st, _ := newTestStore(t, objects)
	spec := api.ComposeSpec{
		ComposePath:    "docker-compose.yml",
		IngressService: "web",
		IngressPort:    3000,
		HostPort:       30001,
		Tier:           "source",
		Services:       []api.ServiceLimit{{Name: "web", MemoryMiB: 512, CPUs: 1}},
	}

	if _, err := st.MaterializeCompose(context.Background(), st.Presigner, "dep1", "deploys/dep1", manifest, spec); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range lsRepoTree(t, st, "dep1") {
		seen[p] = true
	}
	for _, f := range manifest {
		if !seen[f.Path] {
			t.Errorf("source-tier tree must keep every manifest entry, missing %s, tree=%v", f.Path, seen)
		}
	}
	// Same manifest under the empty tier (the legacy wire shape) must behave
	// identically: full tree.
	spec.Tier = ""
	if _, err := st.MaterializeCompose(context.Background(), st.Presigner, "dep2", "deploys/dep2", manifest, spec); err != nil {
		t.Fatal(err)
	}
	seen2 := map[string]bool{}
	for _, p := range lsRepoTree(t, st, "dep2") {
		seen2[p] = true
	}
	for _, f := range manifest {
		if !seen2[f.Path] {
			t.Errorf("empty-tier tree must keep every manifest entry, missing %s, tree=%v", f.Path, seen2)
		}
	}
}
