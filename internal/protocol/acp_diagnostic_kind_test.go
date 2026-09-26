package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// session-output-tiering: bridge-generated prose aimed at the user (turn
// stalled watchdog, refusal) must tag Meta.kind="diagnostic" so the web
// client can fold it into run details instead of the agent's prose bubble.
// The refusal path is exercised end-to-end below; the watchdog path uses
// the same emitMessage mechanism and is guarded by a source assertion (its
// 5-minute idle window is not test-waitable).

func TestRefusalMessageTaggedDiagnostic(t *testing.T) {
	dir := t.TempDir()

	// Fake agent: initialize, create a session, then answer the prompt with
	// stopReason "refusal".
	agent := "read -r line\n" +
		"echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"agentInfo\":{\"name\":\"t\",\"version\":\"1\"}}}'\n" +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in\n" +
		"    *session/new*) echo '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"s1\"}}' ;;\n" +
		"    *session/prompt*) echo '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"stopReason\":\"refusal\"}}' ;;\n" +
		"  esac\n" +
		"done"

	a := NewACPAdapter()
	defer a.Disconnect()

	msgs := make(chan Message, 16)
	a.Subscribe(func(m Message) { msgs <- m })

	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: dir}); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	if err := a.SendMessage(Message{Type: MessageTypeContent, Content: "hello"}); err != nil {
		t.Fatalf("SendMessage(prompt) failed: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case m := <-msgs:
			if m.Type == MessageTypeError {
				kind, _ := m.Meta["kind"].(string)
				if kind != "diagnostic" {
					t.Errorf("refusal error Meta.kind = %q, want \"diagnostic\"", kind)
				}
				if sr, _ := m.Meta["stopReason"].(string); sr != "refusal" {
					t.Errorf("refusal error Meta.stopReason = %q, want \"refusal\"", sr)
				}
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("refusal error message was never emitted")
}

func TestWatchdogStallMessageTaggedDiagnostic(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "acp.go"))
	if err != nil {
		t.Fatalf("cannot read acp.go: %v", err)
	}
	text := string(src)
	stallIdx := strings.Index(text, "agent turn stalled")
	if stallIdx < 0 {
		t.Fatal("watchdog stall message text not found in acp.go")
	}
	// The Meta map for the stall emit is a few lines below the message text;
	// require the diagnostic tag inside that window.
	window := text[stallIdx : stallIdx+600]
	if !strings.Contains(window, `"kind":     "diagnostic"`) &&
		!strings.Contains(window, `"kind": "diagnostic"`) {
		t.Errorf("watchdog stall emit does not tag Meta.kind=\"diagnostic\" within 600 chars of the message text:\n%s", window)
	}
}
