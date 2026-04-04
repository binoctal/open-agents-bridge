package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-agents/open-agents-bridge/internal/protocol"
)

// Config for demo
type DemoConfig struct {
	ServerURL  string
	DeviceID   string
	UserID     string
	DeviceToken string
	WorkDir    string
	Command    string
	Args       []string
}

// Message represents a WebSocket message
type Message struct {
	Type      string      `json:"type"`
	Payload   interface{} `json:"payload"`
	Timestamp int64       `json:"timestamp"`
}

// WebForwarder handles forwarding messages to Web UI via WebSocket
type WebForwarder struct {
	conn       *websocket.Conn
	deviceID   string
	sessionID  string
	pending    map[string]chan bool // permission ID -> response channel
	mu         sync.Mutex
}

// NewWebForwarder creates a new forwarder
func NewWebForwarder(serverURL, deviceID, userID, deviceToken string) (*WebForwarder, error) {
	// Build WebSocket URL
	wsURL := strings.Replace(serverURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	wsURL = fmt.Sprintf("%s/ws/%s?type=demo&deviceId=%s&token=%s", wsURL, userID, deviceID, deviceToken)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %w", err)
	}

	f := &WebForwarder{
		conn:    conn,
		deviceID: deviceID,
		pending: make(map[string]chan bool),
	}

	go f.readLoop()

	return f, nil
}

// readLoop reads messages from WebSocket
func (f *WebForwarder) readLoop() {
	for {
		_, data, err := f.conn.ReadMessage()
		if err != nil {
			log.Printf("WebSocket read error: %v", err)
			return
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "permission:response":
			f.handlePermissionResponse(msg)
		}
	}
}

// handlePermissionResponse handles permission response from Web UI
func (f *WebForwarder) handlePermissionResponse(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}

	// Get permission ID
	var id string
	switch v := payload["id"].(type) {
	case string:
		id = v
	case float64:
		id = fmt.Sprintf("%d", int(v))
	}

	approved, _ := payload["approved"].(bool)

	f.mu.Lock()
	if ch, exists := f.pending[id]; exists {
		ch <- approved
		delete(f.pending, id)
	}
	f.mu.Unlock()

	log.Printf("[WebForwarder] Permission %s response: approved=%v", id, approved)
}

// ForwardPermission forwards a permission request to Web UI and waits for response
func (f *WebForwarder) ForwardPermission(permReq protocol.PermissionRequest) bool {
	// Generate ID if not present
	id := fmt.Sprintf("%v", permReq.ID)
	if id == "" || id == "<nil>" {
		id = fmt.Sprintf("perm_%d", time.Now().UnixNano())
	}

	// Create response channel
	respCh := make(chan bool, 1)
	f.mu.Lock()
	f.pending[id] = respCh
	f.mu.Unlock()

	// Send permission request to Web UI
	msg := Message{
		Type: "permission:request",
		Payload: map[string]interface{}{
			"sessionId":   f.sessionID,
			"deviceId":    f.deviceID,
			"id":          id,
			"toolName":    permReq.ToolName,
			"toolInput":   permReq.ToolInput,
			"description": permReq.Description,
			"risk":        permReq.Risk,
			"options":     permReq.Options,
			"protocol":    "demo",
		},
		Timestamp: time.Now().UnixMilli(),
	}

	data, _ := json.Marshal(msg)
	if err := f.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("Failed to send permission request: %v", err)
		return false
	}

	log.Printf("[WebForwarder] Forwarded permission request: %s - %s", permReq.ToolName, permReq.Description)

	// Wait for response with timeout
	select {
	case approved := <-respCh:
		return approved
	case <-time.After(60 * time.Second):
		log.Printf("[WebForwarder] Permission request timeout: %s", id)
		f.mu.Lock()
		delete(f.pending, id)
		f.mu.Unlock()
		return false
	}
}

// SendMessage sends a message to Web UI
func (f *WebForwarder) SendMessage(msgType string, payload interface{}) {
	msg := Message{
		Type:      msgType,
		Payload:   payload,
		Timestamp: time.Now().UnixMilli(),
	}

	data, _ := json.Marshal(msg)
	if f.conn != nil {
		f.conn.WriteMessage(websocket.TextMessage, data)
	}
}

// SetSessionID sets the current session ID
func (f *WebForwarder) SetSessionID(id string) {
	f.sessionID = id
}

// Close closes the connection
func (f *WebForwarder) Close() {
	if f.conn != nil {
		f.conn.Close()
	}
}

