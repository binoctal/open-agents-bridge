package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestACPAdapter(t *testing.T) {
	adapter := NewACPAdapter()

	if adapter.Name() != "acp" {
		t.Errorf("Expected name 'acp', got '%s'", adapter.Name())
	}

	if !adapter.SupportsPermissions() {
		t.Error("ACP should support permissions")
	}

	if !adapter.SupportsFileOps() {
		t.Error("ACP should support file operations")
	}

	if !adapter.SupportsToolCalls() {
		t.Error("ACP should support tool calls")
	}
}

func TestPTYAdapter(t *testing.T) {
	adapter := NewPTYAdapter()

	if adapter.Name() != "pty" {
		t.Errorf("Expected name 'pty', got '%s'", adapter.Name())
	}

	if adapter.SupportsPermissions() {
		t.Error("PTY should not support permissions")
	}

	if adapter.SupportsFileOps() {
		t.Error("PTY should not support file operations")
	}

	if adapter.SupportsToolCalls() {
		t.Error("PTY should not support tool calls")
	}
}

func TestProtocolManager(t *testing.T) {
	manager := NewManager()

	if manager.IsConnected() {
		t.Error("Manager should not be connected initially")
	}

	if manager.GetProtocolName() != "none" {
		t.Errorf("Expected protocol 'none', got '%s'", manager.GetProtocolName())
	}
}

func TestMessageTypes(t *testing.T) {
	msg := Message{
		Type:    MessageTypeContent,
		Content: "test",
		Meta: map[string]interface{}{
			"protocol": "acp",
		},
	}

	if msg.Type != MessageTypeContent {
		t.Errorf("Expected type 'content', got '%s'", msg.Type)
	}

	if msg.Meta["protocol"] != "acp" {
		t.Error("Meta protocol should be 'acp'")
	}
}

func TestPermissionRequest(t *testing.T) {
	req := PermissionRequest{
		ID:          "test-123",
		ToolName:    "execute_command",
		ToolInput:   map[string]interface{}{"command": "ls"},
		Description: "List files",
		Risk:        "low",
		Options:     []string{"allow_once", "reject_once"},
	}

	if req.ID != "test-123" {
		t.Errorf("Expected ID 'test-123', got '%s'", req.ID)
	}

	if req.Risk != "low" {
		t.Errorf("Expected risk 'low', got '%s'", req.Risk)
	}

	if len(req.Options) != 2 {
		t.Errorf("Expected 2 options, got %d", len(req.Options))
	}
}

func TestToolCall(t *testing.T) {
	call := ToolCall{
		ID:     "call-456",
		Name:   "read_file",
		Input:  map[string]interface{}{"path": "/tmp/test.txt"},
		Status: "pending",
	}

	if call.Status != "pending" {
		t.Errorf("Expected status 'pending', got '%s'", call.Status)
	}
}

func TestToolCallErrorFieldSerialization(t *testing.T) {
	// The web Error section only renders when the wire carries `error`; a
	// failed call must serialize it and a clean call must omit the key.
	call := ToolCall{ID: "c1", Name: "Bash", Status: "failed", Error: "exit status 1"}
	data, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"error":"exit status 1"`) {
		t.Errorf("expected error field in JSON, got %s", data)
	}

	clean := ToolCall{ID: "c2", Name: "Bash", Status: "completed", Result: "ok"}
	data2, _ := json.Marshal(clean)
	if strings.Contains(string(data2), `"error"`) {
		t.Errorf("clean call must omit error, got %s", data2)
	}
}

func TestToolCallErrorExtraction(t *testing.T) {
	// Top-level error string wins alone; ACP error content blocks are joined.
	got := toolCallError(map[string]interface{}{
		"status": "failed",
		"error":  "boom",
	})
	if got != "boom" {
		t.Errorf("expected top-level error, got %q", got)
	}

	got = toolCallError(map[string]interface{}{
		"status": "failed",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "data": "noise"},
			map[string]interface{}{"type": "error", "data": "line 1"},
			map[string]interface{}{"type": "error", "data": "line 2"},
		},
	})
	if got != "line 1\nline 2" {
		t.Errorf("expected joined content-block errors, got %q", got)
	}

	if toolCallError(map[string]interface{}{"status": "completed"}) != "" {
		t.Error("no error input must yield empty string")
	}
}

func TestAgentStatus(t *testing.T) {
	statuses := []AgentStatus{
		StatusIdle,
		StatusThinking,
		StatusStreaming,
		StatusToolExecuting,
		StatusPermissionPending,
	}

	for _, status := range statuses {
		if status == "" {
			t.Error("Status should not be empty")
		}
	}
}

// Integration test (requires actual CLI tool)
func TestACPIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	manager := NewManager()
	received := make(chan Message, 10)

	manager.Subscribe(func(msg Message) {
		received <- msg
	})

	config := AdapterConfig{
		WorkDir: ".",
		Command: "echo", // Use echo as a simple test
		Args:    []string{"test"},
		Cols:    120,
		Rows:    30,
	}

	if err := manager.Connect(config); err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}

	t.Cleanup(func() { _ = manager.Disconnect() })

	// Should fallback to PTY for echo command
	if manager.GetProtocolName() != "pty" {
		t.Errorf("Expected PTY protocol for echo, got '%s'", manager.GetProtocolName())
	}

	// Wait for output. The abandoned ACP attempt (echo dies instantly) now
	// reports its death as a status message with exit_code before the PTY
	// fallback delivers the content — skip that expected noise.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-received:
			if msg.Type == MessageTypeContent {
				return
			}
		case <-deadline:
			t.Error("Timeout waiting for content message")
			return
		}
	}
}
