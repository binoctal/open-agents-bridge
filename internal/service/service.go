package service

import "runtime"

// Manager provides cross-platform service management
type Manager interface {
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Status() (string, error)
}

// New returns a platform-specific service manager
func New() Manager {
	switch runtime.GOOS {
	case "windows":
		return &WindowsService{}
	case "darwin":
		return &DarwinService{}
	default:
		return &LinuxService{}
	}
}
