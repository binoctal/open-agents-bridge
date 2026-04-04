package adapter

// KiroAdapter implements the Adapter interface for Kiro CLI
type KiroAdapter struct {
	BaseAdapter
}

func NewKiroAdapter() *KiroAdapter {
	return &KiroAdapter{
		BaseAdapter: BaseAdapter{
			name:        "kiro",
			displayName: "Kiro CLI",
			command:     "kiro",
			extraEnv: []string{
				"KIRO_HOOKS_ENABLED=true",
			},
		},
	}
}

func (a *KiroAdapter) Name() string {
	return a.name
}

func (a *KiroAdapter) DisplayName() string {
	return a.displayName
}

func (a *KiroAdapter) Start(workDir string, args []string) error {
	// Kiro requires --headless flag for non-interactive use
	cmdArgs := append([]string{"--headless"}, args...)
	return a.BaseAdapter.Start(workDir, cmdArgs)
}

func (a *KiroAdapter) StartWithSize(workDir string, args []string, cols, rows int) error {
	// Kiro requires --headless flag for non-interactive use
	cmdArgs := append([]string{"--headless"}, args...)
	return a.BaseAdapter.StartWithSize(workDir, cmdArgs, cols, rows)
}
