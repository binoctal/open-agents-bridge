package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 2026-10-16: a tool call that never finished left isProcessing=true; the
// user's next prompt waited 60s and was then dropped with "timed out waiting
// for agent". A wedged turn must be cancelled and the new prompt delivered.
func TestNewPromptCancelsWedgedTurn(t *testing.T) {
	oldWait, oldGrace := staleTurnWait, staleTurnCancelGrace
	staleTurnWait, staleTurnCancelGrace = 200*time.Millisecond, 200*time.Millisecond
	defer func() { staleTurnWait, staleTurnCancelGrace = oldWait, oldGrace }()

	dir := t.TempDir()
	capture := filepath.Join(dir, "stdin.log")
	agent := "echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"agentInfo\":{\"name\":\"t\",\"version\":\"1\"}}}'\n" +
		"while IFS= read -r line; do printf '%s\\n' \"$line\" >> " + capture + "; done"

	a := NewACPAdapter()
	defer a.Disconnect()
	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: dir}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	a.setSessionID("s1")
	a.isProcessing.Store(true) // wedged: no turn-end will ever arrive

	if err := a.SendMessage(Message{Type: MessageTypeContent, Content: "hello"}); err != nil {
		t.Fatalf("SendMessage returned %v, want the prompt delivered", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(capture)
		s := string(data)
		ci, pi := strings.Index(s, "session/cancel"), strings.Index(s, "session/prompt")
		if ci >= 0 && pi > ci {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, _ := os.ReadFile(capture)
	t.Fatalf("expected session/cancel followed by session/prompt, agent saw:\n%s", data)
}

// startCaptureAgent connects an adapter to a fake agent that records every
// line it receives, with a short stale-turn window for the test.
func startCaptureAgent(t *testing.T) (*ACPAdapter, func() string) {
	t.Helper()
	oldWait, oldGrace := staleTurnWait, staleTurnCancelGrace
	staleTurnWait, staleTurnCancelGrace = 300*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { staleTurnWait, staleTurnCancelGrace = oldWait, oldGrace })

	dir := t.TempDir()
	capture := filepath.Join(dir, "stdin.log")
	agent := "echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"agentInfo\":{\"name\":\"t\",\"version\":\"1\"}}}'\n" +
		"while IFS= read -r line; do printf '%s\\n' \"$line\" >> " + capture + "; done"

	a := NewACPAdapter()
	t.Cleanup(func() { a.Disconnect() })
	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: dir}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	a.setSessionID("s1")
	return a, func() string { data, _ := os.ReadFile(capture); return string(data) }
}

// 2026-10-16: a turn waiting on the user's approval was cancelled 60s after
// they typed a new message; the approval they then gave arrived too late and
// Claude reported "user rejected". An open permission question is not a
// wedged turn.
func TestNewPromptWaitsWhilePermissionIsOpen(t *testing.T) {
	a, wire := startCaptureAgent(t)
	a.isProcessing.Store(true)
	a.handlePermissionRequest(map[string]interface{}{
		"id": float64(7),
		"params": map[string]interface{}{
			"toolCall": map[string]interface{}{"toolCallId": "tc1", "title": "Write"},
			"options":  []interface{}{map[string]interface{}{"optionId": "allow"}},
		},
	})

	done := make(chan error, 1)
	go func() { done <- a.SendMessage(Message{Type: MessageTypeContent, Content: "next"}) }()

	time.Sleep(1 * time.Second) // > 3x staleTurnWait
	if s := wire(); strings.Contains(s, "session/cancel") || strings.Contains(s, "session/prompt") {
		t.Fatalf("turn awaiting approval was cancelled or overtaken:\n%s", s)
	}

	// The user approves; the turn then ends normally.
	if err := a.SendMessage(Message{Type: MessageTypePermission, Content: PermissionResponse{ID: float64(7), OptionID: "allow"}}); err != nil {
		t.Fatalf("permission reply: %v", err)
	}
	a.isProcessing.Store(false)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new prompt never went out after the turn ended")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(wire(), "session/prompt") {
		time.Sleep(50 * time.Millisecond)
	}
	if s := wire(); strings.Contains(s, "session/cancel") || !strings.Contains(s, "session/prompt") {
		t.Fatalf("want prompt without cancel, agent saw:\n%s", s)
	}
}

// A turn still producing wire traffic is busy, not wedged.
func TestNewPromptWaitsWhileTurnIsActive(t *testing.T) {
	a, wire := startCaptureAgent(t)
	a.isProcessing.Store(true)

	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				a.lastActivity.Store(time.Now().UnixNano())
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- a.SendMessage(Message{Type: MessageTypeContent, Content: "next"}) }()

	time.Sleep(1 * time.Second)
	if s := wire(); strings.Contains(s, "session/cancel") {
		t.Fatalf("active turn was cancelled:\n%s", s)
	}

	// Activity stops: now it is wedged and must be cancelled.
	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wedged turn was never cancelled")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := wire()
		if ci, pi := strings.Index(s, "session/cancel"), strings.Index(s, "session/prompt"); ci >= 0 && pi > ci {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected cancel then prompt, agent saw:\n%s", wire())
}

// 2026-10-16: after the idle watchdog fired, only the bridge-side latch was
// cleared; the agent stayed inside the abandoned turn and every following
// prompt went silent for another full idle window. The watchdog must cancel
// the agent's turn too.
func TestWatchdogCancelsAgentTurn(t *testing.T) {
	a, seen := startCaptureAgent(t)
	a.isProcessing.Store(true)

	a.fireTurnWatchdog(31 * time.Minute)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(seen(), `"session/cancel"`) {
			if a.isProcessing.Load() {
				t.Fatal("isProcessing must be cleared")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("watchdog did not send session/cancel, agent saw:\n%s", seen())
}
