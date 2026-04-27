package session

import (
	"sync"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/logger"
	"github.com/open-agents/open-agents-bridge/internal/protocol"
)

type OutputCallback func(sessionID string, msg protocol.Message)

// ExitCallback is called when a session exits
type ExitCallback func(sessionID string, exitCode int, output []byte)

type Manager struct {
	sessions       map[string]*Session
	mu             sync.RWMutex
	outputCallback OutputCallback
	exitCallback   ExitCallback
	maxConcurrent  int
	queue          []QueueItem
	queueMu        sync.Mutex
	ioLogger       *logger.IOLogger // I/O logger for debugging and auditing
}

type QueueItem struct {
	CLIType    string
	WorkDir    string
	SessionID  string
	Cols       int
	Rows       int
	PermMode   string
	Prompt     string
	EnqueuedAt time.Time
}

type Session struct {
	ID             string
	CLIType        string
	WorkDir        string
	PermissionMode string // "default", "plan", "accept-edits", "accept-all"
	Status         string // "active", "completed", "error", "replaced"
	Protocol       *protocol.Manager
	CreatedAt      time.Time
	LastActiveAt   time.Time              // Track last activity
	Config         protocol.AdapterConfig // Store config for reconnection
	ioLogger       *logger.IOLogger       // I/O logger for this session

	// Multi-agent task metadata
	JobID     string    // Associated multi-agent job ID (if any)
	TaskID    string    // Associated multi-agent task ID (if any)
	StartedAt time.Time // Task start time for duration tracking
	Output    []byte    // Collected CLI output for artifacts extraction
	ExitCode  int       // Process exit code (set when session exits)
}

func NewManager() *Manager {
	return &Manager{
		sessions:      make(map[string]*Session),
		maxConcurrent: 3,
	}
}

func (m *Manager) SetMaxConcurrent(n int) {
	m.maxConcurrent = n
}

func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, s := range m.sessions {
		if s.Status == "active" {
			count++
		}
	}
	return count
}

func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

func (m *Manager) MaxConcurrent() int {
	return m.maxConcurrent
}

func (m *Manager) Enqueue(item QueueItem) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	item.EnqueuedAt = time.Now()
	m.queue = append(m.queue, item)
	logger.Debug("[%s] Enqueued session %s, queue size: %d", logger.ModSession, item.SessionID, len(m.queue))
}

func (m *Manager) DequeueNext() *QueueItem {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	if len(m.queue) == 0 {
		return nil
	}
	item := m.queue[0]
	m.queue = m.queue[1:]
	return &item
}

func (m *Manager) SetOutputCallback(callback OutputCallback) {
	m.outputCallback = callback
}

func (m *Manager) SetExitCallback(callback ExitCallback) {
	m.exitCallback = callback
}

// SetIOLogger sets the I/O logger for the session manager
func (m *Manager) SetIOLogger(ioLogger *logger.IOLogger) {
	m.ioLogger = ioLogger
}

func (m *Manager) Create(cliType, workDir string) (*Session, error) {
	return m.CreateWithID(cliType, workDir, "")
}

func (m *Manager) CreateWithID(cliType, workDir, sessionID string) (*Session, error) {
	return m.CreateWithIDAndSize(cliType, workDir, sessionID, 120, 30, "default")
}


// activeCountLocked returns active session count (must be called with lock held)
func (m *Manager) activeCountLocked() int {
	count := 0
	for _, s := range m.sessions {
		if s.Status == "active" {
			count++
		}
	}
	return count
}

// canResumeSession checks if an existing session can be resumed
func (m *Manager) canResumeSession(sess *Session, cliType, workDir string) bool {
	// Check if session is active
	if sess.Status != "active" {
		logger.Warn("[%s] Cannot resume: status is %s (not active)", logger.ModSession, sess.Status)
		return false
	}

	// Check if CLI type matches
	if sess.CLIType != cliType {
		logger.Warn("[%s] Cannot resume: CLI type mismatch (existing: %s, requested: %s)", logger.ModSession, sess.CLIType, cliType)
		return false
	}

	// Check if working directory matches
	if sess.WorkDir != workDir {
		logger.Warn("[%s] Cannot resume: workDir mismatch (existing: %s, requested: %s)", logger.ModSession, sess.WorkDir, workDir)
		return false
	}

	// Check if protocol exists and is connected
	if sess.Protocol == nil {
		logger.Warn("[%s] Cannot resume: protocol is nil", logger.ModSession)
		return false
	}

	if !sess.Protocol.IsConnected() {
		logger.Warn("[%s] Cannot resume: protocol is disconnected", logger.ModSession)
		return false
	}

	// All checks passed - can resume
	logger.Debug("[%s] Can resume: all checks passed", logger.ModSession)
	return true
}

