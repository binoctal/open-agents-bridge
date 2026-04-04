package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() {
	// Ensure log directory exists for tests
	os.MkdirAll(getLogDir(), 0755)
}

func TestNewIOLogger_Disabled(t *testing.T) {
	cfg := &IOLoggerConfig{
		Enabled: false,
	}

	logger, err := NewIOLogger(cfg)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if logger != nil {
		t.Fatal("Expected nil logger when disabled")
	}
}

func TestNewIOLogger_NilConfig(t *testing.T) {
	logger, err := NewIOLogger(nil)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if logger != nil {
		t.Fatal("Expected nil logger when config is nil")
	}
}

func TestNewIOLogger_DefaultTypes(t *testing.T) {
	cfg := &IOLoggerConfig{
		Enabled: true,
		Types:   []string{}, // Empty types should default to prompt and agent_message
	}

	logger, err := NewIOLogger(cfg)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	if !logger.ShouldLog("prompt") {
		t.Error("Expected prompt to be logged by default")
	}
	if !logger.ShouldLog("agent_message") {
		t.Error("Expected agent_message to be logged by default")
	}
	if logger.ShouldLog("unknown_type") {
		t.Error("Expected unknown_type not to be logged")
	}
}

func TestIOLogger_Log(t *testing.T) {
	cfg := &IOLoggerConfig{
		Enabled:    true,
		Types:      []string{"prompt", "agent_message"},
		MaxSizeMB:  1, // 1MB for testing
		MaxBackups: 3,
	}

	logger, err := NewIOLogger(cfg)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// Use unique session ID to identify our test entries
	sessionID := fmt.Sprintf("test-session-%d", time.Now().UnixNano())

	// Log a prompt
	logger.Log(sessionID, "input", "prompt", "Hello, AI!")

	// Log a response
	logger.Log(sessionID, "output", "agent_message", "Hello, human!")

	// Close to flush buffer
	logger.Close()

	// Read and verify log file
	logFile := findLatestIOLogFile(t)
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")

	// Find our test entries by session ID
	var testLines []string
	for _, line := range lines {
		if strings.Contains(line, sessionID) {
			testLines = append(testLines, line)
		}
	}

	if len(testLines) != 2 {
		t.Fatalf("Expected 2 log lines for session %s, got %d", sessionID, len(testLines))
	}

	// Verify first line (prompt)
	var entry1 IOLogEntry
	if err := json.Unmarshal([]byte(testLines[0]), &entry1); err != nil {
		t.Fatalf("Failed to parse first line: %v", err)
	}
	if entry1.SessionID != sessionID {
		t.Errorf("Expected sessionID %s, got %s", sessionID, entry1.SessionID)
	}
	if entry1.Direction != "input" {
		t.Errorf("Expected direction 'input', got %s", entry1.Direction)
	}
	if entry1.Type != "prompt" {
		t.Errorf("Expected type 'prompt', got %s", entry1.Type)
	}
	if entry1.Content != "Hello, AI!" {
		t.Errorf("Expected content 'Hello, AI!', got %v", entry1.Content)
	}

	// Verify second line (response)
	var entry2 IOLogEntry
	if err := json.Unmarshal([]byte(testLines[1]), &entry2); err != nil {
		t.Fatalf("Failed to parse second line: %v", err)
	}
	if entry2.Direction != "output" {
		t.Errorf("Expected direction 'output', got %s", entry2.Direction)
	}
	if entry2.Type != "agent_message" {
		t.Errorf("Expected type 'agent_message', got %s", entry2.Type)
	}
}

func TestIOLogger_ShouldLog(t *testing.T) {
	cfg := &IOLoggerConfig{
		Enabled: true,
		Types:   []string{"prompt", "tool_call"},
	}

	logger, err := NewIOLogger(cfg)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	if !logger.ShouldLog("prompt") {
		t.Error("Expected prompt to be logged")
	}
	if !logger.ShouldLog("tool_call") {
		t.Error("Expected tool_call to be logged")
	}
	if logger.ShouldLog("agent_message") {
		t.Error("Expected agent_message NOT to be logged")
	}
	if logger.ShouldLog("unknown") {
		t.Error("Expected unknown NOT to be logged")
	}
}

func TestIOLogger_ShouldLog_NilLogger(t *testing.T) {
	var logger *IOLogger
	if logger.ShouldLog("prompt") {
		t.Error("Expected false for nil logger")
	}
}

func TestIOLogger_FilterInvalidTypes(t *testing.T) {
	cfg := &IOLoggerConfig{
		Enabled: true,
		Types:   []string{"prompt", "invalid_type", "agent_message", "another_invalid"},
	}

	logger, err := NewIOLogger(cfg)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}
	defer logger.Close()

	// Only valid types should be logged
	if !logger.ShouldLog("prompt") {
		t.Error("Expected prompt to be logged")
	}
	if !logger.ShouldLog("agent_message") {
		t.Error("Expected agent_message to be logged")
	}
	if logger.ShouldLog("invalid_type") {
		t.Error("Expected invalid_type NOT to be logged")
	}
}

func TestIOLogger_TimestampFormat(t *testing.T) {
	cfg := &IOLoggerConfig{
		Enabled: true,
		Types:   []string{"prompt"},
	}

	logger, err := NewIOLogger(cfg)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	logger.Log("session-1", "input", "prompt", "test")
	logger.Close()

	logFile := findLatestIOLogFile(t)
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	// Parse first line only (JSON Lines format)
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) == 0 {
		t.Fatal("No log entries found")
	}

	var entry IOLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("Failed to parse log entry: %v", err)
	}

	// Verify timestamp is ISO 8601 / RFC 3339 format
	_, err = time.Parse(time.RFC3339, entry.Timestamp)
	if err != nil {
		t.Errorf("Timestamp is not in RFC 3339 format: %v (got: %s)", err, entry.Timestamp)
	}
}

func TestIOLogger_NilLoggerNoOp(t *testing.T) {
	var logger *IOLogger

	// These should not panic
	logger.Log("session", "input", "prompt", "content")
	logger.Close()
}

// findLatestIOLogFile finds the latest io-*.log file
func findLatestIOLogFile(t *testing.T) string {
	dir := getLogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("Failed to read log directory: %v", err)
	}

	var logFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "io-") && strings.HasSuffix(e.Name(), ".log") {
			logFiles = append(logFiles, filepath.Join(dir, e.Name()))
		}
	}

	if len(logFiles) == 0 {
		t.Fatal("No I/O log files found")
	}

	// Return the latest one (sorted by name = date)
	return logFiles[len(logFiles)-1]
}

func TestMain(m *testing.M) {
	// Run tests
	os.Exit(m.Run())
}
