package protocol

import (
	"testing"
	"time"
)

func TestTurnIdleLimit(t *testing.T) {
	t.Setenv(turnIdleTimeoutEnv, "")
	if got := turnIdleLimit(false); got != turnIdleTimeout {
		t.Fatalf("default = %v", got)
	}
	if got := turnIdleLimit(true); got != turnIdleTimeoutWithTools {
		t.Fatalf("with tools = %v", got)
	}
	t.Setenv(turnIdleTimeoutEnv, "10m")
	if got := turnIdleLimit(false); got != 10*time.Minute {
		t.Fatalf("override = %v", got)
	}
	t.Setenv(turnIdleTimeoutEnv, "2h")
	if got := turnIdleLimit(true); got != 2*time.Hour {
		t.Fatalf("override above tool budget = %v", got)
	}
	t.Setenv(turnIdleTimeoutEnv, "garbage")
	if got := turnIdleLimit(false); got != turnIdleTimeout {
		t.Fatalf("invalid override = %v", got)
	}
}

func TestTrackToolLifecycle(t *testing.T) {
	a := &ACPAdapter{}
	a.trackTool("t1", "pending")
	a.trackTool("t1", "in_progress")
	if !a.hasActiveTools() {
		t.Fatal("expected in-flight tool")
	}
	a.trackTool("t1", "completed")
	if a.hasActiveTools() {
		t.Fatal("completed tool must clear")
	}
	a.trackTool("t2", "pending")
	a.resetActiveTools()
	if a.hasActiveTools() {
		t.Fatal("reset must clear")
	}
}
