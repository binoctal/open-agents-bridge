package bridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-agents/open-agents-bridge/internal/alert"
	"github.com/open-agents/open-agents-bridge/internal/api"
	"github.com/open-agents/open-agents-bridge/internal/config"
	"github.com/open-agents/open-agents-bridge/internal/crypto"
	"github.com/open-agents/open-agents-bridge/internal/logger"
	"github.com/open-agents/open-agents-bridge/internal/loopdetect"
	mcpPkg "github.com/open-agents/open-agents-bridge/internal/mcp"
	"github.com/open-agents/open-agents-bridge/internal/metrics"
	"github.com/open-agents/open-agents-bridge/internal/workflows"
	"github.com/open-agents/open-agents-bridge/internal/permission"
	"github.com/open-agents/open-agents-bridge/internal/protocol"
	"github.com/open-agents/open-agents-bridge/internal/reconnect"
	"github.com/open-agents/open-agents-bridge/internal/rules"
	"github.com/open-agents/open-agents-bridge/internal/scanner"
	"github.com/open-agents/open-agents-bridge/internal/session"
	"github.com/open-agents/open-agents-bridge/internal/storage"
)

// logDebug logs debug messages
func (b *Bridge) logDebug(format string, args ...interface{}) {
	logger.Debug(format, args...)
}

// logInfo logs info messages
func (b *Bridge) logInfo(format string, args ...interface{}) {
	logger.Info(format, args...)
}

// logWarn logs warnings
func (b *Bridge) logWarn(format string, args ...interface{}) {
	logger.Warn(format, args...)
}

// logError logs errors
func (b *Bridge) logError(format string, args ...interface{}) {
	logger.Error(format, args...)
}

// Message batching constants
const (
	batchFlushInterval = 500 * time.Millisecond
	maxBatchSize       = 4096 // max bytes per merged message
	offlineMaxMessages = 1000 // max offline buffered messages
)

// contentBatch accumulates small content chunks for a session
type contentBatch struct {
	chunks       []string
	sessionID    string
	protocolName string
	msgType      protocol.MessageType
}

type Bridge struct {
	config            *config.Config
	conn              *websocket.Conn
	sessions          *session.Manager
	permServer        *permission.Server
	permHandler       *permission.Handler
	store             *storage.Store
	s3Uploader        *storage.S3Uploader
	rulesEngine       *rules.Engine
	apiClient         *api.Client
	keyPair           *crypto.KeyPair
	webPubKey         *[crypto.KeySize]byte
	done              chan struct{}
	connMu            sync.Mutex // protects conn read/write and conn lifecycle only
	mu                sync.Mutex // protects other shared state (keyPair, webPubKey, etc.)
	mcpManager        *mcpPkg.Manager
	scanner           *scanner.Scanner
	loopDetectors     map[string]*loopdetect.Detector
	callbackManager   *workflows.CallbackManager
	worktreeManager   *workflows.WorktreeManager
	reconnectStrategy *reconnect.Strategy
	stateManager      *StateManager
	reconnectCallback *reconnect.CallbackManager
	reconnectMetrics  *reconnect.Metrics
	ioLogger          *logger.IOLogger // I/O logger for debugging and auditing

	// Permission ID -> Session ID mapping for precise routing
	permSessionMap map[string]string
	permSessionMu  sync.RWMutex

	// Pending questions for human-in-the-loop (taskId -> answer channel)
	pendingQuestions   map[string]chan string
	pendingQuestionsMu sync.RWMutex

	// Task metadata for workflow tasks (sessionID -> taskMeta)
	taskMeta   map[string]*taskMeta
	taskMetaMu sync.RWMutex

	// Message queue for ordered processing without blocking readLoop
	messageQueue chan Message

	// HTTP client for API requests (reused to avoid connection reset issues)
	httpClient *http.Client

	// Heartbeat failure tracking for dead-connection detection
	heartbeatFailures int

	// Message batching (Scheme 1: merge small content chunks)
	batchMu    sync.Mutex
	batchBuf   map[string]*contentBatch // sessionID → pending chunks
	batchTimer *time.Timer
	batchWait  sync.WaitGroup // wait for flush on shutdown

	// Offline message buffer (Scheme 3: buffer when disconnected)
	offlineMu  sync.Mutex
	offlineBuf []Message

	// Ring buffer for message replay after reconnection
	msgBuffer *MessageBuffer

	// Keep-alive data frame for preventing proxy idle timeout
	keepAliveDone chan struct{}
	lastSendTime  int64 // unix milliseconds of last business message send
}

// taskMeta stores metadata for workflow tasks
type taskMeta struct {
	JobID    string
	TaskID   string
	Title    string
	WorkDir  string
	Worktree bool
}

func New(cfg *config.Config) (*Bridge, error) {
	handler := permission.NewHandler()

	// Initialize storage
	storeDir := filepath.Join(config.ConfigDir(), "sessions")
	store, _ := storage.NewStore(storeDir)

	// Initialize session manager
	sessionMgr := session.NewManager()

	// Initialize I/O logger if configured
	var ioLogger *logger.IOLogger
	if cfg.IOLogging != nil && cfg.IOLogging.Enabled {
		ioLoggerCfg := &logger.IOLoggerConfig{
			Enabled:    cfg.IOLogging.Enabled,
			Types:      cfg.IOLogging.Types,
			MaxSizeMB:  cfg.IOLogging.MaxSizeMB,
			MaxBackups: cfg.IOLogging.MaxBackups,
		}
		var err error
		ioLogger, err = logger.NewIOLogger(ioLoggerCfg)
		if err != nil {
			logger.Warn("Failed to initialize I/O logger: %v", err)
		} else {
			sessionMgr.SetIOLogger(ioLogger)
			logger.Info("I/O logging enabled with types: %v", cfg.IOLogging.Types)
		}
	}

	b := &Bridge{
		config:            cfg,
		sessions:          sessionMgr,
		permHandler:       handler,
		permServer:        permission.NewServer(handler),
		store:             store,
		rulesEngine:       rules.NewEngine(cfg.Rules),
		apiClient:         api.NewClient(cfg),
		done:              make(chan struct{}),
		scanner:           scanner.New(),
		loopDetectors:     make(map[string]*loopdetect.Detector),
		permSessionMap:    make(map[string]string),
		pendingQuestions:  make(map[string]chan string),
		taskMeta:          make(map[string]*taskMeta),
		reconnectStrategy: reconnect.NewStrategy(),
		stateManager:      NewStateManager(),
		reconnectCallback: reconnect.NewCallbackManager(),
		reconnectMetrics:  reconnect.NewMetrics(),
		messageQueue:      make(chan Message, 100), // Buffered queue for ordered processing
		ioLogger:          ioLogger,
		worktreeManager:   workflows.NewWorktreeManager("."),
		batchBuf:          make(map[string]*contentBatch),
		offlineBuf:        nil,
		msgBuffer:         NewMessageBuffer(DefaultBufferCapacity),
		keepAliveDone:     make(chan struct{}),
	}

	// Apply scanner config
	if cfg.ScannerEnabled != nil {
		b.scanner.SetEnabled(*cfg.ScannerEnabled)
	}
	b.scanner.LoadCustomRules(config.ConfigDir())

	// Initialize S3 uploader if configured
	if cfg.S3Config != nil {
		b.s3Uploader = storage.NewS3Uploader(cfg.S3Config)
	}

	// Initialize MCP manager
	b.mcpManager = mcpPkg.NewManager(config.ConfigDir())

	// Load E2EE keys if available
	if cfg.PrivateKey != "" {
		privBytes, err := base64.StdEncoding.DecodeString(cfg.PrivateKey)
		if err == nil && len(privBytes) == crypto.KeySize {
			pubBytes, _ := base64.StdEncoding.DecodeString(cfg.PublicKey)
			b.keyPair = &crypto.KeyPair{}
			copy(b.keyPair.PrivateKey[:], privBytes)
			copy(b.keyPair.PublicKey[:], pubBytes)
			logger.Info("E2EE: Keys loaded")
		}
	}

	if cfg.WebPubKey != "" {
		b.webPubKey, _ = crypto.PublicKeyFromBase64(cfg.WebPubKey)
	}

	// Initialize HTTP client with connection pooling
	b.httpClient = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false, // Enable keep-alive
		},
	}

	// Initialize metrics
	metrics.Init(cfg.DeviceID, "1.0.0")

	// Initialize alert system
	alert.Init(alert.Config{
		Enabled:   true,
		Cooldown:  5 * time.Minute,
		MaxAlerts: 100,
	})

	// Register health checks
	metrics.RegisterHealthCheck("memory", metrics.MemoryHealthChecker(1024)) // 1GB max
	metrics.RegisterHealthCheck("goroutines", metrics.GoroutineHealthChecker(1000))
	metrics.RegisterHealthCheck("websocket", metrics.WebSocketHealthChecker(func() bool {
		b.connMu.Lock()
		connected := b.conn != nil
		b.connMu.Unlock()
		return connected
	}))

	// ✅ Start session cleanup worker
	// Clean up inactive sessions every 5 minutes, remove sessions idle for >30 minutes
	b.sessions.StartCleanupWorker(5*time.Minute, 30*time.Minute)
	logger.Info("Session cleanup worker started (interval: 5m, max idle: 30m)")

	// Auto-detect installed CLI tools
	cfg.CLIDetected = config.DetectInstalledCLIs()

	// If cliEnabled is empty, auto-enable all detected CLIs
	if cfg.CLIEnabled == nil || len(cfg.CLIEnabled) == 0 {
		cfg.CLIEnabled = make(map[string]bool, len(cfg.CLIDetected))
		for cli, installed := range cfg.CLIDetected {
			if installed {
				cfg.CLIEnabled[cli] = true
			}
		}
	}

	return b, nil
}

