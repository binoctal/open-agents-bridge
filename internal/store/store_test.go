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
	"sync"
	"testing"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
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
