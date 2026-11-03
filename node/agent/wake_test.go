package agent

// Wake channel tests (hosted-deploy-push-wakeup tasks 2.1-2.3): URL
// derivation, frame-to-nudge forwarding with coalescing, reconnect backoff
// (fake dial seam + a real gorilla upgrade end to end), and the PollNow entry
// actually bypassing the idle-backoff gate it exists to cut.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/binoctal/open-agents-bridge/node/api"
)

// --- URL derivation ----------------------------------------------------------

func TestWakeURLDerivation(t *testing.T) {
	got := WakeURL("https://api.example.com/", "node-1", "sec-1")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Scheme != "wss" || u.Host != "api.example.com" || u.Path != "/internal/nodes/ws" {
		t.Fatalf("scheme/host/path: got %q", got)
	}
	q := u.Query()
	if q.Get("type") != "node" || q.Get("nodeId") != "node-1" || q.Get("token") != "sec-1" {
		t.Fatalf("query credentials: got %q", got)
	}

	if ws := WakeURL("http://localhost:8989", "n", "s"); !strings.HasPrefix(ws, "ws://") {
		t.Fatalf("http base must map to ws: %q", ws)
	}
	// A base with its own subpath still lands on the wake path.
	if u, err := url.Parse(WakeURL("https://api.example.com/sub", "n", "s")); err != nil || u.Path != "/internal/nodes/ws" {
		t.Fatalf("subpath base must be replaced: %v %q", err, u)
	}
	for _, tc := range []struct{ base, id, sec string }{
		{"", "n", "s"},
		{"https://api.example.com", "", "s"},
		{"https://api.example.com", "n", ""},
		{"ftp://api.example.com", "n", "s"}, // non-HTTP scheme
		{"::::", "n", "s"},                  // unparseable
	} {
		if got := WakeURL(tc.base, tc.id, tc.sec); got != "" {
			t.Fatalf("WakeURL(%q,%q,%q) must be empty, got %q", tc.base, tc.id, tc.sec, got)
		}
	}
}

// --- fake conn ----------------------------------------------------------------

// fakeWakeConn models the wake socket for read-loop tests. Frames are handed
// out one per gate release when gate is set (deterministic pacing), else as
// fast as the loop reads. With dropAfterFrames an empty queue errors out (a
// server drop right after the last delivered frame); otherwise a dry queue
// parks until Close. WriteMessage is a no-op -- the ping cadence is 30s and
// no test here waits that long.
type fakeWakeConn struct {
	mu              sync.Mutex
	frames          [][]byte
	gate            chan struct{}
	dropAfterFrames bool
	closed          chan struct{}
	once            sync.Once
}

func (c *fakeWakeConn) ReadMessage() (int, []byte, error) {
	c.mu.Lock()
	if len(c.frames) > 0 {
		f := c.frames[0]
		c.frames = c.frames[1:]
		gate := c.gate
		c.mu.Unlock()
		if gate != nil {
			<-gate
		}
		return websocket.TextMessage, f, nil
	}
	drop := c.dropAfterFrames
	c.mu.Unlock()
	if drop {
		_ = c.Close()
		return 0, nil, errors.New("dropped")
	}
	<-c.closed
	return 0, nil, errors.New("closed")
}

func (c *fakeWakeConn) WriteMessage(int, []byte) error { return nil }

func (c *fakeWakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

// recvNudge waits for one nudge with a generous bound; failure is fatal.
func recvNudge(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no nudge arrived within 2s")
	}
}

func noNudge(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected nudge")
	case <-time.After(50 * time.Millisecond):
	}
}

// One nudge per work frame, pong skipped. The gate paces each frame, and each
// recvNudge drains the previous frame's nudge before the next is released --
// ordering guaranteed by the nudges themselves.
func TestWakeReadLoopNudgesPerFrameAndSkipsPong(t *testing.T) {
	pollNow := make(chan struct{}, 1)
	gate := make(chan struct{})
	conn := &fakeWakeConn{
		closed: make(chan struct{}),
		gate:   gate,
		frames: [][]byte{
			[]byte(`{"type":"work:available"}`),
			[]byte("pong"),
			[]byte(`{"type":"work:available"}`),
		},
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- wakeReadLoop(ctx, conn, pollNow) }()

	gate <- struct{}{} // frame 1
	recvNudge(t, pollNow)
	gate <- struct{}{} // pong
	noNudge(t, pollNow)
	gate <- struct{}{} // frame 2
	recvNudge(t, pollNow)

	conn.Close()
	if err := <-done; err == nil {
		t.Fatal("read loop must exit with the close error")
	}
}

