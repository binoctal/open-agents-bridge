package bridge

import (
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/protocol"
	"github.com/open-agents/open-agents-bridge/internal/session"
)

// fakeAdapter satisfies protocol.Adapter for liveness tests — no real CLI.
type fakeAdapter struct{ connected bool }

func (f *fakeAdapter) Name() string    { return "fake" }
func (f *fakeAdapter) Version() string { return "0" }
func (f *fakeAdapter) Connect(protocol.AdapterConfig) error {
	return nil
}
func (f *fakeAdapter) Disconnect() error { return nil }
func (f *fakeAdapter) IsConnected() bool { return f.connected }
func (f *fakeAdapter) SendMessage(protocol.Message) error {
	return nil
}
func (f *fakeAdapter) ReceiveMessage() (protocol.Message, error) {
	return protocol.Message{}, nil
}
func (f *fakeAdapter) Subscribe(func(protocol.Message))        {}
func (f *fakeAdapter) Capabilities() []string                  { return nil }
func (f *fakeAdapter) SupportsPermissions() bool               { return false }
func (f *fakeAdapter) SupportsFileOps() bool                   { return false }
func (f *fakeAdapter) SupportsToolCalls() bool                 { return false }
func (f *fakeAdapter) Resize(int, int) error                   { return nil }

// Known-issue #20: a re-dispatch for a task this bridge is already executing
// used to REPLACE the healthy session (worktree branch existed → workDir
// fell back to "." → resume failed on workDir mismatch → old session killed,
// new one in the wrong directory, output lost). The dispatch path must
// recognize a live task session and leave it alone.
func TestIsLiveTaskSession(t *testing.T) {
	live := &session.Session{
		Status:   "active",
		Protocol: protocol.NewManagerWithAdapter(&fakeAdapter{connected: true}),
	}
	if !isLiveTaskSession(live) {
		t.Fatal("active session with connected protocol must be live")
	}

	cases := map[string]*session.Session{
		"nil session":       nil,
		"completed status":  {Status: "completed", Protocol: protocol.NewManagerWithAdapter(&fakeAdapter{connected: true})},
		"nil protocol":      {Status: "active"},
		"disconnected proc": {Status: "active", Protocol: protocol.NewManagerWithAdapter(&fakeAdapter{connected: false})},
	}
	for name, sess := range cases {
		if isLiveTaskSession(sess) {
			t.Errorf("%s: must not be live", name)
		}
	}
}
