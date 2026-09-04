package preview

import (
	"testing"
)

func TestCache_StoreAndManifest(t *testing.T) {
	outputDir := t.TempDir()
	writeFile(t, outputDir, "index.html", "<html></html>")

	cacheDir := t.TempDir()
	cache, err := NewCache(cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	files, err := BuildManifest(outputDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := cache.Store("mission-1", outputDir, files); err != nil {
		t.Fatal(err)
	}

	got, ok := cache.Manifest("mission-1")
	if !ok {
		t.Fatal("expected cached manifest to be found")
	}
	if len(got) != 1 || got[0].Path != "index.html" {
		t.Fatalf("unexpected cached manifest: %+v", got)
	}

	blob, err := cache.Blob(got[0].SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "<html></html>" {
		t.Errorf("unexpected blob content: %q", blob)
	}
}

func TestCache_ManifestMissing(t *testing.T) {
	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Manifest("never-built"); ok {
		t.Error("expected no manifest for a mission never stored")
	}
}

func TestCache_Clear(t *testing.T) {
	outputDir := t.TempDir()
	writeFile(t, outputDir, "index.html", "<html></html>")

	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, err := BuildManifest(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Store("mission-1", outputDir, files); err != nil {
		t.Fatal(err)
	}

	if err := cache.Clear("mission-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Manifest("mission-1"); ok {
		t.Error("expected manifest to be gone after Clear")
	}

	// Clearing something never stored must not error (idempotent).
	if err := cache.Clear("never-stored"); err != nil {
		t.Errorf("clearing an unknown mission should not error, got %v", err)
	}
}

func TestCache_ContentAddressedDedup(t *testing.T) {
	outputDir := t.TempDir()
	// Two files with identical content should share one blob.
	writeFile(t, outputDir, "a.txt", "same content")
	writeFile(t, outputDir, "b.txt", "same content")

	cache, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, err := BuildManifest(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 manifest entries, got %d", len(files))
	}
	if files[0].SHA256 != files[1].SHA256 {
		t.Fatalf("expected identical content to hash the same: %s vs %s", files[0].SHA256, files[1].SHA256)
	}

	if err := cache.Store("mission-1", outputDir, files); err != nil {
		t.Fatal(err)
	}

	blob, err := cache.Blob(files[0].SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) != "same content" {
		t.Errorf("unexpected blob: %q", blob)
	}
}
