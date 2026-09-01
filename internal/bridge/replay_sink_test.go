package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/gorilla/websocket"
)

// This file is the G17 replay suite's message sink: a local httptest.Server
// that stands in for the orchestrator. It hosts the WS route the bridge
// dials (/ws/<UserID>) and the HTTP callback route the CallbackManager
// posts to (/api/missions/internal/orchestrator/event). Everything the
// bridge reports — WS uplink and callbacks — is collected into one merged,
// arrival-ordered sequence the assertions run against.
//
// No keys, no network: the shim is this test binary itself (helper-process
// pattern), the "agent" is a committed script fixture, and the sink is
// loopback-only. The suite therefore runs unconditionally in CI — there is
// nothing to skip on and no claude binary to depend on.

// Sink channels, mirroring replay.ChannelWS / replay.ChannelCallback.
const (
	sinkChannelWS       = "ws"
	sinkChannelCallback = "callback"
)

// sinkEvent is one entry of the merged uplink sequence.
type sinkEvent struct {
	Seq     int    // arrival order at the sink
	Channel string // "ws" | "callback"
	Type    string
	Payload json.RawMessage
}

// replaySink records everything the bridge sends: WS messages from the
// server side of the dial, and HTTP callback events.
type replaySink struct {
	t   *testing.T
	srv *httptest.Server
	url string // ws://127.0.0.1:port — what goes into config.ServerURL

	mu      sync.Mutex
	events  []sinkEvent
	notify  chan struct{} // closed+replaced whenever an event arrives
	wsConn  *websocket.Conn
	writeMu sync.Mutex // serializes server-side WS writes
	wsReady chan struct{}
}

func newReplaySink(t *testing.T) *replaySink {
	t.Helper()
	s := &replaySink{
		t:       t,
		notify:  make(chan struct{}),
		wsReady: make(chan struct{}),
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	mux := http.NewServeMux()
	// The WS route: the bridge dials /ws/<UserID> with bridge auth query
	// params. The sink accepts any dial — the replay suite tests the bridge
	// side of the link, not the server's auth.
	mux.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.wsConn != nil {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.wsConn = conn
		s.mu.Unlock()
		close(s.wsReady)
		s.readWS(conn)
	})
	// The orchestrator callback route (workflows/callback.go posts here).
	mux.HandleFunc("/api/missions/internal/orchestrator/event", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var ev struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(body, &ev); err != nil || ev.Type == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.record(sinkChannelCallback, ev.Type, ev.Payload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	// Everything else (rule-sync GETs and friends) gets an empty 200 so the
	// startup syncs succeed and their retries never add noise.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	s.srv = httptest.NewServer(mux)
	s.url = "ws://" + s.srv.Listener.Addr().String()
	t.Cleanup(s.srv.Close)
	t.Cleanup(func() {
		s.mu.Lock()
		conn := s.wsConn
		s.mu.Unlock()
		if conn != nil {
			conn.Close()
		}
	})
	return s
}

// readWS drains the bridge's WS uplink into the merged sequence.
func (s *replaySink) readWS(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &msg); err != nil || msg.Type == "" {
			continue
		}
		s.record(sinkChannelWS, msg.Type, msg.Payload)
	}
}

func (s *replaySink) record(channel, typ string, payload json.RawMessage) {
	s.mu.Lock()
	// Payloads of live-decoded maps carry no raw bytes — remarshal for a
	// stable comparison basis.
	if payload == nil {
		payload = json.RawMessage("null")
	}
	s.events = append(s.events, sinkEvent{
		Seq:     len(s.events),
		Channel: channel,
		Type:    typ,
		Payload: payload,
	})
	close(s.notify)
	s.notify = make(chan struct{})
	s.mu.Unlock()
}

// sendTaskAssign dispatches a workflow:task_assign from the orchestrator
// side, exactly as the real server would.
func (s *replaySink) sendTaskAssign(jobID, taskID, agent string) {
	s.t.Helper()
	msg := Message{
		Type: "workflow:task_assign",
		Payload: map[string]interface{}{
			"jobId":       jobID,
			"taskId":      taskID,
			"agent":       agent,
			"title":       "Replay fixture task " + taskID,
			"description": "Deterministic fixture task for the replay suite.",
		},
		Timestamp: time.Now().UnixMilli(),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatalf("marshal task_assign: %v", err)
	}
	s.mu.Lock()
	conn := s.wsConn
	s.mu.Unlock()
	if conn == nil {
		s.t.Fatal("sendTaskAssign: bridge WS not connected yet")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		s.t.Fatalf("write task_assign: %v", err)
	}
}

