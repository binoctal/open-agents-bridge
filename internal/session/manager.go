package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	configpkg "github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

type OutputCallback func(sessionID string, msg protocol.Message)

// ExitCallback is called when a session exits
type ExitCallback func(sessionID string, exitCode int, output []byte)

// RemovedCallback is called (asynchronously) whenever a session is deleted
// from the manager, on every removal path (stop, replace, idle cleanup,
// create-failure rollback). The bridge uses it to drop per-session state
// (G18 status trackers) so it cannot leak across the six delete sites.
type RemovedCallback func(sessionID string)

type Manager struct {
	sessions         map[string]*Session
	mu               sync.RWMutex
	outputCallback   OutputCallback
	exitCallback     ExitCallback
	capacityCallback func()
	removedCallback  RemovedCallback
	// credentialHealthCallback reports the claude ACP credential health
	// verdict at session creation (engine-explicit-auth). The bridge maps
	// Dead/ExpiringSoon to an auth_required pre-warning; Missing/Healthy are
	// never reported. Set once at startup, read on creation paths.
	credentialHealthCallback func(sessionID string, h CredentialHealth)
	maxConcurrent            int
	// envResolver supplies the per-session engine-profile env merged into
	// AdapterConfig.CustomEnv at creation. Never applied to the bridge
	// process environment, so two sessions can carry two identities.
	envResolver func(sessionID string) map[string]string
	// testHookTaskRegistered, when set, runs right after a task session is
	// registered (lock released) — lets tests hold the slot deterministically.
	testHookTaskRegistered func()
	queue                  []QueueItem
	queueMu                sync.Mutex
	ioLogger               *logger.IOLogger // I/O logger for debugging and auditing
	// replayDir, when non-empty, mirrors every ACP session's raw wire
	// frames to <replayDir>/<sessionID>.jsonl (G17 replay recording).
	replayDir string
}

type QueueItem struct {
	CLIType   string
	WorkDir   string
	SessionID string
	// JobID pairs with SessionID (the task ID) so a drained task keeps its
	// completion reporting: the exit callback reports results per job/task.
	JobID string
	// Attempt is the dispatch generation (G19) from the task_assign payload;
	// carried through the queue so a drained task's terminal callback echoes
	// the generation that dispatched it, not a stale one.
	Attempt    int
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

	// Multi-agent task metadata. Guarded by metaMu: the session-creation
	// status message fires the output callback chain before
	// launchTaskSession gets to SetMultiAgentMetadata, so JobID/TaskID are
	// written and read from different goroutines (found by the G17 replay
	// suite under -race).
	JobID     string    // Associated multi-agent job ID (if any)
	TaskID    string    // Associated multi-agent task ID (if any)
	StartedAt time.Time // Task start time for duration tracking
	metaMu    sync.RWMutex
	Output    []byte // Collected CLI output for artifacts extraction
	ExitCode  int    // Process exit code (set when session exits)

	// Resume context: prompt prefix injected on first user message
	ResumeContext string

	// connecting is true while CreateWithIDAndSize is still running
	// Connect() — the session is registered in the map early, so its
	// protocol is not connected YET. Reconnect-path cleanup must not
	// mistake it for a dead session. Guarded by Manager.mu.
	connecting bool
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

// ErrTaskPoolFull is returned by StartTaskSession when the task pool has no
// free slot.
var ErrTaskPoolFull = errors.New("task pool full")

type taskSpec struct{ jobID string }

// ActiveTaskCount counts active workflow task sessions (task ID set). Only
// these occupy the task pool: interactive chats must not starve missions.
// ActiveCount stays the all-session figure for health metrics.
func (m *Manager) ActiveTaskCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeTaskCountLocked("")
}

