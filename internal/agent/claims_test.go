package agent

// In-flight claim lifecycle test (image-tier async materialize follow-up):
// synchronous handlers must release their claim on return, and the async
// materialize branch must keep the claim while its goroutine streams. The
// 10-31 leak: register_route finished without releasing, so the next
// materialize offer skipped forever as "already in flight" while no
// goroutine was running.

import (
	"context"
	"testing"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

// TestSyncActionsReleaseTheirClaim dispatches one register_route item and
// asserts the claim is free once handle returns: a synchronous handler that
// leaks its claim wedges every later action on the same deployment.
func TestSyncActionsReleaseTheirClaim(t *testing.T) {
	sa, srvA := newEnvServer(t, nil)
	a := &Agent{
		Endpoints: []APIEndpoint{{Client: api.New(srvA.URL, "t", "sec-a"), BaseURL: srvA.URL}},
		Ingress:   &fakeIngress{},
	}
	item := api.WorkItem{
		DeploymentID: "dep-sync",
		Action:       api.ActionRegisterRoute,
		Host:         "dep-sync.test",
		AppPort:      3999,
	}
	if err := a.handle(context.Background(), a.Endpoints[0], item); err != nil {
		t.Fatalf("handle register_route: %v", err)
	}
	if !a.claim("dep-sync") {
		t.Fatal("a synchronous action must release its claim on return; the leaked claim stuck later offers as in-flight forever")
	}
	a.release("dep-sync")
	if got := sa.count("/internal/hosting-agent/report"); got == 0 {
		t.Fatal("the route-registered report must land on the endpoint")
	}
}
