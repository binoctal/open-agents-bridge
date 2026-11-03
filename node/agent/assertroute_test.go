package agent

// assert_route tests (fix-caddy-route-reassert 1.1-1.3): the periodic
// re-assert must be silent on every path -- success, failure, and missing
// route wiring alike -- because the row it maintains is already healthy; a
// MaterializeFailed report would flip a ready row to failed, and a success
// report would UPDATE the row through the event consumer (orchestrator's
// route_registered handler), breaking the steady-state zero-write contract.
// Unknown actions must stay safe no-ops so an API deployed ahead of an old
// agent degrades without side effects (the deployment-order freedom of D2).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/binoctal/open-agents-bridge/node/api"
)

// fakeRouteIngress is the Ingress seam for assert tests: it counts upserts
// and can be told to fail them, or to report the edge proxy as down.
type fakeRouteIngress struct {
	mu      sync.Mutex
	upserts int
	upsertE error
	pingE   error
}

func (f *fakeRouteIngress) Upsert(ctx context.Context, deploymentID, containerAddr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	return f.upsertE
}

func (f *fakeRouteIngress) Remove(ctx context.Context, deploymentID string) error { return nil }

func (f *fakeRouteIngress) Ping(ctx context.Context) error { return f.pingE }

func (f *fakeRouteIngress) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upserts
}

func assertItem() api.WorkItem {
	return api.WorkItem{
		Action:       api.ActionAssertRoute,
		DeploymentID: "dep1",
		AppPort:      30001,
		Host:         "dep1.deploy.example",
	}
}

// silence asserts the agent made no report of any kind for this item.
func silence(t *testing.T, s *epServer) {
	t.Helper()
	if n := s.requests(); n != 0 {
		t.Fatalf("expected zero reports, got %d requests", n)
	}
}

func TestAssertRouteSilentOnSuccess(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	defer srv.Close()
	fi := &fakeRouteIngress{}
	a := &Agent{Ingress: fi}
	if err := a.handle(context.Background(), APIEndpoint{Client: api.New(srv.URL, "t", "sec"), BaseURL: srv.URL}, assertItem()); err != nil {
		t.Fatalf("assert: %v", err)
	}
	if fi.count() != 1 {
		t.Fatalf("expected 1 upsert, got %d", fi.count())
	}
	silence(t, s)
}

func TestAssertRouteSilentOnFailure(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	defer srv.Close()
	fi := &fakeRouteIngress{upsertE: errors.New("caddy restarting")}
	a := &Agent{Ingress: fi}
	// A Caddy mid-restart must not kill the deployment: no error return and,
	// critically, no MaterializeFailed report.
	if err := a.handle(context.Background(), APIEndpoint{Client: api.New(srv.URL, "t", "sec"), BaseURL: srv.URL}, assertItem()); err != nil {
		t.Fatalf("assert must not fail the item: %v", err)
	}
	silence(t, s)
}

func TestAssertRouteWithoutIngress(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	defer srv.Close()
	a := &Agent{}
	if err := a.handle(context.Background(), APIEndpoint{Client: api.New(srv.URL, "t", "sec"), BaseURL: srv.URL}, assertItem()); err != nil {
		t.Fatalf("assert: %v", err)
	}
	silence(t, s)
}

func TestUnknownActionSafeNoop(t *testing.T) {
	s, srv := newEnvServer(t, nil)
	defer srv.Close()
	fi := &fakeRouteIngress{}
	a := &Agent{Ingress: fi}
	item := assertItem()
	item.Action = "something_new"
	if err := a.handle(context.Background(), APIEndpoint{Client: api.New(srv.URL, "t", "sec"), BaseURL: srv.URL}, item); err != nil {
		t.Fatalf("unknown action must be a no-op, got %v", err)
	}
	if fi.count() != 0 {
		t.Fatalf("unknown action must not touch ingress")
	}
	silence(t, s)
}

func TestCaddyVerdictDebounce(t *testing.T) {
	ctx := context.Background()

	// No route wiring -> no verdict, forever (old posture, no alarms).
	a := &Agent{}
	if v := a.caddyVerdict(ctx); v != nil {
		t.Fatalf("nil ingress must yield nil verdict, got %v", *v)
	}

	// Two failures are not yet a verdict; the third confirms death.
	fi := &fakeRouteIngress{pingE: errors.New("refused")}
	a = &Agent{Ingress: fi}
	for i := 0; i < 2; i++ {
		if v := a.caddyVerdict(ctx); v != nil {
			t.Fatalf("probe %d: expected unconfirmed nil, got %v", i+1, *v)
		}
	}
	v := a.caddyVerdict(ctx)
	if v == nil || *v {
		t.Fatalf("third consecutive failure must confirm dead, got %v", v)
	}

	// Recovery is immediate...
	fi.pingE = nil
	v = a.caddyVerdict(ctx)
	if v == nil || !*v {
		t.Fatalf("recovery must report alive immediately, got %v", v)
	}
	// ...and the counter reset: a fresh outage needs three probes again.
	fi.pingE = errors.New("refused")
	for i := 0; i < 2; i++ {
		if v := a.caddyVerdict(ctx); v != nil {
			t.Fatalf("post-recovery probe %d: expected nil, got %v", i+1, *v)
		}
	}
	if v := a.caddyVerdict(ctx); v == nil || *v {
		t.Fatalf("re-decay must need three fresh failures, got %v", v)
	}
}
