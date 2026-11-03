// AskServer is the user node's local on-demand-TLS ask endpoint
// (byo-hosting-node D2, task 2.4). Caddy's global on_demand_tls ask setting
// points here; on every unknown TLS handshake Caddy asks "may I issue a cert
// for {domain}?" and this server forwards the question to the platform with
// the node credential. 200 = the platform confirms a READY deployment with
// that label sits on THIS node; anything else = Caddy must refuse the
// handshake (fail closed -- an unreachable platform means no new certs, never
// an open one).
package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// askCacheTTL bounds how long a positive authorization is remembered. Caddy
// asks once per cert issuance (not per request), but a browser storm against
// a cold label can produce a burst of handshakes before the first cert lands;
// the cache keeps that burst from tipping the platform's ask rate limit.
// Negative answers are NOT cached: a deployment flipping to ready must issue
// on the very next handshake, not after a TTL.
const askCacheTTL = 30 * time.Second

// AskServer forwards Caddy's ask callbacks to the platform.
type AskServer struct {
	// Ask is the platform query seam (production: api.Client.Ask). Return
	// true only for an explicit platform authorization.
	Ask func(ctx context.Context, domain string) bool

	// Addr is the localhost listener (default 127.0.0.1:20190). Caddy's ask
	// URL and this address MUST agree -- a mismatch fails every handshake.
	Addr string
	Logf Logf

	mu      sync.Mutex
	allowed map[string]time.Time
	srv     *http.Server
}

// Start binds the listener and serves in the background; the returned
// function stops it. Binding failure is returned, not retried: a user node
// whose ask endpoint is down must be loud at boot, not silently refusing
// every TLS handshake.
func (s *AskServer) Start() (stop func(), err error) {
	if s.Addr == "" {
		s.Addr = "127.0.0.1:20190"
	}
	// Localhost only, by construction of the config; assert it anyway so a
	// misconfiguration cannot expose the forwarder off-host.
	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		return nil, fmt.Errorf("ask addr %q: %w", s.Addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("ask addr %q must be a loopback address (Caddy reaches it locally; it must never be exposed)", s.Addr)
	}
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return nil, fmt.Errorf("bind ask endpoint %s: %w", s.Addr, err)
	}
	// Record the BOUND address: an ":0" (test) bind resolves here, and the
	// daemon log can echo exactly what Caddy's ask URL must point at.
	s.Addr = ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/ask", s.handleAsk)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return func() { _ = s.srv.Close() }, nil
}

func (s *AskServer) handleAsk(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	if domain == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if s.cachedAllowed(domain) {
		w.WriteHeader(http.StatusOK)
		return
	}
	// The handshake is synchronous on Caddy's side; a slow platform answer
	// would stall every new connection. 5s decides, then fail closed. The
	// post-call ctx.Err() re-check matters: a seam that ignores its context
	// and "answers" after the deadline must not authorize a cert.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if s.Ask == nil || !s.Ask(ctx, domain) || ctx.Err() != nil {
		if s.Logf != nil {
			s.Logf("ask: label %s refused (no ready deployment on this node, or platform unreachable)", domain)
		}
		w.WriteHeader(http.StatusForbidden)
		return
	}
	s.remember(domain)
	w.WriteHeader(http.StatusOK)
}

func (s *AskServer) cachedAllowed(domain string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.allowed[domain]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.allowed, domain)
		return false
	}
	return true
}

func (s *AskServer) remember(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.allowed == nil {
		s.allowed = make(map[string]time.Time)
	}
	s.allowed[domain] = time.Now().Add(askCacheTTL)
}
