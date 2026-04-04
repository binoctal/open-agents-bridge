package acp

// JSON-RPC 2.0 types for ACP protocol

const JSONRPCVersion = "2.0"

// Request represents a JSON-RPC request
type Request struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// Response represents a JSON-RPC response
type Response struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      int          `json:"id"`
	Result  interface{}  `json:"result,omitempty"`
	Error   *ErrorObject `json:"error,omitempty"`
}

// ErrorObject represents a JSON-RPC error
type ErrorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Notification represents a JSON-RPC notification (no ID)
type Notification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// ACP Methods
const (
	MethodInitialize        = "initialize"
	MethodAuthenticate      = "authenticate"
	MethodSessionNew        = "session/new"
	MethodSessionPrompt     = "session/prompt"
	MethodSessionUpdate     = "session/update"
	MethodRequestPermission = "session/request_permission"
	MethodReadTextFile      = "fs/read_text_file"
	MethodWriteTextFile     = "fs/write_text_file"
)

// InitializeParams for initialize request
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

type ClientCapabilities struct {
	FS FSCapabilities `json:"fs"`
}

type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// SessionNewParams for session/new request
type SessionNewParams struct {
	Cwd        string        `json:"cwd"`
	McpServers []interface{} `json:"mcpServers"`
}

// SessionPromptParams for session/prompt request
type SessionPromptParams struct {
	SessionID string        `json:"sessionId"`
	Prompt    []PromptPart  `json:"prompt"`
}

type PromptPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Session Update Types
type SessionUpdate struct {
	SessionID string            `json:"sessionId"`
	Update    SessionUpdateData `json:"update"`
}

type SessionUpdateData struct {
	SessionUpdate string `json:"sessionUpdate"`
	// For agent_message_chunk / agent_thought_chunk
	Content *ContentItem `json:"content,omitempty"`
	// For tool_call
	ToolCallID string            `json:"toolCallId,omitempty"`
	Status     string            `json:"status,omitempty"`
	Title      string            `json:"title,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	RawInput   map[string]interface{} `json:"rawInput,omitempty"`
	ToolContent []ToolContentItem `json:"content,omitempty"`
	Locations  []LocationItem    `json:"locations,omitempty"`
	// For plan
	Entries []PlanEntry `json:"entries,omitempty"`
}

type ContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type ToolContentItem struct {
	Type    string       `json:"type"`
	Content *ContentItem `json:"content,omitempty"`
	Path    string       `json:"path,omitempty"`
	OldText *string      `json:"oldText,omitempty"`
	NewText string       `json:"newText,omitempty"`
}

type LocationItem struct {
	Path string `json:"path"`
}

type PlanEntry struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority,omitempty"`
}

// Permission Request
type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	Options   []PermissionOption `json:"options"`
	ToolCall  ToolCallInfo       `json:"toolCall"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
}

type ToolCallInfo struct {
	ToolCallID string                 `json:"toolCallId"`
	RawInput   map[string]interface{} `json:"rawInput,omitempty"`
	Status     string                 `json:"status,omitempty"`
	Title      string                 `json:"title,omitempty"`
	Kind       string                 `json:"kind,omitempty"`
	Content    []ToolContentItem      `json:"content,omitempty"`
	Locations  []LocationItem         `json:"locations,omitempty"`
}

// Permission Response
type PermissionResponse struct {
	Outcome PermissionOutcome `json:"outcome"`
}

type PermissionOutcome struct {
	Outcome  string `json:"outcome"` // selected, rejected
	OptionID string `json:"optionId"`
}

// File operation params
type FileReadParams struct {
	Path      string `json:"path"`
	SessionID string `json:"sessionId,omitempty"`
}

type FileWriteParams struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	SessionID string `json:"sessionId,omitempty"`
}
