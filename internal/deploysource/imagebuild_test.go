package deploysource

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestImageTag(t *testing.T) {
	// Mixed-case deployment ids must fold to lowercase: docker rejects
	// uppercase repository names ("invalid tag ... must be lowercase").
	if got, want := ImageTag("MlqbhZR340sM", "web"), "oa-deploy-mlqbhzr340sm-web"; got != want {
		t.Fatalf("ImageTag = %q, want %q", got, want)
	}
	if got, want := ImageTag("d1", "web-user"), "oa-deploy-d1-web-user"; got != want {
		t.Fatalf("ImageTag = %q, want %q", got, want)
	}
}

// rewriteFixture carries a build service, a registry-image service and the
// sibling keys the rewrite must leave byte-identical.
const rewriteFixture = `# top comment survives
services:
  web:
    build:
      context: ./apps/web
      dockerfile: Dockerfile
      args:
        NODE_ENV: production
    ports:
      - "3000:3000"
    profiles:
      - full
    depends_on:
      - redis
  redis:
    image: redis:7-alpine
  worker:
    build: ./worker # inline comment rides along
    image: placeholder/overwritten
`

func rewriteFixtureDoc(t *testing.T) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(rewriteFixture), &doc); err != nil {
		t.Fatalf("fixture parse: %v", err)
	}
	return &doc
}

