package bridge

import (
	"encoding/json"
	"os"
	"testing"
)

// G23 freshness tripwire: the dispatch vocabulary pinned in the main repo's
// contracts package (packages/contracts/bridge-message-types.json) must equal
// what the LIVE switches in this package actually handle. A case added or
// removed without `make export-contracts` (and the main-repo constant update)
// fails here AND in the web contracts test — three-end vocabulary sync stops
// being human discipline (parity rule 3) and becomes a CI failure.
//
// The JSON lives in the parent repository, reachable only when the submodule
// is checked out in its usual place (true in the main repo's CI, false for a
// standalone bridge checkout) — skip silently when absent.
func TestDispatchContractArtifactInSync(t *testing.T) {
	const artifact = "../../../packages/contracts/bridge-message-types.json"
	raw, err := os.ReadFile(artifact)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("contracts artifact not present (standalone bridge checkout)")
		}
		t.Fatalf("read %s: %v", artifact, err)
	}
	var pinned struct {
		Handled       []string `json:"handled"`
		AgentStatuses []string `json:"agentStatuses"`
	}
	if err := json.Unmarshal(raw, &pinned); err != nil {
		t.Fatalf("parse %s: %v", artifact, err)
	}

	live, err := ExtractDispatchTypes(".")
	if err != nil {
		t.Fatalf("extract live dispatch types: %v", err)
	}

	if len(live) != len(pinned.Handled) {
		t.Fatalf("dispatch vocabulary drifted: live has %d types, pinned artifact has %d — run `make export-contracts` and update BRIDGE_HANDLED_TYPES in packages/contracts/src/index.ts", len(live), len(pinned.Handled))
	}
	pinnedSet := make(map[string]bool, len(pinned.Handled))
	for _, h := range pinned.Handled {
		pinnedSet[h] = true
	}
	for _, l := range live {
		if !pinnedSet[l] {
			t.Fatalf("dispatch type %q is handled live but missing from %s — run `make export-contracts` and update BRIDGE_HANDLED_TYPES", l, artifact)
		}
	}

	// Same reconciliation for the six-state AgentStatus vocabulary: pinned in
	// the artifact, mirrored as AGENT_STATUSES in the contracts package.
	liveStatuses, err := ExtractAgentStatuses("../protocol")
	if err != nil {
		t.Fatalf("extract live agent statuses: %v", err)
	}
	if len(liveStatuses) != len(pinned.AgentStatuses) {
		t.Fatalf("agent-status vocabulary drifted: live has %d statuses, pinned artifact has %d — run `make export-contracts` and update AGENT_STATUSES in packages/contracts/src/index.ts", len(liveStatuses), len(pinned.AgentStatuses))
	}
	pinnedStatuses := make(map[string]bool, len(pinned.AgentStatuses))
	for _, s := range pinned.AgentStatuses {
		pinnedStatuses[s] = true
	}
	for _, s := range liveStatuses {
		if !pinnedStatuses[s] {
			t.Fatalf("agent status %q exists live but missing from %s — run `make export-contracts` and update AGENT_STATUSES", s, artifact)
		}
	}
}