// activeTaskCountLocked requires m.mu held. excludeID skips a session about to
// be replaced by the caller.
func (m *Manager) activeTaskCountLocked(excludeID string) int {
	count := 0
	for id, s := range m.sessions {
		if id == excludeID || s.Status != "active" {
			continue
		}
		if s.IsTaskSession() {
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

// SetCapacityCallback registers a hook fired after every session close —
// the bridge drains its task queue there (a freed pool slot may fit a
// queued task). Without a drain trigger, pool-full enqueues were never
// started (DequeueNext had no caller).
func (m *Manager) SetCapacityCallback(callback func()) {
	m.capacityCallback = callback
}

func (m *Manager) SetExitCallback(callback ExitCallback) {
	m.exitCallback = callback
}

// SetEnvResolver registers the per-session profile env source (engine-profiles).
func (m *Manager) SetEnvResolver(fn func(sessionID string) map[string]string) {
	m.mu.Lock()
	m.envResolver = fn
	m.mu.Unlock()
}

// mergeProfileEnv adds profile env to cfg.CustomEnv. Keys the system already
// set (e.g. CLAUDE_CONFIG_DIR, CLAUDECODE) win; a conflict is warned by key
// name only, never the value.
func mergeProfileEnv(cfg *protocol.AdapterConfig, extra map[string]string, sessionID string) {
	if len(extra) == 0 {
		return
	}
	if cfg.CustomEnv == nil {
		cfg.CustomEnv = map[string]string{}
	}
	for k, v := range extra {
		if _, taken := cfg.CustomEnv[k]; taken {
			logger.Warn("[%s] Profile env key %s ignored for session %s: set by the system", logger.ModSession, k, sessionID)
			continue
		}
		cfg.CustomEnv[k] = v
	}
}

// SetRemovedCallback registers the session-removal notification. Fired
// asynchronously from every delete site — safe to re-enter the manager.
func (m *Manager) SetRemovedCallback(callback RemovedCallback) {
	m.removedCallback = callback
}

// SetCredentialHealthCallback registers the creation-time claude credential
// verdict hook (engine-explicit-auth). Fired inline from the creation path,
// after the session is registered — the callback must not block.
func (m *Manager) SetCredentialHealthCallback(callback func(sessionID string, h CredentialHealth)) {
	m.credentialHealthCallback = callback
}

// notifyRemoved fires the removal callback off the lock. Callers hold m.mu
// at the delete sites, so the callback must never block on the manager.
func (m *Manager) notifyRemoved(sessionID string) {
	if m.removedCallback != nil {
		go m.removedCallback(sessionID)
	}
}

// SetIOLogger sets the I/O logger for the session manager
func (m *Manager) SetIOLogger(ioLogger *logger.IOLogger) {
	m.ioLogger = ioLogger
}

// SetReplayDir enables ACP wire-frame recording into dir (one JSONL script
// per session, named <sessionID>.jsonl). An empty dir keeps recording off —
// the default and the production state.
func (m *Manager) SetReplayDir(dir string) {
	m.replayDir = dir
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
				m.notifyRemoved(id)
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

// claudeACPModes maps bridge permission modes to ACP mode ids.
var claudeACPModes = map[string]string{
	"default":      "default",
	"accept-edits": "acceptEdits",
	"accept-all":   "bypassPermissions",
	"plan":         "plan",
}

// claudeACPModeID returns the ACP mode id for a bridge permission mode.
// Unknown modes map to "default" (the strictest usable mode).
func claudeACPModeID(permissionMode string) string {
	if id, ok := claudeACPModes[permissionMode]; ok {
		return id
	}
	return "default"
}

// ClaudeACPModeID is the exported form of claudeACPModeID.
func ClaudeACPModeID(permissionMode string) string { return claudeACPModeID(permissionMode) }

// BridgeModeFromACP maps an ACP mode id back to the bridge permission mode.
func BridgeModeFromACP(modeID string) (string, bool) {
	for bridgeMode, id := range claudeACPModes {
		if id == modeID {
			return bridgeMode, true
		}
	}
	return "", false
}

const claudeSharedMigratedMarker = ".migrated-from-mode-dirs"

// claudeACPSharedDir returns the single isolated CLAUDE_CONFIG_DIR shared by
// every permission mode (idempotent). Its settings.json is "{}": the mode is
// delivered only through ACP session/set_mode, never through settings, so
// switching mode neither splits the login state nor depends on host settings.
// Isolation from the host's ~/.claude/settings.json is preserved (the host's
// defaultMode/allow rules must not leak into a managed agent).
//
// On first use the legacy per-mode directories are COPIED in (never deleted or
// modified), newest file wins on name clashes.
func claudeACPSharedDir() (string, error) {
	root := filepath.Join(configpkg.ConfigDir(), "claude-acp-config")
	dir := filepath.Join(root, "shared")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create claude ACP shared dir: %w", err)
	}
	settings := []byte("{}\n")
	settingsPath := filepath.Join(dir, "settings.json")
	if existing, err := os.ReadFile(settingsPath); err != nil || !bytes.Equal(existing, settings) {
		if err := os.WriteFile(settingsPath, settings, 0o644); err != nil {
			return "", fmt.Errorf("write claude ACP settings: %w", err)
		}
	}
	marker := filepath.Join(dir, claudeSharedMigratedMarker)
	if _, err := os.Stat(marker); err != nil {
		migrateLegacyModeDirs(root, dir)
		if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
			logger.Warn("[%s] write migration marker: %v", logger.ModSession, err)
		}
	}
	return dir, nil
}

// migrateLegacyModeDirs copies credentials and conversation state from the old
// per-mode directories. Best effort: a failed copy is logged, never fatal.
func migrateLegacyModeDirs(root, dst string) {
	for _, mode := range []string{"default", "acceptEdits", "bypassPermissions", "plan"} {
		src := filepath.Join(root, mode)
		if st, err := os.Stat(src); err != nil || !st.IsDir() {
			continue
		}
		for _, name := range []string{".credentials.json", ".claude.json", "plugins", "projects"} {
			if err := copyNewest(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
				logger.Warn("[%s] migrate %s/%s: %v", logger.ModSession, mode, name, err)
			}
		}
	}
}

// copyNewest copies src (file or tree) to dst; a file already at dst is
// replaced only when src is newer.
func copyNewest(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return nil // nothing to migrate
	}
	if st.IsDir() {
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyNewest(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if dstSt, err := os.Stat(dst); err == nil && !st.ModTime().After(dstSt.ModTime()) {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, st.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, st.ModTime(), st.ModTime())
}

// applyPermissionMode configures the adapter based on permission mode
func (m *Manager) applyPermissionMode(permissionMode, cliType string, config *protocol.AdapterConfig) {
	logger.Debug("[%s] Applying permission mode: %s for CLI: %s", logger.ModSession, permissionMode, cliType)

	// Initialize CustomEnv if nil
	if config.CustomEnv == nil {
		config.CustomEnv = make(map[string]string)
	}

	// claude ACP: see claudeACPSharedDir — the shared dir isolates from the host's
	// settings for EVERY mode; the mode itself goes through ACP set_mode.
	if cliType == "claude" {
		dir, err := claudeACPSharedDir()
		if err != nil {
			// Deliberately not fatal: the session still runs, just with the
			// adapter's own defaults instead of the requested mode.
			logger.Warn("[%s] claude ACP permission isolation failed, mode %s may not apply: %v", logger.ModSession, permissionMode, err)
			return
		}
		config.CustomEnv["CLAUDE_CONFIG_DIR"] = dir
		config.ACPModeID = claudeACPModeID(permissionMode)
		return
	}

	switch permissionMode {
	case "accept-all":
		// Auto-accept all operations
		switch cliType {
		case "claude-pty":
			// PTY runs the real claude REPL: its CLI flags are honored (unlike
			// the ACP adapter, which ignores args entirely).
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
		case "claude-pty":
			// The REPL has no accept-edits flag; its settings file is the
			// channel, but the PTY path deliberately stays minimal — edit
			// approvals surface as interactive prompts instead.
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
		case "claude-pty":
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

func (m *Manager) getCLICommand(cliType string) (string, []string, error) {
	switch cliType {
	case "claude":
		// Claude Code ACP via npx (package renamed from @zed-industries/claude-code-acp)
		return "npx", []string{"@agentclientprotocol/claude-agent-acp"}, nil
	case "claude-pty":
		// Claude Code PTY mode - full REPL with slash commands support
		return "claude", nil, nil
	case "qwen":
		return "qwen-code", []string{"--experimental-acp"}, nil
	case "goose":
		return "goose", []string{"acp"}, nil
	case "gemini":
		return "gemini-cli", []string{"--acp"}, nil
	case "opencode":
		// OpenCode (opencode.ai) ships a native ACP server as a subcommand
		return "opencode", []string{"acp"}, nil
	case "dsh":
		// DeepSeek Harness ACP server. The published launcher has no built-in
		// acp profile; users create it once with
		// `dsh plugin --profile acp add @deepseek-ai/dsh-acp`. A missing
		// profile exits non-zero with that same fix-it hint, which fails loud
		// up the session error channel — no detection probe needed here.
		return "dsh", []string{"--profile", "acp"}, nil
	case "cline":
		return "cline", nil, nil
	case "codex":
		return "codex", nil, nil
	case "aider":
		// Aider - AI pair programming in terminal (PTY mode)
		// Installation: pip install aider-chat
		// Uses its own protocol, not ACP
		return "aider", []string{"--no-auto-commits", "--pretty"}, nil
	case "replay":
		// G17 replay shim: the command comes from the environment so
		// production code never references the test binary. The shim reads
		// OA_REPLAY_SCRIPT itself. Missing env is a hard error — a
		// production dispatch with agent="replay" must fail loudly here,
		// not exec an empty command (spec scenario "Missing shim command
		// fails loud").
		cmd := os.Getenv("OA_REPLAY_SHIM")
		if cmd == "" {
			return "", nil, fmt.Errorf("cliType %q requires OA_REPLAY_SHIM to point at the replay shim executable", cliType)
		}
		var args []string
		if extra := os.Getenv("OA_REPLAY_SHIM_ARGS"); extra != "" {
			args = strings.Fields(extra)
		}
		return cmd, args, nil
	default:
		return cliType, nil, nil
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
	jobID, taskID, _ := sess.GetMultiAgentMetadata()

	delete(m.sessions, id)
	m.notifyRemoved(id)

	// A pool slot just freed — let the bridge drain queued tasks.
	if m.capacityCallback != nil {
		go m.capacityCallback()
	}

	// Call exit callback if set and this is a multi-agent task
	if m.exitCallback != nil && jobID != "" && taskID != "" {
		go m.exitCallback(id, exitCode, output)
	}

	return nil
}

func (m *Manager) StopAll() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.sessions))
	for id, sess := range m.sessions {
		ids = append(ids, id)
		if sess.Protocol != nil {
			sess.Protocol.Disconnect()
		}
	}
	m.sessions = make(map[string]*Session)
	return ids
}

// StopDead terminates only sessions that can no longer run — no protocol, or
// a disconnected adapter — and returns their IDs. Live sessions stay
// registered: a lost WebSocket only breaks the forwarding channel, not the
// local CLI processes, so running task sessions keep producing output after
// the reconnect (2026-09-22 prod: the unconditional StopAll on reconnect
// killed running task sessions and zombied platform tasks until stuck
// recovery re-dispatched). Sessions still mid-creation (connecting) are left
// alone — their protocol is not connected yet. StopAll remains the
// process-shutdown path.
func (m *Manager) StopDead() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make([]string, 0, len(m.sessions))
	for id, sess := range m.sessions {
		if sess.connecting {
			continue
		}
		if sess.Protocol != nil && sess.Protocol.IsConnected() {
			continue
		}
		ids = append(ids, id)
		if sess.Protocol != nil {
			sess.Protocol.Disconnect()
		}
		delete(m.sessions, id)
	}
	return ids
}

func (s *Session) Send(input string) error {
	logger.Debug("[%s] Send called for session %s, input: %q, Protocol nil: %v", logger.ModSession, s.ID, input, s.Protocol == nil)

	// Log user input if I/O logging is enabled
	if s.ioLogger != nil && s.ioLogger.ShouldLog("prompt") {
		s.ioLogger.Log(s.ID, "input", "prompt", input)
	}

	if s.Protocol == nil {
		logger.Error("[%s] Protocol is nil for session %s", logger.ModSession, s.ID)
		return fmt.Errorf("protocol not initialized for session %s", s.ID)
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
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	s.JobID = jobID
	s.TaskID = taskID
	s.StartedAt = time.Now()
}

// GetMultiAgentMetadata returns the multi-agent task metadata
func (s *Session) GetMultiAgentMetadata() (jobID, taskID string, startedAt time.Time) {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	return s.JobID, s.TaskID, s.StartedAt
}

// IsTaskSession reports whether this session is a workflow task session —
// the gate for turn-end termination (taskTurnExitCode).
func (s *Session) IsTaskSession() bool {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	return s.JobID != "" && s.TaskID != ""
}

func (s *Session) Resize(cols, rows int) error {
	if s.Protocol == nil {
		return fmt.Errorf("protocol not initialized for session %s", s.ID)
	}
	adapter := s.Protocol.GetAdapter()
	if adapter == nil {
		return fmt.Errorf("adapter not available for session %s", s.ID)
	}
	return adapter.Resize(cols, rows)
}

func (m *Manager) Resize(id string, cols, rows int) error {
	m.mu.RLock()
	sess, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session %s not found", id)
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

// UpdateWorkDir updates the working directory for a session
func (m *Manager) UpdateWorkDir(sessionID, newDir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		return fmt.Errorf("session not found")
	}
	sess.WorkDir = newDir
	return nil
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
