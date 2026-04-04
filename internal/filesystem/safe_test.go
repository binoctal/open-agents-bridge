package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePath(t *testing.T) {
	// Create a temp dir to use as basePath
	tmpDir := t.TempDir()
	fs := New(tmpDir, DefaultMaxFileSize)

	tests := []struct {
		name    string
		path    string
		wantErr bool
		errMsg  string
	}{
		{"normal relative path", "src/main.go", false, ""},
		{"normal absolute within base", filepath.Join(tmpDir, "test.txt"), false, ""},
		{"path traversal", "../../etc/passwd", true, "path traversal"},
		{"double dot at start", "../secret", true, "path traversal"},
		{"absolute outside base", "/etc/shadow", true, "outside allowed directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fs.ValidatePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
			if err != nil && tt.errMsg != "" {
				if !contains(err.Error(), tt.errMsg) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errMsg)
				}
			}
		})
	}
}

func TestSensitiveFileProtection(t *testing.T) {
	tmpDir := t.TempDir()
	fs := New(tmpDir, DefaultMaxFileSize)

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		// Blocked patterns
		{"ssh key", ".ssh/id_rsa", true},
		{"aws credentials", ".aws/credentials", true},
		{"id_rsa direct", "id_rsa", true},
		{".env production", ".env", true},
		{"credentials json", "config/credentials.json", true},

		// Allowed (exceptions)
		{".env.example", ".env.example", false},
		{".env.template", ".env.template", false},
		{".env.sample", ".env.sample", false},
		{".env.dist", ".env.dist", false},
		{".env.local", ".env.local", false},

		// Normal files
		{"normal file", "src/main.go", false},
		{"readme", "README.md", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fs.ValidatePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestFileSizeLimit(t *testing.T) {
	tmpDir := t.TempDir()
	fs := New(tmpDir, 100) // 100 bytes max for testing

	// Create a test file that's too large
	bigFile := filepath.Join(tmpDir, "big.txt")
	bigContent := make([]byte, 200)
	if err := os.WriteFile(bigFile, bigContent, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := fs.SafeReadFile("big.txt")
	if err == nil {
		t.Error("expected error for oversized file read")
	}
	if !contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' error, got: %v", err)
	}
}

func TestWriteSizeLimit(t *testing.T) {
	tmpDir := t.TempDir()
	fs := New(tmpDir, 100) // 100 bytes max for testing

	bigContent := make([]byte, 200)
	err := fs.SafeWriteFile("test.txt", bigContent)
	if err == nil {
		t.Error("expected error for oversized write")
	}
	if !contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' error, got: %v", err)
	}
}

func TestSafeWriteAndRead(t *testing.T) {
	tmpDir := t.TempDir()
	fs := New(tmpDir, DefaultMaxFileSize)

	content := []byte("hello world")
	err := fs.SafeWriteFile("test.txt", content)
	if err != nil {
		t.Fatalf("SafeWriteFile failed: %v", err)
	}

	read, err := fs.SafeReadFile("test.txt")
	if err != nil {
		t.Fatalf("SafeReadFile failed: %v", err)
	}

	if string(read) != "hello world" {
		t.Errorf("read %q, want %q", string(read), "hello world")
	}
}

func TestWriteCreatesDirectories(t *testing.T) {
	tmpDir := t.TempDir()
	fs := New(tmpDir, DefaultMaxFileSize)

	err := fs.SafeWriteFile("deep/nested/dir/test.txt", []byte("content"))
	if err != nil {
		t.Fatalf("SafeWriteFile with nested dirs failed: %v", err)
	}

	read, err := fs.SafeReadFile("deep/nested/dir/test.txt")
	if err != nil {
		t.Fatalf("SafeReadFile failed: %v", err)
	}

	if string(read) != "content" {
		t.Errorf("read %q, want %q", string(read), "content")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
