package protocol

import (
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

	// Should fallback to PTY for echo command
	if manager.GetProtocolName() != "pty" {
		t.Errorf("Expected PTY protocol for echo, got '%s'", manager.GetProtocolName())
	}

	// Wait for output
	select {
	case msg := <-received:
		if msg.Type != MessageTypeContent {
			t.Errorf("Expected content message, got '%s'", msg.Type)
		}
	case <-time.After(2 * time.Second):
		t.Error("Timeout waiting for message")
	}

	manager.Disconnect()
}
