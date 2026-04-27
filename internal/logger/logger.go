package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MaxSize    = 10 * 1024 * 1024 // 10MB
	MaxBackups = 7
	MaxPayload = 200 // Max characters for payload truncation
)

// Log levels
const (
	LevelDebug = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelNames = map[int]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

var levelFromString = map[string]int{
	"debug": LevelDebug,
	"info":  LevelInfo,
	"warn":  LevelWarn,
	"error": LevelError,
}

// Module constants for structured log prefixes
const (
	ModBridge    = "bridge"
	ModSession   = "session"
	ModProtocol  = "protocol"
	ModHeartbeat = "heartbeat"
	ModPermission = "perm"
	ModWorkflow  = "workflow"
	ModScanner   = "scanner"
	ModACP       = "acp"
	ModPTY       = "pty"
	ModAdapter   = "adapter"
)

// Global logger instance
var globalLogger *Logger

// Logger is a rotating file logger with level and console support
type Logger struct {
	dir          string
	file         *os.File
	size         int64
	level        int
	consoleLevel int // minimum level for console (stderr) output
	skipConsole  bool // true when stderr points to the log file (avoid duplicates)
	mu           sync.Mutex
}

// New creates a new rotating logger
func New() (*Logger, error) {
	dir := getLogDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	l := &Logger{dir: dir, level: LevelInfo, consoleLevel: LevelInfo}
	if err := l.openFile(); err != nil {
		return nil, err
	}

	// Detect if stderr points to the same file as the log file
	// (common when bridge is started with stderr redirected to the log)
	l.skipConsole = sameFile(os.Stderr, l.file)

	globalLogger = l
	return l, nil
}

// SetLevel sets the log level from string (debug, info, warn, error)
func (l *Logger) SetLevel(levelStr string) {
	if level, ok := levelFromString[levelStr]; ok {
		l.level = level
	}
}

// SetGlobalLevel sets the global logger level
func SetGlobalLevel(levelStr string) {
	if globalLogger != nil {
		globalLogger.SetLevel(levelStr)
	}
}

// Debug logs at debug level
func Debug(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.log(LevelDebug, format, args...)
	}
}

// Info logs at info level
func Info(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.log(LevelInfo, format, args...)
	}
}

// Warn logs at warn level
func Warn(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.log(LevelWarn, format, args...)
	}
}

// Error logs at error level
func Error(format string, args ...interface{}) {
	if globalLogger != nil {
		globalLogger.log(LevelError, format, args...)
	}
}

func (l *Logger) log(level int, format string, args ...interface{}) {
	if level < l.level {
		return
	}

	msg := fmt.Sprintf(format, args...)
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	line := fmt.Sprintf("[%s] [%s] %s\n", timestamp, levelNames[level], msg)

	// Write to log file (all levels >= file level)
	l.Write([]byte(line))

	// Write to console (stderr) for levels >= console level
	// Debug never goes to console to avoid terminal noise
	// Skip if stderr points to the same file as the log file to avoid duplicates
	if !l.skipConsole && level >= l.consoleLevel && level > LevelDebug {
		fmt.Fprint(os.Stderr, line)
	}
}

// SetConsoleLevel sets the minimum level for console output.
// Debug level messages are never sent to console.
func (l *Logger) SetConsoleLevel(levelStr string) {
	if level, ok := levelFromString[levelStr]; ok {
		l.consoleLevel = level
	}
}

// SetGlobalConsoleLevel sets console output level on the global logger
func SetGlobalConsoleLevel(levelStr string) {
	if globalLogger != nil {
		globalLogger.SetConsoleLevel(levelStr)
	}
}

func (l *Logger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.size+int64(len(p)) > MaxSize {
		l.rotate()
	}

	n, err = l.file.Write(p)
	l.size += int64(n)
	return
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

func (l *Logger) openFile() error {
	name := fmt.Sprintf("bridge-%s.log", time.Now().Format("2006-01-02"))
	path := filepath.Join(l.dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	info, _ := f.Stat()
	l.file = f
	l.size = info.Size()
	return nil
}

func (l *Logger) rotate() {
	l.file.Close()
	l.cleanup()
	l.openFile()
}

func (l *Logger) cleanup() {
	entries, _ := os.ReadDir(l.dir)
	var logs []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".log" {
			logs = append(logs, filepath.Join(l.dir, e.Name()))
		}
	}

	if len(logs) <= MaxBackups {
		return
	}

	sort.Slice(logs, func(i, j int) bool {
		fi, _ := os.Stat(logs[i])
		fj, _ := os.Stat(logs[j])
		return fi.ModTime().Before(fj.ModTime())
	})

	for i := 0; i < len(logs)-MaxBackups; i++ {
		os.Remove(logs[i])
	}
}

// Writer returns an io.Writer for use with log.SetOutput.
// Only writes to the log file, not to stderr — terminal output
// is handled explicitly via fmt.Printf for important events.
func (l *Logger) Writer() io.Writer {
	return l
}

func GetLogDir() string {
	return getLogDir()
}

func getLogDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "open-agents-bridge", "logs")
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Logs", "open-agents-bridge")
	default:
		return filepath.Join(os.Getenv("HOME"), ".open-agents-bridge", "logs")
	}
}

// ============================================
// I/O Logger (for debugging and auditing)
// ============================================

// Valid message types for I/O logging
var ValidIOTypes = map[string]bool{
	"prompt":        true,
	"agent_message": true,
	"agent_thought": true,
	"tool_call":     true,
}

// IOLogEntry represents a single I/O log entry
type IOLogEntry struct {
	Timestamp string      `json:"timestamp"`
	SessionID string      `json:"sessionId"`
	Direction string      `json:"direction"` // "input" or "output"
	Type      string      `json:"type"`      // prompt, agent_message, agent_thought, tool_call
	Content   interface{} `json:"content"`
}

