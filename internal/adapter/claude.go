package adapter

// ClaudeAdapter implements the Adapter interface for Claude CLI
type ClaudeAdapter struct {
	BaseAdapter
}

func NewClaudeAdapter() *ClaudeAdapter {
	return &ClaudeAdapter{
		BaseAdapter: BaseAdapter{
			name:        "claude",
			displayName: "Claude CLI",
			command:     "claude",
		},
	}
}

func (a *ClaudeAdapter) Name() string {
	return a.name
}

func (a *ClaudeAdapter) DisplayName() string {
	return a.displayName
}
