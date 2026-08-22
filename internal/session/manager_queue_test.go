package session

import (
	"testing"
)

// The pool-full enqueue path (bridge handleWorkflowTaskAssign) hands tasks to
// this queue; the drain (bridge drainTaskQueue) takes them back via
// DequeueNext as capacity frees. QueueItem must carry JobID — the completion
// exit callback keys on the session's job/task metadata, so a drained task
// without it would silently lose its result reporting (found live in dogfood
// 2026-08-22: the queue was never drained at all).
func TestQueueRoundTrip(t *testing.T) {
	m := NewManager()

	if item := m.DequeueNext(); item != nil {
		t.Fatalf("empty queue returned %v, want nil", item)
	}

	first := QueueItem{
		CLIType: "claude-pty", WorkDir: ".", SessionID: "task-1", JobID: "job-9",
		Cols: 120, Rows: 30, PermMode: "accept-edits", Prompt: "p1",
	}
	second := QueueItem{CLIType: "claude-pty", SessionID: "task-2", JobID: "job-9", Prompt: "p2"}
	m.Enqueue(first)
	m.Enqueue(second)

	// Drain order is FIFO.
	got := m.DequeueNext()
	if got == nil || got.SessionID != "task-1" || got.JobID != "job-9" {
		t.Fatalf("first dequeue = %+v, want task-1/job-9", got)
	}
	if got.PermMode != "accept-edits" || got.Cols != 120 || got.Rows != 30 || got.Prompt != "p1" {
		t.Fatalf("first dequeue lost fields: %+v", got)
	}
	if got.EnqueuedAt.IsZero() {
		// EnqueuedAt is set by Enqueue; it feeds drain wait logging.
		t.Fatal("EnqueuedAt not stamped by Enqueue")
	}
	if got2 := m.DequeueNext(); got2 == nil || got2.SessionID != "task-2" {
		t.Fatalf("second dequeue = %+v, want task-2", got2)
	}
	if item := m.DequeueNext(); item != nil {
		t.Fatalf("drained queue returned %v, want nil", item)
	}
}
