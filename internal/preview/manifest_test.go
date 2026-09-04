package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestBuildManifest_BasicWalkAndHashes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<html></html>")
	writeFile(t, dir, "assets/app.js", "console.log(1)")

	files, err := BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %+v", len(files), files)
	}

	byPath := map[string]ManifestFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}

	idx, ok := byPath["index.html"]
	if !ok {
		t.Fatalf("missing index.html in manifest: %+v", files)
	}
	if idx.SHA256 != sha256Hex("<html></html>") {
		t.Errorf("wrong sha256 for index.html: %s", idx.SHA256)
	}
	if idx.Size != int64(len("<html></html>")) {
		t.Errorf("wrong size for index.html: %d", idx.Size)
	}

	js, ok := byPath["assets/app.js"]
	if !ok {
		t.Fatalf("missing assets/app.js, forward slashes not used? %+v", files)
	}
	if js.SHA256 != sha256Hex("console.log(1)") {
		t.Errorf("wrong sha256 for assets/app.js: %s", js.SHA256)
	}
}

func TestBuildManifest_ExcludesSourceMaps(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<html></html>")
	writeFile(t, dir, "assets/app.js", "console.log(1)")
	writeFile(t, dir, "assets/app.js.map", `{"version":3,"sources":["../src/app.ts"]}`)

	files, err := BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if filepath.Ext(f.Path) == ".map" {
			t.Fatalf("manifest must not include .map files, found %s", f.Path)
		}
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files (map excluded), got %d: %+v", len(files), files)
	}
}

func TestBuildManifest_NestedMapFileExcluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<html></html>")
	writeFile(t, dir, "deep/nested/chunk.css.map", "{}")

	files, err := BuildManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected only index.html, got %+v", files)
	}
}

func TestHasRootIndexHTML(t *testing.T) {
	cases := []struct {
		name  string
		files []ManifestFile
		want  bool
	}{
		{"has root index", []ManifestFile{{Path: "index.html"}, {Path: "assets/app.js"}}, true},
		{"nested index only", []ManifestFile{{Path: "sub/index.html"}}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := HasRootIndexHTML(c.files); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
