package session

import (
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// stopDeadAdapter satisfies protocol.Adapter with controllable liveness —
// no real CLI behind it.
type stopDeadAdapter struct{ connected bool }

func (f *stopDeadAdapter) Name() string                         { return "fake" }
func (f *stopDeadAdapter) Version() string                      { return "0" }
func (f *stopDeadAdapter) Connect(protocol.AdapterConfig) error { return nil }
func (f *stopDeadAdapter) Disconnect() error                    { return nil }
func (f *stopDeadAdapter) IsConnected() bool                    { return f.connected }
func (f *stopDeadAdapter) SendMessage(protocol.Message) error   { return nil }
func (f *stopDeadAdapter) ReceiveMessage() (protocol.Message, error) {
	return protocol.Message{}, nil
}
func (f *stopDeadAdapter) Subscribe(func(protocol.Message)) {}
func (f *stopDeadAdapter) Capabilities() []string           { return nil }
func (f *stopDeadAdapter) SupportsPermissions() bool        { return false }
func (f *stopDeadAdapter) SupportsFileOps() bool            { return false }
func (f *stopDeadAdapter) SupportsToolCalls() bool          { return false }
func (f *stopDeadAdapter) Resize(int, int) error            { return nil }

// StopDead is the reconnect-path cleanup: a lost WebSocket must not kill
// sessions whose local process is still running (2026-09-22 prod: the
// unconditional StopAll zombied running task tasks until stuck recovery).
func TestManager_StopDead_KeepsLiveStopsDead(t *testing.T) {
	m := NewManager()
	m.Adopt(&Session{
		ID:        "live",
		Status:    "active",
		CreatedAt: time.Now(),
		Protocol:  protocol.NewManagerWithAdapter(&stopDeadAdapter{connected: true}),
	})
	putSession(m, "dead-nil-protocol", "active")
	m.Adopt(&Session{
		ID:        "dead-disconnected",
		Status:    "active",
		CreatedAt: time.Now(),
		Protocol:  protocol.NewManagerWithAdapter(&stopDeadAdapter{connected: false}),
	})
	// Mid-creation: registered early while Connect() is still running, so
	// the adapter is not connected YET — must not be reaped.
	m.Adopt(&Session{
		ID:         "connecting",
		Status:     "active",
		CreatedAt:  time.Now(),
		Protocol:   protocol.NewManagerWithAdapter(&stopDeadAdapter{connected: false}),
		connecting: true,
	})

	ids := m.StopDead()

	if len(ids) != 2 {
		t.Fatalf("StopDead stopped %d sessions (%v), want exactly the 2 dead ones", len(ids), ids)
	}
	stopped := map[string]bool{}
	for _, id := range ids {
		stopped[id] = true
	}
	if !stopped["dead-nil-protocol"] || !stopped["dead-disconnected"] {
		t.Errorf("stopped IDs = %v, want the two dead sessions", ids)
	}
	if stopped["live"] || stopped["connecting"] {
		t.Errorf("live/connecting session was stopped: %v", ids)
	}
	if m.Get("live") == nil {
		t.Error("live session was removed from the manager")
	}
	if m.Get("connecting") == nil {
		t.Error("connecting session was removed from the manager")
	}
	if m.Count() != 2 {
		t.Errorf("after StopDead Count = %d, want 2 (live + connecting)", m.Count())
	}
}

func TestManager_StopDead_EmptyManager(t *testing.T) {
	m := NewManager()
	if ids := m.StopDead(); len(ids) != 0 {
		t.Errorf("StopDead on empty manager returned %v, want empty", ids)
	}
}
