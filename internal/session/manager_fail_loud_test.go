package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// Spec cli-protocol-fallback scenario "ACP-only 的 CLI 握手超时 fail loud":
// an agent that never answers initialize must produce a fail-loud session
// error, never a PTY respawn of the same ACP command (the 2026-11-07
// zombie-session incident).
func TestCreateSessionFailsLoudOnSlowACPAgent(t *testing.T) {
	// A shim that starts, then never says anything: sleep well past the
	// (shortened) handshake timeout.
	shim := filepath.Join(t.TempDir(), "silent-acp.sh")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OA_REPLAY_SHIM", shim)
	t.Setenv("OA_REPLAY_SHIM_ARGS", "")
	t.Setenv("OA_REPLAY_SCRIPT", "")

	m := NewManager()
	m.testProtocolMgrHook = func(pm *protocol.Manager) {
		pm.SetACPHandshakeTimeout(500 * time.Millisecond)
	}

	start := time.Now()
	_, err := m.CreateWithIDAndSize("replay", t.TempDir(), "sess-slow-acp", 120, 30, "default")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected session creation to fail for a silent ACP agent")
	}
	if !strings.Contains(err.Error(), "无 PTY 备用模式") {
		t.Fatalf("error must be the fail-loud branch (no PTY respawn), got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fail loud must be quick, took %v", elapsed)
	}
	// No session may linger in the registry after the failure.
	if s := m.Get("sess-slow-acp"); s != nil {
		t.Fatal("failed session must not stay registered")
	}
}
