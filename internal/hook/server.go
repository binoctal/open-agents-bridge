package hook

import (
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// HookServer receives notifications from CLI tools via hooks
type HookServer struct {
	port     int
	server   *http.Server
	onEvent  func(event HookEvent)
}

// HookEvent represents a hook notification from CLI
type HookEvent struct {
	Type      string                 `json:"type"`
	SessionID string                 `json:"session_id"`
	Data      map[string]interface{} `json:"data"`
	Timestamp int64                  `json:"timestamp"`
}

// NewHookServer creates a new hook server
func NewHookServer(onEvent func(HookEvent)) *HookServer {
	return &HookServer{
		onEvent: onEvent,
	}
}

// Start starts the hook server on a random available port
func (h *HookServer) Start() error {
	mux := http.NewServeMux()
	
	// Hook endpoint
	mux.HandleFunc("/hook/session-start", h.handleSessionStart)
	mux.HandleFunc("/hook/tool-call", h.handleToolCall)
	mux.HandleFunc("/hook/session-end", h.handleSessionEnd)
	mux.HandleFunc("/hook/permission-request", h.handlePermissionRequest)
	
	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	
	// Find available port
	listener, err := findAvailablePort()
	if err != nil {
		return err
	}
	
	h.port = listener.Addr().(*net.TCPAddr).Port
	
	h.server = &http.Server{
		Handler: mux,
	}
	
	go func() {
		if err := h.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("[%s] Hook server error: %v", logger.ModBridge, err)
		}
	}()
	
	logger.Info("[%s] Hook server started on port %d", logger.ModBridge, h.port)
	return nil
}

// Stop stops the hook server
func (h *HookServer) Stop() error {
	if h.server != nil {
		return h.server.Close()
	}
	return nil
}

// Port returns the server port
func (h *HookServer) Port() int {
	return h.port
}

func (h *HookServer) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var event HookEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	
	event.Type = "session:start"
	event.Timestamp = time.Now().UnixMilli()
	
	if h.onEvent != nil {
		h.onEvent(event)
	}
	
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *HookServer) handleToolCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var event HookEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	
	event.Type = "tool:call"
	event.Timestamp = time.Now().UnixMilli()
	
	if h.onEvent != nil {
		h.onEvent(event)
	}
	
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *HookServer) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var event HookEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	
	event.Type = "session:end"
	event.Timestamp = time.Now().UnixMilli()
	
	if h.onEvent != nil {
		h.onEvent(event)
	}
	
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *HookServer) handlePermissionRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var event HookEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	event.Type = "permission:request"
	event.Timestamp = time.Now().UnixMilli()

	if h.onEvent != nil {
		h.onEvent(event)
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func findAvailablePort() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
