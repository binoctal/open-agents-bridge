package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultMaxFileSize is the default maximum file size for read/write operations (10MB).
const DefaultMaxFileSize int64 = 10 * 1024 * 1024

// sensitivePatterns are patterns that indicate sensitive files/directories.
// Access to paths matching these patterns will be blocked.
var sensitivePatterns = []string{
	".ssh",
	".aws",
	".gnupg",
	"id_rsa",
	"id_ed25519",
	"id_ecdsa",
	"credentials",
	".gitconfig",
	".npmrc",
}

// sensitiveExactMatches are full filename patterns that are always blocked.
var sensitiveExactMatches = []string{
	".env",
}

// sensitiveExceptions are suffixes that exempt a path from sensitive matching.
// e.g., .env.example, .env.template, .env.sample are allowed.
var sensitiveExceptions = []string{
	".example",
	".template",
	".sample",
	".dist",
	".local",
	".development",
	".test",
	".stub",
}

// SafeFileSystem provides safe file operations with path traversal detection,
// sensitive file protection, and size limits.
type SafeFileSystem struct {
	basePath string
	maxSize  int64
}

// New creates a new SafeFileSystem rooted at basePath with the given max file size.
func New(basePath string, maxSize int64) *SafeFileSystem {
	abs, _ := filepath.Abs(basePath)
	return &SafeFileSystem{
		basePath: abs,
		maxSize:  maxSize,
	}
}

// ValidatePath validates that the requested path is safe to access.
// It performs: path cleaning → symlink resolution → traversal detection →
// basePath containment check → sensitive file pattern matching.
func (fs *SafeFileSystem) ValidatePath(requestedPath string) (string, error) {
	// Step 1: Clean the path
	cleaned := filepath.Clean(requestedPath)

	// Step 2: Check for path traversal
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("path traversal detected")
	}

	// Step 3: Resolve to absolute path
	var absPath string
	if filepath.IsAbs(cleaned) {
		absPath = cleaned
	} else {
		absPath = filepath.Join(fs.basePath, cleaned)
	}

	// Step 4: Resolve symlinks (if file exists)
	resolved := absPath
	if _, err := os.Lstat(absPath); err == nil {
		evaluated, err := filepath.EvalSymlinks(absPath)
		if err == nil {
			resolved = evaluated
		}
	}

	// Step 5: Ensure resolved path is within basePath
	if !strings.HasPrefix(resolved, fs.basePath+string(os.PathSeparator)) && resolved != fs.basePath {
		return "", fmt.Errorf("access denied: path outside allowed directory")
	}

	// Step 6: Check sensitive file patterns
	if err := checkSensitivePatterns(resolved); err != nil {
		return "", err
	}

	return resolved, nil
}

// checkSensitivePatterns checks if the path matches any sensitive patterns.
func checkSensitivePatterns(path string) error {
	lower := strings.ToLower(path)

	// Check if any exception suffix applies
	for _, exc := range sensitiveExceptions {
		if strings.HasSuffix(lower, exc) {
			return nil
		}
	}

	// Check exact matches (full filename component)
	parts := strings.Split(lower, string(os.PathSeparator))
	for _, part := range parts {
		for _, pattern := range sensitiveExactMatches {
			if part == pattern {
				return fmt.Errorf("access denied: sensitive file pattern '%s'", pattern)
			}
		}
	}

	// Check substring patterns
	for _, pattern := range sensitivePatterns {
		if strings.Contains(lower, pattern) {
			return fmt.Errorf("access denied: sensitive file pattern '%s'", pattern)
		}
	}

	return nil
}

// SafeReadFile safely reads a file after validating the path and checking the file size.
func (fs *SafeFileSystem) SafeReadFile(path string) ([]byte, error) {
	safePath, err := fs.ValidatePath(path)
	if err != nil {
		return nil, err
	}

	// Check file size before reading
	info, err := os.Stat(safePath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	if info.Size() > fs.maxSize {
		return nil, fmt.Errorf("file too large: %d bytes (max: %d)", info.Size(), fs.maxSize)
	}

	return os.ReadFile(safePath)
}

// SafeWriteFile safely writes content to a file after validating the path and checking content size.
func (fs *SafeFileSystem) SafeWriteFile(path string, content []byte) error {
	safePath, err := fs.ValidatePath(path)
	if err != nil {
		return err
	}

	if int64(len(content)) > fs.maxSize {
		return fmt.Errorf("content too large: %d bytes (max: %d)", len(content), fs.maxSize)
	}

	// Create directory if needed
	dir := filepath.Dir(safePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	return os.WriteFile(safePath, content, 0644)
}
