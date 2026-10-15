package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/binoctal/open-agents-bridge/internal/permission"
)

// wsPair connects b to a local WS server and returns a channel of the frames
// the server side receives.
func wsPair(t *testing.T, b *Bridge) <-chan Message {
	t.Helper()
	frames := make(chan Message, 16)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m Message
			if json.Unmarshal(data, &m) == nil {
				frames <- m
			}
		}
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	b.conn = conn
	return frames
}

func permReq(id, sessionID string) Message {
	return Message{
		Type: "permission:request",
		Payload: map[string]interface{}{
			"sessionId": sessionID, "machineId": "dev-recreate", "id": id,
			"toolName": "Bash", "options": []string{"allow_once", "reject_once"},
		},
		Timestamp: time.Now().Add(-10 * time.Minute).UnixMilli(),
	}
}

func expectPermFrame(t *testing.T, frames <-chan Message, wantID string) {
	t.Helper()
	select {
	case m := <-frames:
		if m.Type != "permission:request" {
			t.Fatalf("got %s, want permission:request", m.Type)
		}
		if got := m.Payload.(map[string]interface{})["id"]; got != wantID {
			t.Fatalf("resent id = %v, want %s", got, wantID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no permission:request for %s was re-sent", wantID)
	}
}

func expectNoFrame(t *testing.T, frames <-chan Message) {
	t.Helper()
	select {
	case m := <-frames:
		t.Fatalf("unexpected frame %s", m.Type)
	case <-time.After(300 * time.Millisecond):
	}
}

// An approval written into a dead socket is never buffered offline, so the
// engine waited for an answer nobody could give. A reconnect must re-emit
// every unanswered request whose session is still alive, drop the rest, and
// stop once the user has answered.
func TestPendingPermissionResentAfterReconnect(t *testing.T) {
	dir := FixtureWorkspace(t)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)
	b.permHandler = permission.NewHandler()
	b.handleSessionResume(Message{
		Type: "session:resume",
		Payload: map[string]interface{}{
			"sessionId": "sess-perm", "machineId": "dev-recreate", "cliType": "replay",
			"workDir": dir, "permissionMode": "default",
		},
		Timestamp: time.Now().UnixMilli(),
	})
	if b.sessions.Get("sess-perm") == nil {
		t.Fatal("session should exist")
	}
	frames := wsPair(t, b)

	b.permPending = map[string]Message{
		pendingPermKey("sess-perm", "p-live"): permReq("p-live", "sess-perm"),
		pendingPermKey("sess-dead", "p-gone"): permReq("p-gone", "sess-dead"),
	}
	b.permSessionMap = map[string]string{"p-live": "sess-perm", "p-gone": "sess-dead"}

	b.resendPendingPermissions("")
	expectPermFrame(t, frames, "p-live")
	expectNoFrame(t, frames)
	if _, ok := b.permPending[pendingPermKey("sess-dead", "p-gone")]; ok {
		t.Error("request of a vanished session must be dropped, not kept forever")
	}

	// Answered -> never re-sent again.
	b.handlePermissionResponse(Message{
		Type:    "permission:response",
		Payload: map[string]interface{}{"id": "p-live", "approved": true, "optionId": "allow_once"},
	})
	b.resendPendingPermissions("")
	expectNoFrame(t, frames)
}

// session:resume re-pushes only that session's pending approvals.
func TestPendingPermissionResentOnlyForResumedSession(t *testing.T) {
	dir := FixtureWorkspace(t)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)
	for _, sid := range []string{"sess-a", "sess-b"} {
		b.handleSessionResume(Message{
			Type: "session:resume",
			Payload: map[string]interface{}{
				"sessionId": sid, "machineId": "dev-recreate", "cliType": "replay",
				"workDir": dir, "permissionMode": "default",
			},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	frames := wsPair(t, b)
	b.permPending = map[string]Message{
		pendingPermKey("sess-a", "pa"): permReq("pa", "sess-a"),
		pendingPermKey("sess-b", "pb"): permReq("pb", "sess-b"),
	}

	b.resendPendingPermissions("sess-b")
	expectPermFrame(t, frames, "pb")
	expectNoFrame(t, frames)
}

// A path that stops delivering must surface as a read error within the
// timeout, while a live one (pongs coming back) must survive past it.
func TestReadDeadlineTripsOnSilenceAndSurvivesPongs(t *testing.T) {
	const d = 300 * time.Millisecond
	dial := func(serverReads bool) *websocket.Conn {
		up := websocket.Upgrader{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			if serverReads { // reading is what makes gorilla answer pings
				for {
					if _, _, err := c.ReadMessage(); err != nil {
						return
					}
				}
			}
			time.Sleep(3 * time.Second)
		}))
		t.Cleanup(srv.Close)
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.Close() })
		armReadDeadline(conn, d)
		return conn
	}
	ping := func(conn *websocket.Conn, stop <-chan struct{}) {
		for {
			select {
			case <-stop:
				return
			case <-time.After(80 * time.Millisecond):
				conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
			}
		}
	}

	dead := dial(false)
	stop := make(chan struct{})
	go ping(dead, stop)
	start := time.Now()
	_, _, err := dead.ReadMessage()
	close(stop)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("silent peer must fail the read within the timeout, err=%v after %v", err, time.Since(start))
	}

	live := dial(true)
	stop2 := make(chan struct{})
	go ping(live, stop2)
	done := make(chan error, 1)
	go func() { _, _, err := live.ReadMessage(); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("a peer answering pings must not time out: %v", err)
	case <-time.After(4 * d):
	}
	close(stop2)
}

