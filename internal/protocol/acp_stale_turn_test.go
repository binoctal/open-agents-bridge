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