// IOLogger logs input/output messages for debugging and auditing
type IOLogger struct {
	dir        string
	file       *os.File
	size       int64
	maxSize    int64
	maxBackups int
	types      map[string]bool
	ch         chan *IOLogEntry
	done       chan struct{}
	wg         sync.WaitGroup
	mu         sync.Mutex
}

// IOLoggerConfig holds configuration for IOLogger
type IOLoggerConfig struct {
	Enabled    bool
	Types      []string
	MaxSizeMB  int
	MaxBackups int
}

// NewIOLogger creates a new I/O logger
func NewIOLogger(cfg *IOLoggerConfig) (*IOLogger, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil // Return nil if disabled
	}

	dir := getLogDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	// Set defaults
	maxSize := int64(50 * 1024 * 1024) // 50MB default
	if cfg.MaxSizeMB > 0 {
		maxSize = int64(cfg.MaxSizeMB) * 1024 * 1024
	}
	maxBackups := 7
	if cfg.MaxBackups > 0 {
		maxBackups = cfg.MaxBackups
	}

	// Parse and validate types
	types := make(map[string]bool)
	for _, t := range cfg.Types {
		t = strings.TrimSpace(t)
		if ValidIOTypes[t] {
			types[t] = true
		}
	}

	// If no valid types, default to prompt and agent_message
	if len(types) == 0 {
		types["prompt"] = true
		types["agent_message"] = true
	}

	l := &IOLogger{
		dir:        dir,
		maxSize:    maxSize,
		maxBackups: maxBackups,
		types:      types,
		ch:         make(chan *IOLogEntry, 1000), // buffered channel
		done:       make(chan struct{}),
	}

	if err := l.openIOFile(); err != nil {
		return nil, err
	}

	// Start async writer goroutine
	l.wg.Add(1)
	go l.writeLoop()

	Info("[IOLogger] Started with types: %v", cfg.Types)
	return l, nil
}

// Log queues an I/O log entry for async writing
func (l *IOLogger) Log(sessionID, direction, msgType string, content interface{}) {
	if l == nil {
		return
	}

	// Check if this type should be logged
	if !l.types[msgType] {
		return
	}

	entry := &IOLogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		SessionID: sessionID,
		Direction: direction,
		Type:      msgType,
		Content:   content,
	}

	// Non-blocking send to channel
	select {
	case l.ch <- entry:
	default:
		// Channel full, drop the entry (avoid blocking)
		Warn("[IOLogger] Channel full, dropping entry")
	}
}

// writeLoop processes log entries asynchronously
func (l *IOLogger) writeLoop() {
	defer l.wg.Done()

	for {
		select {
		case entry := <-l.ch:
			l.writeEntry(entry)
		case <-l.done:
			// Drain remaining entries before exiting
			for {
				select {
				case entry := <-l.ch:
					l.writeEntry(entry)
				default:
					return
				}
			}
		}
	}
}

// writeEntry writes a single log entry to file
func (l *IOLogger) writeEntry(entry *IOLogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		Warn("[IOLogger] Failed to marshal entry: %v", err)
		return
	}

	// Check if we need to rotate (before adding newline)
	if l.size+int64(len(data))+1 > l.maxSize {
		l.rotateIO()
	}

	// Write JSON line
	n, err := l.file.Write(append(data, '\n'))
	if err != nil {
		Warn("[IOLogger] Failed to write entry: %v", err)
		return
	}
	l.size += int64(n)
}

// openIOFile opens the I/O log file for today
func (l *IOLogger) openIOFile() error {
	name := fmt.Sprintf("io-%s.log", time.Now().Format("2006-01-02"))
	path := filepath.Join(l.dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	info, _ := f.Stat()
	l.file = f
	l.size = info.Size()
	return nil
}

// rotateIO closes current file and opens a new one
func (l *IOLogger) rotateIO() {
	if l.file != nil {
		l.file.Close()
	}
	l.cleanupIO()
	l.openIOFile()
}

// cleanupIO removes old I/O log files
func (l *IOLogger) cleanupIO() {
	entries, _ := os.ReadDir(l.dir)
	var logs []string
	for _, e := range entries {
		name := e.Name()
		// Only clean up io-*.log files
		if strings.HasPrefix(name, "io-") && filepath.Ext(name) == ".log" {
			logs = append(logs, filepath.Join(l.dir, name))
		}
	}

	if len(logs) <= l.maxBackups {
		return
	}

	sort.Slice(logs, func(i, j int) bool {
		fi, _ := os.Stat(logs[i])
		fj, _ := os.Stat(logs[j])
		return fi.ModTime().Before(fj.ModTime())
	})

	for i := 0; i < len(logs)-l.maxBackups; i++ {
		os.Remove(logs[i])
	}
}

// Close flushes remaining entries and closes the logger
func (l *IOLogger) Close() error {
	if l == nil {
		return nil
	}

	// Signal writeLoop to stop
	close(l.done)

	// Wait for writeLoop to finish
	l.wg.Wait()

	// Close file
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// ShouldLog returns true if the given message type should be logged
func (l *IOLogger) ShouldLog(msgType string) bool {
	if l == nil {
		return false
	}
	return l.types[msgType]
}

// sameFile checks if two files point to the same inode (same file)
func sameFile(a, b *os.File) bool {
	if a == nil || b == nil {
		return false
	}
	aInfo, err := a.Stat()
	if err != nil {
		return false
	}
	bInfo, err := b.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(aInfo, bInfo)
}

// Truncate truncates a string to maxLen characters, appending "..." if truncated.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
