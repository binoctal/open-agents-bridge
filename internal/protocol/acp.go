package protocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/command"
	"github.com/open-agents/open-agents-bridge/internal/filesystem"
	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// terminalState stores the state of a terminal command
type terminalState struct {
	output    string
	exitCode  int
	signal    string
	truncated bool
	done      bool
	doneChan  chan struct{}
}

// ACPAdapter implements the Agent Client Protocol (ACP)
type ACPAdapter struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	connected  atomic.Bool
	callback   func(Message)
	requestID  atomic.Int64
	mu         sync.Mutex
	sessionID  string
	workDir    string
	safeFS     *filesystem.SafeFileSystem // Security-hardened file operations
	terminals  map[string]*terminalState  // terminalId -> state
	terminalMu sync.RWMutex
	// Token usage tracking (estimated)
	inputTokens  atomic.Int64
	outputTokens atomic.Int64
	// Signal channels for initialization sequencing
	initDone chan struct{} // closed when initialize response received
}

// NewACPAdapter creates a new ACP adapter
func NewACPAdapter() *ACPAdapter {
	return &ACPAdapter{
		terminals: make(map[string]*terminalState),
		initDone:  make(chan struct{}),
	}
}

// SetWorkDir sets the working directory and initializes the safe filesystem.
func (a *ACPAdapter) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
	a.safeFS = filesystem.New(dir, filesystem.DefaultMaxFileSize)
}

func (a *ACPAdapter) Name() string {
	return "acp"
}

func (a *ACPAdapter) Version() string {
	return "1.0.0"
}

func (a *ACPAdapter) Connect(config AdapterConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	logger.Info("[%s] Connecting to %s in %s", logger.ModACP, config.Command, config.WorkDir)

	// Store work directory for session/new
	a.workDir = config.WorkDir

	// Start CLI process
	a.cmd = exec.Command(config.Command, config.Args...)
	a.cmd.Dir = config.WorkDir
	a.cmd.Env = os.Environ()

	// Add custom env vars
	for k, v := range config.Env {
		a.cmd.Env = append(a.cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	// Handle CustomEnv - empty value means unset the variable
	for k, v := range config.CustomEnv {
		if v == "" {
			// Remove the variable from environment
			newEnv := make([]string, 0, len(a.cmd.Env))
			prefix := k + "="
			for _, env := range a.cmd.Env {
				if !strings.HasPrefix(env, prefix) {
					newEnv = append(newEnv, env)
				}
			}
			a.cmd.Env = newEnv
		} else {
			a.cmd.Env = append(a.cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	// Set polling mode for file watchers to avoid ENOSPC (inotify watch limit)
	// This prevents CLI tools using chokidar from crashing when the system
	// has too many inotify watches (common on Linux with limited fs.inotify.max_user_watches)
	a.cmd.Env = append(a.cmd.Env,
		"CHOKIDAR_USEPOLLING=true",
		"CHOKIDAR_INTERVAL=1000",
	)

	// Setup pipes
	var err error
	a.stdin, err = a.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	a.stdout, err = a.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	a.stderr, err = a.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start process
	if err := a.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	logger.Info("[%s] Process started (PID: %d)", logger.ModACP, a.cmd.Process.Pid)
	a.connected.Store(true)

	// Start reading messages
	go a.readMessages()
	go a.readErrors()

	// Monitor process exit
	go a.monitorProcess()

	// Send initialize request
	if err := a.initialize(); err != nil {
		a.Disconnect()
		return fmt.Errorf("failed to initialize: %w", err)
	}

	return nil
}

func (a *ACPAdapter) Disconnect() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.connected.Load() {
		return nil
	}

	logger.Info("[%s] Disconnecting", logger.ModACP)
	a.connected.Store(false)

	if a.stdin != nil {
		a.stdin.Close()
	}
	if a.cmd != nil && a.cmd.Process != nil {
		a.cmd.Process.Kill()
	}

	return nil
}

func (a *ACPAdapter) IsConnected() bool {
	if !a.connected.Load() {
		return false
	}

	// Check if process is still running
	if a.cmd != nil && a.cmd.Process != nil {
		// Check if process has exited
		if a.cmd.ProcessState != nil && a.cmd.ProcessState.Exited() {
			a.connected.Store(false)
			return false
		}
	}

	return true
}

func (a *ACPAdapter) SendMessage(msg Message) error {
	if !a.connected.Load() {
		return fmt.Errorf("not connected")
	}

	// Wait for session initialization if needed
	if a.sessionID == "" && msg.Type == MessageTypeContent {
		timeout := time.After(30 * time.Second)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				return fmt.Errorf("session not initialized after 30s timeout")
			case <-ticker.C:
				if a.sessionID != "" {
					goto initialized
				}
			}
		}
	}

initialized:

	// Convert unified message to ACP JSON-RPC format
	switch msg.Type {
	case MessageTypeContent:
		// Send user message as session/prompt request
		content, ok := msg.Content.(string)
		if !ok {
			return fmt.Errorf("invalid content type")
		}

		// Estimate input tokens (roughly 4 characters per token)
		tokens := estimateTokens(content)
		a.inputTokens.Add(tokens)

		// ACP session/prompt expects prompt as an array of content objects
		req := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      a.nextRequestID(),
			"method":  "session/prompt",
			"params": map[string]interface{}{
				"sessionId": a.sessionID,
				"prompt": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": content,
					},
				},
			},
		}

		return a.sendJSONRPC(req)

	case MessageTypePermission:
		// Handle permission response
		perm, ok := msg.Content.(PermissionResponse)
		if !ok {
			return fmt.Errorf("invalid permission response type")
		}

		// ACP expects the response in this format:
		// {"jsonrpc": "2.0", "id": <id>, "result": {"outcome": {"optionId": "allow", "outcome": "selected"}}}
		req := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      perm.ID,
			"result": map[string]interface{}{
				"outcome": map[string]interface{}{
					"optionId": perm.OptionID,
					"outcome":  "selected",
				},
			},
		}

		return a.sendJSONRPC(req)

	case MessageTypeCancel:
		// Cancel/interrupt current operation

		req := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      a.nextRequestID(),
			"method":  "session/cancel",
			"params": map[string]interface{}{
				"sessionId": a.sessionID,
				"reason":    msg.Content,
			},
		}

		return a.sendJSONRPC(req)
	}

	return nil
}

