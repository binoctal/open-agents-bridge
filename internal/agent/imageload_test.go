package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

func newImageAgent(t *testing.T, dk *fakeDocker, shas ...string) *Agent {
	t.Helper()
	dir := t.TempDir()
	objDir := filepath.Join(dir, "objects")
	if err := os.MkdirAll(objDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sha := range shas {
		if err := os.WriteFile(filepath.Join(objDir, sha), []byte("tar-bytes-of-"+sha[:6]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Agent{Store: store.New(dir, nil), dockerRun: dk.run}
}

// imageManifest mirrors an image-tier pack: one rewritten compose plus one
// tar per built service.
func imageManifest() []api.File {
	return []api.File{
		{Path: "docker-compose.yml", SHA256: strings.Repeat("c", 64), Size: 200},
		{Path: "web.tar", SHA256: strings.Repeat("1", 64), Size: 1 << 30},
		{Path: "worker.tar", SHA256: strings.Repeat("2", 64), Size: 2 << 30},
	}
}

// Loads hit every tar straight from the object store (sha path, no copy) and
// leave non-tar members alone.
func TestLoadImageTars(t *testing.T) {
	dk := &fakeDocker{}
	a := newImageAgent(t, dk, strings.Repeat("1", 64), strings.Repeat("2", 64))

	if err := a.loadImageTars(context.Background(), "D1", imageManifest()); err != nil {
		t.Fatalf("loadImageTars: %v", err)
	}
	if len(dk.calls) != 2 {
		t.Fatalf("docker calls = %d, want 2 (compose member must not load)", len(dk.calls))
	}
	for i, want := range []string{strings.Repeat("1", 64), strings.Repeat("2", 64)} {
		args := dk.calls[i].args
		if strings.Join(args, " ") != "load -i "+filepath.Join(a.Store.Dir, "objects", want) {
			t.Fatalf("call %d = %v, want load -i <objects/%s…>", i, args, want[:6])
		}
	}
}

// A failed load must fail materialize loudly: the failing tar's name and the
// docker output tail are the only diagnostics the compose-up failure would
// not give.
func TestLoadImageTarsFailure(t *testing.T) {
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{"load -i": {"invalid tar header", errors.New("exit 1")}}}
	a := newImageAgent(t, dk, strings.Repeat("1", 64), strings.Repeat("2", 64))

	err := a.loadImageTars(context.Background(), "D1", imageManifest())
	if err == nil || !strings.Contains(err.Error(), "web.tar") || !strings.Contains(err.Error(), "invalid tar header") {
		t.Fatalf("want tar-name + docker-tail error, got %v", err)
	}
}

// A tar missing from the store (partial fetch) fails before docker runs.
func TestLoadImageTarsMissingObject(t *testing.T) {
	dk := &fakeDocker{}
	a := newImageAgent(t, dk, strings.Repeat("1", 64)) // worker.tar's sha never landed

	err := a.loadImageTars(context.Background(), "D1", imageManifest())
	if err == nil || !strings.Contains(err.Error(), "worker.tar") || !strings.Contains(err.Error(), "not in store") {
		t.Fatalf("want missing-object error naming the tar, got %v", err)
	}
	// The present tar loaded first; the missing one stops its own load --
	// per-member store check, never a docker call against a phantom path.
	if len(dk.calls) != 1 {
		t.Fatalf("docker calls = %d, want 1 (web.tar only; worker.tar stops at the store check)", len(dk.calls))
	}
}

// Release removes exactly the deployment's local tags; a failed rmi never
// blocks the rest of teardown. Mixed-case ids fold to lowercase — the bridge
// builds the tags that way (docker rejects uppercase repositories).
func TestRmiDeployImages(t *testing.T) {
	dk := &fakeDocker{outcomes: map[string]struct {
		stdout string
		err    error
	}{
		"images --filter":             {"oa-deploy-d1-web:latest\noa-deploy-d1-worker:latest\n", nil},
		"rmi oa-deploy-d1-web:latest": {"", errors.New("image is in use")},
	}}
	a := newImageAgent(t, dk)

	a.rmiDeployImages(context.Background(), "D1")
	if got := strings.Join(dk.calls[0].args, " "); got != "images --filter reference=oa-deploy-d1-* --format {{.Repository}}:{{.Tag}}" {
		t.Fatalf("list filter (must fold the id) = %q", got)
	}
	if len(dk.calls) != 3 {
		t.Fatalf("docker calls = %d, want 3 (list + 2 rmi)", len(dk.calls))
	}
	if got := strings.Join(dk.calls[1].args, " "); got != "rmi oa-deploy-d1-web:latest" {
		t.Fatalf("first rmi = %q", got)
	}
	if got := strings.Join(dk.calls[2].args, " "); got != "rmi oa-deploy-d1-worker:latest" {
		t.Fatalf("second rmi (must survive the first failing) = %q", got)
	}
}
