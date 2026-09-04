package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/session"
)

// updateLastSeen must treat a non-2xx response as a heartbeat failure. It
// used to reset the failure counter before looking at the status, so a
// persistent 401 (stale device token) only DEBUG-logged and the 5-failure
// reconnect never fired — the bridge sat on a dead credential forever.

func newHeartbeatBridge(t *testing.T, status int) (*Bridge, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	b := &Bridge{
		config: &config.Config{
			// updateLastSeen derives the API base from the WS URL (ws->http).
			ServerURL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
			DeviceID:     "device-hb",
			DeviceToken:  "token-hb",
		},
		sessions:   session.NewManager(),
		httpClient: &http.Client{},
	}
	return b, srv
}

func TestHeartbeatNon2xxCountsAsFailure(t *testing.T) {
	b, _ := newHeartbeatBridge(t, http.StatusUnauthorized)

	for i := 1; i <= 4; i++ {
		b.updateLastSeen()
		if b.heartbeatFailures != i {
			t.Fatalf("after %d unauthorized heartbeats, failures = %d, want %d", i, b.heartbeatFailures, i)
		}
	}
}

func TestHeartbeatNon2xxReconnectsOnFifthFailure(t *testing.T) {
	b, _ := newHeartbeatBridge(t, http.StatusUnauthorized)
	// Sentinel: reconnect() clears stale session IDs when no sessions exist,
	// which is the only session-free observable that it ran.
	b.staleSessionIDs = []string{"sentinel"}
	b.heartbeatFailures = 4

	b.updateLastSeen()

	if b.heartbeatFailures != 5 {
		t.Fatalf("failures = %d, want 5", b.heartbeatFailures)
	}
	if b.staleSessionIDs != nil {
		t.Fatal("reconnect was not triggered on the 5th non-2xx heartbeat")
	}
}

func TestHeartbeatSuccessResetsFailureCount(t *testing.T) {
	b, _ := newHeartbeatBridge(t, http.StatusOK)
	b.heartbeatFailures = 4

	b.updateLastSeen()

	if b.heartbeatFailures != 0 {
		t.Fatalf("failures = %d, want 0 after a 200 heartbeat", b.heartbeatFailures)
	}
}
