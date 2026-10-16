package protocol

import (
	"testing"
	"time"
)

// A PTY engine that dies with a non-zero code must surface an error message,
// not just an idle status the user can't distinguish from "finished".
func TestPTYNonZeroExitEmitsError(t *testing.T) {
	a := NewPTYAdapter()
	got := make(chan Message, 8)
	a.Subscribe(func(m Message) { got <- m })
	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", "exit 3"}, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer a.Disconnect()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-got:
			if m.Type == MessageTypeError && m.Meta["code"] == "ENGINE_EXITED" {
				return
			}
		case <-deadline:
			t.Fatal("no ENGINE_EXITED error after non-zero exit")
		}
	}
}
