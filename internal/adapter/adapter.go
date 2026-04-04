package adapter

import "fmt"

// OutputEvent represents output from the CLI
type OutputEvent struct {
	Type    string // "stdout", "stderr", "status"
	Content string
}

// PermissionRequest represents a permission request from the CLI
type PermissionRequest struct {
	ID          string
	ToolName    string
	ToolInput   interface{}
	Description string
	Risk        string // "low", "medium", "high"
	Timeout     int    // seconds
}

// PermissionResponse represents the user's response to a permission request
type PermissionResponse struct {
	ID       string
	Approved bool
}

// Adapter defines the interface for CLI adapters
type Adapter interface {
	Name() string
	DisplayName() string
	IsInstalled() bool

	Start(workDir string, args []string) error
	StartWithSize(workDir string, args []string, cols, rows int) error
	Stop() error
	IsRunning() bool
	Resize(cols, rows int) error

	Send(input string) error
	OnOutput(callback func(OutputEvent))
	OnPermission(callback func(PermissionRequest) PermissionResponse)
	OnExit(callback func(exitCode int))
}

// Registry of available adapters
var registry = map[string]func() Adapter{
	"kiro":   func() Adapter { return NewKiroAdapter() },
	"cline":  func() Adapter { return NewClineAdapter() },
	"claude": func() Adapter { return NewClaudeAdapter() },
	"codex":  func() Adapter { return NewCodexAdapter() },
	"gemini": func() Adapter { return NewGeminiAdapter() },
}

// Get returns an adapter by name
func Get(name string) (Adapter, error) {
	factory, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown adapter: %s", name)
	}
	return factory(), nil
}

// List returns all available adapter names
func List() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	return names
}
