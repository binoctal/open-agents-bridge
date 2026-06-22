package permission

import (
	"testing"
	"time"
)

// A fresh handler has no pending requests.
func TestNewHandler_NoPending(t *testing.T) {
	h := NewHandler()
	if got := len(h.GetPending()); got != 0 {
		t.Errorf("new handler should have no pending requests, got %d", got)
	}
}

// Submit returns the approval state delivered by Resolve.
func TestHandler_Submit_Approved(t *testing.T) {
	h := NewHandler()
	// OnRequest fires after the request is registered in the pending map, so it
	// is a safe point to drive the resolution from a separate goroutine.
	h.OnRequest(func(r Request) {
		go h.Resolve(Response{ID: r.ID, Approved: true})
	})

	done := make(chan bool, 1)
	go func() {
		approved, err := h.Submit(Request{ID: "r1", PermissionType: "file:read", Timeout: 5})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		done <- approved
	}()

	select {
	case approved := <-done:
		if !approved {
			t.Error("expected approved=true after Resolve(approved=true)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not return within timeout")
	}
}

func TestHandler_Submit_Rejected(t *testing.T) {
	h := NewHandler()
	h.OnRequest(func(r Request) {
		go h.Resolve(Response{ID: r.ID, Approved: false})
	})

	done := make(chan bool, 1)
	go func() {
		approved, _ := h.Submit(Request{ID: "r2", Timeout: 5})
		done <- approved
	}()

	select {
	case approved := <-done:
		if approved {
			t.Error("expected approved=false after Resolve(approved=false)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not return within timeout")
	}
}

// Submit returns (false, nil) when no resolution arrives before the timeout.
func TestHandler_Submit_TimesOut(t *testing.T) {
	h := NewHandler()
	start := time.Now()
	approved, err := h.Submit(Request{ID: "r3", Timeout: 1}) // 1 second
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if approved {
		t.Error("expected approved=false on timeout")
	}
	if elapsed < time.Second {
		t.Errorf("expected to wait ~1s, waited %v", elapsed)
	}
	// A timed-out request must be cleaned out of the pending map.
	if got := len(h.GetPending()); got != 0 {
		t.Errorf("expected pending cleared after timeout, got %d", got)
	}
}

// Resolve for an unknown request ID is a no-op (no panic, no blocking).
func TestHandler_Resolve_UnknownID(t *testing.T) {
	h := NewHandler()
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("Resolve panicked on unknown id: %v", r)
			}
			close(done)
		}()
		h.Resolve(Response{ID: "does-not-exist", Approved: true})
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Resolve blocked on unknown id")
	}
	if got := len(h.GetPending()); got != 0 {
		t.Errorf("expected no pending after resolving unknown id, got %d", got)
	}
}

// OnRequest receives the full submitted Request.
func TestHandler_OnRequest_ReceivesRequest(t *testing.T) {
	h := NewHandler()
	got := make(chan Request, 1)
	h.OnRequest(func(r Request) {
		got <- r
		go h.Resolve(Response{ID: r.ID, Approved: true})
	})

	go h.Submit(Request{
		ID:             "r4",
		SessionID:      "sess-1",
		PermissionType: "command:exec",
		Description:    "run tests",
		Risk:           "high",
		Timeout:        5,
	})

	select {
	case r := <-got:
		if r.ID != "r4" {
			t.Errorf("expected id r4, got %s", r.ID)
		}
		if r.PermissionType != "command:exec" {
			t.Errorf("expected permission type command:exec, got %s", r.PermissionType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRequest never fired")
	}

	// Resolve to release the pending Submit goroutine.
	h.Resolve(Response{ID: "r4", Approved: true})
}

// GetPending reports requests that are still waiting for a resolution.
func TestHandler_GetPending_WhileWaiting(t *testing.T) {
	h := NewHandler()
	registered := make(chan struct{})
	h.OnRequest(func(r Request) {
		if r.ID == "p1" {
			select {
			case <-registered:
			default:
				close(registered)
			}
		}
	})

	done := make(chan struct{})
	go func() {
		h.Submit(Request{ID: "p1", Timeout: 5})
		close(done)
	}()

	// Wait until OnRequest confirms the request is registered.
	select {
	case <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("OnRequest never fired")
	}

	pending := h.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending request, got %d", len(pending))
	}
	if pending[0].ID != "p1" {
		t.Errorf("expected pending id p1, got %s", pending[0].ID)
	}

	// Release the waiting Submit.
	h.Resolve(Response{ID: "p1", Approved: true})
	<-done
}
