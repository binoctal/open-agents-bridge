package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestListDirectories_Basic(t *testing.T) {
	tmp := t.TempDir()
	os.Mkdir(filepath.Join(tmp, "aaa"), 0755)
	os.Mkdir(filepath.Join(tmp, "bbb"), 0755)
	// Create a file - should be excluded
	os.WriteFile(filepath.Join(tmp, "readme.md"), []byte("hi"), 0644)

	dirs, truncated, err := listDirectories(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if truncated {
		t.Error("should not be truncated")
	}
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d", len(dirs))
	}
	if dirs[0].Name != "aaa" || dirs[1].Name != "bbb" {
		t.Errorf("expected sorted names aaa,bbb; got %s,%s", dirs[0].Name, dirs[1].Name)
	}
	if !dirs[0].Accessible || !dirs[1].Accessible {
		t.Error("both should be accessible")
	}
}

func TestListDirectories_Empty(t *testing.T) {
	tmp := t.TempDir()

	dirs, _, err := listDirectories(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("expected 0 dirs, got %d", len(dirs))
	}
}

func TestListDirectories_NotFound(t *testing.T) {
	_, _, err := listDirectories("/nonexistent/path/12345")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestListDirectories_SymlinksSkipped(t *testing.T) {
	tmp := t.TempDir()
	os.Mkdir(filepath.Join(tmp, "real_dir"), 0755)
	os.Symlink(filepath.Join(tmp, "real_dir"), filepath.Join(tmp, "link_dir"))

	dirs, _, err := listDirectories(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("expected 1 dir (symlink skipped), got %d", len(dirs))
	}
	if dirs[0].Name != "real_dir" {
		t.Errorf("expected real_dir, got %s", dirs[0].Name)
	}
}

func TestListDirectories_Truncation(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < maxListDirResults+5; i++ {
		name := filepath.Join(tmp, fmt.Sprintf("dir_%03d", i))
		os.Mkdir(name, 0755)
	}

	dirs, truncated, err := listDirectories(tmp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Error("expected truncated=true")
	}
	if len(dirs) != maxListDirResults {
		t.Errorf("expected %d dirs, got %d", maxListDirResults, len(dirs))
	}
}
