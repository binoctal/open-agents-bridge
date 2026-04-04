package adapter

// ClineAdapter implements the Adapter interface for Cline CLI
type ClineAdapter struct {
	BaseAdapter
}

func NewClineAdapter() *ClineAdapter {
	return &ClineAdapter{
		BaseAdapter: BaseAdapter{
			name:        "cline",
			displayName: "Cline CLI",
			command:     "cline",
			extraEnv: []string{
				"CLINE_PERMISSION_MODE=external",
			},
		},
	}
}

func (a *ClineAdapter) Name() string {
	return a.name
}

func (a *ClineAdapter) DisplayName() string {
	return a.displayName
}

func (a *ClineAdapter) Start(workDir string, args []string) error {
	socketPath := getSocketPath()
	if socketPath != "" {
		a.extraEnv = append(a.extraEnv, "CLINE_HOOK_SOCKET="+socketPath)
	}
	cmdArgs := append([]string{"-s", "hooks_enabled=true"}, args...)
	return a.BaseAdapter.Start(workDir, cmdArgs)
}

func (a *ClineAdapter) StartWithSize(workDir string, args []string, cols, rows int) error {
	socketPath := getSocketPath()
	if socketPath != "" {
		a.extraEnv = append(a.extraEnv, "CLINE_HOOK_SOCKET="+socketPath)
	}
	cmdArgs := append([]string{"-s", "hooks_enabled=true"}, args...)
	return a.BaseAdapter.StartWithSize(workDir, cmdArgs, cols, rows)
}