// ACP request ids are per-session counters: two sessions both waiting on id
// "0" must both be re-sent, and answering one must not clear the other.
func TestPendingPermissionSameIDAcrossSessions(t *testing.T) {
	dir := FixtureWorkspace(t)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)
	b.permHandler = permission.NewHandler()
	for _, sid := range []string{"sess-a", "sess-b"} {
		b.handleSessionResume(Message{
			Type: "session:resume",
			Payload: map[string]interface{}{
				"sessionId": sid, "machineId": "dev-recreate", "cliType": "replay",
				"workDir": dir, "permissionMode": "default",
			},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	frames := wsPair(t, b)
	b.permPending = map[string]Message{}
	b.permSessionMap = map[string]string{}
	for _, sid := range []string{"sess-a", "sess-b"} {
		b.permSessionMu.Lock()
		b.permSessionMap["0"] = sid
		b.permPending[pendingPermKey(sid, "0")] = permReq("0", sid)
		b.permSessionMu.Unlock()
	}

	b.resendPendingPermissions("")
	for i := 0; i < 2; i++ {
		select {
		case <-frames:
		case <-time.After(2 * time.Second):
			t.Fatal("both sessions' id-0 approvals must be re-sent")
		}
	}

	// The answer routes to the last-registered session (sess-b); sess-a's stays.
	b.handlePermissionResponse(Message{
		Type:    "permission:response",
		Payload: map[string]interface{}{"id": "0", "approved": true, "optionId": "allow_once"},
	})
	b.resendPendingPermissions("")
	expectPermFrame(t, frames, "0")
	expectNoFrame(t, frames)
}

// A cancelled turn or a stopped session no longer awaits its approvals; they
// must not come back as zombie approvals after the next reconnect.
func TestPendingPermissionClearedOnCancelAndStop(t *testing.T) {
	dir := FixtureWorkspace(t)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)
	for _, sid := range []string{"sess-c", "sess-s"} {
		b.handleSessionResume(Message{
			Type: "session:resume",
			Payload: map[string]interface{}{
				"sessionId": sid, "machineId": "dev-recreate", "cliType": "replay",
				"workDir": dir, "permissionMode": "default",
			},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	frames := wsPair(t, b)
	b.permPending = map[string]Message{
		pendingPermKey("sess-c", "1"): permReq("1", "sess-c"),
		pendingPermKey("sess-s", "2"): permReq("2", "sess-s"),
	}

	b.handleSessionCancel(Message{Payload: map[string]interface{}{"sessionId": "sess-c"}})
	b.handleSessionStop(Message{Payload: map[string]interface{}{"sessionId": "sess-s"}})
	b.resendPendingPermissions("")
	// Only the cancelled/stopped acks may arrive; no permission:request.
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case m := <-frames:
			if m.Type == "permission:request" {
				t.Fatalf("zombie approval re-sent after cancel/stop: %v", m.Payload)
			}
		case <-deadline:
			return
		}
	}
}
