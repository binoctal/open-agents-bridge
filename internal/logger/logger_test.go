package logger

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestLogFormat verifies task 6.4: log file format is [timestamp] [level] [module] message
func TestLogFormat(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")
	os.MkdirAll(logDir, 0755)

	// Override getLogDir by using a direct approach
	// Create logger with custom dir
	l := &Logger{dir: logDir, level: LevelDebug, consoleLevel: LevelError}
	if err := l.openFile(); err != nil {
		t.Fatalf("openFile failed: %v", err)
	}
	defer l.Close()

	// Set as global so package-level functions work
	globalLogger = l

	// Log messages with module prefix (as done in production code)
	Info("[%s] Connected to server", ModBridge)
	Debug("[%s] Received raw data: %s", ModProtocol, Truncate("some payload data here", MaxPayload))
	Warn("[%s] Reconnection failed", ModSession)
	Error("[%s] WebSocket read error: %v", ModBridge, "connection reset")

	// Read log file
	entries, _ := os.ReadDir(logDir)
	if len(entries) == 0 {
		t.Fatal("No log file created")
	}

	content, err := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) < 4 {
		t.Fatalf("Expected at least 4 log lines, got %d", len(lines))
	}

	// Expected format: [2026-04-04 12:00:00.000] [INFO] [bridge] Connected to server
	logFormatRegex := regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] \[(DEBUG|INFO|WARN|ERROR)\] \[\w+\] .+$`)

	for i, line := range lines {
		if !logFormatRegex.MatchString(line) {
			t.Errorf("Line %d does not match expected format [timestamp] [level] [module] message:\n  %s", i+1, line)
		}
	}

	// Verify specific module prefixes
	if !strings.Contains(lines[0], "[bridge]") {
		t.Errorf("Line 1 missing [bridge] module prefix: %s", lines[0])
	}
	if !strings.Contains(lines[1], "[protocol]") {
		t.Errorf("Line 2 missing [protocol] module prefix: %s", lines[1])
	}
	if !strings.Contains(lines[2], "[session]") {
		t.Errorf("Line 3 missing [session] module prefix: %s", lines[2])
	}
}

// TestInfoLevelFiltering verifies task 6.2: at info level, only lifecycle events appear (no debug details)
func TestInfoLevelFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")
	os.MkdirAll(logDir, 0755)

	l := &Logger{dir: logDir, level: LevelInfo, consoleLevel: LevelInfo}
	if err := l.openFile(); err != nil {
		t.Fatalf("openFile failed: %v", err)
	}
	defer l.Close()
	globalLogger = l

	// Simulate typical info-level session: lifecycle events + debug details
	Info("[%s] Connected to server", ModBridge)
	Debug("[%s] Received raw data: {type:'heartbeat',payload:...}", ModProtocol)
	Debug("[%s] Heartbeat request sent", ModHeartbeat)
	Info("[%s] Session created: abc123", ModSession)
	Debug("[%s] Message routing: queue size=0", ModBridge)
	Warn("[%s] Session not found, creating new", ModSession)
	Error("[%s] WebSocket read error", ModBridge)

	// Read log file
	entries, _ := os.ReadDir(logDir)
	content, _ := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")

	// Count levels present
	debugCount := 0
	infoCount := 0
	warnCount := 0
	errorCount := 0
	for _, line := range lines {
		if strings.Contains(line, "[DEBUG]") {
			debugCount++
		}
		if strings.Contains(line, "[INFO]") {
			infoCount++
		}
		if strings.Contains(line, "[WARN]") {
			warnCount++
		}
		if strings.Contains(line, "[ERROR]") {
			errorCount++
		}
	}

	// At info level, no debug messages should appear
	if debugCount != 0 {
		t.Errorf("At info level, expected 0 DEBUG lines, got %d", debugCount)
	}
	if infoCount != 2 {
		t.Errorf("Expected 2 INFO lines (lifecycle events), got %d", infoCount)
	}
	if warnCount != 1 {
		t.Errorf("Expected 1 WARN line, got %d", warnCount)
	}
	if errorCount != 1 {
		t.Errorf("Expected 1 ERROR line, got %d", errorCount)
	}
}

// TestDebugLevelShowsDetails verifies task 6.3: at debug level, message details are visible
func TestDebugLevelShowsDetails(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")
	os.MkdirAll(logDir, 0755)

	l := &Logger{dir: logDir, level: LevelDebug, consoleLevel: LevelError}
	if err := l.openFile(); err != nil {
		t.Fatalf("openFile failed: %v", err)
	}
	defer l.Close()
	globalLogger = l

	// Log messages at debug level
	Info("[%s] Connected to server", ModBridge)
	Debug("[%s] Received raw data: %s", ModProtocol, Truncate(`{"type":"heartbeat","payload":{"status":"ok","timestamp":"2026-04-04T12:00:00Z","details":"some long detail"}}`, MaxPayload))
	Debug("[%s] Heartbeat response sent", ModHeartbeat)

	// Read log file
	entries, _ := os.ReadDir(logDir)
	content, _ := os.ReadFile(filepath.Join(logDir, entries[0].Name()))
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")

	// At debug level, ALL messages should appear
	debugCount := 0
	for _, line := range lines {
		if strings.Contains(line, "[DEBUG]") {
			debugCount++
		}
	}
	if debugCount != 2 {
		t.Errorf("At debug level, expected 2 DEBUG lines, got %d", debugCount)
	}

	// Verify message details are present in debug lines
	foundRawData := false
	foundHeartbeat := false
	for _, line := range lines {
		if strings.Contains(line, "Received raw data") {
			foundRawData = true
		}
		if strings.Contains(line, "Heartbeat response sent") {
			foundHeartbeat = true
		}
	}
	if !foundRawData {
		t.Error("Debug detail 'Received raw data' not found in log output")
	}
	if !foundHeartbeat {
		t.Error("Debug detail 'Heartbeat response sent' not found in log output")
	}
}

// TestTruncate verifies task 6.3: payload is truncated to 200 characters
func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "short string unchanged",
			input:  "hello",
			maxLen: 200,
			want:   "hello",
		},
		{
			name:   "exactly at limit",
			input:  strings.Repeat("a", 200),
			maxLen: 200,
			want:   strings.Repeat("a", 200),
		},
		{
			name:   "truncated with ellipsis",
			input:  strings.Repeat("a", 250),
			maxLen: 200,
			want:   strings.Repeat("a", 200) + "...",
		},
		{
			name:   "truncated JSON payload",
			input:  `{"type":"message","content":"` + strings.Repeat("x", 300) + `"}`,
			maxLen: 200,
			want:   `{"type":"message","content":"` + strings.Repeat("x", 171) + `...`,
		},
		{
			name:   "empty string",
			input:  "",
			maxLen: 200,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("Truncate() = %q (len=%d), want %q (len=%d)",
					got, len(got), tt.want, len(tt.want))
			}
		})
	}
}

// TestConsoleNoDebug verifies that Debug never appears on console (stderr)
func TestConsoleNoDebug(t *testing.T) {
	tmpDir := t.TempDir()
	logDir := filepath.Join(tmpDir, "logs")
	os.MkdirAll(logDir, 0755)

	l := &Logger{dir: logDir, level: LevelDebug, consoleLevel: LevelInfo}
	if err := l.openFile(); err != nil {
		t.Fatalf("openFile failed: %v", err)
	}
	defer l.Close()
	globalLogger = l

	// Capture stderr
oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	Debug("[%s] This is debug", ModBridge)
	Info("[%s] This is info", ModBridge)

	w.Close()
	os.Stderr = oldStderr

	// Read captured stderr
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	consoleOutput := string(buf[:n])

	// Debug should NOT appear on console
	if strings.Contains(consoleOutput, "This is debug") {
		t.Error("Debug message should not appear on console (stderr)")
	}
	// Info SHOULD appear on console
	if !strings.Contains(consoleOutput, "This is info") {
		t.Error("Info message should appear on console (stderr)")
	}
}