// snapshot returns the merged sequence observed so far.
func (s *replaySink) snapshot() []sinkEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sinkEvent, len(s.events))
	copy(out, s.events)
	return out
}

// waitFor blocks until an event matching pred arrives, or the timeout
// fires. The timeout error lists everything that DID arrive, so a failure
// reads as "event X missing — here is what the bridge actually sent"
// instead of a bare deadline.
func (s *replaySink) waitFor(timeout time.Duration, desc string, pred func(sinkEvent) bool) sinkEvent {
	deadline := time.Now().Add(timeout)
	for {
		s.mu.Lock()
		for _, ev := range s.events {
			if pred(ev) {
				s.mu.Unlock()
				return ev
			}
		}
		notify := s.notify
		s.mu.Unlock()

		timeUntilDeadline := time.Until(deadline)
		if timeUntilDeadline <= 0 {
			var seen []string
			for _, ev := range s.snapshot() {
				seen = append(seen, fmt.Sprintf("%s %s", ev.Channel, ev.Type))
			}
			s.t.Fatalf("timed out after %v waiting for %s; events seen so far: %v",
				timeout, desc, seen)
		}
		select {
		case <-notify:
		case <-time.After(minDuration(timeUntilDeadline, 100*time.Millisecond)):
			// Safety poll: catches events recorded before we grabbed notify.
		}
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// payloadString digs a nested string field out of a JSON payload.
func payloadString(t *testing.T, ev sinkEvent, fields ...string) string {
	t.Helper()
	var cur any
	if err := json.Unmarshal(ev.Payload, &cur); err != nil {
		t.Fatalf("event %s %s: payload is not JSON: %s", ev.Channel, ev.Type, ev.Payload)
	}
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("event %s %s: payload is not an object at field %q", ev.Channel, ev.Type, f)
		}
		cur, ok = m[f]
		if !ok {
			t.Fatalf("event %s %s: no field %q in payload %s", ev.Channel, ev.Type, f, ev.Payload)
		}
	}
	str, ok := cur.(string)
	if !ok {
		t.Fatalf("event %s %s: field %q is not a string", ev.Channel, ev.Type, fields[len(fields)-1])
	}
	return str
}

// startReplayBridge boots a real Bridge against the sink with the replay
// cliType wired through the environment (design D4): the shim is this test
// binary, the script is scriptPath. maxConcurrent sets the process pool
// size. The bridge is torn down via Stop() in t.Cleanup.
func startReplayBridge(t *testing.T, sink *replaySink, scriptPath string, maxConcurrent int) *Bridge {
	t.Helper()

	// HOME isolation keeps the session store and config dir out of the real
	// home; the socket dir keeps the permission unix socket collision-free.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPEN_AGENTS_SOCKET_DIR", filepath.Join(home, "sock"))
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", scriptPath)

	cfg := &config.Config{
		UserID:      "user-replay",
		DeviceID:    "device-replay",
		DeviceToken: "token-replay",
		ServerURL:   sink.url,
		// No E2EE keys: uplink stays plaintext, which is what the sink parses.
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	b.sessions.SetMaxConcurrent(maxConcurrent)

	t.Cleanup(b.Stop)

	done := make(chan error, 1)
	go func() { done <- b.Start() }()

	select {
	case <-sink.wsReady:
	case err := <-done:
		t.Fatalf("bridge exited during startup: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("bridge never dialed the sink WS")
	}

	// Surface a late Start() failure (e.g. connect error after a reconnect)
	// instead of letting the assertions time out with no clue.
	go func() {
		if err := <-done; err != nil {
			t.Errorf("bridge.Start returned error: %v", err)
		}
	}()
	return b
}

// fixtureScript returns the absolute path of a committed replay fixture.
func fixtureScript(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "test", "fixtures", "replay", name)
}
