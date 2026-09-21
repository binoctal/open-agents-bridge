package bridge

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/reconnect"
	"github.com/binoctal/open-agents-bridge/internal/session"
)

// 2026-09-21 e2e: the reconnect time budget ran from process start, so a
// connection that stayed healthy for >10 minutes exhausted the budget on its
// FIRST read error and fell straight into the 5-minute slow-retry window,
// silently dropping messages the whole time. reconnect() must reset the
// budget at the disconnect moment.
func TestReconnectResetsTimeBudget(t *testing.T) {
	b := &Bridge{
		sessions:          session.NewManager(),
		reconnectStrategy: reconnect.NewStrategy(),
		stateManager:      NewStateManager(),
	}

	// Simulate a budget that already burned: startTime 11 minutes in the past
	// is exactly the state a long-lived healthy connection used to die on.
	// startTime is unexported in another package, so reach it via reflection.
	f := reflect.ValueOf(b.reconnectStrategy).Elem().FieldByName("startTime")
	p := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	p.Set(reflect.ValueOf(time.Now().Add(-11 * time.Minute)))
	if !b.reconnectStrategy.HasExhaustedBudget() {
		t.Fatal("precondition: budget must read exhausted before reconnect()")
	}

	b.reconnect()

	if b.reconnectStrategy.HasExhaustedBudget() {
		t.Fatal("reconnect() must reset the time budget so the next outage gets a fresh 10-minute window")
	}
	if got := b.stateManager.GetState(); got != StateReconnecting {
		t.Fatalf("state after reconnect() = %v, want StateReconnecting", got)
	}
}

// The budget-reset used to live at the tail of sendSessionRestore() — i.e. it
// ran only AFTER a successful reconnect+fetch, mislabeled the state, and
// fired a spurious "connection lost" alert while fully connected. The restore
// path must only fetch the list and forward it.
func TestSendSessionRestoreOnlyFetchesAndForwards(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Write([]byte(`{"sessions":[{"id":"s1","cliType":"claude","workDir":"/tmp","status":"active","effectiveStatus":"active","startedAt":1}]}`))
	}))
	t.Cleanup(srv.Close)

	b := &Bridge{
		config:            &config.Config{ServerURL: srv.URL, DeviceID: "dev-restore", DeviceToken: "token-restore"},
		sessions:          session.NewManager(),
		reconnectStrategy: reconnect.NewStrategy(),
		stateManager:      NewStateManager(),
	}
	b.apiClient = api.NewClient(b.config)
	b.stateManager.SetState(StateConnected, "test")

	b.sendSessionRestore()

	if gotPath != "/api/bridge/sessions?limit=20" {
		t.Fatalf("restore fetched %q, want /api/bridge/sessions?limit=20 (device-token endpoint, no deviceId param)", gotPath)
	}
	if got := b.stateManager.GetState(); got != StateConnected {
		t.Fatalf("sendSessionRestore() changed state to %v; it must not touch connection state", got)
	}

	// No conn set, so the restore message lands in the offline buffer —
	// proving the fetched list was actually forwarded.
	b.offlineMu.Lock()
	defer b.offlineMu.Unlock()
	for _, m := range b.offlineBuf {
		if m.Type == "sessions:restore" {
			return // pass
		}
	}
	t.Fatal("sessions:restore was never sent/buffered")
}
