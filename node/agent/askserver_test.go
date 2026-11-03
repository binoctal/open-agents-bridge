package agent

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startAsk runs one forwarder on an ephemeral loopback port and returns its
// base URL. Addr is rewritten to the bound address by Start.
func startAsk(t *testing.T, ask func(ctx context.Context, domain string) bool) string {
	t.Helper()
	s := &AskServer{Ask: ask, Addr: "127.0.0.1:0", Logf: func(string, ...any) {}}
	stop, err := s.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(stop)
	return "http://" + s.Addr
}

func askStatus(t *testing.T, base, domain string) int {
	t.Helper()
	resp, err := http.Get(base + "/ask?domain=" + domain)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// An authorized label answers 200 and is cached: Caddy asks once per cert
// issuance, but a browser storm can burst handshakes and the platform's ask
// endpoint is rate-limited (200/min strict) -- the burst must not trip it.
func TestAskServerAuthorizedCached(t *testing.T) {
	var calls atomic.Int64
	base := startAsk(t, func(ctx context.Context, d string) bool {
		calls.Add(1)
		return true
	})
	for i := 0; i < 5; i++ {
		if got := askStatus(t, base, "abc123.deploy.openagents.top"); got != http.StatusOK {
			t.Fatalf("authorized label = %d, want 200", got)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("burst of 5 handshakes must consult the platform once, got %d", calls.Load())
	}
}

// Fail closed: a refused (or unreachable) platform answer is 403, and a
// NEGATIVE answer is never cached -- a deployment flipping ready must issue
// on the very next handshake, not after a TTL.
func TestAskServerFailClosedAndNegativeNotCached(t *testing.T) {
	var allow atomic.Bool
	base := startAsk(t, func(ctx context.Context, d string) bool { return allow.Load() })

	if got := askStatus(t, base, "abc123.deploy.openagents.top"); got != http.StatusForbidden {
		t.Fatalf("refused label must fail closed with 403, got %d", got)
	}
	// Flip to ready: the very next handshake must pass (no negative cache).
	allow.Store(true)
	if got := askStatus(t, base, "abc123.deploy.openagents.top"); got != http.StatusOK {
		t.Fatal("label flipped ready must authorize on the next handshake (negatives not cached)")
	}
	// Empty domain is a malformed ask, not a 500.
	if got := askStatus(t, base, ""); got != http.StatusForbidden {
		t.Errorf("empty domain must 403, got %d", got)
	}
}

// A slow platform answer decides within 5s: the handshake is synchronous on
// Caddy's side; a hung platform must not stall every new TLS connection.
func TestAskServerTimesOutSlowPlatform(t *testing.T) {
	base := startAsk(t, func(ctx context.Context, d string) bool {
		<-ctx.Done() // never answers in time
		return true
	})
	start := time.Now()
	if got := askStatus(t, base, "slow.deploy.openagents.top"); got != http.StatusForbidden {
		t.Errorf("timed-out ask = %d, want 403 (fail closed)", got)
	}
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Errorf("ask took %v; the 5s budget must bound it", elapsed)
	}
}

// The forwarder is loopback-only by construction: an off-host bind address
// is a configuration error and must fail loudly at Start, not silently
// expose the authorization forwarder to the network.
func TestAskServerRefusesNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:20190", "192.0.2.10:20190", "not-an-addr"} {
		s := &AskServer{Addr: addr, Ask: func(context.Context, string) bool { return true }}
		if _, err := s.Start(); err == nil {
			t.Errorf("addr %q must be refused", addr)
		} else if addr == "not-an-addr" && !strings.Contains(err.Error(), "ask addr") {
			t.Errorf("malformed addr error should name the ask addr, got %q", err)
		}
	}
}