func (a *ACPAdapter) ReceiveMessage() (Message, error) {
	// Not used in callback mode
	return Message{}, fmt.Errorf("not implemented")
}

func (a *ACPAdapter) Subscribe(callback func(Message)) {
	a.callback = callback
}

func (a *ACPAdapter) Capabilities() []string {
	return []string{"permissions", "file_ops", "tool_calls", "streaming"}
}

func (a *ACPAdapter) SupportsPermissions() bool {
	return true
}

func (a *ACPAdapter) SupportsFileOps() bool {
	return true
}

func (a *ACPAdapter) SupportsToolCalls() bool {
	return true
}

// initialize sends the initialize request followed by session/new
func (a *ACPAdapter) initialize() error {
	// Step 1: Send initialize request
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      a.nextRequestID(),
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": 1,
			"clientInfo": map[string]interface{}{
				"name":    "open-agents-bridge",
				"title":   "Open Agents Bridge",
				"version": "1.0.0",
			},
			"clientCapabilities": map[string]interface{}{
				"fs": map[string]interface{}{
					"readTextFile":  true,
					"writeTextFile": true,
				},
				"terminal": true,
			},
		},
	}

	if err := a.sendJSONRPC(req); err != nil {
		return err
	}

	// Step 2: Wait for initialize response before sending session/new
	select {
	case <-a.initDone:
		logger.Info("[%s] Initialize response received, sending session/new", logger.ModACP)
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timeout waiting for initialize response")
	}

	// Step 3: Create a new session
	// Get absolute path for workDir
	absWorkDir := a.workDir
	if absWorkDir == "" || absWorkDir == "." {
		absWorkDir, _ = os.Getwd()
	}

	sessionReq := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      a.nextRequestID(),
		"method":  "session/new",
		"params": map[string]interface{}{
			"cwd":        absWorkDir,
			"mcpServers": []interface{}{},
		},
	}

	return a.sendJSONRPC(sessionReq)
}

