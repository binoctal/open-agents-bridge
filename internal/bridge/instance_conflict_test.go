package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/gorilla/websocket"
)

func TestOnInstanceConflictBacksOffThenStandsBy(t *testing.T) {
	old := instanceConflictBackoff
	instanceConflictBackoff = time.Millisecond
	defer func() { instanceConflictBackoff = old }()

	b := &Bridge{done: make(chan struct{})}
	for i := 1; i <= maxInstanceConflictRetries; i++ {
		if b.onInstanceConflict() {
			t.Fatalf("rejection %d must retry, not stand by", i)
		}
		if b.standby.Load() {
			t.Fatalf("standby set too early at rejection %d", i)
		}
	}
	if !b.onInstanceConflict() {
		t.Fatal("rejection beyond the retry budget must stop the read loop")
	}
	if !b.standby.Load() {
		t.Fatal("standby flag must be set")
	}
}

func TestOnInstanceConflictAbortsOnShutdown(t *testing.T) {
	old := instanceConflictBackoff
	instanceConflictBackoff = time.Hour
	defer func() { instanceConflictBackoff = old }()

	b := &Bridge{done: make(chan struct{})}
	close(b.done)
	if !b.onInstanceConflict() {
		t.Fatal("shutdown during backoff must stop the loop")
	}
	if b.standby.Load() {
		t.Fatal("shutdown is not standby")
	}
}

func TestInstanceConflictIsNotPermanent(t *testing.T) {
	b := &Bridge{}
	if b.isPermanentCloseCode(closeCodeAnotherInstance) {
		t.Fatal("4009 must stay out of permanentCloseCodes (restart must self-heal)")
	}
}

func TestNewInstanceIDIsUnique(t *testing.T) {
	a, c := newInstanceID(), newInstanceID()
	if a == "" || a == c {
		t.Fatalf("ids must be non-empty and unique: %q %q", a, c)
	}
}

// The dial query carries instanceId and keep_alive frames carry the same id.
func TestConnectQueryAndKeepAliveCarryInstanceID(t *testing.T) {
	var ids []string
	frames := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/server-proof" { // 5e.1 identity probe (dev build: warns, continues)
			http.NotFound(w, r)
			return
		}
		ids = append(ids, r.URL.Query().Get("instanceId"))
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			frames <- string(data)
		}
	}))
	defer srv.Close()

	b := &Bridge{
		config:     &config.Config{ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http"), UserID: "u1", MachineID: "d1", MachineToken: "t"},
		instanceID: newInstanceID(),
		done:       make(chan struct{}),
	}
	if err := b.connect(); err != nil {
		t.Fatal(err)
	}
	b.sendKeepAlive()
	select {
	case f := <-frames:
		if !strings.Contains(f, `"type":"keep_alive"`) || !strings.Contains(f, b.instanceID) {
			t.Fatalf("keep_alive frame missing type/instanceId: %s", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no keep_alive frame received")
	}
	if len(ids) != 1 || ids[0] != b.instanceID {
		t.Fatalf("connect query instanceId = %v, want %s", ids, b.instanceID)
	}
}

func TestKeepAliveSkippedWhileStandbyOrRecentTraffic(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
			got.Add(1)
		}
	}))
	defer srv.Close()

	b := &Bridge{
		config:        &config.Config{ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http"), UserID: "u1", MachineID: "d1"},
		instanceID:    "x",
		done:          make(chan struct{}),
		keepAliveDone: make(chan struct{}),
	}
	if err := b.connect(); err != nil {
		t.Fatal(err)
	}
	defer close(b.done)

	oldInterval := keepAliveInterval
	keepAliveInterval = 20 * time.Millisecond
	defer func() { keepAliveInterval = oldInterval }()
	b.standby.Store(true)
	go b.keepAliveLoop()
	time.Sleep(120 * time.Millisecond)
	if got.Load() != 0 {
		t.Fatalf("standby must not send keep_alive, got %d", got.Load())
	}
}
