package session

import (
	"time"

	"github.com/google/uuid"
	"github.com/open-agents/open-agents-bridge/internal/logger"
	"github.com/open-agents/open-agents-bridge/internal/protocol"
)

// CreateWithIDAndSize creates a new session with a specific ID and terminal size.
// The session is registered in the manager's map under a short lock, and the
// potentially slow Connect() call runs outside the lock to avoid blocking
// other session operations (Get, Stop, etc.) for up to 60 seconds.
func (m *Manager) CreateWithIDAndSize(cliType, workDir, sessionID string, cols, rows int, permissionMode string) (*Session, error) {
	// Use provided sessionID or generate a new one
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	if permissionMode == "" {
		permissionMode = "default"
	}

	// --- Phase 1: map operations under lock (fast) ---
	m.mu.Lock()

	if existingSess, exists := m.sessions[sessionID]; exists {
		logger.Info("[%s] Session %s already exists, attempting recovery...", logger.ModSession, sessionID)
		logger.Debug("[%s] Existing: cliType=%s, workDir=%s, status=%s, created=%v",
			logger.ModSession, existingSess.CLIType, existingSess.WorkDir, existingSess.Status, existingSess.CreatedAt)
		logger.Debug("[%s] New request: cliType=%s, workDir=%s, permMode=%s",
			logger.ModSession, cliType, workDir, permissionMode)

		if m.canResumeSession(existingSess, cliType, workDir) {
			logger.Info("[%s] RESUMING existing session %s", logger.ModSession, sessionID)
			logger.Debug("[%s] Protocol: %s (still connected)", logger.ModSession, existingSess.Protocol.GetProtocolName())
			logger.Debug("[%s] Status: %s", logger.ModSession, existingSess.Status)
			logger.Debug("[%s] History preserved!", logger.ModSession)
			existingSess.PermissionMode = permissionMode
			existingSess.LastActiveAt = time.Now()
			m.mu.Unlock()
			return existingSess, nil
		}

		if existingSess.Status == "active" && existingSess.Protocol != nil && !existingSess.Protocol.IsConnected() {
			m.mu.Unlock()
			logger.Info("[%s] Attempting to reconnect session %s", logger.ModSession, sessionID)
			if err := existingSess.Protocol.Reconnect(existingSess.Config); err == nil {
				logger.Info("[%s] Successfully reconnected session %s", logger.ModSession, sessionID)
				existingSess.LastActiveAt = time.Now()
				return existingSess, nil
			}
			logger.Warn("[%s] Reconnection failed for session %s", logger.ModSession, sessionID)
			m.mu.Lock()
		}

		logger.Warn("[%s] Cannot resume session %s, replacing it", logger.ModSession, sessionID)
		logger.Warn("[%s] Reason: Protocol disconnected or incompatible parameters", logger.ModSession)
		if existingSess.Protocol != nil {
			logger.Debug("[%s] Disconnecting old protocol connection", logger.ModSession)
			existingSess.Protocol.Disconnect()
		}
		existingSess.Status = "replaced"
		delete(m.sessions, sessionID)
		logger.Info("[%s] Old session cleaned up and removed", logger.ModSession)
	} else {
		logger.Info("[%s] Creating new session: ID=%s, cliType=%s, workDir=%s",
			logger.ModSession, sessionID, cliType, workDir)
	}

	protocolMgr := protocol.NewManager()
	sess := &Session{
		ID:             sessionID,
		CLIType:        cliType,
		WorkDir:        workDir,
		PermissionMode: permissionMode,
		Status:         "active",
		Protocol:       protocolMgr,
		CreatedAt:      time.Now(),
		ioLogger:       m.ioLogger,
	}

	logger.Debug("[%s] Setting up message callback for session %s", logger.ModSession, sessionID)
	protocolMgr.Subscribe(func(msg protocol.Message) {
		logger.Debug("[%s] Message received: type=%s", logger.ModSession, msg.Type)
		if sess.ioLogger != nil {
			switch msg.Type {
			case protocol.MessageTypeContent:
				if sess.ioLogger.ShouldLog("agent_message") {
					sess.ioLogger.Log(sess.ID, "output", "agent_message", msg.Content)
				}
			case protocol.MessageTypeThought:
				if sess.ioLogger.ShouldLog("agent_thought") {
					sess.ioLogger.Log(sess.ID, "output", "agent_thought", msg.Content)
				}
			case protocol.MessageTypeToolCall:
				if sess.ioLogger.ShouldLog("tool_call") {
					sess.ioLogger.Log(sess.ID, "output", "tool_call", msg.Content)
				}
			}
		}
		if sess.JobID != "" && msg.Type == protocol.MessageTypeContent {
			if content, ok := msg.Content.(string); ok {
				sess.Output = append(sess.Output, []byte(content)...)
			}
		}
		if m.outputCallback != nil {
			m.outputCallback(sess.ID, msg)
		}
	})

	// Register session to map EARLY so it's visible while Connect() is in progress
	m.sessions[sess.ID] = sess
	m.mu.Unlock()
	// --- End of locked phase ---

	// --- Phase 2: Connect() outside lock (may block up to 60s for ACP handshake) ---
	command, args := m.getCLICommand(cliType)
	config := protocol.AdapterConfig{
		WorkDir: workDir,
		Command: command,
		Args:    args,
		Cols:    cols,
		Rows:    rows,
	}
	if cliType == "claude" {
		config.CustomEnv = map[string]string{"CLAUDECODE": ""}
	}
	if cliType == "claude-pty" {
		config.CustomEnv = map[string]string{"CLAUDECODE": ""}
		config.ForceProtocol = "pty"
	}
	m.applyPermissionMode(permissionMode, cliType, &config)

	if err := protocolMgr.Connect(config); err != nil {
		// Connect failed — remove from map
		m.mu.Lock()
		delete(m.sessions, sess.ID)
		m.mu.Unlock()
		return nil, err
	}

	sess.Config = config
	sess.LastActiveAt = time.Now()

	logger.Info("[%s] Session %s connected using protocol: %s", logger.ModSession, sessionID, protocolMgr.GetProtocolName())
	logger.Debug("[%s] Config stored for reconnection capability", logger.ModSession)
	logger.Info("[%s] Session created successfully", logger.ModSession)
	logger.Debug("[%s] ID: %s", logger.ModSession, sessionID)
	logger.Debug("[%s] CLI Type: %s", logger.ModSession, cliType)
	logger.Debug("[%s] Protocol: %s", logger.ModSession, protocolMgr.GetProtocolName())
	return sess, nil
}
