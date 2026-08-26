package bridge

import (
	"testing"
	"time"
)

// Known-issue #22: when the reconnect time budget was exhausted the readLoop
// returned, but Start() blocks on <-b.done — the process stayed alive with no
// WS, no heartbeats, and no exit, holding task sessions, worktrees, and the
// device slot as a half-live zombie. The fix: never give up; fall back to a
// slow keep-alive cadence (one attempt every slowRetryInterval) so the bridge
// self-recovers when the server returns.
func TestSlowRetryMode(t *testing.T) {
	if slowRetryInterval != 5*time.Minute {
		t.Errorf("slowRetryInterval = %v, want 5m (same ceiling philosophy as the open-agents #21 alarm backoff)", slowRetryInterval)
	}

	b := &Bridge{}
	if b.slowRetry.Load() {
		t.Fatal("slow retry must start cleared")
	}

	// The exhausted-budget announcement (error log, StateFailed transition,
	// EventMaxRetry notification) must fire exactly once, not on every
	// 5-minute keep-alive attempt.
	if !b.enterSlowRetry() {
		t.Fatal("first enterSlowRetry must report the transition")
	}
	if b.enterSlowRetry() {
		t.Fatal("subsequent enterSlowRetry calls must not re-announce")
	}

	// A successful reconnect clears the mode so a LATER exhaustion (fresh
	// budget, another long flap) announces again.
	b.exitSlowRetry()
	if b.slowRetry.Load() {
		t.Fatal("exitSlowRetry must clear the mode")
	}
	if !b.enterSlowRetry() {
		t.Fatal("enterSlowRetry after exitSlowRetry must report a fresh transition")
	}
}
