package adapter

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// getSocketPath returns the Open Agents socket path from environment
func getSocketPath() string {
	return os.Getenv("OPEN_AGENTS_SOCKET_PATH")
}

// BaseAdapter provides common functionality for CLI adapters
type BaseAdapter struct {
	name           string
	displayName    string
	command        string
	cmd            *exec.Cmd
	ptmx           *os.File // PTY master file descriptor
	running        bool
	extraEnv       []string // Additional environment variables
	outputCallback func(OutputEvent)
	permCallback   func(PermissionRequest) PermissionResponse
	exitCallback   func(int)
	mu             sync.Mutex
}

func (a *BaseAdapter) IsInstalled() bool {
	_, err := exec.LookPath(a.command)
	return err == nil
}

func (a *BaseAdapter) Start(workDir string, args []string) error {
	return a.StartWithSize(workDir, args, 120, 30)
}

func (a *BaseAdapter) StartWithSize(workDir string, args []string, cols, rows int) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	logger.Info("[%s] Starting %s in %s with args: %v, size: %dx%d", logger.ModAdapter, a.command, workDir, args, cols, rows)

	a.cmd = exec.Command(a.command, args...)
	a.cmd.Dir = workDir
	a.cmd.Env = append(os.Environ(), a.extraEnv...)

	// Start with PTY (pseudo-terminal) - required for interactive CLIs like claude
	ptmx, err := pty.StartWithSize(a.cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		logger.Error("[%s] Failed to start PTY: %v", logger.ModAdapter, err)
		return err
	}
	a.ptmx = ptmx

	logger.Info("[%s] Process started with PTY (PID: %d), size: %dx%d", logger.ModAdapter, a.cmd.Process.Pid, cols, rows)
	a.running = true

	// Read output from PTY (combines stdout and stderr)
	go a.readOutput(ptmx, "stdout")

	go func() {
		err := a.cmd.Wait()
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()

		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}

		logger.Info("[%s] Process exited with code %d", logger.ModAdapter, exitCode)
		if a.exitCallback != nil {
			a.exitCallback(exitCode)
		}
	}()

	return nil
}

func (a *BaseAdapter) readOutput(r io.Reader, outputType string) {
	logger.Debug("[%s] Starting to read %s (raw mode)", logger.ModAdapter, outputType)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			content := string(buf[:n])
			// Log truncated for readability (raw output may contain control chars)
			if len(content) > 100 {
				logger.Debug("[%s] %s: %d bytes", logger.ModAdapter, outputType, n)
			} else {
				logger.Debug("[%s] %s: %q", logger.ModAdapter, outputType, content)
			}
			if a.outputCallback != nil {
				a.outputCallback(OutputEvent{
					Type:    outputType,
					Content: content,
				})
			}
		}
		if err != nil {
			if err.Error() != "EOF" {
				logger.Error("[%s] Read error for %s: %v", logger.ModAdapter, outputType, err)
			}
			break
		}
	}
	logger.Debug("[%s] Stopped reading %s", logger.ModAdapter, outputType)
}

func (a *BaseAdapter) Stop() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.ptmx != nil {
		a.ptmx.Close()
	}
	if a.cmd != nil && a.cmd.Process != nil {
		return a.cmd.Process.Kill()
	}
	return nil
}

func (a *BaseAdapter) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *BaseAdapter) Resize(cols, rows int) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.ptmx == nil {
		logger.Debug("[%s] Resize skipped: PTY not initialized", logger.ModAdapter)
		return nil
	}

	logger.Debug("[%s] Resizing PTY to %dx%d", logger.ModAdapter, cols, rows)
	err := pty.Setsize(a.ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		logger.Error("[%s] Resize failed: %v", logger.ModAdapter, err)
	}
	return err
}

func (a *BaseAdapter) Send(input string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.ptmx == nil {
		logger.Warn("[%s] Send failed: PTY not initialized", logger.ModAdapter)
		return nil
	}

	logger.Debug("[%s] Sending input: %q", logger.ModAdapter, input)
	_, err := a.ptmx.Write([]byte(input + "\n"))
	if err != nil {
		logger.Error("[%s] Send error: %v", logger.ModAdapter, err)
	}
	return err
}

func (a *BaseAdapter) OnOutput(callback func(OutputEvent)) {
	a.outputCallback = callback
}

func (a *BaseAdapter) OnPermission(callback func(PermissionRequest) PermissionResponse) {
	a.permCallback = callback
}

func (a *BaseAdapter) OnExit(callback func(int)) {
	a.exitCallback = callback
}