func (b *Bridge) Start() error {
	// Start permission server
	if err := b.permServer.Start(); err != nil {
		b.logWarn("[%s] Could not start permission server: %v", logger.ModPermission, err)
	}

	// Sync rules from API on startup
	go b.syncRulesFromAPI()

	// Set up permission request forwarding with rules engine
	b.permHandler.OnRequest(func(req permission.Request) {
		req.DeviceID = b.config.DeviceID

		// Check auto-approval rules
		path := ""
		command := ""
		if req.Detail != nil {
			if p, ok := req.Detail["path"].(string); ok {
				path = p
			}
			if c, ok := req.Detail["command"].(string); ok {
				command = c
			}
		}

		action, ruleID := b.rulesEngine.Evaluate(req.PermissionType, path, command)

		switch action {
		case "auto-approve":
			b.logInfo("[%s] Auto-approved by rule %s: %s", logger.ModPermission, ruleID, req.Description)
			b.permHandler.Resolve(permission.Response{ID: req.ID, Approved: true})
			return
		case "deny":
			b.logInfo("[%s] Auto-denied by rule %s: %s", logger.ModPermission, ruleID, req.Description)
			b.permHandler.Resolve(permission.Response{ID: req.ID, Approved: false})
			return
		}

		// Default: forward to Web for user decision
		b.sendMessage(Message{
			Type:      "permission:request",
			Payload:   req,
			Timestamp: time.Now().UnixMilli(),
		})
	})

	// Set up session output forwarding
	// NOTE: outputCallback is called from ACP's readMessages goroutine.
	// All sendMessage calls are dispatched asynchronously to avoid deadlock
	// between logger mutex and bridge mutex across goroutines.
	b.sessions.SetOutputCallback(func(sessionID string, msg protocol.Message) {
		go b.forwardSessionOutput(sessionID, msg)
	})
	b.sessions.SetExitCallback(func(sessionID string, exitCode int, output []byte) {
		go b.handleSessionExit(sessionID, exitCode, output)
	})
	if err := b.connect(); err != nil {
		return err
	}

	// Clean up stale worktrees from previous sessions
	if b.worktreeManager != nil {
		cleaned, err := b.worktreeManager.CleanupStaleWorktrees(nil)
		if err != nil {
			b.logWarn("[%s] Failed to clean stale worktrees: %v", logger.ModWorkflow, err)
		} else if len(cleaned) > 0 {
			b.logInfo("[%s] Cleaned %d stale worktrees: %v", logger.ModWorkflow, len(cleaned), cleaned)
		}
	}

	// Note: device:online message is sent by the server (room.ts) when bridge connects
	// No need to send it here to avoid duplicate notifications

	b.logInfo("[%s] Starting goroutines...", logger.ModBridge)

	// Start message worker for ordered processing
	b.logInfo("[%s] Launching messageWorker goroutine...", logger.ModBridge)
	go b.messageWorker()

	// Start message handler
	b.logInfo("[%s] Launching readLoop goroutine...", logger.ModBridge)
	go b.readLoop()

	// Start heartbeat
	b.logInfo("[%s] Launching heartbeat goroutine...", logger.ModBridge)
	go b.heartbeat()

	// Start keep-alive data frame loop
	b.logInfo("[%s] Launching keepAliveLoop goroutine...", logger.ModBridge)
	go b.keepAliveLoop()

	b.logInfo("[%s] All goroutines started, entering main loop", logger.ModBridge)

	// Wait for shutdown
	<-b.done
	b.logInfo("[%s] Shutdown signal received, stopping...", logger.ModBridge)
	return nil
}

func (b *Bridge) Stop() {
	// Stop keep-alive loop
	b.stopKeepAlive()

	// Flush remaining batched messages before shutdown
	b.batchMu.Lock()
	if b.batchTimer != nil {
		b.batchTimer.Stop()
		b.doFlushLocked()
		b.batchTimer = nil
	}
	b.batchMu.Unlock()
	b.batchWait.Wait()

	close(b.done)
	b.sessions.StopAll()
	b.permServer.Stop()
	b.connMu.Lock()
	if b.conn != nil {
		b.conn.Close()
	}
	b.connMu.Unlock()

	// Close I/O logger to flush remaining entries
	if b.ioLogger != nil {
		b.ioLogger.Close()
	}
}

func (b *Bridge) connect() error {
	u, err := url.Parse(b.config.ServerURL)
	if err != nil {
		return err
	}

	// Add connection parameters
	q := u.Query()
	q.Set("type", "bridge")
	q.Set("deviceId", b.config.DeviceID)
	q.Set("token", b.config.DeviceToken)
	// Report CLI capabilities so the server knows which agents are available
	if len(b.config.CLIEnabled) > 0 {
		cliNames := make([]string, 0, len(b.config.CLIEnabled))
		for cli, enabled := range b.config.CLIEnabled {
			if enabled {
				cliNames = append(cliNames, cli)
			}
		}
		if len(cliNames) > 0 {
			q.Set("cliEnabled", strings.Join(cliNames, ","))
		}
	}
	// Report auto-detected installed CLIs
	if len(b.config.CLIDetected) > 0 {
		detected := make([]string, 0, len(b.config.CLIDetected))
		for cli, installed := range b.config.CLIDetected {
			if installed {
				detected = append(detected, cli)
			}
		}
		if len(detected) > 0 {
			q.Set("cliDetected", strings.Join(detected, ","))
		}
	}
	u.RawQuery = q.Encode()
	u.Path = fmt.Sprintf("/ws/%s", b.config.UserID)

	b.logInfo("[%s] Connecting to %s", logger.ModBridge, u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		b.logInfo("[%s] Could not connect to server: %v", logger.ModBridge, err)
		return err
	}

	b.connMu.Lock()
	b.conn = conn
	b.connMu.Unlock()

	// Reset heartbeat failure counter on new connection
	b.heartbeatFailures = 0

	b.logInfo("[%s] Connected to server successfully", logger.ModBridge)
	return nil
}

func (b *Bridge) readLoop() {
	for {
		select {
		case <-b.done:
			return
		default:
		}

		// If not connected, try to connect with exponential backoff
		if b.conn == nil {
			if b.reconnectStrategy.HasExhaustedBudget() {
				b.logError("[%s] Reconnect time budget exhausted (%v), giving up", logger.ModBridge, b.reconnectStrategy.TimeBudget())
				b.stateManager.SetState(StateFailed, "time_budget_exhausted")
				b.reconnectCallback.Notify(reconnect.Event{
					Type:      reconnect.EventMaxRetry,
					Attempts:  b.reconnectStrategy.Attempts(),
					Timestamp: time.Now(),
					Layer:     "websocket",
				})
				return
			}

			delay := b.reconnectStrategy.NextDelay()
			if delay > 0 {
				b.logInfo("[%s] Waiting %v before reconnection attempt %d",
					logger.ModBridge, delay, b.reconnectStrategy.Attempts())
				time.Sleep(delay)
			}

			b.stateManager.SetState(StateConnecting, "reconnect_attempt")
			b.reconnectCallback.Notify(reconnect.Event{
				Type:      reconnect.EventStarted,
				Attempts:  b.reconnectStrategy.Attempts(),
				Timestamp: time.Now(),
				Layer:     "websocket",
			})

			startTime := time.Now()
			if err := b.connect(); err != nil {
				elapsed := time.Since(startTime)
				b.logInfo("[%s] Connection failed (attempt %d): %v",
					logger.ModBridge, b.reconnectStrategy.Attempts(), err)
				b.reconnectMetrics.RecordAttempt(false, elapsed)
				b.reconnectCallback.Notify(reconnect.Event{
					Type:      reconnect.EventFailed,
					Attempts:  b.reconnectStrategy.Attempts(),
					Error:     err,
					Timestamp: time.Now(),
					Layer:     "websocket",
				})
				continue
			}

			// Connection successful
			elapsed := time.Since(startTime)
			b.reconnectMetrics.RecordAttempt(true, elapsed)
			b.reconnectStrategy.Reset()
			b.stateManager.SetState(StateConnected, "connection_established")
			b.flushOffline() // Send buffered messages from offline period
			b.reconnectCallback.Notify(reconnect.Event{
				Type:      reconnect.EventSuccess,
				Attempts:  b.reconnectStrategy.Attempts(),
				Timestamp: time.Now(),
				Layer:     "websocket",
			})

			// Note: device:online message is sent by the server (room.ts) when bridge reconnects
			b.logInfo("[%s] Connected successfully", logger.ModBridge)
		}

		b.logDebug("[%s] Waiting for message on WebSocket...", logger.ModBridge)
		_, data, err := b.conn.ReadMessage()
		if err != nil {
			closeCode := b.extractCloseCode(err)
			if b.isPermanentCloseCode(closeCode) {
				b.logInfo("[%s] WebSocket closed with permanent code %d: %v", logger.ModBridge, closeCode, err)
				b.stateManager.SetState(StateFailed, fmt.Sprintf("permanent_close_%d", closeCode))
				b.reconnectCallback.Notify(reconnect.Event{
					Type:      reconnect.EventAborted,
					Attempts:  b.reconnectStrategy.Attempts(),
					Error:     err,
					Timestamp: time.Now(),
					Layer:     "websocket",
					Extra:     map[string]interface{}{"closeCode": closeCode},
				})
				return
			}
			if closeCode > 0 && !isTemporaryCloseCode(closeCode) {
				b.logInfo("[%s] WebSocket closed with unknown code %d, attempts=%d: %v",
					logger.ModBridge, closeCode, b.reconnectStrategy.Attempts(), err)
			} else {
				lastSend := atomic.LoadInt64(&b.lastSendTime)
				lastActivity := "never"
				if lastSend > 0 {
					lastActivity = fmt.Sprintf("%v ago", time.Since(time.UnixMilli(lastSend)).Round(time.Second))
				}
				b.logInfo("[%s] WebSocket read error (closeCode=%d, lastActivity=%s, attempts=%d): %v",
					logger.ModBridge, closeCode, lastActivity, b.reconnectStrategy.Attempts(), err)
			}
			b.stateManager.SetState(StateDisconnected, "read_error")
			b.reconnect()
			continue
		}

		b.logDebug("[%s] Received raw data: length=%d, data=%s", logger.ModBridge, len(data), logger.Truncate(string(data), logger.MaxPayload))

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			b.logDebug("[%s] Failed to parse message: %v", logger.ModBridge, err)
			continue
		}

		b.logDebug("[%s] Parsed message: type=%s", logger.ModBridge, msg.Type)

		// Queue message for ordered processing without blocking readLoop
		// The messageWorker will process messages sequentially
		select {
		case b.messageQueue <- msg:
			b.logDebug("[%s] Message queued successfully: type=%s", logger.ModBridge, msg.Type)
		case <-b.done:
			return
		}
	}
}

// messageWorker processes messages from the queue sequentially
// This ensures ordered processing while not blocking the readLoop
func (b *Bridge) messageWorker() {
	defer func() {
		if r := recover(); r != nil {
			b.logError("[%s] messageWorker panic: %v, restarting...", logger.ModBridge, r)
			// Restart worker after panic
			time.Sleep(time.Second)
			go b.messageWorker()
		}
	}()

	b.logInfo("[%s] messageWorker started and ready to process messages", logger.ModBridge)

	for {
		select {
		case <-b.done:
			b.logInfo("[%s] messageWorker stopped due to shutdown", logger.ModBridge)
			return
		case msg := <-b.messageQueue:
			queueLen := len(b.messageQueue)
			b.logDebug("[%s] messageWorker dequeued: type=%s (queue remaining: %d)", logger.ModBridge, msg.Type, queueLen)
			b.handleMessage(msg)
		}
	}
}


