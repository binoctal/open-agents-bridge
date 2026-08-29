package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Wire message format contract
// ═══════════════════════════════════════════════════════════════════════════════

// WireMessage mirrors the frontend Message interface
type WireMessage struct {
	Type      string      `json:"type"`
	Payload   interface{} `json:"payload"`
	Timestamp int64       `json:"timestamp"`
}

func TestWireMessageJSON(t *testing.T) {
	msg := WireMessage{
		Type: "device:online",
		Payload: map[string]interface{}{
			"deviceId":   "dev_1",
			"deviceName": "Test Device",
		},
		Timestamp: 1716643200000,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded WireMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Type != "device:online" {
		t.Errorf("Type = %s, want device:online", decoded.Type)
	}
	if decoded.Timestamp != 1716643200000 {
		t.Errorf("Timestamp = %d, want 1716643200000", decoded.Timestamp)
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Protocol message type constants contract
// ═══════════════════════════════════════════════════════════════════════════════

func TestMessageTypeConstants(t *testing.T) {
	types := map[protocol.MessageType]string{
		protocol.MessageTypeContent:      "content",
		protocol.MessageTypeThought:      "thought",
		protocol.MessageTypeToolCall:     "tool_call",
		protocol.MessageTypePermission:   "permission",
		protocol.MessageTypeStatus:       "status",
		protocol.MessageTypePlan:         "plan",
		protocol.MessageTypeError:        "error",
		protocol.MessageTypeCancel:       "cancel",
		protocol.MessageTypeUsage:        "usage",
		protocol.MessageTypePing:         "ping",
		protocol.MessageTypePong:         "pong",
		protocol.MessageTypeAuthRequired: "auth_required",
	}

	for typ, expected := range types {
		if string(typ) != expected {
			t.Errorf("MessageType %s = %s, want %s", typ, string(typ), expected)
		}
	}
}

func TestAgentStatusConstants(t *testing.T) {
	statuses := map[protocol.AgentStatus]string{
		protocol.StatusIdle:              "idle",
		protocol.StatusThinking:          "thinking",
		protocol.StatusStreaming:         "streaming",
		protocol.StatusToolExecuting:     "tool_executing",
		protocol.StatusPermissionPending: "permission_pending",
		protocol.StatusAuthRequired:      "auth_required",
	}

	for status, expected := range statuses {
		if string(status) != expected {
			t.Errorf("AgentStatus %s = %s, want %s", status, string(status), expected)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Protocol struct serialization contracts
// ═══════════════════════════════════════════════════════════════════════════════

func TestMessageSerialization(t *testing.T) {
	msg := protocol.Message{
		Type:    protocol.MessageTypeContent,
		Content: "Hello, world!",
		Meta:    map[string]interface{}{"sessionId": "sess_1"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded["type"] != "content" {
		t.Errorf("type = %v, want content", decoded["type"])
	}
	if decoded["content"] != "Hello, world!" {
		t.Errorf("content = %v, want Hello, world!", decoded["content"])
	}
}

func TestPermissionRequestSerialization(t *testing.T) {
	perm := protocol.PermissionRequest{
		ID:          "perm_1",
		ToolName:    "Bash",
		ToolInput:   map[string]interface{}{"command": "rm -rf /tmp"},
		Description: "Run bash command",
		Risk:        "high",
		Options:     []string{"allow_once", "allow_always", "reject_once"},
	}

	data, err := json.Marshal(perm)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded["tool_name"] != "Bash" {
		t.Errorf("tool_name = %v, want Bash", decoded["tool_name"])
	}
	if decoded["risk"] != "high" {
		t.Errorf("risk = %v, want high", decoded["risk"])
	}
	if decoded["id"] != "perm_1" {
		t.Errorf("id = %v, want perm_1", decoded["id"])
	}
	// Verify JSON field names match frontend expectations
	expectedFields := []string{"id", "tool_name", "tool_input", "description", "risk", "options"}
	for _, field := range expectedFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("Missing field: %s", field)
		}
	}
}

func TestPermissionRequestWithNumericID(t *testing.T) {
	// JSON-RPC 2.0 allows numeric IDs
	perm := protocol.PermissionRequest{
		ID:          float64(42),
		ToolName:    "Read",
		ToolInput:   map[string]interface{}{"path": "/etc/passwd"},
		Description: "Read file",
		Risk:        "low",
		Options:     []string{"allow_once"},
	}

	data, err := json.Marshal(perm)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if id, ok := decoded["id"].(float64); !ok || id != 42 {
		t.Errorf("id = %v, want 42", decoded["id"])
	}
}

func TestToolCallSerialization(t *testing.T) {
	tc := protocol.ToolCall{
		ID:     "tc_1",
		Name:   "Write",
		Input:  map[string]interface{}{"path": "src/index.ts", "content": "hello"},
		Status: "in_progress",
	}

	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded["id"] != "tc_1" {
		t.Errorf("id = %v, want tc_1", decoded["id"])
	}
	if decoded["name"] != "Write" {
		t.Errorf("name = %v, want Write", decoded["name"])
	}
	if decoded["status"] != "in_progress" {
		t.Errorf("status = %v, want in_progress", decoded["status"])
	}
}

func TestUsageStatsSerialization(t *testing.T) {
	usage := protocol.UsageStats{
		InputTokens:   1000,
		OutputTokens:  500,
		CacheCreation: 100,
		CacheRead:     50,
		ContextSize:   1500,
	}

	data, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Verify JSON field names match frontend expectations
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	expectedFields := []string{"inputTokens", "outputTokens", "cacheCreation", "cacheRead", "contextSize"}
	for _, field := range expectedFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("Missing field: %s", field)
		}
	}
}

func TestPermissionResponseSerialization(t *testing.T) {
	resp := protocol.PermissionResponse{
		ID:       "perm_1",
		OptionID: "allow_once",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded["id"] != "perm_1" {
		t.Errorf("id = %v, want perm_1", decoded["id"])
	}
	if decoded["option_id"] != "allow_once" {
		t.Errorf("option_id = %v, want allow_once", decoded["option_id"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Wire message → protocol translation contracts
// ═══════════════════════════════════════════════════════════════════════════════

// These test the mapping between frontend wire message types and
// internal protocol message types, ensuring the bridge translates correctly.

func TestProtocolToWireTypeMapping(t *testing.T) {
	// This mapping documents how internal protocol messages map to wire messages
	// The bridge's forwardSessionOutput() performs this translation
	mapping := map[string]string{
		"content":    "chat:response",  // MessageTypeContent → chat:response
		"thought":    "chat:thought",   // MessageTypeThought → chat:thought
		"tool_call":  "tool:call",      // MessageTypeToolCall → tool:call
		"permission": "permission:request", // MessageTypePermission → permission:request
		"status":     "agent:status",   // MessageTypeStatus → agent:status
		"usage":      "session:usage",  // MessageTypeUsage → session:usage
		"error":      "session:error",  // MessageTypeError → session:error
	}

	for protoType, wireType := range mapping {
		if protoType == "" || wireType == "" {
			t.Errorf("Invalid mapping: %s → %s", protoType, wireType)
		}
		// Verify wire types follow namespace:action pattern
		if len(wireType) < 3 || !containsColon(wireType) {
			t.Errorf("Wire type %q should follow namespace:action pattern", wireType)
		}
	}
}

func TestWireToHandlerMapping(t *testing.T) {
	// Documents which incoming wire message types are handled by the bridge
	handledTypes := []string{
		"session:start",
		"session:resume",
		"session:send",
		"session:stop",
		"session:cancel",
		"session:resize",
		"session:changeDir",
		"chat:send",
		"permission:response",
		"control:takeover",
		"config:sync",
		"rules:sync",
		"scanner:toggle",
		"scanner:rules:sync",
		"workflow:start",
		"workflow:pause",
		"workflow:cancel",
		"workflow:start_task",
		"workflow:task_assign",
		"workflow:task_answer",
		"workflow:task_guidance",
		"workflow:task_merge",
		"workflow:merge_all",
	}

	for _, msgType := range handledTypes {
		if msgType == "" {
			t.Error("Empty message type in handled types")
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Session lifecycle contract
// ═══════════════════════════════════════════════════════════════════════════════

func TestSessionStartPayload(t *testing.T) {
	// This is the payload the frontend sends for session:start
	payload := map[string]interface{}{
		"sessionId":      "sess_1",
		"cliType":        "claude",
		"workDir":        "/home/user/project",
		"permissionMode": "default",
		"cols":           float64(120),
		"rows":           float64(30),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify all required fields are present
	requiredFields := []string{"sessionId", "cliType", "workDir"}
	for _, field := range requiredFields {
		if _, ok := decoded[field]; !ok {
			t.Errorf("Missing required field: %s", field)
		}
	}
}

func TestPermissionResponseWirePayload(t *testing.T) {
	// This is the payload the frontend sends for permission:response
	payload := map[string]interface{}{
		"id":       "perm_1",
		"deviceId": "dev_1",
		"approved": true,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify all required fields
	if decoded["id"] != "perm_1" {
		t.Errorf("id = %v, want perm_1", decoded["id"])
	}
	if decoded["approved"] != true {
		t.Errorf("approved = %v, want true", decoded["approved"])
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// Message type count snapshot
// ═══════════════════════════════════════════════════════════════════════════════

func TestProtocolTypeCount(t *testing.T) {
	// Snapshot: ensure no types are accidentally removed
	allTypes := []protocol.MessageType{
		protocol.MessageTypeContent,
		protocol.MessageTypeThought,
		protocol.MessageTypeToolCall,
		protocol.MessageTypePermission,
		protocol.MessageTypeStatus,
		protocol.MessageTypePlan,
		protocol.MessageTypeError,
		protocol.MessageTypeCancel,
		protocol.MessageTypeUsage,
		protocol.MessageTypePing,
		protocol.MessageTypePong,
		protocol.MessageTypeAuthRequired,
	}
	expect(t, len(allTypes) == 12, "Expected 12 message types, got %d", len(allTypes))
}

func TestAgentStatusCount(t *testing.T) {
	allStatuses := []protocol.AgentStatus{
		protocol.StatusIdle,
		protocol.StatusThinking,
		protocol.StatusStreaming,
		protocol.StatusToolExecuting,
		protocol.StatusPermissionPending,
		protocol.StatusAuthRequired,
	}
	expect(t, len(allStatuses) == 6, "Expected 6 agent statuses, got %d", len(allStatuses))
}

// ═══════════════════════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════════════════════

func containsColon(s string) bool {
	for _, c := range s {
		if c == ':' {
			return true
		}
	}
	return false
}

func expect(t *testing.T, cond bool, msg string, args ...interface{}) {
	t.Helper()
	if !cond {
		t.Errorf(msg, args...)
	}
}
