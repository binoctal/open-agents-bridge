package protocol

import (
	"strings"
	"testing"
	"time"
)

// Known-issue #17: when the ACP agent process DIES (stdout EOF), the
// adapter used to swallow both death signals — readMessages set
// connected=false silently and monitorProcess discarded the wait result —
// so the session never stopped, the manager's exit callback never fired,
// and a workflow task wedged in `running` with no task_error/retry. The
// PTY path reports death as a MessageTypeStatus carrying Meta exit_code
// (bridge.go stops the session on it); the ACP adapter must do the same.
func TestACPAdapterAgentDeathEmitsExitCodeStatus(t *testing.T) {
	// Scripted "agent": answers initialize + session/new, then dies 7.
	agent := `read l; echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"die","version":"1"}}}'; read l; echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}'; exit 7`
	a := NewACPAdapter()
	defer a.Disconnect()

	msgs := make(chan Message, 8)
	a.Subscribe(func(m Message) { msgs <- m })

	// Initialize handshake needs id 1 (initialize) and id 2 (session/new):
	// the adapter assigns ids 1 and 2 in Connect.
	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-msgs:
			if m.Type == MessageTypeStatus {
				if code, ok := m.Meta["exit_code"]; ok {
					if got, ok2 := code.(int); ok2 && got == 7 {
						return // pass: death reported like the PTY path
					}
				}
			}
			if m.Type == MessageTypeError && strings.Contains(str(m.Content), "die") {
				// stderr noise, ignore
			}
		case <-deadline:
			t.Fatal("agent death never reported as a status message with exit_code")
		}
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
