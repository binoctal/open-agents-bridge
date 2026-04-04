package permission

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

const SocketName = "open-agents-bridge.sock"

// GetSocketPath returns the platform-specific socket path
func GetSocketPath() string {
	if dir := os.Getenv("OPEN_AGENTS_SOCKET_DIR"); dir != "" {
		return filepath.Join(dir, SocketName)
	}
	return filepath.Join(os.TempDir(), SocketName)
}

// HookRequest is sent from hook script to bridge
type HookRequest struct {
	Type      string         `json:"type"`
	ToolName  string         `json:"toolName"`
	ToolInput map[string]any `json:"toolInput"`
	SessionID string         `json:"sessionId"`
}

// HookResponse is sent from bridge to hook script
type HookResponse struct {
	Approved bool   `json:"approved"`
	Message  string `json:"message,omitempty"`
}

// Server listens for hook requests via Unix socket
type Server struct {
	listener net.Listener
	handler  *Handler
	done     chan struct{}
}

func NewServer(handler *Handler) *Server {
	return &Server{
		handler: handler,
		done:    make(chan struct{}),
	}
}

func (s *Server) Start() error {
	socketPath := GetSocketPath()
	os.Remove(socketPath)

	var err error
	s.listener, err = net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	go s.acceptLoop()
	return nil
}

func (s *Server) Stop() {
	close(s.done)
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(GetSocketPath())
}

func (s *Server) acceptLoop() {
	for {
		select {
		case <-s.done:
			return
		default:
			conn, err := s.listener.Accept()
			if err != nil {
				continue
			}
			go s.handleConn(conn)
		}
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}

	var req HookRequest
	if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
		logger.Warn("[%s] Failed to parse permission request: %v", logger.ModPermission, err)
		return
	}

	permReq := Request{
		ID:             fmt.Sprintf("perm_%d_%d", os.Getpid(), time.Now().UnixNano()),
		SessionID:      req.SessionID,
		PermissionType: toolToPermissionType(req.ToolName),
		Description:    buildDescription(req.ToolName, req.ToolInput),
		Detail:         req.ToolInput,
		Risk:           classifyRisk(req.ToolName),
		Timeout:        60,
	}

	approved, _ := s.handler.Submit(permReq)

	resp := HookResponse{Approved: approved}
	data, _ := json.Marshal(resp)
	conn.Write(append(data, '\n'))
}

func toolToPermissionType(toolName string) string {
	switch toolName {
	case "fs_read":
		return "file:read"
	case "fs_write":
		return "file:write"
	case "execute_bash":
		return "command:exec"
	case "use_aws":
		return "aws:api"
	default:
		return "tool:" + toolName
	}
}

func buildDescription(toolName string, input map[string]any) string {
	switch toolName {
	case "fs_write":
		if path, ok := input["path"].(string); ok {
			return fmt.Sprintf("Write to file: %s", path)
		}
	case "execute_bash":
		if cmd, ok := input["command"].(string); ok {
			return fmt.Sprintf("Execute command: %s", cmd)
		}
	case "use_aws":
		if svc, ok := input["service_name"].(string); ok {
			op, _ := input["operation_name"].(string)
			return fmt.Sprintf("AWS %s: %s", svc, op)
		}
	}
	return fmt.Sprintf("Use tool: %s", toolName)
}

func classifyRisk(toolName string) string {
	switch toolName {
	case "execute_bash", "use_aws":
		return "high"
	case "fs_write":
		return "medium"
	default:
		return "low"
	}
}
