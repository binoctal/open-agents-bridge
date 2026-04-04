package adapter

// GeminiAdapter implements the Adapter interface for Gemini CLI (Google)
type GeminiAdapter struct {
	BaseAdapter
}

func NewGeminiAdapter() *GeminiAdapter {
	return &GeminiAdapter{
		BaseAdapter: BaseAdapter{
			name:        "gemini",
			displayName: "Gemini CLI",
			command:     "gemini",
			extraEnv: []string{
				"GEMINI_PERMISSION_MODE=external",
			},
		},
	}
}

func (a *GeminiAdapter) Name() string {
	return a.name
}

func (a *GeminiAdapter) DisplayName() string {
	return a.displayName
}

func (a *GeminiAdapter) Start(workDir string, args []string) error {
	// Add socket path at runtime if available
	socketPath := getSocketPath()
	if socketPath != "" {
		a.extraEnv = append(a.extraEnv, "GEMINI_HOOK_SOCKET="+socketPath)
	}
	return a.BaseAdapter.Start(workDir, args)
}

func (a *GeminiAdapter) StartWithSize(workDir string, args []string, cols, rows int) error {
	// Add socket path at runtime if available
	socketPath := getSocketPath()
	if socketPath != "" {
		a.extraEnv = append(a.extraEnv, "GEMINI_HOOK_SOCKET="+socketPath)
	}
	return a.BaseAdapter.StartWithSize(workDir, args, cols, rows)
}
