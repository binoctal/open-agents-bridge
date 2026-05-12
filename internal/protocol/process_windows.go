//go:build windows

package protocol

import (
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {
	// Windows doesn't support Setpgid; rely on cmd.Process.Kill()
}

func killProcessGroup(cmd *exec.Cmd) {
	cmd.Process.Kill()
}
