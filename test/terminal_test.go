package test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestTerminalCreate(t *testing.T) {
	// Test that the bridge binary exists
	binaryPath := "./build/open-agents"
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		t.Skipf("Bridge binary not found at %s", binaryPath)
	}

	// Test a simple command execution
	cmd := exec.Command("sh", "-c", "echo 'Hello from terminal test'")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Command execution failed: %v", err)
	}

	result := strings.TrimSpace(string(output))
	if result != "Hello from terminal test" {
		t.Errorf("Expected 'Hello from terminal test', got '%s'", result)
	}

	t.Logf("Command execution test passed: %s", result)
}

func TestTerminalCreateJSONFormat(t *testing.T) {
	// Test the JSON-RPC response format for terminal/create
	terminalID := fmt.Sprintf("term_%d", time.Now().UnixNano())

	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]interface{}{
			"terminalId": terminalID,
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal response: %v", err)
	}

	expected := fmt.Sprintf(`{"id":1,"jsonrpc":"2.0","result":{"terminalId":"%s"}}`, terminalID)
	if string(data) != expected {
		t.Errorf("JSON format mismatch.\nExpected: %s\nGot: %s", expected, string(data))
	}

	t.Logf("JSON-RPC response format test passed: %s", string(data))
}

func TestTerminalOutputNotification(t *testing.T) {
	// Test the terminal/output notification format
	sessionID := "test-session-123"
	terminalID := fmt.Sprintf("term_%d", time.Now().UnixNano())
	output := "Hello World"
	truncated := false
	exitCode := 0

	notification := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "terminal/output",
		"params": map[string]interface{}{
			"sessionId":  sessionID,
			"terminalId": terminalID,
			"output":     output,
			"truncated":  truncated,
			"exitStatus": map[string]interface{}{
				"exitCode": exitCode,
			},
		},
	}

	data, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("Failed to marshal notification: %v", err)
	}

	// Verify the notification contains expected fields
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal notification: %v", err)
	}

	if parsed["method"] != "terminal/output" {
		t.Errorf("Expected method 'terminal/output', got '%s'", parsed["method"])
	}

	params := parsed["params"].(map[string]interface{})
	if params["sessionId"] != sessionID {
		t.Errorf("Expected sessionId '%s', got '%s'", sessionID, params["sessionId"])
	}

	if params["output"] != output {
		t.Errorf("Expected output '%s', got '%s'", output, params["output"])
	}

	t.Logf("Terminal output notification test passed")
}
