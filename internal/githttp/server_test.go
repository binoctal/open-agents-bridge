package githttp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func freePortRange(t *testing.T) (int, int) {
	t.Helper()
	// A wide private range; the allocator binds for real so collisions with
	// other tests just skip a port, and 200 ports is far more than the tests
	// consume.
	return 24700, 24899
}

func TestServeIdempotentSamePort(t *testing.T) {
	min, max := freePortRange(t)
	m := New(t.TempDir(), min, max)

	p1, err := m.Serve("dep1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	p1again, err := m.Serve("dep1")
	if err != nil {
		t.Fatalf("serve again: %v", err)
	}
	if p1 != p1again {
		t.Fatalf("serving the same id twice must return the same port: %d != %d", p1, p1again)
	}
	if got, ok := m.Port("dep1"); !ok || got != p1 {
		t.Fatalf("Port() disagrees with Serve(): %d %v", got, ok)
	}
}

func TestPortRecycledAfterRelease(t *testing.T) {
	min, max := freePortRange(t)
	m := New(t.TempDir(), min, max)

	pa, err := m.Serve("a")
	if err != nil {
		t.Fatalf("serve a: %v", err)
	}
	pb, err := m.Serve("b")
	if err != nil {
		t.Fatalf("serve b: %v", err)
	}
	if err := m.Release("a"); err != nil {
		t.Fatalf("release a: %v", err)
	}
	// Scan-from-min allocator: the freed port is the first handed out again.
	pc, err := m.Serve("c")
	if err != nil {
		t.Fatalf("serve c: %v", err)
	}
	if pc != pa {
		t.Fatalf("released port %d must be recycled before fresh ports (got %d, b holds %d)", pa, pc, pb)
	}
}

func TestReleaseAbsentIsSuccess(t *testing.T) {
	min, max := freePortRange(t)
	m := New(t.TempDir(), min, max)
	if err := m.Release("never-served"); err != nil {
		t.Fatalf("releasing an unknown id must succeed, got %v", err)
	}
}

func TestServeRejectsInvalidID(t *testing.T) {
	min, max := freePortRange(t)
	m := New(t.TempDir(), min, max)
	for _, id := range []string{"", "a/b", ".."} {
		if _, err := m.Serve(id); err == nil {
			t.Fatalf("id %q must be rejected", id)
		}
	}
}

func TestValidGitPath(t *testing.T) {
	cases := map[string]bool{
		"/dep1.git/info/refs":       true,
		"/dep1.git/git-upload-pack": true,
		"/dep1.git":                 false, // no subpath
		"/dep1":                     false, // missing .git
		"/../etc.git/info/refs":     false, // traversal
		"/a/b.git/info/refs":        false, // nested id
		"/.git/info/refs":           false, // empty id
		"/de..p1.git/info/refs":     false, // dotdot inside id
	}
	for path, want := range cases {
		if got := validGitPath(path); got != want {
			t.Errorf("validGitPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestCloneThroughCGI is the loopback pull path end to end: a real
// `git clone` against the http-backend adapter must produce the same tree the
// bare repo holds.
func TestCloneThroughCGI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	root := t.TempDir()
	min, max := freePortRange(t)
	m := New(root, min, max)

	// Build a one-commit bare repo by hand (store-level wiring is covered by
	// store's own tests).
	src := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "--quiet", "-m", "x")
	run("clone", "--quiet", "--bare", src, filepath.Join(root, "dep1.git"))

	port, err := m.Serve("dep1")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	dst := t.TempDir()
	cmd := exec.Command("git", "clone", "--quiet", "http://127.0.0.1:"+strconv.Itoa(port)+"/dep1.git", "out")
	cmd.Dir = dst
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone through CGI failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dst, "out", "hello.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("cloned tree mismatch: %q %v", got, err)
	}

	if err := m.Release("dep1"); err != nil {
		t.Fatalf("release: %v", err)
	}
}
