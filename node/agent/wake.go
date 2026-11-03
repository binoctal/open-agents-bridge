package agent

// Wake channel (hosted-deploy-push-wakeup): an optional WebSocket to the
// platform's node room. Whenever a deployment row becomes agent-pickable the
// API pokes the room with {"type":"work:available"}; each frame nudges an
// immediate work poll so a backoff-deep agent picks the row up in <2s instead
// of after the idle window.
//
// The poll stays authoritative in every respect: a lost, absent, or duplicate
// wake only changes WHEN the next poll happens, never whether the work
// converges (the CAS claim on the API side absorbs duplicate polls). The
// platform form has no lane credentials for this socket (its identity is the
// shared INTERNAL_SECRET, not a registry node credential) and keeps polling
// exactly as before -- user-node form only.
//
// Process contract: this goroutine must never take the daemon down. Every
// failure path is a log line plus a reconnect with exponential backoff, and
// the loop exits only when ctx is done.

import (
	"context"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Wake keepalive: the room answers a literal "ping" text frame at the edge
// (its auto-response pair), keeping the socket alive without waking the DO.
// Cloudflare drops idle WebSockets well inside ten minutes, so this cadence
// only needs to stay comfortably under that.
const (
	wakePingEvery  = 30 * time.Second
	wakeJitterFrac = 4 // jitter is up to 1/4 of the current backoff
)

// Backoff bounds as vars so tests can shrink the reconnect waits.
var (
	wakeBackoffMin = time.Second
	wakeBackoffMax = 60 * time.Second
)

// WakeConn is the read side of the wake socket -- a seam so tests can drive
// frames without a real server.
type WakeConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// wakeDial dials the wake URL; overridable in tests.
var wakeDial = func(ctx context.Context, rawURL string) (WakeConn, error) {
	d := &websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := d.DialContext(ctx, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// WakeURL derives the wake WebSocket URL from the user-node credentials:
// scheme https->wss (http->ws), path /internal/nodes/ws, credentials in the
// query (type=node lane -- the API verifies them against the registry, the
// user's machine never holds INTERNAL_SECRET). Empty string when the base
// URL does not parse: callers treat that as "no wake goroutine".
func WakeURL(apiBase, nodeID, nodeSecret string) string {
	if apiBase == "" || nodeID == "" || nodeSecret == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimRight(apiBase, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = "/internal/nodes/ws"
	q := u.Query()
	q.Set("type", "node")
	q.Set("nodeId", nodeID)
	q.Set("token", nodeSecret)
	u.RawQuery = q.Encode()
	return u.String()
}

// RunWakeLoop connects, reads frames, and forwards each as a non-blocking
// send on pollNow (buffered-1: a wake arriving while a poll is in flight
// coalesces -- an idle backoff needs one nudge, not a queue). Reconnects
// with exponential backoff plus jitter so many agents dropping off a restart
// do not stampede the upgrade endpoint. Returns only on ctx.Done; every
// network failure is logged and retried.
func RunWakeLoop(ctx context.Context, rawURL string, pollNow chan<- struct{}, logf Logf) {
	backoff := wakeBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := wakeDial(ctx, rawURL)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logf("wake channel dial failed (%v); retrying in %s", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextWakeBackoff(backoff)
			continue
		}
		logf("wake channel connected")
		backoff = wakeBackoffMin
		err = wakeReadLoop(ctx, conn, pollNow)
		_ = conn.Close()
		if ctx.Err() != nil {
			return
		}
		logf("wake channel dropped (%v); reconnecting in %s", err, backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextWakeBackoff(backoff)
	}
}

// wakeReadLoop reads until an error/close and nudges pollNow on every frame
// except the keepalive echo.
func wakeReadLoop(ctx context.Context, conn WakeConn, pollNow chan<- struct{}) error {
	pingCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Cancel watchdog: a read blocked on a silent socket only wakes on a conn
	// error, so ctx cancellation must close the socket to unblock it (and a
	// loop exit cancels pingCtx, which no-ops on an already-closed conn).
	go func() {
		<-pingCtx.Done()
		_ = conn.Close()
	}()
	// Keepalive writer: the only writer on this conn, so no write mutex.
	go func() {
		t := time.NewTicker(wakePingEvery)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
					cancel() // socket is gone; unblock the read side via ctx
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if string(data) == "pong" {
			continue // keepalive echo from the edge auto-response
		}
		// The only frame the room sends is {"type":"work:available"}; treat
		// anything else as one too -- the nudge is idempotent and cheap.
		select {
		case pollNow <- struct{}{}:
		default:
		}
	}
}

// nextWakeBackoff doubles the current backoff up to the cap and adds jitter
// (up to a quarter of it) so reconnects from many agents de-synchronize.
func nextWakeBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > wakeBackoffMax {
		d = wakeBackoffMax
	}
	return d + time.Duration(rand.Int63n(int64(d/wakeJitterFrac)+1))
}

// sleepCtx waits for d or ctx.Done; reports whether the wait elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
