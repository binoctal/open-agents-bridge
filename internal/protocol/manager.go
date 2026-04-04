package protocol

import (
	"fmt"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// Manager manages protocol adapters and auto-detection
type Manager struct {
	adapter  Adapter
	callback func(Message)
}

// NewManager creates a new protocol manager
func NewManager() *Manager {
	return &Manager{}
}

// Connect attempts to connect using the best available protocol
// For ACP-capable CLIs, we always prefer ACP and don't fallback to PTY
// If ForceProtocol is set to "pty", skip ACP and use PTY directly
func (m *Manager) Connect(config AdapterConfig) error {
	logger.Info("[%s] Auto-detecting protocol for %s", logger.ModProtocol, config.Command)

	// Force PTY mode if requested
	if config.ForceProtocol == "pty" {
		logger.Info("[%s] Force PTY mode requested", logger.ModProtocol)
		return m.tryPTY(config)
	}

	// Try ACP first - this is the preferred protocol for Claude Code
	err := m.tryACP(config)
	if err == nil {
		logger.Info("[%s] Using ACP protocol", logger.ModProtocol)
		return nil
	}

	// Only fallback to PTY if ACP process failed to start entirely
	// (e.g., command not found, not ACP-capable CLI)
	logger.Info("[%s] ACP failed (%v), falling back to PTY", logger.ModProtocol, err)
	return m.tryPTY(config)
}

// tryACP attempts to connect using ACP protocol
// Unlike before, we don't timeout once ACP process starts successfully
// because ACP is the preferred protocol and may need authentication
func (m *Manager) tryACP(config AdapterConfig) error {
	adapter := NewACPAdapter()

	// Channel to receive initialization status
	// We wait up to 60 seconds for initial connection, but once connected,
	// we stay with ACP regardless of authentication status
	initialized := make(chan bool, 1)
	initError := make(chan error, 1)

	// Subscribe to messages to detect initialization
	originalCallback := m.callback
	initCallback := func(msg Message) {
		// Any status message means ACP is working
		if msg.Type == MessageTypeStatus {
			select {
			case initialized <- true:
			default:
			}
		}
		if originalCallback != nil {
			originalCallback(msg)
		}
	}

	// Set callback before connecting
	adapter.Subscribe(initCallback)

	// Attempt to connect
	if err := adapter.Connect(config); err != nil {
		// Connection failed entirely - CLI might not support ACP
		initError <- err
		return err
	}

	// Wait for initial handshake (60 seconds timeout for slow networks)
	// This only waits for the ACP process to respond, not for full session setup
	select {
	case <-initialized:
		m.adapter = adapter
		// Restore original callback after initialization
		if originalCallback != nil {
			adapter.Subscribe(originalCallback)
			m.callback = originalCallback
		}
		logger.Info("[%s] ACP initialized successfully", logger.ModProtocol)
		return nil
	case err := <-initError:
		adapter.Disconnect()
		return fmt.Errorf("ACP connection failed: %w", err)
	case <-time.After(60 * time.Second):
		// Only timeout if ACP process doesn't respond at all
		// This indicates the CLI doesn't support ACP
		adapter.Disconnect()
		return fmt.Errorf("ACP process did not respond within 60 seconds")
	}
}

// tryPTY attempts to connect using PTY protocol
func (m *Manager) tryPTY(config AdapterConfig) error {
	adapter := NewPTYAdapter()
	adapter.Subscribe(m.callback)

	if err := adapter.Connect(config); err != nil {
		return err
	}

	m.adapter = adapter
	return nil
}

// Disconnect disconnects the current adapter
func (m *Manager) Disconnect() error {
	if m.adapter == nil {
		return nil
	}
	return m.adapter.Disconnect()
}

// IsConnected returns whether the adapter is connected
func (m *Manager) IsConnected() bool {
	if m.adapter == nil {
		return false
	}
	return m.adapter.IsConnected()
}

// SendMessage sends a message through the current adapter
func (m *Manager) SendMessage(msg Message) error {
	logger.Debug("[%s] SendMessage: adapter=%s, type=%s", logger.ModProtocol, m.adapter.Name(), msg.Type)
	if m.adapter == nil {
		return fmt.Errorf("no adapter connected")
	}
	err := m.adapter.SendMessage(msg)
	if err != nil {
		logger.Warn("[%s] Adapter %s returned error: %v", logger.ModProtocol, m.adapter.Name(), err)
	}
	return err
}

// Subscribe sets the message callback
func (m *Manager) Subscribe(callback func(Message)) {
	m.callback = callback
	if m.adapter != nil {
		m.adapter.Subscribe(callback)
	}
}

// GetAdapter returns the current adapter
func (m *Manager) GetAdapter() Adapter {
	return m.adapter
}

// GetProtocolName returns the name of the current protocol
func (m *Manager) GetProtocolName() string {
	if m.adapter == nil {
		return "none"
	}
	return m.adapter.Name()
}

// Reconnect attempts to reconnect a disconnected session
func (m *Manager) Reconnect(config AdapterConfig) error {
	if m.IsConnected() {
		logger.Debug("[%s] Already connected, skipping reconnect", logger.ModProtocol)
		return nil
	}

	logger.Info("[%s] Attempting to reconnect...", logger.ModProtocol)

	// Disconnect old adapter if exists
	if m.adapter != nil {
		m.Disconnect()
	}

	// Reconnect using same detection logic
	return m.Connect(config)
}
