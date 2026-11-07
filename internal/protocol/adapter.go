package protocol

// Adapter defines the interface that all protocol adapters must implement
type Adapter interface {
	// Protocol information
	Name() string
	Version() string

	// Lifecycle
	Connect(config AdapterConfig) error
	Disconnect() error
	IsConnected() bool

	// Message handling
	SendMessage(msg Message) error
	ReceiveMessage() (Message, error)
	Subscribe(callback func(Message))

	// Capabilities
	Capabilities() []string
	SupportsPermissions() bool
	SupportsFileOps() bool
	SupportsToolCalls() bool

	// Terminal
	Resize(cols, rows int) error
}

// AdapterConfig contains configuration for protocol adapters
type AdapterConfig struct {
	WorkDir       string
	Command       string
	Args          []string
	Env           map[string]string
	Cols          int
	Rows          int
	CustomArgs    []string
	CustomEnv     map[string]string
	ForceProtocol string // "pty" to force PTY, empty for auto-detect
	// PTYFallbackAllowed reports whether this CLI type has a PTY-equivalent
	// command. Known ACP-only servers (claude via npm wrapper, qwen/goose/
	// gemini/opencode/dsh/cline/codex) MUST leave it false: driving a
	// JSON-RPC server with keystrokes makes it answer every user message with
	// a -32700 parse error — a session that looks connected but can never
	// work (2026-11-07 incident). Only unknown/custom commands keep the
	// auto-detect fallback, where PTY is a legitimate guess.
	PTYFallbackAllowed bool
	// PermissionMode is the session's bridge-level permission mode
	// ("default", "accept-edits", "accept-all", "plan"). The ACP adapter
	// gates terminal commands on user approval for every mode that does not
	// explicitly auto-approve (see acp.go handleTerminalCreate).
	PermissionMode string
	// ACPModeID, when set, is applied via ACP session/set_mode before the
	// session is exposed (claude: the shared config dir carries no defaultMode).
	ACPModeID string
}