// forwardSessionOutput forwards protocol messages from CLI to WebSocket
func (b *Bridge) forwardSessionOutput(sessionID string, msg protocol.Message) {
	// Record metrics
	metrics.RecordMessage(sessionID)

	b.logDebug("[%s] Forwarding: session=%s, type=%s", logger.ModBridge, sessionID, msg.Type)

	// Get session to check protocol
	sess := b.sessions.Get(sessionID)
	protocolName := "unknown"
	if sess != nil {
		protocolName = sess.GetProtocolName()
	}

	// Security scan output content
	if contentStr, ok := msg.Content.(string); ok {
		if alerts := b.scanner.Scan(contentStr); len(alerts) > 0 {
			for _, a := range alerts {
				b.sendMessage(Message{
					Type: "security:alert",
					Payload: map[string]interface{}{
						"sessionId":   sessionID,
						"deviceId":    b.config.DeviceID,
						"category":    a.Category,
						"level":       a.Level,
						"ruleId":      a.RuleID,
						"title":       a.Title,
						"description": a.Description,
						"match":       a.Match,
					},
					Timestamp: time.Now().UnixMilli(),
				})
			}
			b.logInfo("[%s] %d alert(s) in session %s", logger.ModScanner, len(alerts), sessionID)
		}
	}

	switch msg.Type {
	case protocol.MessageTypeContent:
		if contentStr, ok := msg.Content.(string); ok {
			b.batchContent(sessionID, protocolName, contentStr, protocol.MessageTypeContent)
		}

		// Forward to workflow task output if this is a workflow task
		if sess != nil && sess.JobID != "" && sess.TaskID != "" {
			if contentStr, ok := msg.Content.(string); ok {
				b.sendTaskOutput(sess.JobID, sess.TaskID, "stdout", contentStr)

				// Detect [QUESTION] marker for human-in-the-loop
				b.handleQuestionMarker(sessionID, sess, contentStr)
			}
		}

	case protocol.MessageTypeThought:
		if contentStr, ok := msg.Content.(string); ok {
			b.batchContent(sessionID, protocolName, contentStr, protocol.MessageTypeThought)
		}

	case protocol.MessageTypeToolCall:
		metrics.RecordToolCall(sessionID, fmt.Sprintf("%v", msg.Content))

		toolName := fmt.Sprintf("%v", msg.Content)
		if _, ok := b.loopDetectors[sessionID]; !ok {
			b.loopDetectors[sessionID] = loopdetect.New(30, 5, 10)
		}
		if result := b.loopDetectors[sessionID].Record(toolName, toolName); result.Level > loopdetect.None {
			b.logWarn("[%s] Loop detection [%s]: %s", logger.ModSession, sessionID, result.Message)
			b.sendMessage(Message{
				Type: "session:output",
				Payload: map[string]interface{}{
					"sessionId":  sessionID,
					"outputType": "stderr",
					"content":    fmt.Sprintf("⚠ %s", result.Message),
				},
				Timestamp: time.Now().UnixMilli(),
			})
		}

		b.sendMessage(Message{
			Type: "tool:call",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"deviceId":  b.config.DeviceID,
				"toolCall":  msg.Content,
				"protocol":  protocolName,
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case protocol.MessageTypePermission:
		permReq := msg.Content.(protocol.PermissionRequest)

		permIDStr := fmt.Sprintf("%v", permReq.ID)
		b.permSessionMu.Lock()
		b.permSessionMap[permIDStr] = sessionID
		b.permSessionMu.Unlock()

		b.sendMessage(Message{
			Type: "permission:request",
			Payload: map[string]interface{}{
				"sessionId":   sessionID,
				"deviceId":    b.config.DeviceID,
				"id":          permReq.ID,
				"toolName":    permReq.ToolName,
				"toolInput":   permReq.ToolInput,
				"description": permReq.Description,
				"risk":        permReq.Risk,
				"options":     permReq.Options,
				"protocol":    protocolName,
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case protocol.MessageTypeStatus:
		b.sendMessage(Message{
			Type: "agent:status",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"deviceId":  b.config.DeviceID,
				"status":    msg.Content,
				"protocol":  protocolName,
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case protocol.MessageTypeUsage:
		usage, ok := msg.Content.(protocol.UsageStats)
		if !ok {
			b.logError("[%s] Invalid usage stats type", logger.ModSession)
			return
		}

		metrics.RecordTokenUsage(sessionID, int64(usage.InputTokens), int64(usage.OutputTokens), int64(usage.CacheCreation), int64(usage.CacheRead))

		b.sendMessage(Message{
			Type: "session:usage",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"deviceId":  b.config.DeviceID,
				"usage": map[string]interface{}{
					"inputTokens":   usage.InputTokens,
					"outputTokens":  usage.OutputTokens,
					"cacheCreation": usage.CacheCreation,
					"cacheRead":     usage.CacheRead,
					"contextSize":   usage.ContextSize,
				},
				"protocol": protocolName,
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case protocol.MessageTypePlan:
		b.sendMessage(Message{
			Type: "agent:plan",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"deviceId":  b.config.DeviceID,
				"plan":      msg.Content,
				"protocol":  protocolName,
			},
			Timestamp: time.Now().UnixMilli(),
		})

	case protocol.MessageTypeError:
		metrics.RecordError(sessionID, "protocol")

		effectiveFallbacks := b.config.GetEffectiveFallbacks()
		if len(effectiveFallbacks) > 0 {
			if sess := b.sessions.Get(sessionID); sess != nil {
				fallback := b.sessions.GetFallbackCLI(sess.CLIType, toFallbackConfigs(effectiveFallbacks))
				if fallback != "" {
					b.logInfo("[%s] Attempting fallback from %s to %s for session %s", logger.ModSession, sess.CLIType, fallback, sessionID)
					b.sendMessage(Message{
						Type: "session:output",
						Payload: map[string]interface{}{
							"sessionId":  sessionID,
							"deviceId":   b.config.DeviceID,
							"outputType": "stderr",
							"content":    fmt.Sprintf("[fallback] %s failed, switching to %s", sess.CLIType, fallback),
						},
						Timestamp: time.Now().UnixMilli(),
					})
					_ = b.sessions.Stop(sessionID)
					_, _ = b.sessions.CreateWithIDAndSize(fallback, sess.WorkDir, sessionID+"-fb", 120, 30, sess.PermissionMode)
					return
				}
			}
		}

		b.sendMessage(Message{
			Type: "session:error",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"deviceId":  b.config.DeviceID,
				"error":     msg.Content,
				"protocol":  protocolName,
			},
			Timestamp: time.Now().UnixMilli(),
		})

	default:
		if protocolName == "pty" {
			b.sendMessage(Message{
				Type: "session:output",
				Payload: map[string]interface{}{
					"sessionId":  sessionID,
					"deviceId":   b.config.DeviceID,
					"outputType": "stdout",
					"content":    msg.Content,
					"protocol":   protocolName,
				},
				Timestamp: time.Now().UnixMilli(),
			})
		}
	}
}
func (b *Bridge) handleMessage(msg Message) {
	b.logDebug("[%s] Received message type: %s, payload: %+v", logger.ModBridge, msg.Type, msg.Payload)
	switch msg.Type {
	case "session:start":
		b.handleSessionStart(msg)
	case "session:resume":
		b.handleSessionResume(msg)
	case "session:send":
		b.handleSessionSend(msg)
	case "session:stop":
		b.handleSessionStop(msg)
	case "session:cancel":
		b.handleSessionCancel(msg)
	case "session:resize":
		b.handleSessionResize(msg)
	case "chat:send":
		b.handleChatSend(msg)
	case "permission:response":
		b.handlePermissionResponse(msg)
	case "control:takeover":
		b.handleControlTakeover(msg)
	case "config:sync":
		b.handleConfigSync(msg)
	case "rules:sync":
		b.handleRulesSync(msg)
	case "storage:sync":
		b.handleStorageSync(msg)
	case "device:restart":
		b.handleDeviceRestart(msg)
	case "prompts:sync":
		b.handlePromptsSync(msg)
	case "mcp:sync":
		b.handleMCPSync(msg)
	case "mcp:list":
		b.handleMCPList(msg)
	case "workflow:start":
		b.handleWorkflowStartJob(msg)
	case "workflow:pause":
		b.handleWorkflowPauseJob(msg)
	case "workflow:cancel":
		b.handleWorkflowCancelJob(msg)
	case "workflow:start_task":
		b.handleWorkflowStartTask(msg)
	case "workflow:task_assign":
		b.handleWorkflowTaskAssign(msg)
	case "workflow:task_cleanup":
		b.handleWorkflowTaskCleanup(msg)
	case "workflow:task_answer":
		b.handleWorkflowTaskAnswer(msg)
	case "workflow:task_guidance":
		b.handleWorkflowTaskGuidance(msg)
	case "workflow:task_merge":
		b.handleWorkflowTaskMerge(msg)
	case "workflow:merge_all":
		b.handleWorkflowMergeAll(msg)
	case "workflow:get_state":
		b.handleWorkflowGetState(msg)
	case "workflow:set_state":
		b.handleWorkflowSetState(msg)
	case "acp:query_status":
		b.handleACPQueryStatus(msg)
	case "scanner:toggle":
		b.handleScannerToggle(msg)
	case "scanner:rules:sync":
		b.handleScannerRulesSync(msg)
	default:
		b.logDebug("[%s] Unknown message type: %s", logger.ModBridge, msg.Type)
	}
}

// handleDeviceRestart handles restart command from web
func (b *Bridge) handleDeviceRestart(msg Message) {
	b.logInfo("[%s] Received restart command", logger.ModBridge)
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		b.logError("[%s] Invalid restart payload", logger.ModBridge)
		return
	}

	deviceId, _ := payload["deviceId"].(string)
	if deviceId != b.config.DeviceID {
		b.logDebug("[%s] Restart command not for this device (got %s, expected %s)", logger.ModBridge, deviceId, b.config.DeviceID)
		return
	}

	b.logInfo("[%s] Restarting bridge...", logger.ModBridge)
	b.Stop()
	// Exit the process - the service manager or user will restart it
	os.Exit(0)
}

func (b *Bridge) handleSessionStart(msg Message) {
	b.logDebug("[%s] handleSessionStart called", logger.ModSession)
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		b.logError("[%s] handleSessionStart: invalid payload type", logger.ModSession)
		return
	}

	sessionID, _ := payload["sessionId"].(string)
	cliType, _ := payload["cliType"].(string)
	workDir, _ := payload["workDir"].(string)
	initialCommand, _ := payload["command"].(string)
	permissionMode, _ := payload["permissionMode"].(string)

	// Get terminal size from payload
	cols := 120 // default
	rows := 30  // default
	if c, ok := payload["cols"].(float64); ok && c > 0 {
		cols = int(c)
	}
	if r, ok := payload["rows"].(float64); ok && r > 0 {
		rows = int(r)
	}

	b.logDebug("[%s] sessionID=%s, cliType=%s, workDir=%s, cols=%d, rows=%d, permissionMode=%s", logger.ModSession, sessionID, cliType, workDir, cols, rows, permissionMode)

	if cliType == "" {
		cliType = "kiro" // default
	}
	if workDir == "" {
		workDir = "."
	}

	sess, err := b.sessions.CreateWithIDAndSize(cliType, workDir, sessionID, cols, rows, permissionMode)
	if err != nil {
		b.logError("[%s] Failed to create session: %v", logger.ModSession, err)
		metrics.RecordError(sessionID, "session_create")
		b.sendMessage(Message{
			Type: "session:error",
			Payload: map[string]interface{}{
				"error": err.Error(),
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}

	// Send session started notification
	b.sendMessage(Message{
		Type: "session:started",
		Payload: map[string]interface{}{
			"sessionId": sess.ID,
			"deviceId":  b.config.DeviceID,
			"cliType":   cliType,
			"workDir":   workDir,
		},
		Timestamp: time.Now().UnixMilli(),
	})

	metrics.StartSession(sess.ID)

	// Send initial command if provided
	if initialCommand != "" {
		sess.Send(initialCommand)
	}
}

// handleSessionResume handles session resume after WebSocket reconnection
func (b *Bridge) handleSessionResume(msg Message) {
	b.logDebug("[%s] handleSessionResume called", logger.ModSession)
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		b.logError("[%s] handleSessionResume: invalid payload type", logger.ModSession)
		return
	}

	sessionID, _ := payload["sessionId"].(string)
	deviceID, _ := payload["deviceId"].(string)

	b.logDebug("[%s] Resume request: sessionID=%s, deviceID=%s", logger.ModSession, sessionID, deviceID)

	// Verify device ID matches
	if deviceID != "" && deviceID != b.config.DeviceID {
		b.logDebug("[%s] Resume not for this device (got %s, expected %s)", logger.ModSession, deviceID, b.config.DeviceID)
		return
	}

	// Get existing session
	sess := b.sessions.Get(sessionID)
	if sess == nil {
		b.logWarn("[%s] Session %s not found for resume", logger.ModSession, sessionID)
		b.sendMessage(Message{
			Type: "session:resume:failed",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"reason":    "not_found",
				"message":   "Session not found",
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}

	// Check if session is active
	if sess.Status != "active" {
		b.logWarn("[%s] Session %s is not active (status: %s)", logger.ModSession, sessionID, sess.Status)
		b.sendMessage(Message{
			Type: "session:resume:failed",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"reason":    "not_active",
				"message":   "Session is not active",
				"status":    sess.Status,
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}

	// Update last active time
	sess.LastActiveAt = time.Now()

	b.logInfo("[%s] Session %s resumed successfully", logger.ModSession, sessionID)

	// Send resume success notification
	b.sendMessage(Message{
		Type: "session:resumed",
		Payload: map[string]interface{}{
			"sessionId":     sess.ID,
			"deviceId":      b.config.DeviceID,
			"cliType":       sess.CLIType,
			"workDir":       sess.WorkDir,
			"permissionMode": sess.PermissionMode,
			"agentStatus":   "idle", // Reset to idle on resume
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (b *Bridge) handleSessionSend(msg Message) {
	b.logDebug("[%s] handleSessionSend called", logger.ModSession)

	// Step 1: Validate payload type
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		b.logError("[%s] handleSessionSend: invalid payload type, got %T", logger.ModSession, msg.Payload)
		return
	}
	b.logDebug("[%s] Payload validation passed", logger.ModSession)

	// Step 2: Extract parameters
	sessionID, ok := payload["sessionId"].(string)
	if !ok {
		b.logError("[%s] sessionId missing or invalid type", logger.ModSession)
		return
	}
	content, ok := payload["content"].(string)
	if !ok {
		b.logError("[%s] content missing or invalid type", logger.ModSession)
		return
	}
	b.logDebug("[%s] Parameters extracted: sessionID=%s, contentLength=%d, content=%s",
		logger.ModSession, sessionID, len(content), logger.Truncate(content, logger.MaxPayload))

	// Step 3: Security scanning
	if alerts := b.scanner.ScanWithDirection(content, scanner.DirInput); len(alerts) > 0 {
		for _, a := range alerts {
			b.sendMessage(Message{
				Type: "security:alert",
				Payload: map[string]interface{}{
					"sessionId":   sessionID,
					"deviceId":    b.config.DeviceID,
					"category":    a.Category,
					"level":       a.Level,
					"ruleId":      a.RuleID,
					"title":       a.Title,
					"description": a.Description,
					"match":       a.Match,
					"direction":   "input",
				},
				Timestamp: time.Now().UnixMilli(),
			})
		}
		b.logWarn("[%s] %d input alert(s) in session %s", logger.ModScanner, len(alerts), sessionID)
	}

	// Step 4: Get session (auto-create if lost after bridge restart)
	b.logDebug("[%s] Looking up session: %s", logger.ModSession, sessionID)
	sess := b.sessions.Get(sessionID)
	if sess == nil {
		b.logWarn("[%s] Session %s not found, auto-creating...", logger.ModSession, sessionID)
		cliType, _ := payload["cliType"].(string)
		if cliType == "" {
			cliType = "claude"
		}
		workDir, _ := payload["workDir"].(string)
		if workDir == "" {
			workDir = "."
		}
		var err error
		sess, err = b.sessions.CreateWithIDAndSize(cliType, workDir, sessionID, 120, 30, "")
		if err != nil {
			b.logError("[%s] Failed to auto-create session %s: %v", logger.ModSession, sessionID, err)
			return
		}
		b.sendMessage(Message{
			Type: "session:started",
			Payload: map[string]interface{}{
				"sessionId": sess.ID,
				"deviceId":  b.config.DeviceID,
				"cliType":   cliType,
				"workDir":   workDir,
			},
			Timestamp: time.Now().UnixMilli(),
		})
		b.logInfo("[%s] Auto-created session: %s", logger.ModSession, sessionID)
	}
	b.logDebug("[%s] Session found: ID=%s, CLI=%s, Protocol=%s, Status=%s",
		logger.ModSession, sess.ID, sess.CLIType, sess.GetProtocolName(), sess.Status)

	// Step 5: Check if session protocol is ready
	if sess.Protocol == nil {
		b.logError("[%s] Session protocol is nil for session %s", logger.ModSession, sessionID)
		return
	}
	b.logDebug("[%s] Session protocol ready: %s", logger.ModSession, sess.GetProtocolName())

	// Step 6: Send message to CLI
	b.logDebug("[%s] Calling sess.Send() with content length: %d", logger.ModSession, len(content))
	if err := sess.Send(content); err != nil {
		b.logError("[%s] Send error: %v", logger.ModSession, err)
		// Send error notification back to web
		b.sendMessage(Message{
			Type: "session:error",
			Payload: map[string]interface{}{
				"sessionId": sessionID,
				"deviceId":  b.config.DeviceID,
				"error":     fmt.Sprintf("Failed to send message: %v", err),
			},
			Timestamp: time.Now().UnixMilli(),
		})
	} else {
		b.logDebug("[%s] Message sent successfully to CLI", logger.ModSession)
	}
}

func (b *Bridge) handleSessionStop(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	sessionID, _ := payload["sessionId"].(string)
	if err := b.sessions.Stop(sessionID); err != nil {
		b.logDebug("[%s] Failed to stop session: %v", logger.ModSession, err)
	}

	// End session metrics
	metrics.EndSession(sessionID)

	// Send session stopped notification
	b.sendMessage(Message{
		Type: "session:stopped",
		Payload: map[string]interface{}{
			"sessionId": sessionID,
			"deviceId":  b.config.DeviceID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (b *Bridge) handleSessionCancel(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	sessionID, _ := payload["sessionId"].(string)
	b.logInfo("[%s] Cancelling session: %s", logger.ModSession, sessionID)

	// Send cancel to the session (ACP protocol)
	sess := b.sessions.Get(sessionID)
	if sess == nil {
		b.logDebug("[%s] Session not found: %s", logger.ModSession, sessionID)
		return
	}

	// Send session/cancel via protocol
	if sess.Protocol != nil {
		sess.Protocol.SendMessage(protocol.Message{
			Type:    protocol.MessageTypeCancel,
			Content: "user_cancelled",
		})
	}

	// Send cancelled notification
	b.sendMessage(Message{
		Type: "session:cancelled",
		Payload: map[string]interface{}{
			"sessionId": sessionID,
			"deviceId":  b.config.DeviceID,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (b *Bridge) handleSessionResize(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	sessionID, _ := payload["sessionId"].(string)
	cols := 80
	rows := 24

	if c, ok := payload["cols"].(float64); ok {
		cols = int(c)
	}
	if r, ok := payload["rows"].(float64); ok {
		rows = int(r)
	}

	b.logDebug("[%s] Resizing session %s to %dx%d", logger.ModSession, sessionID, cols, rows)
	if err := b.sessions.Resize(sessionID, cols, rows); err != nil {
		b.logWarn("[%s] Failed to resize session: %v", logger.ModSession, err)
	}
}

func (b *Bridge) handlePermissionResponse(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		b.logDebug("[%s] Invalid permission response payload", logger.ModPermission)
		return
	}

	// ID can be string or number in JSON-RPC 2.0
	var id interface{}
	if idVal, ok := payload["id"]; ok {
		id = idVal
	}
	approved, _ := payload["approved"].(bool)
	optionID, _ := payload["optionId"].(string)

	b.logDebug("[%s] Permission response: id=%v, approved=%v, optionId=%s", logger.ModPermission, id, approved, optionID)

	// Convert ID to string for internal permission handler
	var idStr string
	switch v := id.(type) {
	case string:
		idStr = v
	case float64:
		idStr = fmt.Sprintf("%d", int(v))
	}

	// First resolve internal permission handler
	b.permHandler.Resolve(permission.Response{
		ID:       idStr,
		Approved: approved,
	})

	// Record permission metric
	metrics.RecordPermission("", approved)

	// Also send to ACP protocol if optionId is provided
	if optionID != "" {
		// Look up the exact session for this permission ID
		b.permSessionMu.RLock()
		targetSessionID := b.permSessionMap[idStr]
		b.permSessionMu.RUnlock()

		if targetSessionID != "" {
			// Route to the specific session
			sess := b.sessions.Get(targetSessionID)
			if sess != nil && sess.Protocol != nil && sess.Protocol.GetProtocolName() == "acp" {
				b.logDebug("[%s] Sending permission response to ACP session: %s", logger.ModPermission, sess.ID)
				sess.Protocol.SendMessage(protocol.Message{
					Type: protocol.MessageTypePermission,
					Content: protocol.PermissionResponse{
						ID:       id,
						OptionID: optionID,
					},
				})
			}

			// Clean up mapping
			b.permSessionMu.Lock()
			delete(b.permSessionMap, idStr)
			b.permSessionMu.Unlock()
		} else {
			// Fallback: send to all ACP sessions (backward compatibility)
			b.logDebug("[%s] No session mapping for permission %s, broadcasting to all ACP sessions", logger.ModPermission, idStr)
			for _, sess := range b.sessions.List() {
				if sess.Protocol != nil && sess.Protocol.GetProtocolName() == "acp" {
					b.logDebug("[%s] Sending permission response to ACP session: %s", logger.ModPermission, sess.ID)
					sess.Protocol.SendMessage(protocol.Message{
						Type: protocol.MessageTypePermission,
						Content: protocol.PermissionResponse{
							ID:       id,
							OptionID: optionID,
						},
					})
				}
			}
		}
	}
}

func (b *Bridge) handleControlTakeover(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	sessionID, _ := payload["sessionId"].(string)
	b.logDebug("[%s] Control takeover for session: %s", logger.ModSession, sessionID)
}

func (b *Bridge) handleConfigSync(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	// Sync environment variables
	if envVars, ok := payload["envVars"].(map[string]interface{}); ok {
		b.config.EnvVars = make(map[string]string)
		for k, v := range envVars {
			if s, ok := v.(string); ok {
				b.config.EnvVars[k] = s
			}
		}
		// Apply to current process
		for k, v := range b.config.EnvVars {
			os.Setenv(k, v)
		}
		b.logDebug("[%s] Synced %d environment variables", logger.ModBridge, len(b.config.EnvVars))
	}

	// Sync CLI enabled status
	if cliEnabled, ok := payload["cliEnabled"].(map[string]interface{}); ok {
		b.config.CLIEnabled = make(map[string]bool)
		for k, v := range cliEnabled {
			if bv, ok := v.(bool); ok {
				b.config.CLIEnabled[k] = bv
			}
		}
		b.logDebug("[%s] Synced CLI enabled: %v", logger.ModBridge, b.config.CLIEnabled)
	}

	// Sync permissions
	if perms, ok := payload["permissions"].(map[string]interface{}); ok {
		b.config.Permissions = make(map[string]bool)
		for k, v := range perms {
			if bv, ok := v.(bool); ok {
				b.config.Permissions[k] = bv
			}
		}
		b.logDebug("[%s] Synced permissions: %v", logger.ModBridge, b.config.Permissions)
	}

	// Save config
	if err := config.Save(b.config); err != nil {
		b.logDebug("[%s] Failed to save config: %v", logger.ModBridge, err)
	}

	// Send ack
	b.sendMessage(Message{
		Type:      "config:synced",
		Payload:   map[string]string{"deviceId": b.config.DeviceID},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (b *Bridge) handleRulesSync(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	rulesData, ok := payload["rules"].([]interface{})
	if !ok {
		return
	}

	var newRules []config.AutoApprovalRule
	for _, r := range rulesData {
		if ruleMap, ok := r.(map[string]interface{}); ok {
			rule := config.AutoApprovalRule{
				ID:      getString(ruleMap, "id"),
				Pattern: getString(ruleMap, "pattern"),
				Tool:    getString(ruleMap, "tool"),
				Action:  getString(ruleMap, "action"),
			}
			newRules = append(newRules, rule)
		}
	}

	b.config.Rules = newRules
	b.rulesEngine.UpdateRules(newRules)
	config.Save(b.config)

	b.logDebug("[%s] Synced %d auto-approval rules", logger.ModBridge, len(newRules))

	b.sendMessage(Message{
		Type:      "rules:synced",
		Payload:   map[string]interface{}{"deviceId": b.config.DeviceID, "count": len(newRules)},
		Timestamp: time.Now().UnixMilli(),
	})
}

func (b *Bridge) handleStorageSync(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	storageType, _ := payload["storageType"].(string)
	b.config.StorageType = storageType

	if storageType == "s3" {
		if s3Data, ok := payload["s3Config"].(map[string]interface{}); ok {
			b.config.S3Config = &config.S3Config{
				Bucket:    getString(s3Data, "bucket"),
				Region:    getString(s3Data, "region"),
				AccessKey: getString(s3Data, "accessKey"),
				SecretKey: getString(s3Data, "secretKey"),
				Endpoint:  getString(s3Data, "endpoint"),
			}
			b.s3Uploader = storage.NewS3Uploader(b.config.S3Config)
		}
	}

	config.Save(b.config)
	b.logInfo("[%s] Storage type set to: %s", logger.ModBridge, storageType)

	b.sendMessage(Message{
		Type:      "storage:synced",
		Payload:   map[string]string{"deviceId": b.config.DeviceID, "storageType": storageType},
		Timestamp: time.Now().UnixMilli(),
	})
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func (b *Bridge) handleChatSend(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	sessionID, _ := payload["sessionId"].(string)
	content, _ := payload["content"].(string)

	b.logDebug("[%s] Chat message for session %s: %s", logger.ModSession, sessionID, logger.Truncate(content, logger.MaxPayload))

	// Scan input direction
	if alerts := b.scanner.ScanWithDirection(content, scanner.DirInput); len(alerts) > 0 {
		for _, a := range alerts {
			b.sendMessage(Message{
				Type: "security:alert",
				Payload: map[string]interface{}{
					"sessionId":   sessionID,
					"deviceId":    b.config.DeviceID,
					"category":    a.Category,
					"level":       a.Level,
					"ruleId":      a.RuleID,
					"title":       a.Title,
					"description": a.Description,
					"match":       a.Match,
					"direction":   "input",
				},
				Timestamp: time.Now().UnixMilli(),
			})
		}
	}

	sess := b.sessions.Get(sessionID)
	if sess == nil {
		var err error
		sess, err = b.sessions.Create("kiro", ".")
		if err != nil {
			b.logError("[%s] Failed to create session: %v", logger.ModSession, err)
			return
		}
	}

	if err := sess.Send(content); err != nil {
		b.logDebug("[%s] Failed to send to CLI: %v", logger.ModSession, err)
	}
}

func (b *Bridge) sendMessage(msg Message) error {
	// Phase 1: Prepare data without any lock
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	b.logDebug("[%s] Sending: type=%s, size=%d, payload=%s", logger.ModBridge, msg.Type, len(data), logger.Truncate(string(data), logger.MaxPayload))

	// Read encryption keys under mu (brief)
	b.mu.Lock()
	kp := b.keyPair
	wpk := b.webPubKey
	b.mu.Unlock()

	// Encrypt if E2EE is enabled
	if kp != nil && wpk != nil {
		encrypted, err := kp.Encrypt(data, wpk)
		if err == nil {
			envelope := Message{
				Type: "encrypted",
				Payload: map[string]string{
					"data":   base64.StdEncoding.EncodeToString(encrypted),
					"pubKey": kp.PublicKeyBase64(),
				},
				Timestamp: time.Now().UnixMilli(),
			}
			data, _ = json.Marshal(envelope)
		}
		// On encrypt error, fall through and send unencrypted
	}

	// Phase 2: Write under connMu only
	b.connMu.Lock()
	if b.conn == nil {
		b.connMu.Unlock()
		b.bufferOffline(msg)
		return nil
	}
	b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err = b.conn.WriteMessage(websocket.TextMessage, data)
	if err != nil {
		b.conn.Close()
		b.conn = nil
	} else {
		// Store in ring buffer for potential replay after reconnection
		b.msgBuffer.Push(data, time.Now().UnixMilli())
		// Update last send time for keep-alive timer reset
		atomic.StoreInt64(&b.lastSendTime, time.Now().UnixMilli())
	}
	b.connMu.Unlock()

	if err != nil {
		b.logError("[%s] WebSocket write failed: %v", logger.ModBridge, err)
	}
	return err
}

// sendTaskOutput sends task output to the workflow orchestrator via the callback
func (b *Bridge) sendTaskOutput(jobID, taskID, stream, content string) {
	if b.callbackManager == nil {
		return
	}

	// Use the callback manager to send task output to the	// the orchestrator API endpoint
	b.callbackManager.SendTaskOutput(jobID, taskID, stream, content)
}

func (b *Bridge) heartbeat() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var lastTickTime time.Time

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			// Process suspend detection via wall-clock gap
			now := time.Now()
			if !lastTickTime.IsZero() {
				gap := now.Sub(lastTickTime)
				if gap > 60*time.Second {
					b.logInfo("[%s] System suspend detected (gap: %v), triggering reconnect", logger.ModBridge, gap)
					// Reset reconnect time budget on suspend recovery
					b.reconnectStrategy.ResetBudget()
					b.reconnect()
					lastTickTime = now
					continue
				}
			}
			lastTickTime = now

			b.connMu.Lock()
			if b.conn != nil {
				b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				b.conn.WriteMessage(websocket.PingMessage, nil)
			}
			b.connMu.Unlock()
			// Update last_seen via API heartbeat
			go b.updateLastSeen()
		}
	}
}

// keepAliveLoop sends periodic keep-alive data frames to prevent proxy idle timeout.
// Sends a JSON frame every 5 minutes, resetting the timer on business message sends.
func (b *Bridge) keepAliveLoop() {
	ticker := time.NewTicker(1 * time.Minute) // Check every minute
	defer ticker.Stop()

	interval := 5 * time.Minute
	lastKeepAlive := time.Now()

	for {
		select {
		case <-b.done:
			return
		case <-b.keepAliveDone:
			return
		case <-ticker.C:
			// Check if we've sent any business message recently
			lastSend := atomic.LoadInt64(&b.lastSendTime)
			if lastSend > 0 {
				lastSendTime := time.UnixMilli(lastSend)
				if time.Since(lastSendTime) < interval {
					// Business message sent recently, reset keep-alive timer
					lastKeepAlive = lastSendTime
					continue
				}
			}

			// Send keep-alive if interval has passed since last activity
			if time.Since(lastKeepAlive) >= interval {
				b.connMu.Lock()
				if b.conn != nil {
					keepAliveMsg := map[string]interface{}{
						"type":      "keep_alive",
						"timestamp": time.Now().UnixMilli(),
					}
					data, err := json.Marshal(keepAliveMsg)
					if err == nil {
						b.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
						writeErr := b.conn.WriteMessage(websocket.TextMessage, data)
						if writeErr != nil {
							b.logDebug("[%s] Keep-alive send failed: %v", logger.ModBridge, writeErr)
						} else {
							b.logDebug("[%s] Keep-alive frame sent", logger.ModBridge)
						}
					}
				}
				b.connMu.Unlock()
				lastKeepAlive = time.Now()
			}
		}
	}
}

// startKeepAlive starts the keep-alive loop.
func (b *Bridge) startKeepAlive() {
	select {
	case <-b.keepAliveDone:
		// Channel was closed, create new one
		b.keepAliveDone = make(chan struct{})
	default:
		// Channel is open, keep-alive may already be running
	}
	go b.keepAliveLoop()
}

// stopKeepAlive stops the keep-alive loop.
func (b *Bridge) stopKeepAlive() {
	select {
	case <-b.keepAliveDone:
		// Already stopped
	default:
		close(b.keepAliveDone)
	}
}

// updateLastSeen sends a heartbeat to the API to update the device's last_seen timestamp
func (b *Bridge) updateLastSeen() {
	// Derive API URL from WebSocket URL
	apiURL := b.config.ServerURL
	if len(apiURL) > 3 && apiURL[:3] == "wss" {
		apiURL = "https" + apiURL[3:]
	} else if len(apiURL) > 2 && apiURL[:2] == "ws" {
		apiURL = "http" + apiURL[2:]
	}

	url := fmt.Sprintf("%s/api/devices/%s/heartbeat", apiURL, b.config.DeviceID)

	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		b.logDebug("[%s] Failed to create heartbeat request: %v", logger.ModHeartbeat, err)
		return
	}

	req.Header.Set("Authorization", "Bearer "+b.config.DeviceToken)
	req.Header.Set("Content-Type", "application/json")

	// Use the reused HTTP client instead of creating a new one
	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.heartbeatFailures++
		b.logWarn("[%s] Heartbeat failed (%d/5): %v", logger.ModHeartbeat, b.heartbeatFailures, err)
		if b.heartbeatFailures >= 5 {
			b.logWarn("[%s] 5 consecutive heartbeat failures, reconnecting...", logger.ModHeartbeat)
			b.reconnect()
		}
		return
	}
	defer resp.Body.Close()

	// Read the entire response body to prevent connection reset
	// This ensures the connection is properly closed after the server finishes writing
	_, _ = io.ReadAll(resp.Body)

	// Reset failure counter on success
	b.heartbeatFailures = 0

	if resp.StatusCode != 200 {
		b.logDebug("[%s] Heartbeat returned status %d", logger.ModHeartbeat, resp.StatusCode)
	}
}

func (b *Bridge) reconnect() {
	// Close existing connection
	b.connMu.Lock()
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	b.connMu.Unlock()

	// Reset reconnect time budget so we get a fresh 10-minute window
	b.reconnectStrategy.ResetBudget()

	b.stateManager.SetState(StateReconnecting, "initiating_reconnect")
	alert.WebSocketDisconnected("connection lost")

	b.logInfo("[%s] Reconnecting (attempt %d)...",
		logger.ModBridge, b.reconnectStrategy.Attempts()+1)

	// The actual reconnection will happen in readLoop with exponential backoff
}

// batchContent accumulates small content chunks for a session, flushing them
// as merged messages on a timer or when the batch exceeds maxBatchSize.
func (b *Bridge) batchContent(sessionID, protocolName, content string, msgType protocol.MessageType) {
	b.batchMu.Lock()
	defer b.batchMu.Unlock()

	key := sessionID
	batch, ok := b.batchBuf[key]
	if !ok {
		batch = &contentBatch{sessionID: sessionID, protocolName: protocolName, msgType: msgType}
		b.batchBuf[key] = batch
		// Start flush timer on first entry
		if b.batchTimer == nil {
			b.batchWait.Add(1)
			b.batchTimer = time.AfterFunc(batchFlushInterval, b.flushBatches)
		}
	}

	batch.chunks = append(batch.chunks, content)

	// Immediate flush if accumulated content exceeds threshold
	totalSize := 0
	for _, c := range batch.chunks {
		totalSize += len(c)
	}
	if totalSize >= maxBatchSize {
		b.doFlushLocked()
	}
}

// flushBatches is called by the timer to flush all pending batches.
func (b *Bridge) flushBatches() {
	b.batchMu.Lock()
	defer b.batchMu.Unlock()
	b.doFlushLocked()
	b.batchTimer = nil
	b.batchWait.Done()
}

// doFlushLocked merges and sends all pending batches. Must be called with batchMu held.
func (b *Bridge) doFlushLocked() {
	for key, batch := range b.batchBuf {
		if len(batch.chunks) == 0 {
			continue
		}
		merged := strings.Join(batch.chunks, "")
		msgType := "chat:response"
		if batch.msgType == protocol.MessageTypeThought {
			msgType = "chat:thought"
		}
		b.sendMessage(Message{
			Type: msgType,
			Payload: map[string]interface{}{
				"sessionId": batch.sessionID,
				"deviceId":  b.config.DeviceID,
				"content":   merged,
				"protocol":  batch.protocolName,
			},
			Timestamp: time.Now().UnixMilli(),
		})
		b.logDebug("[%s] Flushed batch: session=%s, chunks=%d, merged=%d bytes",
			logger.ModBridge, batch.sessionID, len(batch.chunks), len(merged))
		delete(b.batchBuf, key)
	}
}

// bufferOffline stores a message for later delivery when the WebSocket is disconnected.
func (b *Bridge) bufferOffline(msg Message) {
	b.offlineMu.Lock()
	defer b.offlineMu.Unlock()

	if len(b.offlineBuf) >= offlineMaxMessages {
		// Evict oldest half to avoid frequent trimming
		b.offlineBuf = b.offlineBuf[offlineMaxMessages/2:]
	}
	b.offlineBuf = append(b.offlineBuf, msg)

	// Also push to ring buffer for replay after reconnection
	data, err := json.Marshal(msg)
	if err == nil {
		b.msgBuffer.Push(data, time.Now().UnixMilli())
	}

	b.logDebug("[%s] Buffered offline: %s (buf: %d)", logger.ModBridge, msg.Type, len(b.offlineBuf))
}

// flushOffline sends all buffered messages after reconnection.
func (b *Bridge) flushOffline() {
	b.offlineMu.Lock()
	msgs := b.offlineBuf
	b.offlineBuf = nil
	b.offlineMu.Unlock()

	if len(msgs) == 0 {
		return
	}
	b.logInfo("[%s] Flushing %d buffered messages", logger.ModBridge, len(msgs))
	for _, msg := range msgs {
		if err := b.sendMessage(msg); err != nil {
			b.logWarn("[%s] Failed to flush message: %s, %v", logger.ModBridge, msg.Type, err)
			// Put remaining messages back into buffer
			b.offlineMu.Lock()
			b.offlineBuf = append(b.offlineBuf, msg)
			b.offlineMu.Unlock()
			return
		}
	}
}

// WebSocket close code classification
var permanentCloseCodes = map[int]bool{
	1002: true, // Protocol Error
	1008: true, // Policy Violation
	4001: true, // Session Expired
	4003: true, // Unauthorized
}

var temporaryCloseCodes = map[int]bool{
	1001: true, // Going Away
	1005: true, // No Status Received
	1006: true, // Abnormal Closure
}

// extractCloseCode extracts the WebSocket close code from a ReadMessage error.
func (b *Bridge) extractCloseCode(err error) int {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code
	}
	return 1006 // Default: abnormal closure
}

// isPermanentCloseCode returns true for close codes that should not trigger reconnection.
func (b *Bridge) isPermanentCloseCode(code int) bool {
	return permanentCloseCodes[code]
}

// isTemporaryCloseCode returns true for known temporary close codes.
func isTemporaryCloseCode(code int) bool {
	return temporaryCloseCodes[code]
}

func getDeviceName() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "Unknown Device"
	}
	return hostname
}

// syncRulesFromAPI fetches permission rules from API and updates local config
func (b *Bridge) syncRulesFromAPI() {
	rules, err := b.apiClient.GetPermissionRules("")
	if err != nil {
		b.logWarn("[%s] Failed to sync rules from API: %v", logger.ModBridge, err)
		return
	}

	var configRules []config.AutoApprovalRule
	for _, r := range rules {
		configRules = append(configRules, config.AutoApprovalRule{
			ID:      r.ID,
			Pattern: r.Pattern,
			Tool:    r.Tool,
			Action:  r.Action,
		})
	}

	b.config.Rules = configRules
	b.rulesEngine.UpdateRules(configRules)
	config.Save(b.config)
	b.logInfo("[%s] Synced %d rules from API", logger.ModBridge, len(configRules))
}

// ReportSessionToAPI reports session status to API
func (b *Bridge) ReportSessionToAPI(sessionID, cliType, workDir, status string) {
	err := b.apiClient.ReportSession(api.SessionReport{
		SessionID: sessionID,
		CLIType:   cliType,
		WorkDir:   workDir,
		Status:    status,
	})
	if err != nil {
		b.logDebug("[%s] Failed to report session to API: %v", logger.ModSession, err)
	}
}

// Message represents a WebSocket message
type Message struct {
	Type      string      `json:"type"`
	Payload   interface{} `json:"payload"`
	Timestamp int64       `json:"timestamp"`
}

// handlePromptsSync handles prompt sync from web
func (b *Bridge) handlePromptsSync(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		b.logDebug("[%s] Invalid prompts:sync payload", logger.ModBridge)
		return
	}

	deviceId := getString(payload, "deviceId")

	// Store prompts locally in config
	if prompts, ok := payload["prompts"]; ok {
		b.config.Prompts = prompts
		config.Save(b.config)
		b.logDebug("[%s] Synced prompts to local config", logger.ModBridge)
	}

	// Send ack back
	b.sendMessage(Message{
		Type: "prompts:synced",
		Payload: map[string]interface{}{
			"deviceId": deviceId,
			"success":  true,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleScannerToggle handles enabling/disabling the security scanner from web
func (b *Bridge) handleScannerToggle(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	enabled, _ := payload["enabled"].(bool)
	b.scanner.SetEnabled(enabled)
	boolVal := enabled
	b.config.ScannerEnabled = &boolVal
	config.Save(b.config)
	b.logInfo("[%s] Toggled to %v", logger.ModScanner, enabled)

	b.sendMessage(Message{
		Type: "scanner:status",
		Payload: map[string]interface{}{
			"deviceId": b.config.DeviceID,
			"enabled":  enabled,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleScannerRulesSync handles custom scanner rules pushed from web
func (b *Bridge) handleScannerRulesSync(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	rulesData, ok := payload["rules"].([]interface{})
	if !ok {
		return
	}

	var defs []scanner.CustomRuleDef
	for _, r := range rulesData {
		if m, ok := r.(map[string]interface{}); ok {
			defs = append(defs, scanner.CustomRuleDef{
				ID:       getString(m, "id"),
				Pattern:  getString(m, "pattern"),
				Category: getString(m, "category"),
				Level:    getString(m, "level"),
				Title:    getString(m, "title"),
				Desc:     getString(m, "desc"),
			})
		}
	}

	b.scanner.ReplaceCustomRules(defs)

	// Persist to local file
	config.SaveScannerRules(defs)
	b.logInfo("[%s] Synced %d custom rules from web", logger.ModScanner, len(defs))

	b.sendMessage(Message{
		Type: "scanner:rules:synced",
		Payload: map[string]interface{}{
			"deviceId": b.config.DeviceID,
			"count":    len(defs),
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleWorkflowStartJob handles starting a workflow job
func (b *Bridge) handleWorkflowStartJob(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")
	b.logInfo("[%s] Workflow job started: %s", logger.ModWorkflow, jobId)

	// Get tasks from payload
	tasks, _ := payload["tasks"].([]interface{})
	for _, t := range tasks {
		task, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		taskId := getString(task, "id")
		// Notify web that task has started
		b.sendMessage(Message{
			Type: "workflow:task_started",
			Payload: map[string]interface{}{
				"jobId":  jobId,
				"taskId": taskId,
			},
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

// handleWorkflowPauseJob handles pausing a workflow job
func (b *Bridge) handleWorkflowPauseJob(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	jobId := getString(payload, "jobId")
	b.logInfo("[%s] Workflow job paused: %s", logger.ModWorkflow, jobId)
}

// handleWorkflowCancelJob handles cancelling a workflow job
func (b *Bridge) handleWorkflowCancelJob(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	jobId := getString(payload, "jobId")
	b.logInfo("[%s] Workflow job cancelled: %s", logger.ModWorkflow, jobId)
}

// handleWorkflowStartTask handles starting a specific task in a job
func (b *Bridge) handleWorkflowStartTask(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")
	taskId := getString(payload, "taskId")
	agentId := getString(payload, "agentId")

	b.logInfo("[%s] Starting workflow task %s (agent: %s) in job %s", logger.ModWorkflow, taskId, agentId, jobId)

	// Notify progress
	b.sendMessage(Message{
		Type: "workflow:task_started",
		Payload: map[string]interface{}{
			"jobId":  jobId,
			"taskId": taskId,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleWorkflowTaskAssign handles task assignment from Orchestrator
func (b *Bridge) handleWorkflowTaskAssign(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")
	taskId := getString(payload, "taskId")
	agent := getString(payload, "agent")
	title := getString(payload, "title")
	description := getString(payload, "description")
	context := getString(payload, "context")
	worktreeBranch := getString(payload, "worktreeBranch")

	b.logInfo("[%s] Workflow task assign: %s (agent: %s) in job %s", logger.ModWorkflow, taskId, agent, jobId)

	// Determine working directory — use worktree if available
	workDir := "."
	if worktreeBranch != "" {
		if b.worktreeManager.IsGitRepo() {
			wtPath, err := b.worktreeManager.CreateWorktree(jobId, taskId)
			if err != nil {
				b.logInfo("[%s] Worktree creation failed for task %s: %v, falling back to original dir", logger.ModWorkflow, taskId, err)
			} else {
				workDir = wtPath
				b.logInfo("[%s] Created worktree for task %s at %s", logger.ModWorkflow, taskId, wtPath)
			}
		} else {
			b.logDebug("[%s] Not a git repo, skipping worktree for task %s", logger.ModWorkflow, taskId)
		}
	}

	// Check process pool capacity
	if b.sessions.ActiveCount() >= b.sessions.MaxConcurrent() {
		b.logInfo("[%s] Process pool full, queuing task %s", logger.ModWorkflow, taskId)
		b.sessions.Enqueue(session.QueueItem{
			CLIType:   agent,
			WorkDir:   workDir,
			SessionID: taskId,
			Cols:      120,
			Rows:      30,
			PermMode:  "accept-edits",
			Prompt:    buildTaskPrompt(title, description, context),
		})
		return
	}

	b.startTaskSession(jobId, taskId, agent, title, description, context, workDir)
}

func (b *Bridge) startTaskSession(jobId, taskId, agent, title, description, context, workDir string) {
	prompt := buildTaskPrompt(title, description, context)

	sess, err := b.sessions.CreateWithIDAndSize(agent, workDir, taskId, 120, 30, "accept-edits")
	if err != nil {
		b.logInfo("[%s] Failed to create session for task %s: %v", logger.ModWorkflow, taskId, err)
		b.sendMessage(Message{
			Type: "workflow:task_error",
			Payload: map[string]interface{}{
				"jobId":     jobId,
				"taskId":    taskId,
				"deviceId":  b.config.DeviceID,
				"error":     err.Error(),
				"errorType": "crash",
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}

	// Report progress
	b.sendMessage(Message{
		Type: "workflow:task_progress",
		Payload: map[string]interface{}{
			"jobId":    jobId,
			"taskId":   taskId,
			"deviceId": b.config.DeviceID,
			"progress": 0,
			"step":     "started",
		},
		Timestamp: time.Now().UnixMilli(),
	})

	// Send the prompt to the CLI agent
	if err := sess.Send(prompt); err != nil {
		b.logDebug("[%s] Failed to send prompt for task %s: %v", logger.ModWorkflow, taskId, err)
	}

	// Store job/task metadata for exit callback
	sess.SetMultiAgentMetadata(jobId, taskId)

	// Store task meta for worktree handling on exit
	isWorktree := workDir != "."
	b.taskMetaMu.Lock()
	b.taskMeta[taskId] = &taskMeta{
		JobID:    jobId,
		TaskID:   taskId,
		Title:    title,
		WorkDir:  workDir,
		Worktree: isWorktree,
	}
	b.taskMetaMu.Unlock()
}

func buildTaskPrompt(title, description, context string) string {
	prompt := title + "\n\n" + description
	if context != "" {
		prompt += "\n\n--- Upstream task output ---\n" + context
	}
	prompt += "\n\n--- Instruction ---\nIf you need to ask the user a question during execution, output a line starting with [QUESTION] followed by your question. Example: [QUESTION] Should I use JWT or session-based authentication?"
	return prompt
}

// handleWorkflowTaskCleanup handles worktree cleanup after task merge
func (b *Bridge) handleWorkflowTaskCleanup(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")
	taskId := getString(payload, "taskId")

	b.logInfo("[%s] Workflow task cleanup: %s in job %s", logger.ModWorkflow, taskId, jobId)

	if err := b.worktreeManager.RemoveWorktree(jobId, taskId); err != nil {
		b.logInfo("[%s] Worktree cleanup failed for task %s: %v", logger.ModWorkflow, taskId, err)
	} else {
		b.logInfo("[%s] Cleaned up worktree for task %s", logger.ModWorkflow, taskId)
	}
}

// handleWorkflowTaskMerge handles branch merge request from web UI
func (b *Bridge) handleWorkflowTaskMerge(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")
	taskId := getString(payload, "taskId")

	b.logInfo("[%s] Workflow task merge: %s in job %s", logger.ModWorkflow, taskId, jobId)

	conflictFiles, err := b.worktreeManager.MergeBranch(jobId, taskId)
	if err != nil {
		b.logInfo("[%s] Merge failed for task %s: %v", logger.ModWorkflow, taskId, err)
		b.sendMessage(Message{
			Type: "workflow:task_error",
			Payload: map[string]interface{}{
				"jobId":     jobId,
				"taskId":    taskId,
				"deviceId":  b.config.DeviceID,
				"error":     err.Error(),
				"errorType": "merge_failed",
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}

	if len(conflictFiles) > 0 {
		b.logInfo("[%s] Merge conflict for task %s: %v", logger.ModWorkflow, taskId, conflictFiles)
		b.sendMessage(Message{
			Type: "workflow:task_merge_conflict",
			Payload: map[string]interface{}{
				"jobId":        jobId,
				"taskId":       taskId,
				"deviceId":     b.config.DeviceID,
				"conflictFiles": conflictFiles,
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}

	// Merge succeeded
	b.logInfo("[%s] Merge succeeded for task %s", logger.ModWorkflow, taskId)
	b.sendMessage(Message{
		Type: "workflow:task_result",
		Payload: map[string]interface{}{
			"jobId":  jobId,
			"taskId": taskId,
			"merged": true,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleWorkflowMergeAll handles multi-branch merge request for multi-device jobs
func (b *Bridge) handleWorkflowMergeAll(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")

	// Extract branches array
	branchesRaw, ok := payload["branches"]
	if !ok {
		b.logDebug("[%s] merge_all: no branches in payload", logger.ModWorkflow)
		return
	}
	branchesSlice, ok := branchesRaw.([]interface{})
	if !ok {
		b.logDebug("[%s] merge_all: branches is not an array", logger.ModWorkflow)
		return
	}

	var branches []workflows.BranchSpec
	for _, br := range branchesSlice {
		brMap, ok := br.(map[string]interface{})
		if !ok {
			continue
		}
		branches = append(branches, workflows.BranchSpec{
			TaskID:     getString(brMap, "taskId"),
			BranchName: getString(brMap, "branchName"),
		})
	}

	b.logInfo("[%s] Workflow merge_all: %d branches for job %s", logger.ModWorkflow, len(branches), jobId)

	// Execute sequential merge
	for _, branch := range branches {
		b.logInfo("[%s] Merging branch %s (task %s)", logger.ModWorkflow, branch.BranchName, branch.TaskID)

		// Fetch remote branch first
		if err := b.worktreeManager.FetchBranch(branch.BranchName); err != nil {
			b.logInfo("[%s] Fetch failed for branch %s: %v", logger.ModWorkflow, branch.BranchName, err)
			b.sendMessage(Message{
				Type: "workflow:merge_progress",
				Payload: map[string]interface{}{
					"jobId":  jobId,
					"taskId": branch.TaskID,
					"status": "fetch_failed",
				},
				Timestamp: time.Now().UnixMilli(),
			})
			continue
		}

		// Merge the branch
		conflictFiles, err := b.worktreeManager.MergeBranchByRef(branch.BranchName)
		if err != nil {
			b.logInfo("[%s] Merge failed for branch %s: %v", logger.ModWorkflow, branch.BranchName, err)
			b.sendMessage(Message{
				Type: "workflow:merge_progress",
				Payload: map[string]interface{}{
					"jobId":  jobId,
					"taskId": branch.TaskID,
					"status": "error",
				},
				Timestamp: time.Now().UnixMilli(),
			})
			continue
		}

		if len(conflictFiles) > 0 {
			b.logInfo("[%s] Merge conflict for branch %s: %v", logger.ModWorkflow, branch.BranchName, conflictFiles)
			b.sendMessage(Message{
				Type: "workflow:merge_progress",
				Payload: map[string]interface{}{
					"jobId":        jobId,
					"taskId":       branch.TaskID,
					"status":       "conflict",
					"conflictFiles": conflictFiles,
				},
				Timestamp: time.Now().UnixMilli(),
			})
			// Stop merging on conflict
			break
		}

		// Push main after each successful merge
		if err := b.worktreeManager.PushMain(); err != nil {
			b.logInfo("[%s] Push main failed after merging %s: %v", logger.ModWorkflow, branch.BranchName, err)
		}

		b.sendMessage(Message{
			Type: "workflow:merge_progress",
			Payload: map[string]interface{}{
				"jobId":  jobId,
				"taskId": branch.TaskID,
				"status": "merged",
			},
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

// handleSessionExit handles session exit events for workflow tasks
func (b *Bridge) handleSessionExit(sessionID string, exitCode int, output []byte) {
	// Look up task metadata
	b.taskMetaMu.RLock()
	meta, ok := b.taskMeta[sessionID]
	b.taskMetaMu.RUnlock()

	if !ok {
		b.logDebug("[%s] No task meta for session %s, skipping workflow result", logger.ModWorkflow, sessionID)
		return
	}

	// Clean up task meta
	b.taskMetaMu.Lock()
	delete(b.taskMeta, sessionID)
	b.taskMetaMu.Unlock()

	// Auto-commit in worktree if applicable
	var commitHash string
	if meta.Worktree {
		hash, err := b.worktreeManager.CommitAll(meta.WorkDir, meta.TaskID, meta.Title)
		if err != nil {
			b.logInfo("[%s] Worktree commit failed for task %s: %v", logger.ModWorkflow, meta.TaskID, err)
		} else if hash != "" {
			commitHash = hash
			b.logInfo("[%s] Committed worktree for task %s: %s", logger.ModWorkflow, meta.TaskID, hash)

			// Push branch to remote for cross-device merging
			branchName := workflows.GetBranchName(meta.JobID, meta.TaskID)
			if err := b.worktreeManager.PushBranch(meta.WorkDir, branchName); err != nil {
				b.logInfo("[%s] Push failed for task %s branch %s: %v", logger.ModWorkflow, meta.TaskID, branchName, err)
			} else {
				b.logInfo("[%s] Pushed branch %s for task %s", logger.ModWorkflow, branchName, meta.TaskID)
			}
		}
	}

	// Send task result via callback manager
	if b.callbackManager != nil {
		summary, artifacts := b.callbackManager.ExtractArtifacts(output)
		result := workflows.TaskResult{
			JobID:      meta.JobID,
			TaskID:     meta.TaskID,
			Success:    exitCode == 0,
			ExitCode:   exitCode,
			Summary:    summary,
			Artifacts:  artifacts,
			DurationMs: 0, // Duration tracked by orchestrator
		}

		if commitHash != "" {
			result.CommitHash = commitHash
		}

		if exitCode != 0 {
			result.Error = fmt.Sprintf("Process exited with code %d", exitCode)
			result.ErrorType = "crash"
			b.callbackManager.SendTaskError(result)
		} else {
			b.callbackManager.SendTaskResult(result)
		}
	}
}

// handleQuestionMarker detects [QUESTION] markers in CLI output and triggers human-in-the-loop
func (b *Bridge) handleQuestionMarker(sessionID string, sess *session.Session, content string) {
	if !strings.Contains(content, "[QUESTION]") {
		return
	}

	// Extract question text from lines containing [QUESTION]
	var questionParts []string
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "[QUESTION]") {
			q := strings.TrimSpace(strings.SplitN(trimmed, "[QUESTION]", 2)[1])
			if q != "" {
				questionParts = append(questionParts, q)
			}
		}
	}
	question := strings.Join(questionParts, " ")
	if question == "" {
		question = "Agent is asking a question"
	}

	b.logInfo("[%s] Task %s asking question: %s", logger.ModWorkflow, sess.TaskID, question)

	// Send question to frontend via WebSocket
	b.sendMessage(Message{
		Type: "workflow:task_question",
		Payload: map[string]interface{}{
			"missionId": sess.JobID,
			"taskId":    sess.TaskID,
			"question":  question,
			"deviceId":  b.config.DeviceID,
		},
		Timestamp: time.Now().UnixMilli(),
	})

	// Create channel to wait for answer (5 min timeout)
	answerCh := make(chan string, 1)
	b.pendingQuestionsMu.Lock()
	// Clean up any existing pending question for this task
	if oldCh, exists := b.pendingQuestions[sess.TaskID]; exists {
		close(oldCh)
	}
	b.pendingQuestions[sess.TaskID] = answerCh
	b.pendingQuestionsMu.Unlock()

	// Wait for answer asynchronously
	go func() {
		select {
		case answer := <-answerCh:
			b.logInfo("[%s] Received answer for task %s, injecting into session", logger.ModWorkflow, sess.TaskID)
			answerPrompt := fmt.Sprintf("User answered your question:\n%s\n\nContinue with this information.", answer)
			if s := b.sessions.Get(sessionID); s != nil {
				s.Send(answerPrompt)
			}
		case <-time.After(5 * time.Minute):
			b.logInfo("[%s] Task %s question timed out (5min), continuing", logger.ModWorkflow, sess.TaskID)
			b.pendingQuestionsMu.Lock()
			delete(b.pendingQuestions, sess.TaskID)
			b.pendingQuestionsMu.Unlock()
		}
	}()
}

// handleWorkflowTaskAnswer handles user answers to agent questions
func (b *Bridge) handleWorkflowTaskAnswer(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	taskId := getString(payload, "taskId")
	answer := getString(payload, "answer")

	b.logInfo("[%s] Received task answer for %s", logger.ModWorkflow, taskId)

	b.pendingQuestionsMu.RLock()
	ch, exists := b.pendingQuestions[taskId]
	b.pendingQuestionsMu.RUnlock()

	if exists {
		ch <- answer
		b.pendingQuestionsMu.Lock()
		delete(b.pendingQuestions, taskId)
		b.pendingQuestionsMu.Unlock()
	} else {
		// No pending question — inject directly as guidance
		b.logInfo("[%s] No pending question for task %s, injecting as direct message", logger.ModWorkflow, taskId)
		if sess := b.sessions.Get(taskId); sess != nil {
			sess.Send(fmt.Sprintf("User guidance:\n%s", answer))
		}
	}
}

// handleWorkflowTaskGuidance handles proactive user guidance during task execution
func (b *Bridge) handleWorkflowTaskGuidance(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	taskId := getString(payload, "taskId")
	guidance := getString(payload, "guidance")

	b.logInfo("[%s] Received task guidance for %s", logger.ModWorkflow, taskId)

	if sess := b.sessions.Get(taskId); sess != nil {
		sess.Send(fmt.Sprintf("User guidance:\n%s", guidance))
	}
}

// handleACPQueryStatus responds with current protocol status for all sessions
func (b *Bridge) handleACPQueryStatus(msg Message) {
	b.logDebug("[%s] Received ACP status query", logger.ModSession)

	sessions := b.sessions.List()
	sessionInfos := make([]map[string]interface{}, 0, len(sessions))
	hasACP := false

	for _, sess := range sessions {
		proto := sess.GetProtocolName()
		if proto == "acp" {
			hasACP = true
		}

		var capabilities []string
		if sess.Protocol != nil {
			adapter := sess.Protocol.GetAdapter()
			if adapter != nil {
				capabilities = adapter.Capabilities()
			}
		}

		sessionInfos = append(sessionInfos, map[string]interface{}{
			"id":           sess.ID,
			"cliType":      sess.CLIType,
			"protocol":     proto,
			"status":       sess.Status,
			"capabilities": capabilities,
			"createdAt":    sess.CreatedAt.Format(time.RFC3339),
		})
	}

	b.sendMessage(Message{
		Type: "acp:status",
		Payload: map[string]interface{}{
			"deviceId":    b.config.DeviceID,
			"supportsAcp": hasACP,
			"sessions":    sessionInfos,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleMCPSync syncs MCP server configurations from Web dashboard
func (b *Bridge) handleMCPSync(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	if b.mcpManager == nil {
		b.logDebug("[%s] MCP manager not initialized, skipping sync", logger.ModBridge)
		return
	}

	serversRaw, ok := payload["servers"]
	if !ok {
		return
	}

	// Convert to JSON and back to typed struct
	data, err := json.Marshal(serversRaw)
	if err != nil {
		b.logError("[%s] Failed to marshal MCP servers: %v", logger.ModBridge, err)
		return
	}

	var servers map[string]struct {
		Command string            `json:"command"`
		Args    []string          `json:"args,omitempty"`
		Env     map[string]string `json:"env,omitempty"`
		Enabled bool              `json:"enabled"`
	}
	if err := json.Unmarshal(data, &servers); err != nil {
		b.logError("[%s] Failed to unmarshal MCP servers: %v", logger.ModBridge, err)
		return
	}

	// Import into MCP manager
	for name, s := range servers {
		b.mcpManager.AddServer(name, mcpPkg.ServerConfig{
			Command: s.Command,
			Args:    s.Args,
			Env:     s.Env,
			Enabled: s.Enabled,
		})
	}

	b.logInfo("[%s] MCP config synced: %d servers", logger.ModBridge, len(servers))

	b.sendMessage(Message{
		Type: "mcp:synced",
		Payload: map[string]interface{}{
			"deviceId": b.config.DeviceID,
			"count":    len(servers),
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleMCPList responds with current MCP server configurations
func (b *Bridge) handleMCPList(msg Message) {
	if b.mcpManager == nil {
		b.sendMessage(Message{
			Type: "mcp:list_response",
			Payload: map[string]interface{}{
				"deviceId": b.config.DeviceID,
				"servers":  map[string]interface{}{},
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}

	servers := b.mcpManager.ListServers()
	b.sendMessage(Message{
		Type: "mcp:list_response",
		Payload: map[string]interface{}{
			"deviceId": b.config.DeviceID,
			"servers":  servers,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleWorkflowGetState handles shared state read requests from agents
func (b *Bridge) handleWorkflowGetState(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")
	key := getString(payload, "key")

	// Call API to get state
	apiURL := b.config.ServerURL
	if len(apiURL) > 3 && apiURL[:3] == "wss" {
		apiURL = "https" + apiURL[3:]
	} else if len(apiURL) > 2 && apiURL[:2] == "ws" {
		apiURL = "http" + apiURL[2:]
	}

	url := fmt.Sprintf("%s/api/workflows/jobs/%s/state/%s", apiURL, jobId, key)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+b.config.DeviceToken)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.logDebug("[%s] Failed to get agent state: %v", logger.ModWorkflow, err)
		b.sendMessage(Message{
			Type: "workflow:state_result",
			Payload: map[string]interface{}{
				"jobId": jobId,
				"key":   key,
				"error": err.Error(),
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// Forward response
	b.sendMessage(Message{
		Type: "workflow:state_result",
		Payload: map[string]interface{}{
			"jobId":  jobId,
			"key":    key,
			"result": string(body),
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

// handleWorkflowSetState handles shared state write requests from agents
func (b *Bridge) handleWorkflowSetState(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	jobId := getString(payload, "jobId")
	key := getString(payload, "key")
	value, _ := payload["value"].(string)
	writtenBy := getString(payload, "writtenBy")

	// Call API to set state
	apiURL := b.config.ServerURL
	if len(apiURL) > 3 && apiURL[:3] == "wss" {
		apiURL = "https" + apiURL[3:]
	} else if len(apiURL) > 2 && apiURL[:2] == "ws" {
		apiURL = "http" + apiURL[2:]
	}

	url := fmt.Sprintf("%s/api/workflows/jobs/%s/state/%s", apiURL, jobId, key)
	body := map[string]string{"value": value}
	if writtenBy != "" {
		body["writtenBy"] = writtenBy
	}
	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+b.config.DeviceToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.httpClient.Do(req)
	if err != nil {
		b.logDebug("[%s] Failed to set agent state: %v", logger.ModWorkflow, err)
		b.sendMessage(Message{
			Type: "workflow:state_set",
			Payload: map[string]interface{}{
				"jobId": jobId,
				"key":   key,
				"error": err.Error(),
			},
			Timestamp: time.Now().UnixMilli(),
		})
		return
	}
	defer resp.Body.Close()

	b.sendMessage(Message{
		Type: "workflow:state_set",
		Payload: map[string]interface{}{
			"jobId":  jobId,
			"key":    key,
			"success": true,
		},
		Timestamp: time.Now().UnixMilli(),
	})
}

func toFallbackConfigs(mf []config.ModelFallback) []session.FallbackConfig {
	out := make([]session.FallbackConfig, len(mf))
	for i, f := range mf {
		out[i] = session.FallbackConfig{CLIType: f.CLIType, Fallback: f.Fallback, OnError: f.OnError}
	}
	return out
}