func main() {
	// Parse command line flags
	serverURL := flag.String("server", "", "WebSocket server URL (e.g., wss://api.example.com)")
	deviceID := flag.String("device-id", "demo-device", "Device ID")
	userID := flag.String("user-id", "", "User ID")
	deviceToken := flag.String("token", "", "Device token")
	workDir := flag.String("workdir", ".", "Working directory")
	cliType := flag.String("cli", "claude", "CLI type (claude, kiro, cline, codex, gemini)")
	headless := flag.Bool("headless", false, "Run without Web UI forwarding")
	flag.Parse()

	config := &DemoConfig{
		ServerURL:   *serverURL,
		DeviceID:    *deviceID,
		UserID:      *userID,
		DeviceToken: *deviceToken,
		WorkDir:     *workDir,
		Command:     *cliType,
	}

	// Create protocol manager
	manager := protocol.NewManager()

	// Web forwarder (optional)
	var forwarder *WebForwarder
	if !*headless && config.ServerURL != "" && config.UserID != "" {
		var err error
		forwarder, err = NewWebForwarder(config.ServerURL, config.DeviceID, config.UserID, config.DeviceToken)
		if err != nil {
			log.Printf("Warning: Could not connect to Web UI: %v", err)
			log.Println("Running in standalone mode...")
		} else {
			defer forwarder.Close()
			log.Println("Connected to Web UI for permission forwarding")
		}
	}

	// Session ID for this demo
	sessionID := fmt.Sprintf("demo_%d", time.Now().UnixNano())
	if forwarder != nil {
		forwarder.SetSessionID(sessionID)
	}

	// Subscribe to messages
	manager.Subscribe(func(msg protocol.Message) {
		switch msg.Type {
		case protocol.MessageTypeContent:
			content, _ := msg.Content.(string)
			fmt.Printf("\n[Content] %s\n", content)
			if forwarder != nil {
				forwarder.SendMessage("chat:response", map[string]interface{}{
					"sessionId": sessionID,
					"deviceId":  config.DeviceID,
					"content":   content,
					"protocol":  manager.GetProtocolName(),
				})
			}

		case protocol.MessageTypeThought:
			thought, _ := msg.Content.(string)
			fmt.Printf("\n[Thought] %s\n", thought)
			if forwarder != nil {
				forwarder.SendMessage("chat:thought", map[string]interface{}{
					"sessionId": sessionID,
					"deviceId":  config.DeviceID,
					"content":   thought,
					"protocol":  manager.GetProtocolName(),
				})
			}

		case protocol.MessageTypeToolCall:
			toolCall, ok := msg.Content.(protocol.ToolCall)
			if !ok {
				// Try map format
				if m, ok := msg.Content.(map[string]interface{}); ok {
					toolCall = protocol.ToolCall{
						ID:     fmt.Sprintf("%v", m["id"]),
						Name:   fmt.Sprintf("%v", m["name"]),
						Status: fmt.Sprintf("%v", m["status"]),
					}
				}
			}
			fmt.Printf("\n[Tool] %s: %s\n", toolCall.Name, toolCall.Status)
			if forwarder != nil {
				forwarder.SendMessage("tool:call", map[string]interface{}{
					"sessionId": sessionID,
					"deviceId":  config.DeviceID,
					"toolCall":  toolCall,
					"protocol":  manager.GetProtocolName(),
				})
			}

		case protocol.MessageTypePermission:
			permReq, ok := msg.Content.(protocol.PermissionRequest)
			if !ok {
				// Try map format
				if m, ok := msg.Content.(map[string]interface{}); ok {
					permReq = protocol.PermissionRequest{
						ToolName:    fmt.Sprintf("%v", m["tool_name"]),
						Description: fmt.Sprintf("%v", m["description"]),
						Risk:        fmt.Sprintf("%v", m["risk"]),
					}
					if id, ok := m["id"]; ok {
						permReq.ID = id
					}
				}
			}

			fmt.Printf("\n[Permission] %s wants to: %s (risk: %s)\n", permReq.ToolName, permReq.Description, permReq.Risk)

			// Forward to Web UI if available
			if forwarder != nil {
				approved := forwarder.ForwardPermission(permReq)
				// Send response back to CLI
				manager.SendMessage(protocol.Message{
					Type: protocol.MessageTypePermission,
					Content: protocol.PermissionResponse{
						ID:       permReq.ID,
						OptionID: "allow_once",
					},
				})
				fmt.Printf("[Permission] Web UI response: %v\n", approved)
			} else {
				// Interactive mode - ask user
				fmt.Print("Approve? (y/n): ")
				reader := bufio.NewReader(os.Stdin)
				response, _ := reader.ReadString('\n')
				approved := strings.TrimSpace(strings.ToLower(response)) == "y"

				// Send response back to CLI
				manager.SendMessage(protocol.Message{
					Type: protocol.MessageTypePermission,
					Content: protocol.PermissionResponse{
						ID:       permReq.ID,
						OptionID: "allow_once",
					},
				})
				fmt.Printf("[Permission] User response: %v\n", approved)
			}

		case protocol.MessageTypeStatus:
			status, _ := msg.Content.(protocol.AgentStatus)
			fmt.Printf("\n[Status] %s\n", status)
			if forwarder != nil {
				forwarder.SendMessage("agent:status", map[string]interface{}{
					"sessionId": sessionID,
					"deviceId":  config.DeviceID,
					"status":    status,
					"protocol":  manager.GetProtocolName(),
				})
			}

		case protocol.MessageTypeUsage:
			if usage, ok := msg.Content.(protocol.UsageStats); ok {
				fmt.Printf("\n[Usage] Input: %d, Output: %d tokens\n", usage.InputTokens, usage.OutputTokens)
				if forwarder != nil {
					forwarder.SendMessage("session:usage", map[string]interface{}{
						"sessionId": sessionID,
						"deviceId":  config.DeviceID,
						"usage": map[string]interface{}{
							"inputTokens":   usage.InputTokens,
							"outputTokens":  usage.OutputTokens,
							"cacheCreation": usage.CacheCreation,
							"cacheRead":     usage.CacheRead,
							"contextSize":   usage.ContextSize,
						},
						"protocol": manager.GetProtocolName(),
					})
				}
			}

		case protocol.MessageTypeError:
			fmt.Printf("\n[Error] %v\n", msg.Content)
			if forwarder != nil {
				forwarder.SendMessage("session:error", map[string]interface{}{
					"sessionId": sessionID,
					"deviceId":  config.DeviceID,
					"error":     msg.Content,
					"protocol":  manager.GetProtocolName(),
				})
			}
		}

		// Show protocol info
		if proto, ok := msg.Meta["protocol"].(string); ok {
			fmt.Printf("  (via %s)\n", proto)
		}
	})

	// Prepare adapter config
	adapterArgs := []string{}
	if config.Command == "claude" {
		adapterArgs = []string{"--experimental-acp"}
	}

	adapterConfig := protocol.AdapterConfig{
		WorkDir: config.WorkDir,
		Command: config.Command,
		Args:    adapterArgs,
		Cols:    120,
		Rows:    30,
	}

	// Connect with auto-detection
	fmt.Printf("Connecting to %s in %s...\n", config.Command, config.WorkDir)
	if err := manager.Connect(adapterConfig); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}

	fmt.Printf("Connected using protocol: %s\n", manager.GetProtocolName())
	fmt.Printf("Session ID: %s\n", sessionID)
	fmt.Println("\nType your message and press Enter. Press Ctrl+C to exit.")
	fmt.Println("=" + strings.Repeat("=", 60))

	// Send session started notification
	if forwarder != nil {
		forwarder.SendMessage("session:started", map[string]interface{}{
			"sessionId": sessionID,
			"deviceId":  config.DeviceID,
			"cliType":   config.Command,
			"workDir":   config.WorkDir,
		})
	}

	// Handle user input in goroutine
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Print("\n> ")
			input, err := reader.ReadString('\n')
			if err != nil {
				break
			}

			input = strings.TrimSpace(input)
			if input == "" {
				continue
			}

			// Send message to CLI
			manager.SendMessage(protocol.Message{
				Type:    protocol.MessageTypeContent,
				Content: input,
			})

			// Also forward to Web UI
			if forwarder != nil {
				forwarder.SendMessage("chat:send", map[string]interface{}{
					"sessionId": sessionID,
					"deviceId":  config.DeviceID,
					"content":   input,
				})
			}
		}
	}()

	// Wait for interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n\nDisconnecting...")

	// Send session stopped notification
	if forwarder != nil {
		forwarder.SendMessage("session:stopped", map[string]interface{}{
			"sessionId": sessionID,
			"deviceId":  config.DeviceID,
		})
	}

	manager.Disconnect()
	fmt.Println("Done.")
}
