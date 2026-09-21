package protocol

import (
	"strings"
	"testing"
	"time"
)

// 2026-09-21 e2e: the Kiri CLI writes structured progress/probe logs to
// stderr ([session/create] phase=…, [session/query] resume=…, [authStatus] …).
// readErrors() used to forward them as MessageTypeError, so creating a
// perfectly healthy session painted a burst of fake session:error events in
// the Web UI.

func TestIsCliProgressLog(t *testing.T) {
	suppressed := []string{
		"[session/create] sessionId=66c9aeb5 phase=validate-cwd durationMs=0 totalMs=0",
		"[session/create] sessionId=66c9aeb5 phase=settings durationMs=37 totalMs=38",
		"[session/create] sessionId=66c9aeb5 phase=prepare-query resume=none",
		"[session/query] sessionId=66c9aeb5 resume=none apiType=native baseUrl=native",
		"[session/create] sessionId=66c9aeb5 phase=sdk-initialize durationMs=2100 totalMs=2245",
		"[authStatus] session account carries no identity signal; keeping probe",
	}
	for _, line := range suppressed {
		if !isCliProgressLog(line) {
			t.Errorf("progress log not suppressed: %q", line)
		}
	}

	forwarded := []string{
		"[session/create] failed: command not found",
		"fatal: agent process crashed",
		"Error: EACCES permission denied",
		"",
	}
	for _, line := range forwarded {
		if isCliProgressLog(line) {
			t.Errorf("real error wrongly classified as progress: %q", line)
		}
	}
}

// End-to-end over a real stderr pipe: progress lines must never surface as
// MessageTypeError, while an unknown error line on the same stream must.
func TestReadErrorsSuppressesProgressForwardsUnknown(t *testing.T) {
	agent := `echo '[session/create] sessionId=s1 phase=validate-cwd durationMs=0 totalMs=0' >&2
echo '[session/query] sessionId=s1 resume=none apiType=native baseUrl=native' >&2
echo '[authStatus] no identity signal; keeping probe' >&2
echo 'fatal: agent crashed unexpectedly' >&2
read l; echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"p","version":"1"}}}'
read l; echo '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s1"}}'
sleep 2`

	a := NewACPAdapter()
	defer a.Disconnect()

	msgs := make(chan Message, 16)
	a.Subscribe(func(m Message) { msgs <- m })

	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-msgs:
			if m.Type != MessageTypeError {
				continue
			}
			content, _ := m.Content.(string)
			if strings.HasPrefix(content, "[session/") || strings.HasPrefix(content, "[authStatus]") {
				t.Fatalf("progress log leaked as session:error: %q", content)
			}
			if strings.Contains(content, "fatal: agent crashed") {
				return // pass: unknown error still forwarded
			}
		case <-deadline:
			t.Fatal("unknown stderr error was never forwarded")
		}
	}
}
