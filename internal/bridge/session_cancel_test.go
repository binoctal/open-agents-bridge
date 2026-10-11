package bridge

import (
	"testing"
	"time"
)

// A cancel for a session the bridge no longer holds (restart lost it) must
// still be acked, or the web stays on "cancelling…" forever.
func TestSessionCancelUnknownSessionStillAcked(t *testing.T) {
	b := newSendBridge(t)

	b.handleSessionCancel(Message{
		Type: "session:cancel",
		Payload: map[string]interface{}{
			"sessionId": "sess-gone",
			"machineId": "dev-recreate",
		},
		Timestamp: time.Now().UnixMilli(),
	})

	payload, ok := findOffline(t, b, "session:cancelled")
	if !ok {
		t.Fatal("expected session:cancelled ack for an unknown session")
	}
	if payload["sessionId"] != "sess-gone" {
		t.Fatalf("sessionId = %v", payload["sessionId"])
	}
}
