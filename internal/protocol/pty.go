package protocol

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"

	"github.com/creack/pty"
	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// PTYAdapter implements the legacy PTY (pseudo-terminal) protocol
type PTYAdapter struct {
	cmd       *exec.Cmd
	ptmx      *os.File
	connected atomic.Bool
	callback  func(Message)
	mu        sync.Mutex
}

// NewPTYAdapter creates a new PTY adapter
func NewPTYAdapter() *PTYAdapter {
	return &PTYAdapter{}
}

func (a *PTYAdapter) Name() string {
	return "pty"
}

func (a *PTYAdapter) Version() string {
	return "1.0.0"
}

func (a *PTYAdapter) Connect(config AdapterConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	logger.Info("[%s] Connecting to %s in %s", logger.ModPTY, config.Command, config.WorkDir)

	a.cmd = exec.Command(config.Command, config.Args...)
	a.cmd.Dir = config.WorkDir
	a.cmd.Env = os.Environ()

	// Add custom env vars
	for k, v := range config.Env {
		a.cmd.Env = append(a.cmd.Env, k+"="+v)
	}
	for k, v := range config.CustomEnv {
		a.cmd.Env = append(a.cmd.Env, k+"="+v)
	}

	// Start with PTY
	cols, rows := config.Cols, config.Rows
	if cols == 0 {
		cols = 120
	}
	if rows == 0 {
		rows = 30
	}

	ptmx, err := pty.StartWithSize(a.cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return err
	}
	a.ptmx = ptmx

	logger.Info("[%s] Process started (PID: %d), size: %dx%d", logger.ModPTY, a.cmd.Process.Pid, cols, rows)
	a.connected.Store(true)

	// Read output
	go a.readOutput()

	// Wait for exit
	go func() {
		err := a.cmd.Wait()
		a.connected.Store(false)

		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			}
		}

		logger.Info("[%s] Process exited with code %d", logger.ModPTY, exitCode)

		if a.callback != nil {
			a.callback(Message{
				Type:    MessageTypeStatus,
				Content: StatusIdle,
				Meta: map[string]interface{}{
					"protocol":  "pty",
					"exit_code": exitCode,
				},
			})
		}
	}()

	return nil
}

func (a *PTYAdapter) Disconnect() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.connected.Load() {
		return nil
	}

	logger.Info("[%s] Disconnecting", logger.ModPTY)
	a.connected.Store(false)

	if a.ptmx != nil {
		a.ptmx.Close()
	}
	if a.cmd != nil && a.cmd.Process != nil {
		a.cmd.Process.Kill()
	}

	return nil
}

func (a *PTYAdapter) IsConnected() bool {
	return a.connected.Load()
}

func (a *PTYAdapter) SendMessage(msg Message) error {
	// For PTY, only content messages are supported
	if msg.Type != MessageTypeContent {
		logger.Debug("[%s] Ignoring non-content message type: %s", logger.ModPTY, msg.Type)
		return nil
	}

	content, ok := msg.Content.(string)
	if !ok {
		logger.Warn("[%s] Invalid content type (expected string, got %T)", logger.ModPTY, msg.Content)
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.ptmx == nil {
		logger.Warn("[%s] PTY not initialized (ptmx is nil)", logger.ModPTY)
		return nil
	}

	logger.Debug("[%s] Sending input: %q", logger.ModPTY, content)
	_, err := a.ptmx.Write([]byte(content + "\n"))
	if err != nil {
		logger.Error("[%s] Send error: %v", logger.ModPTY, err)
	}
	return err
}

func (a *PTYAdapter) ReceiveMessage() (Message, error) {
	// Not used in callback mode
	return Message{}, nil
}

func (a *PTYAdapter) Subscribe(callback func(Message)) {
	a.callback = callback
}

func (a *PTYAdapter) Capabilities() []string {
	return []string{"raw_output"}
}

func (a *PTYAdapter) SupportsPermissions() bool {
	return false
}

func (a *PTYAdapter) SupportsFileOps() bool {
	return false
}

func (a *PTYAdapter) SupportsToolCalls() bool {
	return false
}

// readOutput reads raw output from PTY
func (a *PTYAdapter) readOutput() {
	buf := make([]byte, 4096)
	for {
		if !a.connected.Load() {
			break
		}

		n, err := a.ptmx.Read(buf)
		if n > 0 {
			content := string(buf[:n])
			logger.Debug("[%s] Output: %d bytes", logger.ModPTY, n)

			if a.callback != nil {
				a.callback(Message{
					Type:    MessageTypeContent,
					Content: content,
					Meta: map[string]interface{}{
						"protocol": "pty",
						"raw":      true,
					},
				})
			}
		}

		if err != nil {
			if err != io.EOF {
				logger.Error("[%s] Read error: %v", logger.ModPTY, err)
			}
			break
		}
	}
}