// StartCleanupWorker starts a background worker to clean up inactive sessions
func (m *Manager) StartCleanupWorker(interval time.Duration, maxIdleTime time.Duration) {
	logger.Info("[%s] Starting cleanup worker (interval: %v, maxIdleTime: %v)", logger.ModSession, interval, maxIdleTime)
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			m.cleanupIdleSessions(maxIdleTime)
		}
	}()
}

// cleanupIdleSessions removes inactive sessions that have been idle for too long
func (m *Manager) cleanupIdleSessions(maxIdleTime time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	cleaned := 0
	checked := 0

	for id, sess := range m.sessions {
		checked++
		// Only clean up non-active sessions
		if sess.Status != "active" {
			idleTime := now.Sub(sess.CreatedAt)

			if idleTime > maxIdleTime {
				// Disconnect protocol if still connected
				if sess.Protocol != nil {
					sess.Protocol.Disconnect()
				}
				delete(m.sessions, id)
				cleaned++
				logger.Debug("[%s] Cleaned up idle session %s (status: %s, idle: %v)",
					logger.ModSession, id, sess.Status, idleTime)
			}
		}
	}

	if cleaned > 0 {
		logger.Info("[%s] Cleanup complete: removed=%d, remaining=%d (active: %d)",
			logger.ModSession, cleaned, len(m.sessions), m.activeCountLocked())
	}
}

// GetStats returns session statistics
func (m *Manager) GetStats() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := map[string]int{
		"total":     len(m.sessions),
		"active":    0,
		"completed": 0,
		"error":     0,
		"replaced":  0,
	}

	for _, sess := range m.sessions {
		switch sess.Status {
		case "active":
			stats["active"]++
		case "completed":
			stats["completed"]++
		case "error":
			stats["error"]++
		case "replaced":
			stats["replaced"]++
		}
	}

	return stats
}

// applyPermissionMode configures the adapter based on permission mode
func (m *Manager) applyPermissionMode(permissionMode, cliType string, config *protocol.AdapterConfig) {
	logger.Debug("[%s] Applying permission mode: %s for CLI: %s", logger.ModSession, permissionMode, cliType)

	// Initialize CustomEnv if nil
	if config.CustomEnv == nil {
		config.CustomEnv = make(map[string]string)
	}

	switch permissionMode {
	case "accept-all":
		// Auto-accept all operations
		switch cliType {
		case "claude", "claude-pty":
			config.CustomEnv["CLAUDE_PERMISSION_MODE"] = "accept-all"
			config.Args = append(config.Args, "--dangerously-skip-permissions")
		case "qwen":
			config.CustomEnv["QWEN_PERMISSION_MODE"] = "accept-all"
		case "goose":
			config.CustomEnv["GOOSE_MODE"] = "auto"
		case "gemini":
			config.CustomEnv["GEMINI_PERMISSION_MODE"] = "accept-all"
		case "aider":
			// Aider uses --yes for auto-accept
			config.Args = append(config.Args, "--yes")
		}

	case "accept-edits":
		// Auto-accept file edits only
		switch cliType {
		case "claude", "claude-pty":
			config.CustomEnv["CLAUDE_PERMISSION_MODE"] = "accept-edits"
		case "qwen":
			config.CustomEnv["QWEN_PERMISSION_MODE"] = "accept-edits"
		case "goose":
			config.CustomEnv["GOOSE_MODE"] = "auto-edit"
		case "gemini":
			config.CustomEnv["GEMINI_PERMISSION_MODE"] = "accept-edits"
		case "aider":
			// Aider doesn't distinguish between edits and commands
			// Use --yes for auto-accept in edit mode too
			config.Args = append(config.Args, "--yes")
		}

	case "plan":
		// Plan mode - show plan before execution
		switch cliType {
		case "claude", "claude-pty":
			config.CustomEnv["CLAUDE_PERMISSION_MODE"] = "plan"
			config.Args = append(config.Args, "--plan")
		case "qwen":
			config.CustomEnv["QWEN_PERMISSION_MODE"] = "plan"
		case "goose":
			config.CustomEnv["GOOSE_MODE"] = "plan"
		case "gemini":
			config.CustomEnv["GEMINI_PERMISSION_MODE"] = "plan"
		}

	default:
		// Default mode - ask for confirmation on sensitive operations
		// Most CLIs use this as default, no env vars needed
		logger.Debug("[%s] Using default permission mode", logger.ModSession)
	}
}

