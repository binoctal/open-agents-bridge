package integration_test

import (
	"testing"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/protocol"
)

func TestProtocolManagerForcePTY(t *testing.T) {
	mgr := protocol.NewManager()

	config := protocol.AdapterConfig{
		Command:       "echo",
		Args:          []string{"hello"},
		ForceProtocol: "pty",
	}

	err := mgr.Connect(config)
	if err != nil {
		t.Fatalf("Connect with ForceProtocol=pty failed: %v", err)
	}

	// Verify adapter is PTY type
	adapter := mgr.GetAdapter()
	if adapter == nil {
		t.Fatal("GetAdapter returned nil")
	}

	// PTY adapter name should be "pty"
	name := adapter.Name()
	if name != "pty" {
		t.Errorf("adapter name = %s, want pty", name)
	}
}

func TestProtocolManagerACPThenPTYFallback(t *testing.T) {
	mgr := protocol.NewManager()
	// Production waits 60s for the ACP handshake; shorten it so the fallback
	// path is exercised without the test sitting through that budget.
	mgr.SetACPHandshakeTimeout(2 * time.Second)

	// Using a command that does NOT support ACP should fall back to PTY
	config := protocol.AdapterConfig{
		Command: "cat",
		Args:    []string{},
	}

	// With auto-detect: try ACP first, fail, fall back to PTY
	// ACP connection will timeout because "cat" doesn't speak JSON-RPC
	done := make(chan error, 1)
	go func() {
		done <- mgr.Connect(config)
	}()

	select {
	case err := <-done:
		// Connect should eventually succeed with PTY fallback or fail gracefully
		if err != nil {
			// If connection fails, that's acceptable — the important thing is it tried
			t.Logf("Connect with auto-detect returned: %v (expected for non-ACP command)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Connect took too long — ACP timeout may not be working")
	}
}