// monitorProcess watches for process exit and updates connection status
func (a *ACPAdapter) monitorProcess() {
	if a.cmd == nil || a.cmd.Process == nil {
		return
	}

	// Wait for process to exit
	err := a.cmd.Wait()

	// Mark as disconnected
	a.connected.Store(false)

	if err != nil {
		logger.Warn("[%s] Process exited with error: %v", logger.ModACP, err)
	}
}

// readMessages reads JSON-RPC messages from stdout
func (a *ACPAdapter) readMessages() {
	scanner := bufio.NewScanner(a.stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer

	for scanner.Scan() {
		if !a.connected.Load() {
			break
		}

		line := scanner.Text()

		var msg map[string]interface{}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			logger.Debug("[%s] Failed to parse JSON: %v", logger.ModACP, err)
			continue
		}

		a.handleMessage(msg)
	}

	if err := scanner.Err(); err != nil {
		logger.Warn("[%s] Scanner error: %v", logger.ModACP, err)
	}
}

// readErrors reads stderr and only forwards actionable errors.
// CLI debug output (object dumps, stack traces) is logged locally but not sent to the Web UI.
func (a *ACPAdapter) readErrors() {
	scanner := bufio.NewScanner(a.stderr)
	for scanner.Scan() {
		if !a.connected.Load() {
			break
		}
		line := scanner.Text()

		// Skip informational hook messages that are not real errors
		if strings.HasPrefix(line, "[PreToolUseHook]") || strings.HasPrefix(line, "[PostToolUseHook]") {
			continue
		}

		// Skip npm non-error output (npm writes warn/info to stderr)
		if strings.HasPrefix(line, "npm warn") || strings.HasPrefix(line, "npm info") || strings.HasPrefix(line, "npm notice") {
			continue
		}

		// Skip claude-code-acp debug dumps: indented object lines and multiline error headers
		// e.g. "Error handling request {", "  method: 'session/prompt',", "    sessionId: '...'", "}"
		if strings.HasPrefix(line, "Error handling request") ||
			strings.HasPrefix(line, "  ") ||
			line == "}" ||
			line == "{" {
			logger.Debug("[%s] stderr (suppressed): %s", logger.ModACP, line)
			continue
		}

		// Forward genuine stderr errors to Web UI
		a.emitMessage(Message{
			Type:    MessageTypeError,
			Content: line,
			Meta: map[string]interface{}{
				"protocol": "acp",
				"source":   "stderr",
			},
		})
	}
}

// handleMessage processes incoming JSON-RPC messages
func (a *ACPAdapter) handleMessage(msg map[string]interface{}) {
	method, _ := msg["method"].(string)

	switch method {
	case "session/update":
		a.handleSessionUpdate(msg)
	case "session/request_permission":
		a.handlePermissionRequest(msg)
	case "fs/read_text_file":
		a.handleFileRead(msg)
	case "fs/write_text_file":
		a.handleFileWrite(msg)
	case "terminal/create":
		a.handleTerminalCreate(msg)
	case "terminal/wait_for_exit":
		a.handleTerminalWaitForExit(msg)
	case "terminal/output":
		// This is a request from agent to get output - respond with stored output
		a.handleTerminalOutputRequest(msg)
	case "terminal/release":
		// Release terminal resources
		a.handleTerminalRelease(msg)
	default:
		// Handle responses
		if _, ok := msg["result"]; ok {
			a.handleResponse(msg)
		} else if _, ok := msg["error"]; ok {
			a.handleError(msg)
		} else {
			// Unknown request
			logger.Warn("[%s] Unknown method: %s", logger.ModACP, method)
		}
	}
}

// handleSessionUpdate processes session/update notifications
func (a *ACPAdapter) handleSessionUpdate(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		return
	}

	// Collect all updates to process
	var updates []map[string]interface{}

	// Handle single update format (used by claude-code-acp)
	if update, ok := params["update"].(map[string]interface{}); ok {
		updates = append(updates, update)
	} else if updatesArr, ok := params["updates"].([]interface{}); ok {
		// Handle array format - process ALL updates
		for _, u := range updatesArr {
			if update, ok := u.(map[string]interface{}); ok {
				updates = append(updates, update)
			}
		}
	}

	for _, update := range updates {
		a.processSessionUpdate(update)
	}
}

