package session

import (
	"errors"
	"sync"
	"testing"
)

func putTaskSession(m *Manager, id string, connecting bool) *Session {
	s := putSession(m, id, "active")
	s.SetMultiAgentMetadata("job-"+id, id)
	m.mu.Lock()
	s.connecting = connecting
	m.mu.Unlock()
	return s
}

func TestActiveTaskCount_IgnoresInteractiveSessions(t *testing.T) {
	m := NewManager()
	for _, id := range []string{"chat-1", "chat-2", "chat-3"} {
		putSession(m, id, "active")
	}
	if got := m.ActiveTaskCount(); got != 0 {
		t.Fatalf("ActiveTaskCount with 3 interactive = %d, want 0", got)
	}
	if got := m.ActiveCount(); got != 3 {
		t.Fatalf("ActiveCount must keep counting all sessions, got %d", got)
	}
}

func TestStartTaskSession_InteractiveDoNotBlock(t *testing.T) {
	m := NewManager()
	for _, id := range []string{"chat-1", "chat-2", "chat-3"} {
		putSession(m, id, "active")
	}
	// Admission passes (the failure below is the unknown CLI, not a full pool).
	_, err := m.StartTaskSession("no-such-cli", ".", "t1", "j1", 80, 24, "default")
	if errors.Is(err, ErrTaskPoolFull) {
		t.Fatal("3 interactive sessions must not fill the task pool")
	}
}

func TestStartTaskSession_FullPoolRejectsAndFreesOnEnd(t *testing.T) {
	m := NewManager()
	for _, id := range []string{"t1", "t2", "t3"} {
		putTaskSession(m, id, false)
	}
	if _, err := m.StartTaskSession("no-such-cli", ".", "t4", "j4", 80, 24, "default"); !errors.Is(err, ErrTaskPoolFull) {
		t.Fatalf("4th task: err = %v, want ErrTaskPoolFull", err)
	}
	m.mu.Lock()
	m.sessions["t1"].Status = "completed"
	m.mu.Unlock()
	if _, err := m.StartTaskSession("no-such-cli", ".", "t4", "j4", 80, 24, "default"); errors.Is(err, ErrTaskPoolFull) {
		t.Fatal("slot freed by a finished task must admit the next one")
	}
}

func TestStartTaskSession_ConnectingTaskOccupiesSlot(t *testing.T) {
	m := NewManager()
	putTaskSession(m, "t1", true)
	putTaskSession(m, "t2", true)
	putTaskSession(m, "t3", true)
	if got := m.ActiveTaskCount(); got != 3 {
		t.Fatalf("sessions still in Connect must count, got %d", got)
	}
}

func TestStartTaskSession_RegistersTaskIdentityAtCreation(t *testing.T) {
	m := NewManager()
	seen := make(chan *Session, 1)
	m.testHookTaskRegistered = func() {
		m.mu.RLock()
		seen <- m.sessions["t1"]
		m.mu.RUnlock()
	}
	go func() { _, _ = m.StartTaskSession("no-such-cli", ".", "t1", "j1", 80, 24, "default") }()
	s := <-seen
	if s == nil {
		t.Fatal("session not registered when hook ran")
	}
	job, task, started := s.GetMultiAgentMetadata()
	if job != "j1" || task != "t1" || started.IsZero() || !s.IsTaskSession() {
		t.Fatalf("identity not set at registration: job=%q task=%q started=%v", job, task, started)
	}
}

func TestStartTaskSession_ConcurrentAdmissionNeverExceedsPool(t *testing.T) {
	m := NewManager()
	release := make(chan struct{})
	m.testHookTaskRegistered = func() { <-release }

	const n = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, full := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
			_, err := m.StartTaskSession("no-such-cli", ".", id, "j", 80, 24, "default")
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrTaskPoolFull) {
				full++
			} else {
				admitted++
			}
		}(i)
	}
	// Wait until every goroutine is either parked in the hook or rejected.
	for {
		mu.Lock()
		f := full
		mu.Unlock()
		if f == n-m.MaxConcurrent() {
			break
		}
	}
	if got := m.ActiveTaskCount(); got != m.MaxConcurrent() {
		t.Fatalf("registered tasks = %d, want exactly %d", got, m.MaxConcurrent())
	}
	close(release)
	wg.Wait()
	if admitted != m.MaxConcurrent() {
		t.Fatalf("admitted = %d, want %d", admitted, m.MaxConcurrent())
	}
}
