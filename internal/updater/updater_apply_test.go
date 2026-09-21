package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestApplyUpdateFromDifferentDirectory covers the cross-device failure mode:
// the downloaded binary lands in os.TempDir(), which may sit on a different
// filesystem than the install path. ApplyUpdate must stage the new binary in
// the target's own directory before renaming, so the rename never crosses a
// filesystem boundary.
func TestApplyUpdateFromDifferentDirectory(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	src := filepath.Join(srcDir, "downloaded-binary")
	newContent := []byte("new binary contents v0.11.1")
	if err := os.WriteFile(src, newContent, 0755); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dstDir, "open-agents-bridge")
	oldContent := []byte("old binary contents v0.11.0")
	if err := os.WriteFile(dst, oldContent, 0755); err != nil {
		t.Fatal(err)
	}

	if err := applyUpdateTo(dst, src); err != nil {
		t.Fatalf("ApplyUpdate failed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(newContent) {
		t.Fatalf("target not replaced: got %q", got)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("target not executable: %v", info.Mode().Perm())
	}

	// The downloaded temp file is the caller's to clean up; but no staging
	// residue may remain next to the target, and no .bak backup either.
	entries, err := os.ReadDir(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "open-agents-bridge" {
			t.Fatalf("residue left in target dir: %s", e.Name())
		}
	}
}

// TestApplyUpdateRestoresBackupOnFailure verifies the target survives when
// the swap blows up after the backup step. The stage step is made to fail by
// pointing the source at a missing file.
func TestApplyUpdateRestoresBackupOnFailure(t *testing.T) {
	dstDir := t.TempDir()
	dst := filepath.Join(dstDir, "open-agents-bridge")
	oldContent := []byte("old binary contents v0.11.0")
	if err := os.WriteFile(dst, oldContent, 0755); err != nil {
		t.Fatal(err)
	}

	err := applyUpdateTo(dst, filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing source")
	}
	if !strings.Contains(err.Error(), "stage failed") {
		t.Fatalf("expected stage failure, got: %v", err)
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(oldContent) {
		t.Fatalf("target corrupted by failed update: got %q", got)
	}

	entries, dirErr := os.ReadDir(dstDir)
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	if len(entries) != 1 || entries[0].Name() != "open-agents-bridge" {
		t.Fatalf("residue left after failed update: %v", entries)
	}
}