// processSessionUpdate processes a single session update entry
func (a *ACPAdapter) processSessionUpdate(update map[string]interface{}) {

	// Get update type - ACP uses "sessionUpdate" field
	updateType, _ := update["sessionUpdate"].(string)

	switch updateType {
	case "agent_message_chunk":
		// Content is an object with type and text fields
		contentObj, ok := update["content"].(map[string]interface{})
		if !ok {
			return
		}
		text, _ := contentObj["text"].(string)

		// Track output tokens
		a.outputTokens.Add(estimateTokens(text))

		a.emitMessage(Message{
			Type:    MessageTypeContent,
			Content: text,
			Meta: map[string]interface{}{
				"protocol": "acp",
			},
		})

	case "agent_thought_chunk":
		contentObj, ok := update["content"].(map[string]interface{})
		if !ok {
			return
		}
		text, _ := contentObj["text"].(string)

		// Track output tokens for thinking as well
		a.outputTokens.Add(estimateTokens(text))

		a.emitMessage(Message{
			Type:    MessageTypeThought,
			Content: text,
			Meta: map[string]interface{}{
				"protocol": "acp",
			},
		})

	case "tool_call":
		// ACP uses toolCallId and title, not id and name
		toolCallID, _ := update["toolCallId"].(string)
		if toolCallID == "" {
			toolCallID, _ = update["id"].(string) // fallback
		}
		title, _ := update["title"].(string)
		if title == "" {
			title, _ = update["name"].(string) // fallback
		}
		status, _ := update["status"].(string)
		if status == "" {
			status = "pending"
		}
		a.emitMessage(Message{
			Type: MessageTypeToolCall,
			Content: ToolCall{
				ID:     toolCallID,
				Name:   title,
				Status: status,
			},
			Meta: map[string]interface{}{
				"protocol": "acp",
			},
		})

	case "tool_call_update":
		toolCallID, _ := update["toolCallId"].(string)
		if toolCallID == "" {
			toolCallID, _ = update["id"].(string) // fallback
		}
		status, _ := update["status"].(string)
		a.emitMessage(Message{
			Type: MessageTypeToolCall,
			Content: ToolCall{
				ID:     toolCallID,
				Status: status,
				Result: update["result"],
			},
			Meta: map[string]interface{}{
				"protocol": "acp",
			},
		})

	case "end_turn":
		// Send status update
		a.emitMessage(Message{
			Type:    MessageTypeStatus,
			Content: StatusIdle,
			Meta: map[string]interface{}{
				"protocol": "acp",
			},
		})

		// Send usage statistics
		inputTokens := a.inputTokens.Load()
		outputTokens := a.outputTokens.Load()
		a.emitMessage(Message{
			Type: MessageTypeUsage,
			Content: UsageStats{
				InputTokens:   int(inputTokens),
				OutputTokens:  int(outputTokens),
				CacheCreation: 0, // Not available from ACP
				CacheRead:     0, // Not available from ACP
				ContextSize:   int(inputTokens + outputTokens),
			},
			Meta: map[string]interface{}{
				"protocol": "acp",
			},
		})
	}
}

