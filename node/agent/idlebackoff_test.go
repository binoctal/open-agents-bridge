package agent

// Idle backoff of the work poll: every poll is one billable Worker request on
// an account-wide free quota, so an endpoint that keeps answering "no items"
// must be polled less often, and must snap back the moment work (or an error)
// shows up.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/node/api"
)

// tickAgent builds an Agent over one fake endpoint with a controllable clock.
func tickAgent(t *testing.T, srv *epServer, url string, idleMax time.Duration, clk *time.Time) *Agent {
	t.Helper()
	_ = srv
	return &Agent{
		Endpoints:        []APIEndpoint{{Client: api.New(url, "t", "sec"), BaseURL: url}},
		WorkPollInterval: 10 * time.Second,
		WorkIdleMax:      idleMax,
		now:              func() time.Time { return *clk },
	}
}

func TestIdleBackoffGrowsThenCaps(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	clk := time.Unix(1_700_000_000, 0)
	a := tickAgent(t, s, srv.URL, 30*time.Second, &clk)

	// 10 minutes of 10s ticks over an always-empty endpoint.
	for i := 0; i < 60; i++ {
		if err := a.pollWork(context.Background()); err != nil {
			t.Fatalf("pollWork: %v", err)
		}
		clk = clk.Add(10 * time.Second)
	}
	got := s.count("/internal/hosting-agent/work")
	// Without backoff: 60. With 10->20->30(cap): first polls at t=0,20,50,
	// then every 30s => ~2 + 600/30 = about 22; assert a decisive cut, and
	// that it never starves (the cap must still bound the gap).
	if got > 24 || got < 15 {
		t.Fatalf("expected ~20 polls in 10 idle minutes with a 30s cap, got %d", got)
	}
}

func TestIdleBackoffResetsOnItems(t *testing.T) {
	var offer atomic.Bool
	s, srv := newEnvServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/hosting-agent/work" {
			http.NotFound(w, r)
			return
		}
		items := []api.WorkItem{}
		if offer.Load() {
			// Unknown action: handle() logs and ignores it, which is all
			// this test needs -- a non-empty answer.
			items = append(items, api.WorkItem{DeploymentID: "d", Action: "noop"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	clk := time.Unix(1_700_000_000, 0)
	a := tickAgent(t, s, srv.URL, 60*time.Second, &clk)

	// Drive the endpoint deep into backoff.
	for i := 0; i < 30; i++ {
		_ = a.pollWork(context.Background())
		clk = clk.Add(10 * time.Second)
	}
	before := s.count("/internal/hosting-agent/work")

	// Work appears. The poll that discovers it happens at the next due time
	// (<= 60s away), after which the cadence must be the base 10s again.
	offer.Store(true)
	for i := 0; i < 6; i++ {
		_ = a.pollWork(context.Background())
		clk = clk.Add(10 * time.Second)
	}
	mid := s.count("/internal/hosting-agent/work")
	for i := 0; i < 6; i++ {
		_ = a.pollWork(context.Background())
		clk = clk.Add(10 * time.Second)
	}
	after := s.count("/internal/hosting-agent/work")
	if after-mid != 6 {
		t.Fatalf("with work pending every 10s tick must poll: want 6, got %d (before=%d mid=%d)", after-mid, before, mid)
	}
}

func TestIdleBackoffDisabledByDefault(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	clk := time.Unix(1_700_000_000, 0)
	a := tickAgent(t, s, srv.URL, 0, &clk)
	for i := 0; i < 12; i++ {
		_ = a.pollWork(context.Background())
		clk = clk.Add(10 * time.Second)
	}
	if got := s.count("/internal/hosting-agent/work"); got != 12 {
		t.Fatalf("zero WorkIdleMax must poll every tick, got %d/12", got)
	}
}
