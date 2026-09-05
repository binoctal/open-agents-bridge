package preview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readOut re-reads a file under the output dir after a rewrite.
func readOut(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRewriteHTMLAbsolutePaths_RootLevel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", `<html><head><link href="/assets/app.css"></head><body><script src="/assets/app.js"></script></body></html>`)
	writeFile(t, dir, "assets/app.css", "body{}")
	writeFile(t, dir, "assets/app.js", "code")

	n, err := RewriteHTMLAbsolutePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rewrites, got %d", n)
	}
	got := readOut(t, dir, "index.html")
	if !strings.Contains(got, `href="./assets/app.css"`) || !strings.Contains(got, `src="./assets/app.js"`) {
		t.Fatalf("root-level refs not relativized: %s", got)
	}
}

func TestRewriteHTMLAbsolutePaths_SubdirectoryDepth(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<html></html>")
	writeFile(t, dir, "assets/app.js", "code")
	writeFile(t, dir, "blog/2026/post.html", `<img src="/assets/app.js">`)
	writeFile(t, dir, "a/b/c/deep.html", `<img src="/assets/app.js">`)

	if _, err := RewriteHTMLAbsolutePaths(dir); err != nil {
		t.Fatal(err)
	}
	if got := readOut(t, dir, "blog/2026/post.html"); !strings.Contains(got, `src="../../assets/app.js"`) {
		t.Fatalf("depth-2 ref not relativized: %s", got)
	}
	if got := readOut(t, dir, "a/b/c/deep.html"); !strings.Contains(got, `src="../../../assets/app.js"`) {
		t.Fatalf("depth-3 ref not relativized: %s", got)
	}
}

func TestRewriteHTMLAbsolutePaths_ExternalRefsUntouched(t *testing.T) {
	dir := t.TempDir()
	orig := `<html>
<a href="https://example.com/page">abs</a>
<img src="//cdn.example.com/img.png">
<a href="/not/in/manifest">local but missing</a>
<a href="relative/already.html">relative</a>
</html>`
	writeFile(t, dir, "index.html", orig)

	n, err := RewriteHTMLAbsolutePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rewrites, got %d", n)
	}
	if got := readOut(t, dir, "index.html"); got != orig {
		t.Fatalf("external/missing refs must be untouched:\n got: %s\nwant: %s", got, orig)
	}
}

func TestRewriteHTMLAbsolutePaths_QueryAndFragmentPreserved(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", `<link href="/assets/app.css?v=3#fonts">`)
	writeFile(t, dir, "assets/app.css", "body{}")

	if _, err := RewriteHTMLAbsolutePaths(dir); err != nil {
		t.Fatal(err)
	}
	if got := readOut(t, dir, "index.html"); !strings.Contains(got, `href="./assets/app.css?v=3#fonts"`) {
		t.Fatalf("query/fragment not preserved: %s", got)
	}
}

// A bare "/" self-link points at the root index — rewrite it as such so the
// preview prefix cannot swallow it.
func TestRewriteHTMLAbsolutePaths_BareSlashPointsAtRootIndex(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", `<a href="/">home</a>`)
	writeFile(t, dir, "about/index.html", `<a href="/">home</a>`)

	if _, err := RewriteHTMLAbsolutePaths(dir); err != nil {
		t.Fatal(err)
	}
	if got := readOut(t, dir, "index.html"); !strings.Contains(got, `href="./index.html"`) {
		t.Fatalf("bare slash not rewritten at root: %s", got)
	}
	if got := readOut(t, dir, "about/index.html"); !strings.Contains(got, `href="../index.html"`) {
		t.Fatalf("bare slash not rewritten at depth 1: %s", got)
	}
}

func TestRewriteHTMLAbsolutePaths_SingleQuotesAndNonHTML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<html><script src='/assets/app.js'></script></html>")
	writeFile(t, dir, "assets/app.js", "code")
	writeFile(t, dir, "assets/raw.txt", "src=\"/assets/app.js\"")

	if _, err := RewriteHTMLAbsolutePaths(dir); err != nil {
		t.Fatal(err)
	}
	if got := readOut(t, dir, "index.html"); !strings.Contains(got, `src='./assets/app.js'`) {
		t.Fatalf("single-quoted ref not relativized: %s", got)
	}
	if got := readOut(t, dir, "assets/raw.txt"); got != "src=\"/assets/app.js\"" {
		t.Fatalf("non-HTML file must be untouched, got %s", got)
	}
}

// The rewritten bytes are what BuildManifest hashes: after a rewrite the
// index.html hash must differ from the pre-rewrite bytes' hash. This pins
// the ordering contract (rewrite BEFORE manifest) from D4.
func TestRewriteHTMLAbsolutePaths_OrderingWithManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", `<html><script src="/assets/app.js"></script></html>`)
	writeFile(t, dir, "assets/app.js", "code")

	hashOf := func(files []ManifestFile, path string) string {
		for _, f := range files {
			if f.Path == path {
				return f.SHA256
			}
		}
		t.Fatalf("no manifest entry for %s", path)
		return ""
	}

	before, err := BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RewriteHTMLAbsolutePaths(dir); err != nil {
		t.Fatal(err)
	}
	after, err := BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if hashOf(before, "index.html") == hashOf(after, "index.html") {
		t.Fatal("index.html hash unchanged: the rewrite would be invisible to the manifest")
	}
	if hashOf(before, "assets/app.js") != hashOf(after, "assets/app.js") {
		t.Fatal("untouched file must keep its hash")
	}
	if readOut(t, dir, "index.html") != `<html><script src="./assets/app.js"></script></html>` {
		t.Fatalf("unexpected rewrite result: %s", readOut(t, dir, "index.html"))
	}
}
