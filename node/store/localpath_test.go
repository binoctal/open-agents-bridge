package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/node/api"
)

// scratchSetup builds a store with a scratch root and one declared-local file
// inside it (the same-machine declare-only protocol, byo-hosting-node D3).
func scratchSetup(t *testing.T) (*Store, string, api.File, []byte) {
	t.Helper()
	dir := t.TempDir()
	st := New(dir, nil)
	scratch := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(filepath.Join(scratch, "dep1"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.ScratchRoot = scratch
	content := []byte("local-blob-bytes")
	if err := os.WriteFile(filepath.Join(scratch, "dep1", "app.tar"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	f := api.File{Path: "app.tar", SHA256: sha(content), LocalPath: filepath.Join(scratch, "dep1", "app.tar")}
	return st, scratch, f, content
}

// A localPath entry imports into the content store BEFORE the missing scan:
// the presigner is never asked for it (the platform holds no blob -- a
// presign download would 404), and a second pass is a no-op.
func TestFetchMissingImportsLocalBeforePresign(t *testing.T) {
	st, _, f, _ := scratchSetup(t)
	calls := 0
	presign := func(ctx context.Context, shas []string) (*api.PresignResponse, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	if err := st.FetchMissing(context.Background(), []api.File{f}, presign); err != nil {
		t.Fatalf("FetchMissing: %v", err)
	}
	if calls != 0 {
		t.Errorf("presigner must not be consulted for a local entry, got %d calls", calls)
	}
	if !st.hasObject(f.SHA256) {
		t.Error("local object must land in the content store")
	}
	// Warm store: still no presign, no error.
	if err := st.FetchMissing(context.Background(), []api.File{f}, presign); err != nil {
		t.Fatalf("warm FetchMissing: %v", err)
	}
	if calls != 0 {
		t.Errorf("warm pass must not presign either, got %d calls", calls)
	}
}

// Every escape shape refuses BEFORE any read: outside-the-root absolute,
// ../ lexical, a symlink planted inside the scratch tree pointing out, a
// relative path, and a node with no scratch root at all. The refusal error
// is the user-facing detail on the deployment row, so it must name the path
// and say what to do.
func TestLocalPathEscapeRefused(t *testing.T) {
	st, scratch, _, _ := scratchSetup(t)

	outside := filepath.Join(st.Dir, "secret.env")
	if err := os.WriteFile(outside, []byte("root-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(scratch, "dep1", "smuggled.tar")); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		store *Store
		path  string
		want  string
	}{
		"absolute outside root":   {st, outside, "escapes the scratch root"},
		"lexical dotdot":          {st, filepath.Join(scratch, "dep1", "..", "..", "secret.env"), "escapes the scratch root"},
		"symlink inside root":     {st, filepath.Join(scratch, "dep1", "smuggled.tar"), "escapes the scratch root"},
		"relative path":           {st, "dep1/app.tar", "not absolute"},
		"no scratch root at all":  {&Store{Dir: st.Dir}, "/etc/passwd", "not allowed on this node"},
		"nonexistent local path":  {st, filepath.Join(scratch, "dep1", "missing.tar"), "unusable"},
		"nonexistent scratch dir": {&Store{Dir: st.Dir, ScratchRoot: filepath.Join(st.Dir, "no-such-root")}, "/etc/passwd", "unusable"},
	} {
		f := api.File{Path: "app.tar", SHA256: strings.Repeat("ab", 32), LocalPath: tc.path}
		err := tc.store.FetchMissing(context.Background(), []api.File{f}, nil)
		if err == nil {
			t.Errorf("%s: want refusal", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q should contain %q", name, err, tc.want)
		}
		if tc.store == st && st.hasObject(f.SHA256) {
			t.Errorf("%s: nothing may be imported on refusal", name)
		}
	}
}

// The imported bytes are verified against the declaration: a stale or lying
// sha refuses with the mismatch spelled out (both hashes short), and the
// declared object never enters the store.
func TestLocalPathShaMismatchRefused(t *testing.T) {
	st, _, f, _ := scratchSetup(t)
	f.SHA256 = strings.Repeat("cd", 32) // wrong hash for the file's bytes
	err := st.FetchMissing(context.Background(), []api.File{f}, nil)
	if err == nil {
		t.Fatal("want sha mismatch refusal")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") || !strings.Contains(err.Error(), "stale") {
		t.Errorf("error should name the mismatch and the likely cause, got %q", err)
	}
	if st.hasObject(f.SHA256) {
		t.Error("mismatched object must not be imported")
	}
}

// Worktree materializes the manifest as a PLAIN directory: local entries and
// fetched objects land side by side, extras (the rewritten compose file)
// ride through, and there is no .git anywhere -- the user node runs docker
// compose from this tree directly (no git http, no Coolify pull).
func TestWorktreePlainTreeWithLocalAndRemote(t *testing.T) {
	remote := []byte("from-r2")
	remoteFile := file("src/app.ts", "from-r2")
	st, presigner := newTestStore(t, map[string][]byte{remoteFile.SHA256: remote})
	scratch := filepath.Join(st.Dir, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	st.ScratchRoot = scratch
	localBytes := []byte("local-blob-bytes")
	if err := os.WriteFile(filepath.Join(scratch, "app.tar"), localBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	local := api.File{Path: "app.tar", SHA256: sha(localBytes), LocalPath: filepath.Join(scratch, "app.tar")}

	manifest := []api.File{local, remoteFile}
	extra := map[string][]byte{"docker-compose.yml": []byte("services: {}")}
	work, err := st.Worktree(context.Background(), presigner, "depX", "deploys/depX", manifest, extra)
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "app.tar")); err != nil || string(got) != string(localBytes) {
		t.Errorf("local entry missing from worktree (err %v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "src", "app.ts")); err != nil || string(got) != string(remote) {
		t.Errorf("remote entry missing from worktree (err %v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "docker-compose.yml")); err != nil || string(got) != "services: {}" {
		t.Errorf("extra compose missing from worktree (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(work, ".git")); !os.IsNotExist(err) {
		t.Error("worktree must be plain (no .git)")
	}
	if work != st.WorktreePath("depX") {
		t.Errorf("worktree path = %q, want %q", work, st.WorktreePath("depX"))
	}
	// Remove recycles the worktree too (the release path on a user node).
	if err := st.Remove("depX"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Error("Remove must drop worktrees/{id}")
	}
}
