package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

// reportSink captures /report posts so the streak gate is observable end to
// end: silence for transient dips, exactly one runtime_fail per sustained
// outage, with the detail the API stores as fail_log.
type reportSink struct {
	mu      sync.Mutex
	reports []map[string]any
}

func newReportAPIServer(t *testing.T) (*reportSink, *api.Client) {
	t.Helper()
	sink := &reportSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/hosting-agent/report" {
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad report body: %v", err)
		}
		sink.mu.Lock()
		sink.reports = append(sink.reports, body)
		sink.mu.Unlock()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return sink, api.New(srv.URL, "agent-test", "secret")
}

func (s *reportSink) count(event string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reports {
		if r["event"] == event {
			n++
		}
	}
	return n
}

func (s *reportSink) lastDetail(event string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.reports) - 1; i >= 0; i-- {
		if s.reports[i]["event"] == event {
			d, _ := s.reports[i]["detail"].(string)
			return d
		}
	}
	return ""
}

// S4 (fix-seed-audit-blockers): the API cannot see a crash-looped container
// on its own -- Coolify's deployment finished and the row reads ready
// forever. The probe streak gate is the whole detection story, so pin it
// end to end: 2 failures stay silent, the 3rd+ failure inside
// probeKillWindow stays silent too (8.12 cold start: the vnivni ingress
// container started listening 78s after the row flipped ready), crossing the
// window reports runtime_fail exactly once, recovery clears the streak, and
// a later outage builds a fresh one.
func TestHandleProbeStreakGate(t *testing.T) {
	sink, client := newReportAPIServer(t)
	var dialErr error
	now := time.Unix(1_700_000_000, 0)
	a := &Agent{
		Client:    client,
		probeDial: func(ctx context.Context, port int) error { return dialErr },
		now:       func() time.Time { return now },
	}
	item := api.WorkItem{DeploymentID: "d1", MissionID: "m1", Action: api.ActionProbe, AppPort: 30000}
	probe := func() {
		if err := a.handleProbe(context.Background(), a.endpoints()[0], item); err != nil {
			t.Fatalf("handleProbe: %v", err)
		}
	}

	// Two consecutive failures: silence (a container restart is noise).
	dialErr = errors.New("connection refused")
	probe()
	probe()
	if got := sink.count(api.EventRuntimeFail); got != 0 {
		t.Fatalf("2 failures must stay silent, got %d reports", got)
	}
	// Recovery clears the streak.
	dialErr = nil
	probe()
	// A fresh outage: three failures inside probeKillWindow stay silent --
	// the row went ready before the ingress container cold-started (8.12).
	dialErr = errors.New("i/o timeout")
	probe()
	now = now.Add(30 * time.Second)
	probe()
	now = now.Add(30 * time.Second)
	probe()
	if got := sink.count(api.EventRuntimeFail); got != 0 {
		t.Fatalf("3 failures within the kill window must stay silent, got %d reports", got)
	}
	// Crossing the window is believed: this 4th failure lands 105s into the
	// streak and reports exactly once.
	now = now.Add(45 * time.Second)
	probe()
	if got := sink.count(api.EventRuntimeFail); got != 1 {
		t.Fatalf("failure crossing the kill window must report once, got %d reports", got)
	}
	if d := sink.lastDetail(api.EventRuntimeFail); !strings.Contains(d, "127.0.0.1:30000") || !strings.Contains(d, "4 consecutive probes over 1m45s") {
		t.Fatalf("detail must name the port, streak and window, got %q", d)
	}
	// The streak entry is dropped on report: the next single failure must not
	// immediately re-report (a duplicate would spam an already-failed row).
	probe()
	if got := sink.count(api.EventRuntimeFail); got != 1 {
		t.Fatalf("failure right after a report must start a new streak, got %d reports", got)
	}
}

func (s *reportSink) lastArtifactsStatus(event string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.reports) - 1; i >= 0; i-- {
		if s.reports[i]["event"] == event {
			v, _ := s.reports[i]["artifactsStatus"].(string)
			return v
		}
	}
	return ""
}
