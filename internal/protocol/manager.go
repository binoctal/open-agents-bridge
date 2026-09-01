package protocol

import (
	"fmt"
	"sync"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/replay"
)

// defaultACPHandshakeTimeout is how long we wait for the ACP process to emit
// its first status message. Generous on purpose: a slow network or a cold npx
// download must not be mistaken for a CLI that cannot speak ACP.
const defaultACPHandshakeTimeout = 60 * time.Second

// Manager manages protocol adapters and auto-detection
type Manager struct {
	adapter  Adapter
	callback func(Message)

	// mu guards adapter/callback: the init callback chain can deliver
	// messages (and readers like GetProtocolName run on those goroutines)
	// concurrently with tryACP/tryPTY still finishing their assignment.
	mu sync.RWMutex

	// acpHandshakeTimeout bounds the wait in tryACP. Zero means
	// defaultACPHandshakeTimeout; tests shorten it via SetACPHandshakeTimeout.
	acpHandshakeTimeout time.Duration

	// recorder, when non-nil, is attached to the ACP adapter before Connect
	// so its wire frames are mirrored to a replay script. Nil (default)
	// keeps recording off. Only the ACP path records — PTY has no frames.
	recorder *replay.Recorder
}

// NewManager creates a new protocol manager
func NewManager() *Manager {
	return &Manager{}
}

// SetACPHandshakeTimeout overrides how long Connect waits for the ACP handshake
// before falling back to PTY. Intended for tests that drive a non-ACP command
// and must not sit through the production timeout.
func (m *Manager) SetACPHandshakeTimeout(d time.Duration) {
	m.acpHandshakeTimeout = d
}

// SetRecorder enables replay recording for the next ACP connection. Must be
// called before Connect; a nil recorder (the default) keeps recording off.
func (m *Manager) SetRecorder(r *replay.Recorder) {
	m.recorder = r
}

// handshakeTimeout returns the configured timeout, or the default.
func (m *Manager) handshakeTimeout() time.Duration {
	if m.acpHandshakeTimeout > 0 {
		return m.acpHandshakeTimeout
	}
	return defaultACPHandshakeTimeout
}

// NewManagerWithAdapter builds a manager around a pre-connected adapter.
// Dependency-injection seam for callers (and tests) that need a manager
// reporting a specific connection state without auto-detecting a real CLI.
func NewManagerWithAdapter(a Adapter) *Manager {
	return &Manager{adapter: a}
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
	// Connect() blocks on the initialize response, so the handshake budget has
	// to reach the adapter — the select below is only entered after it returns.
	if m.acpHandshakeTimeout > 0 {
		adapter.SetInitTimeout(m.acpHandshakeTimeout)
	}
	// Wire-frame recording, if requested. Must precede Connect so the
	// initialize handshake itself is captured. Nil is a no-op.
	adapter.SetRecorder(m.recorder)

	// Channel to receive initialization status
	// We wait up to 60 seconds for initial connection, but once connected,
	// we stay with ACP regardless of authentication status
	initialized := make(chan bool, 1)
	initError := make(chan error, 1)

	// Subscribe to messages to detect initialization
	m.mu.RLock()
	originalCallback := m.callback
	m.mu.RUnlock()
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
		m.mu.Lock()
		m.adapter = adapter
		// Restore original callback after initialization
		if originalCallback != nil {
			adapter.Subscribe(originalCallback)
			m.callback = originalCallback
		}
		m.mu.Unlock()
		logger.Info("[%s] ACP initialized successfully", logger.ModProtocol)
		return nil
	case err := <-initError:
		adapter.Disconnect()
		return fmt.Errorf("ACP connection failed: %w", err)
	case <-time.After(m.handshakeTimeout()):
		// Only timeout if ACP process doesn't respond at all
		// This indicates the CLI doesn't support ACP
		adapter.Disconnect()
		return fmt.Errorf("ACP process did not respond within %s", m.handshakeTimeout())
	}
}

// tryPTY attempts to connect using PTY protocol
func (m *Manager) tryPTY(config AdapterConfig) error {
	adapter := NewPTYAdapter()
	adapter.Subscribe(m.callback)

	if err := adapter.Connect(config); err != nil {
		return err
	}

	m.mu.Lock()
	m.adapter = adapter
	m.mu.Unlock()
	return nil
}

// Disconnect disconnects the current adapter
func (m *Manager) Disconnect() error {
	m.mu.RLock()
	adapter := m.adapter
	m.mu.RUnlock()
	if adapter == nil {
		return nil
	}
	return adapter.Disconnect()
}

// IsConnected returns whether the adapter is connected
func (m *Manager) IsConnected() bool {
	m.mu.RLock()
	adapter := m.adapter
	m.mu.RUnlock()
	if adapter == nil {
		return false
	}
	return adapter.IsConnected()
}

// SendMessage sends a message through the current adapter
func (m *Manager) SendMessage(msg Message) error {
	m.mu.RLock()
	adapter := m.adapter
	m.mu.RUnlock()
	if adapter == nil {
		return fmt.Errorf("no adapter connected")
	}
	logger.Debug("[%s] SendMessage: adapter=%s, type=%s", logger.ModProtocol, adapter.Name(), msg.Type)
	err := adapter.SendMessage(msg)
	if err != nil {
		logger.Warn("[%s] Adapter %s returned error: %v", logger.ModProtocol, adapter.Name(), err)
	}
	return err
}

// Subscribe sets the message callback
func (m *Manager) Subscribe(callback func(Message)) {
	m.mu.Lock()
	m.callback = callback
	adapter := m.adapter
	m.mu.Unlock()
	if adapter != nil {
		adapter.Subscribe(callback)
	}
}

// GetAdapter returns the current adapter
func (m *Manager) GetAdapter() Adapter {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.adapter
}

// GetProtocolName returns the name of the current protocol
func (m *Manager) GetProtocolName() string {
	m.mu.RLock()
	adapter := m.adapter
	m.mu.RUnlock()
	if adapter == nil {
		return "none"
	}
	return adapter.Name()
}

// Reconnect attempts to reconnect a disconnected session
func (m *Manager) Reconnect(config AdapterConfig) error {
	if m.IsConnected() {
		logger.Debug("[%s] Already connected, skipping reconnect", logger.ModProtocol)
		return nil
	}

	logger.Info("[%s] Attempting to reconnect...", logger.ModProtocol)

	// Disconnect old adapter if exists
	if m.GetAdapter() != nil {
		m.Disconnect()
	}

	// Reconnect using same detection logic
	return m.Connect(config)
}
