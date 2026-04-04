package adapter

// CodexAdapter implements the Adapter interface for Codex CLI (OpenAI)
type CodexAdapter struct {
	BaseAdapter
}

func NewCodexAdapter() *CodexAdapter {
	return &CodexAdapter{
		BaseAdapter: BaseAdapter{
			name:        "codex",
			displayName: "Codex CLI",
			command:     "codex",
			extraEnv: []string{
				"CODEX_PERMISSION_MODE=external",
			},
		},
	}
}

func (a *CodexAdapter) Name() string {
	return a.name
}

func (a *CodexAdapter) DisplayName() string {
	return a.displayName
}

func (a *CodexAdapter) Start(workDir string, args []string) error {
	// Add socket path at runtime if available
	socketPath := getSocketPath()
	if socketPath != "" {
		a.extraEnv = append(a.extraEnv, "CODEX_HOOK_SOCKET="+socketPath)
	}
	return a.BaseAdapter.Start(workDir, args)
}

func (a *CodexAdapter) StartWithSize(workDir string, args []string, cols, rows int) error {
	// Add socket path at runtime if available
	socketPath := getSocketPath()
	if socketPath != "" {
		a.extraEnv = append(a.extraEnv, "CODEX_HOOK_SOCKET="+socketPath)
	}
	return a.BaseAdapter.StartWithSize(workDir, args, cols, rows)
}
