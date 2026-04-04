package protocol

// MessageType represents the type of message
type MessageType string

const (
	MessageTypeContent       MessageType = "content"        // AI response text
	MessageTypeThought       MessageType = "thought"        // AI thinking process
	MessageTypeToolCall      MessageType = "tool_call"      // Tool invocation
	MessageTypePermission    MessageType = "permission"     // Permission request
	MessageTypeStatus        MessageType = "status"         // Agent status change
	MessageTypePlan          MessageType = "plan"           // Task plan
	MessageTypeError         MessageType = "error"          // Error message
	MessageTypeCancel        MessageType = "cancel"         // Cancel/interrupt operation
	MessageTypeUsage         MessageType = "usage"          // Token usage statistics
	MessageTypePing          MessageType = "ping"           // Ping message for connection verification
	MessageTypePong          MessageType = "pong"           // Pong response to ping
	MessageTypeAuthRequired  MessageType = "auth_required"  // Authentication required
)

// AgentStatus represents the current state of the agent
type AgentStatus string

const (
	StatusIdle              AgentStatus = "idle"
	StatusThinking          AgentStatus = "thinking"
	StatusStreaming         AgentStatus = "streaming"
	StatusToolExecuting     AgentStatus = "tool_executing"
	StatusPermissionPending AgentStatus = "permission_pending"
	StatusAuthRequired      AgentStatus = "auth_required"
)

// Message represents a unified message format across all protocols
type Message struct {
	Type    MessageType            `json:"type"`
	Content interface{}            `json:"content"`
	Meta    map[string]interface{} `json:"meta,omitempty"`
}

// PermissionRequest represents a permission request
type PermissionRequest struct {
	ID          interface{}            `json:"id"` // Can be string or number (JSON-RPC 2.0)
	ToolName    string                 `json:"tool_name"`
	ToolInput   map[string]interface{} `json:"tool_input"`
	Description string                 `json:"description"`
	Risk        string                 `json:"risk"` // "low", "medium", "high"
	Options     []string               `json:"options"`
}

// PermissionResponse represents the user's response
type PermissionResponse struct {
	ID       interface{} `json:"id"`        // Can be string or number (JSON-RPC 2.0 requires same type as request)
	OptionID string      `json:"option_id"` // "allow_once", "allow_always", "reject_once", "reject_always"
}

// ToolCall represents a tool invocation
type ToolCall struct {
	ID     string                 `json:"id"`
	Name   string                 `json:"name"`
	Input  map[string]interface{} `json:"input"`
	Status string                 `json:"status"` // "pending", "in_progress", "completed", "failed"
	Result interface{}            `json:"result,omitempty"`
}

// UsageStats represents token usage statistics
type UsageStats struct {
	InputTokens   int `json:"inputTokens"`
	OutputTokens  int `json:"outputTokens"`
	CacheCreation int `json:"cacheCreation"`
	CacheRead     int `json:"cacheRead"`
	ContextSize   int `json:"contextSize"`
}