func (m *Manager) getCLICommand(cliType string) (string, []string) {
	switch cliType {
	case "claude":
		// Claude Code ACP via npx
		return "npx", []string{"@zed-industries/claude-code-acp"}
	case "claude-pty":
		// Claude Code PTY mode - full REPL with slash commands support
		return "claude", nil
	case "qwen":
		return "qwen-code", []string{"--experimental-acp"}
	case "goose":
		return "goose", []string{"acp"}
	case "gemini":
		return "gemini-cli", []string{"--acp"}
	case "kiro":
		return "kiro", []string{"chat"}
	case "cline":
		return "cline", nil
	case "codex":
		return "codex", nil
	case "aider":
		// Aider - AI pair programming in terminal (PTY mode)
		// Installation: pip install aider-chat
		// Uses its own protocol, not ACP
		return "aider", []string{"--no-auto-commits", "--pretty"}
	default:
		return cliType, nil
	}
}

func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess := m.sessions[id]
	if sess == nil {
		logger.Warn("[%s] Session not found: %s. Active sessions: %v", logger.ModSession, id, m.getSessionIDs())
	}
	return sess
}

func (m *Manager) getSessionIDs() []string {
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) List() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result
}

func (m *Manager) Stop(id string) error {
	return m.StopWithExitCode(id, 0)
}

// StopWithExitCode stops a session and reports the exit code
func (m *Manager) StopWithExitCode(id string, exitCode int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[id]
	if !ok {
		return nil
	}

	// Get output before disconnecting
	output := sess.Output

	if sess.Protocol != nil {
		sess.Protocol.Disconnect()
	}

	// Determine final status based on exit code
	if exitCode == 0 {
		sess.Status = "completed"
	} else {
		sess.Status = "error"
	}

	// Store session info before deletion for callback
	jobID := sess.JobID
	taskID := sess.TaskID

	delete(m.sessions, id)

	// Call exit callback if set and this is a multi-agent task
	if m.exitCallback != nil && jobID != "" && taskID != "" {
		go m.exitCallback(id, exitCode, output)
	}

	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sess := range m.sessions {
		if sess.Protocol != nil {
			sess.Protocol.Disconnect()
		}
	}
	m.sessions = make(map[string]*Session)
}

func (s *Session) Send(input string) error {
	logger.Debug("[%s] Send called for session %s, input: %q, Protocol nil: %v", logger.ModSession, s.ID, input, s.Protocol == nil)

	// Log user input if I/O logging is enabled
	if s.ioLogger != nil && s.ioLogger.ShouldLog("prompt") {
		s.ioLogger.Log(s.ID, "input", "prompt", input)
	}

	if s.Protocol == nil {
		logger.Error("[%s] Protocol is nil for session %s", logger.ModSession, s.ID)
		return nil
	}
	err := s.Protocol.SendMessage(protocol.Message{
		Type:    protocol.MessageTypeContent,
		Content: input,
	})
	if err != nil {
		logger.Error("[%s] SendMessage error: %v", logger.ModSession, err)
	}
	return err
}

// SetMultiAgentMetadata sets the multi-agent task metadata for a session
func (s *Session) SetMultiAgentMetadata(jobID, taskID string) {
	s.JobID = jobID
	s.TaskID = taskID
	s.StartedAt = time.Now()
}

// GetMultiAgentMetadata returns the multi-agent task metadata
func (s *Session) GetMultiAgentMetadata() (jobID, taskID string, startedAt time.Time) {
	return s.JobID, s.TaskID, s.StartedAt
}

func (s *Session) Resize(cols, rows int) error {
	// Resize is handled by the protocol adapter
	// For now, we don't expose this in the protocol interface
	// TODO: Add resize support to protocol.Adapter interface if needed
	return nil
}

func (m *Manager) Resize(id string, cols, rows int) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return nil
	}
	return sess.Resize(cols, rows)
}

func (s *Session) GetProtocolName() string {
	if s.Protocol == nil {
		return "none"
	}
	return s.Protocol.GetProtocolName()
}

// FallbackConfig holds model fallback chain configuration
type FallbackConfig struct {
	CLIType  string
	Fallback string
	OnError  string // "rate_limit", "timeout", "any"
}

// GetFallbackCLI returns the fallback CLI type for a given CLI, or empty string if none
func (m *Manager) GetFallbackCLI(cliType string, fallbacks []FallbackConfig) string {
	for _, f := range fallbacks {
		if f.CLIType == cliType {
			return f.Fallback
		}
	}
	return ""
}