// A burst of frames with nobody draining must never wedge the read loop: the
// buffered-1 send is non-blocking (drops when full), so the loop keeps reading
// and exits cleanly on close instead of parking on a full channel.
func TestWakeReadLoopSendNeverBlocks(t *testing.T) {
	pollNow := make(chan struct{}, 1)
	conn := &fakeWakeConn{
		closed: make(chan struct{}),
		frames: [][]byte{
			[]byte(`{"type":"work:available"}`),
			[]byte(`{"type":"work:available"}`),
			[]byte(`{"type":"work:available"}`),
			[]byte(`{"type":"work:available"}`),
			[]byte(`{"type":"work:available"}`),
		},
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- wakeReadLoop(ctx, conn, pollNow) }()

	// The burst yields at least one nudge (queued or delivered).
	recvNudge(t, pollNow)

	// Closing must end the loop promptly: it is parked in ReadMessage, not in
	// a blocked channel send.
	conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop wedged -- the pollNow send must be non-blocking")
	}
}

// --- dial loop (fake seam) -----------------------------------------------------

func shrinkBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax := wakeBackoffMin, wakeBackoffMax
	wakeBackoffMin, wakeBackoffMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { wakeBackoffMin, wakeBackoffMax = oldMin, oldMax })
}

func TestRunWakeLoopDialFailuresRetryUntilCtxDone(t *testing.T) {
	shrinkBackoff(t)
	var dials atomic.Int32
	origDial := wakeDial
	wakeDial = func(ctx context.Context, rawURL string) (WakeConn, error) {
		dials.Add(1)
		return nil, errors.New("refused")
	}
	t.Cleanup(func() { wakeDial = origDial })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunWakeLoop(ctx, "ws://x/ws", make(chan struct{}, 1), func(string, ...any) {}); close(done) }()

	waitFor(t, func() bool { return dials.Load() >= 3 }, "dial was not retried")
	cancel()
	waitClosed(t, done, "wake loop did not exit on ctx cancel")
}

// A dropped connection forwards its frames first, then reconnects: the loop
// must dial again. Cancellation while parked on the replacement conn's reads
// must also end the loop (the watchdog closes the socket).
func TestRunWakeLoopReconnectsAfterDrop(t *testing.T) {
	shrinkBackoff(t)
	var dials atomic.Int32
	origDial := wakeDial
	wakeDial = func(ctx context.Context, rawURL string) (WakeConn, error) {
		n := dials.Add(1)
		c := &fakeWakeConn{closed: make(chan struct{})}
		if n == 1 {
			c.mu.Lock()
			c.frames = [][]byte{[]byte(`{"type":"work:available"}`)}
			c.dropAfterFrames = true
			c.mu.Unlock()
		}
		return c, nil
	}
	t.Cleanup(func() { wakeDial = origDial })

	pollNow := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { RunWakeLoop(ctx, "ws://x/ws", pollNow, func(string, ...any) {}); close(done) }()

	recvNudge(t, pollNow) // frame from conn 1, before its drop
	waitFor(t, func() bool { return dials.Load() >= 2 }, "no reconnect after the drop")
	cancel()
	waitClosed(t, done, "wake loop did not exit on ctx cancel (read parked, watchdog must close)")
}

func TestNextWakeBackoffDoublesCapsAndJitters(t *testing.T) {
	oldMax := wakeBackoffMax
	wakeBackoffMax = 10 * time.Millisecond
	t.Cleanup(func() { wakeBackoffMax = oldMax })

	for i := 0; i < 20; i++ {
		got := nextWakeBackoff(3 * time.Millisecond)
		// 3ms -> 6ms, jitter up to a quarter of it.
		if got < 6*time.Millisecond || got > 6*time.Millisecond+1500*time.Microsecond {
			t.Fatalf("backoff(3ms) out of [6ms, 7.5ms]: %v", got)
		}
		// 100ms doubles past the cap -> capped at 10ms.
		got = nextWakeBackoff(100 * time.Millisecond)
		if got < 10*time.Millisecond || got > 10*time.Millisecond+2500*time.Microsecond {
			t.Fatalf("backoff(100ms) out of [10ms, 12.5ms]: %v", got)
		}
	}
}

// --- real WebSocket end to end ---------------------------------------------------

// TestWakeLoopRealServer drives the real gorilla dialer against an httptest
// WS server: a work frame arrives as a nudge, a server-side drop triggers a
// reconnect, and the second connection's frame nudges as well.
func TestWakeLoopRealServer(t *testing.T) {
	shrinkBackoff(t)

	var upgrades atomic.Int32
	conns := make(chan *websocket.Conn, 4)
	up := websocket.Upgrader{}
	wsURL := newWSServer(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upgrades.Add(1)
		conns <- c
		// Park on reads so the connection stays server-side alive until the
		// test drops it; read errors just end this handler.
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})

	pollNow := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { RunWakeLoop(ctx, wsURL, pollNow, func(string, ...any) {}); close(done) }()

	// First connection: server sends one work frame, client nudges.
	first := waitConn(t, conns)
	if err := first.WriteMessage(websocket.TextMessage, []byte(`{"type":"work:available"}`)); err != nil {
		t.Fatalf("server write: %v", err)
	}
	recvNudge(t, pollNow)

	// Server drops the socket; the client must reconnect and the second
	// connection's frame must nudge as well.
	_ = first.Close()
	second := waitConn(t, conns)
	if err := second.WriteMessage(websocket.TextMessage, []byte(`{"type":"work:available"}`)); err != nil {
		t.Fatalf("server write 2: %v", err)
	}
	recvNudge(t, pollNow)

	cancel()
	waitClosed(t, done, "wake loop did not exit on ctx cancel")
	if got := upgrades.Load(); got != 2 {
		t.Fatalf("want exactly 2 upgrades, got %d", got)
	}
}

func newWSServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	_, srv := newEnvServer(t, h)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/internal/nodes/ws"
}

func waitConn(t *testing.T, conns chan *websocket.Conn) *websocket.Conn {
	t.Helper()
	select {
	case c := <-conns:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no websocket connection arrived")
		return nil
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func waitClosed(t *testing.T, done chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(msg)
	}
}

// --- PollNow bypasses the idle backoff ---------------------------------------------

// TestPollNowBypassesIdleBackoff locks the 2.2 wiring at the state-machine
// level: an endpoint deep in idle backoff is skipped by a plain pollWork, but
// the forced (wake-nudge) entry still polls it.
func TestPollNowBypassesIdleBackoff(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	clk := time.Unix(1_700_000_000, 0)
	a := tickAgent(t, s, srv.URL, 30*time.Second, &clk)

	// Drive deep into backoff (same shape as TestIdleBackoffGrowsThenCaps).
	for i := 0; i < 60; i++ {
		if err := a.pollWork(context.Background()); err != nil {
			t.Fatalf("pollWork: %v", err)
		}
		clk = clk.Add(10 * time.Second)
	}
	gated := s.count("/internal/hosting-agent/work")
	if gated == 0 {
		t.Fatal("expected some polls before backoff kicked in")
	}

	// The gate is closed: a plain poll must not reach the endpoint...
	if err := a.pollWork(context.Background()); err != nil {
		t.Fatalf("pollWork: %v", err)
	}
	if got := s.count("/internal/hosting-agent/work"); got != gated {
		t.Fatalf("plain poll must be gated mid-backoff: %d -> %d", gated, got)
	}

	// ...but the wake-nudge entry must poll immediately.
	if err := a.pollWorkForce(context.Background()); err != nil {
		t.Fatalf("pollWorkForce: %v", err)
	}
	if got := s.count("/internal/hosting-agent/work"); got != gated+1 {
		t.Fatalf("forced poll must hit the endpoint exactly once: %d -> %d", gated, got)
	}

	// The forced empty answer keeps the backoff state honest: still gated.
	if a.workDue(a.endpoints()[0].Client) {
		t.Fatal("forced empty poll must not reset the backoff")
	}
	if err := a.pollWork(context.Background()); err != nil {
		t.Fatalf("pollWork: %v", err)
	}
	if got := s.count("/internal/hosting-agent/work"); got != gated+1 {
		t.Fatalf("plain poll must still be gated after a forced empty poll: %d", got)
	}
}

// TestRunWakesOnNudge is the Run-level wiring: with the ticker polling gated
// by deep backoff, a PollNow send must produce a fresh /work request well
// before the backoff window would. Deterministic by alignment: right after a
// gated ticker poll fires, the next one is a full gap away, so any poll within
// 200ms of the nudge can only be the nudge's.
func TestRunWakesOnNudge(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	const workPath = "/internal/hosting-agent/work"
	pollNow := make(chan struct{}, 1)
	a := &Agent{
		Endpoints:        []APIEndpoint{{Client: api.New(srv.URL, "t", "sec"), BaseURL: srv.URL}},
		PollNow:          pollNow,
		WorkPollInterval: 2 * time.Millisecond,
		WorkIdleMax:      time.Hour, // backoff on, cap far beyond the test window
		Logf:             func(string, ...any) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// Warm up until the endpoint is idle-gated: gaps double off a 2ms base
	// and cap at base<<8 = 512ms, so within ~1s no poll happens for 300ms.
	waitFor(t, func() bool {
		c := s.count(workPath)
		time.Sleep(300 * time.Millisecond)
		return s.count(workPath) == c
	}, "agent never went idle-deep")

	// Catch one gated ticker poll and nudge immediately after it: the next
	// ticker poll is a full gap (~510ms) away.
	base := s.count(workPath)
	waitFor(t, func() bool { return s.count(workPath) > base }, "no ticker poll while waiting")
	pollNow <- struct{}{}

	start := time.Now()
	waitFor(t, func() bool { return s.count(workPath) > base+1 }, "nudge did not produce a poll")
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("nudge poll took %v -- too slow to be the forced poll (next ticker ~510ms out)", el)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}
