package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("cannot determine home dir for test: %v", err)
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde expands to home", "~", home},
		{"tilde slash expands", "~/", home},
		{"tilde with subpath joins home", "~/projects/demo", filepath.Join(home, "projects", "demo")},
		{"absolute path unchanged", "/tmp/work", "/tmp/work"},
		{"relative path unchanged", "work", "work"},
		{"dot unchanged", ".", "."},
		{"empty unchanged", "", ""},
		// Documented boundary: another user's home is NOT expanded.
		{"tilde-user form unsupported, passed through", "~user/x", "~user/x"},
		{"tilde-prefixed name passed through", "~foo", "~foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandTilde(tt.in)
			if err != nil {
				t.Fatalf("ExpandTilde(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ExpandTilde(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The chdir failure must surface a truthful "workDir does not exist" error
// instead of os/exec's misleading "fork/exec <cmd>: no such file or directory".
func TestCreateWithIDAndSizeMissingWorkDir(t *testing.T) {
	m := NewManager()
	_, err := m.CreateWithIDAndSize("claude", "~/.definitely-not-here-0922", "sess-tilde-missing", 120, 30, "")
	if err == nil {
		t.Fatal("expected error for non-existent workDir, got nil")
	}
	home, _ := os.UserHomeDir()
	want := "workDir does not exist: " + filepath.Join(home, ".definitely-not-here-0922")
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
