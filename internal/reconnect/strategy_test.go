package reconnect

import (
	"testing"
	"time"
)

func TestStrategy_TimeBudgetNotExhausted(t *testing.T) {
	s := NewStrategy()
	// Fresh strategy should not be exhausted
	if s.HasExhaustedBudget() {
		t.Fatal("fresh strategy should not be exhausted")
	}
}

func TestStrategy_TimeBudgetExhausted(t *testing.T) {
	s := NewStrategy()
	// Manually set startTime to 11 minutes ago to simulate budget exhaustion
	s.startTime = time.Now().Add(-11 * time.Minute)

	if !s.HasExhaustedBudget() {
		t.Fatal("strategy with 11min elapsed should be exhausted (budget=10min)")
	}
}

func TestStrategy_ResetBudget(t *testing.T) {
	s := NewStrategy()
	// Exhaust budget
	s.startTime = time.Now().Add(-11 * time.Minute)
	if !s.HasExhaustedBudget() {
		t.Fatal("should be exhausted")
	}

	// Reset budget
	s.ResetBudget()
	if s.HasExhaustedBudget() {
		t.Fatal("after ResetBudget, should not be exhausted")
	}
}

func TestStrategy_ResetResetsStartTime(t *testing.T) {
	s := NewStrategy()
	s.startTime = time.Now().Add(-11 * time.Minute)

	s.Reset()
	if s.HasExhaustedBudget() {
		t.Fatal("after Reset, should not be exhausted")
	}
}

func TestStrategy_NextDelayBudgetMode(t *testing.T) {
	s := NewStrategy()

	// Should be able to get delays in budget mode
	delay := s.NextDelay()
	if delay == 0 {
		t.Fatal("expected non-zero delay within budget")
	}
	// Delay should be approximately minDelay (with jitter, it can be slightly less)
	if delay < time.Duration(float64(s.minDelay)*0.8) {
		t.Fatalf("delay %v much less than minDelay %v", delay, s.minDelay)
	}
}

func TestStrategy_NextDelayBudgetExhausted(t *testing.T) {
	s := NewStrategy()
	s.startTime = time.Now().Add(-11 * time.Minute)

	delay := s.NextDelay()
	if delay != 0 {
		t.Fatalf("expected 0 delay when budget exhausted, got %v", delay)
	}
}

func TestStrategy_HasReachedMaxBudgetMode(t *testing.T) {
	s := NewStrategy()
	if s.HasReachedMax() {
		t.Fatal("fresh strategy should not have reached max")
	}

	s.startTime = time.Now().Add(-11 * time.Minute)
	if !s.HasReachedMax() {
		t.Fatal("exhausted budget should report HasReachedMax=true")
	}
}

func TestStrategy_NonBudgetModeFallsBackToMaxAttempts(t *testing.T) {
	s := NewCustomStrategy(1*time.Second, 60*time.Second, 2.0, 0.1, 3)
	s.budgetMode = false

	// Consume all attempts
	for i := 0; i < 3; i++ {
		delay := s.NextDelay()
		if delay == 0 {
			t.Fatalf("attempt %d should have non-zero delay", i+1)
		}
	}

	// Next should return 0
	delay := s.NextDelay()
	if delay != 0 {
		t.Fatalf("expected 0 after max attempts, got %v", delay)
	}

	if !s.HasReachedMax() {
		t.Fatal("should have reached max")
	}
}

func TestStrategy_TimeBudgetReturnsDuration(t *testing.T) {
	s := NewStrategy()
	if s.TimeBudget() != 10*time.Minute {
		t.Fatalf("expected 10m, got %v", s.TimeBudget())
	}
}
