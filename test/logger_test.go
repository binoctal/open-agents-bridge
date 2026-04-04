package test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

func TestLoggerWrite(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Unsetenv("HOME")

	// Create log dir manually since logger uses fixed path
	logDir := filepath.Join(tmpDir, ".open-agents-bridge", "logs")
	os.MkdirAll(logDir, 0755)

	l, err := logger.New()
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer l.Close()

	msg := []byte("Test log message\n")
	n, err := l.Write(msg)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(msg) {
		t.Errorf("Write returned %d, want %d", n, len(msg))
	}

	// Verify file exists
	entries, _ := os.ReadDir(logDir)
	if len(entries) == 0 {
		t.Error("No log file created")
	}
}

func TestLoggerWriter(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv("HOME", tmpDir)
	defer os.Unsetenv("HOME")

	logDir := filepath.Join(tmpDir, ".open-agents-bridge", "logs")
	os.MkdirAll(logDir, 0755)

	l, err := logger.New()
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer l.Close()

	w := l.Writer()
	if w == nil {
		t.Error("Writer returned nil")
	}
}