func TestRewriteCompose(t *testing.T) {
	doc := rewriteFixtureDoc(t)
	var out strings.Builder
	if err := rewriteCompose(doc, "DEP1", &out); err != nil {
		t.Fatalf("rewriteCompose: %v", err)
	}
	rendered := out.String()

	// The build keys are gone; rewritten tags point at the local tag contract.
	if strings.Contains(rendered, "build") {
		t.Fatalf("rewrite left a `build` key behind:\n%s", rendered)
	}
	if !strings.Contains(rendered, "image: oa-deploy-dep1-web") {
		t.Fatalf("web not rewritten to local tag:\n%s", rendered)
	}
	if !strings.Contains(rendered, "image: oa-deploy-dep1-worker") {
		t.Fatalf("worker (image+build coexist) not overwritten:\n%s", rendered)
	}
	if strings.Contains(rendered, "placeholder/overwritten") {
		t.Fatalf("image alongside build survived the rewrite:\n%s", rendered)
	}
	// Registry references and sibling keys pass through untouched. Build
	// args ride the `build:` subtree and are gone with it — runtime compose
	// has no use for them.
	if !strings.Contains(rendered, "image: redis:7-alpine") {
		t.Fatalf("registry image service was disturbed:\n%s", rendered)
	}
	for _, keep := range []string{"profiles:", "depends_on:", `"3000:3000"`} {
		if !strings.Contains(rendered, keep) {
			t.Fatalf("sibling key %q lost by rewrite:\n%s", keep, rendered)
		}
	}
	if strings.Contains(rendered, "NODE_ENV") {
		t.Fatalf("build arg leaked past the build-key removal:\n%s", rendered)
	}
	if !strings.Contains(rendered, "# top comment survives") {
		t.Fatalf("top-level comment lost:\n%s", rendered)
	}

	// The platform's complete-time validator resolves the service name from
	// the tar filename — the two spellings must agree for every built service.
	var back map[string]any
	if err := yaml.Unmarshal([]byte(rendered), &back); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	services, _ := back["services"].(map[string]any)
	for _, svc := range []string{"web", "worker"} {
		body, _ := services[svc].(map[string]any)
		tag, _ := body["image"].(string)
		if tag != ImageTag("DEP1", svc) && tag != ImageTag("DEP1", svc)+":latest" {
			t.Fatalf("service %s image %q does not match tag contract", svc, tag)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	var ctxNode yaml.Node
	if err := yaml.Unmarshal([]byte(`./svc`), &ctxNode); err != nil {
		t.Fatal(err)
	}
	got, err := buildArgs("/repo", "/repo", "arm64", ctxNode.Content[0])
	if err != nil {
		t.Fatalf("string form: %v", err)
	}
	// fix-image-tier-arch-mismatch: the platform pin rides right after the
	// verb, before the positional context.
	if len(got) != 4 || got[0] != "build" || got[1] != "--platform" || got[2] != "linux/arm64" || got[3] != "/repo/svc" {
		t.Fatalf("string form argv = %v", got)
	}

	var mapNode yaml.Node
	if err := yaml.Unmarshal([]byte(`
context: ./apps/web
dockerfile: Dockerfile.dev
target: build-stage
args:
  NODE_ENV: production
  TOKEN:`), &mapNode); err != nil {
		t.Fatal(err)
	}
	got, err = buildArgs("/repo", "/repo/deploy", "arm64", mapNode.Content[0])
	if err != nil {
		t.Fatalf("mapping form: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"build --platform linux/arm64 /repo/deploy/apps/web",
		"-f /repo/deploy/apps/web/Dockerfile.dev", "--target build-stage",
		"--build-arg NODE_ENV=production", "--build-arg TOKEN",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("mapping form argv missing %q: %v", want, got)
		}
	}
}

// The arch whitelist (fix-image-tier-arch-mismatch D3): only amd64|arm64 are
// protocol spellings. Missing (old API / omitted registry fact) and abnormal
// values both degrade to amd64 — a bad fact must never inject an arbitrary
// --platform token into the docker argv.
func TestNormalizeArch(t *testing.T) {
	cases := []struct{ in, want string }{
		{"amd64", "amd64"},
		{"arm64", "arm64"},
		{"", "amd64"},           // field omitted by the API or old API skew
		{"riscv64", "amd64"},    // abnormal registry value
		{"AMD64", "amd64"},      // case-sensitive: no accidental matches
		{"amd64;rm", "amd64"},   // injection attempt degrades
	}
	for _, c := range cases {
		if got := normalizeArch(c.in); got != c.want {
			t.Fatalf("normalizeArch(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRunImageTierUploadFailFast(t *testing.T) {
	root := t.TempDir()

	// No convention compose -> loud error before anything docker-shaped.
	err := runImageTierUpload(nil, "m", "d", root, "", nil)
	if err == nil || !strings.Contains(err.Error(), "no convention-path compose") {
		t.Fatalf("want compose-miss error, got %v", err)
	}

	// Compose present but no build services -> image tier has nothing to do.
	writeFile(t, filepath.Join(root, "docker-compose.yml"), "services:\n  db:\n    image: postgres:16\n")
	err = runImageTierUpload(nil, "m", "d", root, "", nil)
	if err == nil || !strings.Contains(err.Error(), "no `build:` services") {
		t.Fatalf("want no-build-services error, got %v", err)
	}

	// Docker unusable -> explicit reason, never a silent source-tier fallback.
	writeFile(t, filepath.Join(root, "docker-compose.yml"), "services:\n  web:\n    build: .\n")
	orig := dockerProbe
	dockerProbe = func() error { return os.ErrPermission }
	defer func() { dockerProbe = orig }()
	err = runImageTierUpload(nil, "m", "d", root, "", nil)
	if err == nil || !strings.Contains(err.Error(), "docker not usable") {
		t.Fatalf("want docker-unusable error, got %v", err)
	}
}

func TestTailOutput(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := tailOutput([]byte("  "+long+"\n"), 100); len(got) != 103 || !strings.HasPrefix(got, "...") {
		t.Fatalf("tailOutput len = %d, want 103 (…marker + 100)", len(got))
	}
	if got := tailOutput([]byte(" short "), 100); got != "short" {
		t.Fatalf("tailOutput = %q, want %q", got, "short")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSaveGzipped (image-tier-gzip-and-default 1.2): the pipe contract —
// gzip magic at the head of the file, gunzip round-trips the payload, and
// the returned size is the compressed bytes on disk (what sha256/size in the
// manifest must describe). Uses a plain printf command so no docker daemon
// is needed.
func TestSaveGzipped(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "web.tar")
	payload := strings.Repeat("docker-layer-bytes-", 4096) // ~74KB, compresses well
	cmd := exec.Command("sh", "-c", "printf %s "+shQuote(payload))
	size, err := saveGzipped(cmd, tarPath, "web")
	if err != nil {
		t.Fatalf("saveGzipped: %v", err)
	}
	raw, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := raw[0]; got != 0x1f {
		t.Fatalf("gzip magic byte0 = %#x, want 0x1f", got)
	}
	if got := raw[1]; got != 0x8b {
		t.Fatalf("gzip magic byte1 = %#x, want 0x8b", got)
	}
	if int64(len(raw)) != size {
		t.Fatalf("returned size %d != on-disk %d", size, len(raw))
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a gzip stream: %v", err)
	}
	round, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if string(round) != payload {
		t.Fatalf("round-trip mismatch: %d bytes vs %d", len(round), len(payload))
	}
}

// shQuote single-quotes s for the sh -c printf argument.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
