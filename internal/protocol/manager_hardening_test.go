package protocol

import (
	"strings"
	"testing"
	"time"
)

// The 2026-11-07 zombie-session incident: manager.go defined a generous 60s
// handshake timeout but only injected it when a test had called
// SetACPHandshakeTimeout explicitly; production silently ran on the adapter's
// own 30s default while tests stayed green. These tests pin the single-source
// contract: the adapter is constructed with the one constant, and the manager
// passes its timeout unconditionally.

func TestACPAdapterConstructsWithSingleSourceTimeout(t *testing.T) {
	a := NewACPAdapter()
	a.mu.Lock()
	got := a.initTimeout
	a.mu.Unlock()
	if got != defaultACPHandshakeTimeout {
		t.Fatalf("adapter initTimeout = %v, want the single source default %v", got, defaultACPHandshakeTimeout)
	}
}

func TestManagerHandshakeTimeoutDefaultsToSingleSource(t *testing.T) {
	m := NewManager()
	if got := m.handshakeTimeout(); got != defaultACPHandshakeTimeout {
		t.Fatalf("manager handshakeTimeout() = %v, want %v", got, defaultACPHandshakeTimeout)
	}
	m.SetACPHandshakeTimeout(1500 * time.Millisecond)
	if got := m.handshakeTimeout(); got != 1500*time.Millisecond {
		t.Fatalf("explicit override not honored: %v", got)
	}
}

// Known ACP-only CLIs must fail loud when the ACP handshake fails: retrying
// the same JSON-RPC server under PTY would answer every user message with a
// -32700 parse error while looking connected.
func TestConnectFailsLoudWithoutPTYFallback(t *testing.T) {
	m := NewManager()
	// A command that cannot start makes tryACP fail fast (exec error), long
	// before any handshake timeout.
	m.SetACPHandshakeTimeout(2 * time.Second)
	err := m.Connect(AdapterConfig{
		Command:            "/nonexistent-cli-for-fail-loud-test",
		PTYFallbackAllowed: false,
	})
	if err == nil {
		t.Fatal("expected Connect to fail for a nonexistent command")
	}
	if !strings.Contains(err.Error(), "无 PTY 备用模式") {
		t.Fatalf("error must identify the fail-loud branch, got: %v", err)
	}
	if m.GetProtocolName() != "none" {
		t.Fatalf("no adapter may be left connected, got %q", m.GetProtocolName())
	}
}

// Unknown/custom commands keep the auto-detect fallback: PTY is a legitimate
// guess when the command may be an interactive REPL. With PTYFallbackAllowed
// the error (if any) must NOT be the fail-loud message.
func TestUnknownCommandKeepsPTYFallback(t *testing.T) {
	m := NewManager()
	m.SetACPHandshakeTimeout(2 * time.Second)
	err := m.Connect(AdapterConfig{
		Command:            "/nonexistent-cli-for-fallback-test",
		PTYFallbackAllowed: true,
	})
	if err == nil {
		// Both adapters failed to start a nonexistent binary — an error is
		// expected, but it must come from the PTY attempt, not the fail-loud
		// branch.
		t.Log("command unexpectedly connected")
	}
	if err != nil && strings.Contains(err.Error(), "无 PTY 备用模式") {
		t.Fatalf("fallback-allowed config must not hit the fail-loud branch: %v", err)
	}
}
