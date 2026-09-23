package protocol

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Terminal gating (staging forensics 2026-09-23): the ACP agent delegates
// shell execution to the bridge via terminal/create, so the SDK's own
// permission flow never fires for these commands. Sessions whose permission
// mode does not explicitly auto-approve must gate the command on a user
// permission round-trip; accept-edits/accept-all execute immediately
// (workflow task sessions run accept-edits with no human to answer).

// newGatedAdapter returns a connected-enough adapter: handleMessage and
// SendMessage only need a writable stdin (JSON-RPC replies) and the
// connected flag.
func newGatedAdapter(t *testing.T, permMode string) (*ACPAdapter, chan Message) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	a := NewACPAdapter()
	a.permMode = permMode
	a.stdin = w
	a.connected.Store(true)
	msgs := make(chan Message, 16)
	a.Subscribe(func(m Message) { msgs <- m })
	return a, msgs
}

func terminalCreateFrame(command string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "terminal/create",
		"params": map[string]interface{}{
			"command": command,
		},
	}
}

func waitTerminalDone(t *testing.T, a *ACPAdapter, terminalID string) *terminalState {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		a.terminalMu.RLock()
		state, ok := a.terminals[terminalID]
		a.terminalMu.RUnlock()
		if ok && state.done {
			return state
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatal("terminal command never finished")
		}
	}
}

func TestACPTerminalGatedInDefaultMode(t *testing.T) {
	a, msgs := newGatedAdapter(t, "default")

	a.handleMessage(terminalCreateFrame("echo gated-hi"))

	// The permission request must arrive before any execution.
	select {
	case m := <-msgs:
		if m.Type != MessageTypePermission {
			t.Fatalf("expected permission request first, got %v", m.Type)
		}
		req, ok := m.Content.(PermissionRequest)
		if !ok {
			t.Fatalf("permission content type %T", m.Content)
		}
		id, ok := req.ID.(string)
		if !ok || id == "" {
			t.Fatalf("permission id must be the terminalID string, got %v", req.ID)
		}
		if req.ToolInput["command"] != "echo gated-hi" {
			t.Fatalf("command not surfaced to the user: %v", req.ToolInput)
		}
		if len(req.Options) == 0 {
			t.Fatal("options must be non-empty or the web UI replies without optionId and the answer never reaches the adapter")
		}

		// Not executed yet — the gate must hold until the answer.
		a.terminalMu.RLock()
		state := a.terminals[id]
		a.terminalMu.RUnlock()
		if state == nil || state.done {
			t.Fatal("terminal command executed before approval")
		}

		// Approve: the reply routes through SendMessage with the same id.
		if err := a.SendMessage(Message{Type: MessageTypePermission, Content: PermissionResponse{ID: id, OptionID: "allow_once"}}); err != nil {
			t.Fatalf("send permission response: %v", err)
		}
		done := waitTerminalDone(t, a, id)
		if done.exitCode != 0 || !strings.Contains(done.output, "gated-hi") {
			t.Fatalf("approved command did not run: exit=%d output=%q", done.exitCode, done.output)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("default mode executed a terminal command without asking")
	}
}

func TestACPTerminalGatedReject(t *testing.T) {
	a, msgs := newGatedAdapter(t, "default")

	a.handleMessage(terminalCreateFrame("echo should-not-run"))

	m := <-msgs
	req := m.Content.(PermissionRequest)
	id := req.ID.(string)

	if err := a.SendMessage(Message{Type: MessageTypePermission, Content: PermissionResponse{ID: id, OptionID: "reject_once"}}); err != nil {
		t.Fatalf("send permission response: %v", err)
	}
	done := waitTerminalDone(t, a, id)
	if done.exitCode != 126 {
		t.Fatalf("rejected command must exit 126, got %d", done.exitCode)
	}
	if strings.Contains(done.output, "should-not-run") {
		t.Fatal("rejected command was executed")
	}
	if !strings.Contains(done.output, "rejected") {
		t.Fatalf("rejection output must say so, got %q", done.output)
	}
}

func TestACPTerminalGatedTimeout(t *testing.T) {
	a, msgs := newGatedAdapter(t, "default")
	a.permPromptTimeout = 50 * time.Millisecond

	a.handleMessage(terminalCreateFrame("echo never"))

	m := <-msgs
	id := m.Content.(PermissionRequest).ID.(string)

	done := waitTerminalDone(t, a, id)
	if done.exitCode != 126 {
		t.Fatalf("timed-out command must exit 126, got %d", done.exitCode)
	}
	a.terminalMu.RLock()
	pending := len(a.pendingPerms)
	a.terminalMu.RUnlock()
	if pending != 0 {
		t.Fatalf("timed-out entry leaked in pendingPerms (%d)", pending)
	}
}

func TestACPTerminalNotGatedInAutoApproveModes(t *testing.T) {
	for _, mode := range []string{"accept-edits", "accept-all"} {
		a, msgs := newGatedAdapter(t, mode)

		a.handleMessage(terminalCreateFrame("echo free-" + mode))

		// No permission request, command runs to completion.
		done := waitTerminalDone(t, a, lastTerminalID(a))
		if done.exitCode != 0 {
			t.Fatalf("%s: command should run freely, exit=%d output=%q", mode, done.exitCode, done.output)
		}
		select {
		case m := <-msgs:
			t.Fatalf("%s: unexpected message while ungated: %v", mode, m.Type)
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// lastTerminalID returns the newest terminal in the map (single entry in
// these tests, but the id is time-derived so we cannot predict it).
func lastTerminalID(a *ACPAdapter) string {
	a.terminalMu.RLock()
	defer a.terminalMu.RUnlock()
	for id := range a.terminals {
		return id
	}
	return ""
}

// A permission response whose id names no pending terminal must fall
// through to the agent JSON-RPC reply path untouched (an agent-initiated
// session/request_permission answer).
func TestACPPendingTerminalMissFallsThrough(t *testing.T) {
	a, _ := newGatedAdapter(t, "default")

	if a.resolvePendingTerminal("term_unknown", true, "") {
		t.Fatal("unknown id must not resolve")
	}
}