// handlePermissionRequest processes permission requests
func (a *ACPAdapter) handlePermissionRequest(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		return
	}

	// ACP permission request format
	// id is at root level for JSON-RPC request
	// Preserve the original ID type (string or number) for correct JSON-RPC response
	var id interface{}
	if idVal, ok := msg["id"]; ok {
		id = idVal // Keep original type (string, float64, etc.)
	}

	// Tool call info
	toolCall, _ := params["toolCall"].(map[string]interface{})
	toolCallID, _ := toolCall["toolCallId"].(string)
	title, _ := toolCall["title"].(string)
	rawInput, _ := toolCall["rawInput"].(map[string]interface{})

	// Options - array of objects with optionId
	optionsRaw, _ := params["options"].([]interface{})
	optionStrs := make([]string, 0, len(optionsRaw))
	for _, opt := range optionsRaw {
		if optMap, ok := opt.(map[string]interface{}); ok {
			if optionID, ok := optMap["optionId"].(string); ok {
				optionStrs = append(optionStrs, optionID)
			}
		} else if optStr, ok := opt.(string); ok {
			// Fallback for string format
			optionStrs = append(optionStrs, optStr)
		}
	}

	// Determine risk based on tool type
	risk := "medium"
	if title != "" {
		// Commands with rm, sudo, etc. are high risk
		if containsDangerousCommand(title) {
			risk = "high"
		}
	}

	// Use toolCallId as the permission ID if no id provided
	if id == nil {
		id = toolCallID
	}

	logger.Debug("[%s] Permission request: tool=%s, options=%v", logger.ModACP, title, optionStrs)

	a.emitMessage(Message{
		Type: MessageTypePermission,
		Content: PermissionRequest{
			ID:          id,
			ToolName:    title,
			ToolInput:   rawInput,
			Description: title,
			Risk:        risk,
			Options:     optionStrs,
		},
		Meta: map[string]interface{}{
			"protocol": "acp",
		},
	})
}

func containsDangerousCommand(cmd string) bool {
	dangerous := []string{"rm ", "sudo ", "chmod ", "chown ", "mkfs", "dd ", "> /dev/", "shutdown", "reboot"}
	for _, d := range dangerous {
		if strings.Contains(cmd, d) {
			return true
		}
	}
	return false
}

// handleFileRead processes file read requests
func (a *ACPAdapter) handleFileRead(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		logger.Debug("[%s] Invalid fs/read_text_file params", logger.ModACP)
		return
	}

	// Get request ID for response
	var reqID interface{}
	if idVal, ok := msg["id"]; ok {
		reqID = idVal
	}

	path, _ := params["path"].(string)

	// Read file content via safe filesystem
	var content []byte
	var err error
	if a.safeFS != nil {
		content, err = a.safeFS.SafeReadFile(path)
		if err != nil {
			logger.Warn("[%s] SECURITY: path_reject path=%q reason=%q", logger.ModACP, path, err.Error())
		}
	} else {
		content, err = os.ReadFile(path)
	}
	if err != nil {
		// Send error response
		errResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error": map[string]interface{}{
				"code":    -32603,
				"message": err.Error(),
			},
		}
		a.sendJSONRPC(errResp)
		return
	}

	// Send success response
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result": map[string]interface{}{
			"content": string(content),
		},
	}
	a.sendJSONRPC(response)
}

// handleFileWrite processes file write requests
func (a *ACPAdapter) handleFileWrite(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		logger.Debug("[%s] Invalid fs/write_text_file params", logger.ModACP)
		return
	}

	// Get request ID for response
	var reqID interface{}
	if idVal, ok := msg["id"]; ok {
		reqID = idVal
	}

	path, _ := params["path"].(string)
	content, _ := params["content"].(string)

	// Write file content via safe filesystem
	var writeErr error
	if a.safeFS != nil {
		writeErr = a.safeFS.SafeWriteFile(path, []byte(content))
		if writeErr != nil {
			logger.Warn("[%s] SECURITY: path_reject path=%q reason=%q", logger.ModACP, path, writeErr.Error())
		}
	} else {
		// Fallback: create directory and write directly
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			os.MkdirAll(dir, 0755)
		}
		writeErr = os.WriteFile(path, []byte(content), 0644)
	}
	if writeErr != nil {
		// Send error response
		errResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error": map[string]interface{}{
				"code":    -32603,
				"message": writeErr.Error(),
			},
		}
		a.sendJSONRPC(errResp)
		return
	}

	// Send success response
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result":  map[string]interface{}{},
	}
	a.sendJSONRPC(response)
}

