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
}