// handleTerminalCreate processes terminal creation and command execution requests
func (a *ACPAdapter) handleTerminalCreate(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		logger.Debug("[%s] Invalid terminal/create params", logger.ModACP)
		return
	}

	// Get request ID for response
	var reqID interface{}
	if idVal, ok := msg["id"]; ok {
		reqID = idVal
	}

	// Parse parameters
	command, _ := params["command"].(string)
	_ = params["sessionId"] // consumed by ACP protocol but not used locally
	outputByteLimit := 32000 // default
	if limit, ok := params["outputByteLimit"].(float64); ok {
		outputByteLimit = int(limit)
	}

	// Parse environment variables
	env := os.Environ()
	if envVars, ok := params["env"].([]interface{}); ok {
		for _, e := range envVars {
			if envMap, ok := e.(map[string]interface{}); ok {
				name, _ := envMap["name"].(string)
				value, _ := envMap["value"].(string)
				if name != "" {
					env = append(env, fmt.Sprintf("%s=%s", name, value))
				}
			}
		}
	}

	// Generate terminal ID
	terminalID := fmt.Sprintf("term_%d", time.Now().UnixNano())

	// Create terminal state
	state := &terminalState{
		doneChan: make(chan struct{}),
	}
	a.terminalMu.Lock()
	a.terminals[terminalID] = state
	a.terminalMu.Unlock()

	// Send response with terminalId immediately
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result": map[string]interface{}{
			"terminalId": terminalID,
		},
	}
	a.sendJSONRPC(response)

	// Execute the command in background
	go a.executeTerminalCommand(terminalID, command, env, outputByteLimit)
}

// executeTerminalCommand runs a command and stores the result
func (a *ACPAdapter) executeTerminalCommand(terminalID, cmdStr string, env []string, outputLimit int) {
	// Validate command through security module
	if err := command.ValidateCommand(cmdStr); err != nil {
		logger.Warn("[%s] SECURITY: command_reject cmd=%q reason=%q", logger.ModACP, cmdStr, err.Error())
		// Store error as terminal result
		a.terminalMu.Lock()
		if state, ok := a.terminals[terminalID]; ok {
			state.output = fmt.Sprintf("Command rejected: %v", err)
			state.exitCode = 126 // Command not executable (security)
			state.done = true
			close(state.doneChan)
		}
		a.terminalMu.Unlock()
		return
	}

	// Execute command
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Env = env
	cmd.Dir = a.workDir

	output, err := cmd.CombinedOutput()

	// Truncate output if needed
	truncated := false
	if len(output) > outputLimit {
		output = output[:outputLimit]
		truncated = true
	}

	// Get exit status
	var exitCode int
	var signal string
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Store result in terminal state
	a.terminalMu.Lock()
	if state, ok := a.terminals[terminalID]; ok {
		state.output = string(output)
		state.exitCode = exitCode
		state.signal = signal
		state.truncated = truncated
		state.done = true
		close(state.doneChan)
	}
	a.terminalMu.Unlock()

	// Clean up old terminals after a delay
	go func() {
		time.Sleep(30 * time.Second)
		a.terminalMu.Lock()
		delete(a.terminals, terminalID)
		a.terminalMu.Unlock()
	}()
}

// handleTerminalWaitForExit handles terminal/wait_for_exit requests
func (a *ACPAdapter) handleTerminalWaitForExit(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		logger.Debug("[%s] Invalid terminal/wait_for_exit params", logger.ModACP)
		return
	}

	var reqID interface{}
	if idVal, ok := msg["id"]; ok {
		reqID = idVal
	}

	terminalID, _ := params["terminalId"].(string)

	// Wait for command to complete
	a.terminalMu.RLock()
	state, ok := a.terminals[terminalID]
	a.terminalMu.RUnlock()

	if !ok {
		// Terminal not found
		errResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error": map[string]interface{}{
				"code":    -32602,
				"message": "terminal not found",
			},
		}
		a.sendJSONRPC(errResp)
		return
	}

	// Wait for command to complete
	<-state.doneChan

	// Send response with exit status
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result": map[string]interface{}{
			"exitStatus": map[string]interface{}{
				"exitCode": state.exitCode,
			},
		},
	}
	a.sendJSONRPC(response)
}

// handleTerminalOutputRequest handles terminal/output requests from agent
func (a *ACPAdapter) handleTerminalOutputRequest(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		logger.Debug("[%s] Invalid terminal/output params", logger.ModACP)
		return
	}

	var reqID interface{}
	if idVal, ok := msg["id"]; ok {
		reqID = idVal
	}

	terminalID, _ := params["terminalId"].(string)

	// Get terminal state
	a.terminalMu.RLock()
	state, ok := a.terminals[terminalID]
	a.terminalMu.RUnlock()

	if !ok {
		// Terminal not found
		errResp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      reqID,
			"error": map[string]interface{}{
				"code":    -32602,
				"message": "terminal not found",
			},
		}
		a.sendJSONRPC(errResp)
		return
	}

	// Wait for command to complete if not done
	if !state.done {
		<-state.doneChan
	}

	// Send response with output
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result": map[string]interface{}{
			"output":    state.output,
			"truncated": state.truncated,
			"exitStatus": map[string]interface{}{
				"exitCode": state.exitCode,
			},
		},
	}
	a.sendJSONRPC(response)
}

// handleTerminalRelease handles terminal/release requests - releases terminal resources
func (a *ACPAdapter) handleTerminalRelease(msg map[string]interface{}) {
	params, ok := msg["params"].(map[string]interface{})
	if !ok {
		logger.Debug("[%s] Invalid terminal/release params", logger.ModACP)
		return
	}

	var reqID interface{}
	if idVal, ok := msg["id"]; ok {
		reqID = idVal
	}

	terminalID, _ := params["terminalId"].(string)

	// Remove terminal from map
	a.terminalMu.Lock()
	delete(a.terminals, terminalID)
	a.terminalMu.Unlock()

	// Send success response
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      reqID,
		"result":  map[string]interface{}{},
	}
	a.sendJSONRPC(response)
}

// handleResponse processes JSON-RPC responses
func (a *ACPAdapter) handleResponse(msg map[string]interface{}) {
	result, _ := msg["result"].(map[string]interface{})

	// Handle session/new response (contains sessionId)
	if sessionID, ok := result["sessionId"].(string); ok {
		a.sessionID = sessionID
		logger.Info("[%s] Session created: %s", logger.ModACP, sessionID)

		// Send initialized/ready status to signal successful initialization
		a.emitMessage(Message{
			Type:    MessageTypeStatus,
			Content: StatusIdle,
			Meta: map[string]interface{}{
				"protocol":  "acp",
				"sessionId": sessionID,
			},
		})
		return
	}

	// Handle initialize response (contains agentInfo, capabilities, and authMethods)
	if agentInfo, ok := result["agentInfo"].(map[string]interface{}); ok {
		name, _ := agentInfo["name"].(string)
		version, _ := agentInfo["version"].(string)
		logger.Info("[%s] Connected to agent: %s v%s", logger.ModACP, name, version)

		// Signal that initialize is done so session/new can be sent
		select {
		case <-a.initDone:
			// already closed
		default:
			close(a.initDone)
		}
		return
	}
}

// handleError processes JSON-RPC errors
func (a *ACPAdapter) handleError(msg map[string]interface{}) {
	errObj, _ := msg["error"].(map[string]interface{})
	code, _ := errObj["code"].(float64)
	message, _ := errObj["message"].(string)

	// Include error.data for richer diagnostics (e.g. 429 rate-limit details)
	detail := message
	if data, ok := errObj["data"].(string); ok && data != "" {
		detail = message + ": " + data
	}

	logger.Error("[%s] Error %d: %s", logger.ModACP, int(code), detail)

	a.emitMessage(Message{
		Type:    MessageTypeError,
		Content: detail,
		Meta: map[string]interface{}{
			"protocol": "acp",
			"code":     int(code),
		},
	})
}

// emitMessage sends a message to the callback
func (a *ACPAdapter) emitMessage(msg Message) {
	if a.callback != nil {
		a.callback(msg)
	}
}

// sendJSONRPC sends a JSON-RPC message
func (a *ACPAdapter) sendJSONRPC(msg interface{}) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	_, err = a.stdin.Write(append(data, '\n'))
	return err
}

// nextRequestID generates the next request ID
func (a *ACPAdapter) nextRequestID() int64 {
	return a.requestID.Add(1)
}

// estimateTokens provides a rough token count estimation
// Uses approximately 4 characters per token (common for English text)
func estimateTokens(text string) int64 {
	if len(text) == 0 {
		return 0
	}
	// Rough estimation: ~4 characters per token
	return int64((len(text) + 3) / 4)
}
