package bridge

import (
	"strings"
	"testing"
	"time"
)

// G18 wire-level e2e: a dispatched task session must surface its real
// AgentStatus sequence on the WS — thinking (prompt delivered) → streaming
// (agent output) → idle (turn end / session stop) — with the status field
// strictly an enum value. This is the producer the vocabulary never had: PTY
// previously reported nothing until exit, and ACP label updates were being
// shipped as statuses.
func TestReplayAgentStatusSequence(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "success.script.jsonl"), 3)

	sink.sendTaskAssign("job-status", "task-status", "replay", 1)

	// Wait for the turn to reach its terminal report first — by then every
	// status transition has already been emitted.
	sink.assertExactlyOneTerminal(t, "task-status")

	// Collect agent:status events for this session, in arrival order.
	type statusEvt struct{ status string }
	var got []statusEvt
	valid := map[string]bool{
		"idle": true, "thinking": true, "streaming": true,
		"tool_executing": true, "permission_pending": true, "auth_required": true,
	}
	for _, ev := range sink.snapshot() {
		if ev.Channel != sinkChannelWS || ev.Type != "agent:status" {
			continue
		}
		if payloadString(t, ev, "sessionId") != "task-status" {
			continue
		}
		status := payloadString(t, ev, "status")
		if !valid[status] {
			t.Fatalf("agent:status carried non-enum status %q (label leak)", status)
		}
		got = append(got, statusEvt{status})
	}

	var seq []string
	for _, e := range got {
		seq = append(seq, e.status)
	}
	// The adapter's session-ready idle (the tracker's first observation)
	// races every turn transition — it may land anywhere before the final
	// idle. Drop every idle except the last one and assert the turn itself.
	last := len(seq) - 1
	var turn []string
	for i, s := range seq {
		if s == "idle" && i != last {
			continue
		}
		turn = append(turn, s)
	}
	joined := strings.Join(turn, ",")
	want := "thinking,streaming,idle"
	if joined != want {
		t.Fatalf("agent:status sequence = %q, want %q (raw events: %v)", joined, want, seq)
	}

	// thinking and streaming are each reported exactly once (dedup held);
	// the final idle is the exit report.
	for _, s := range []string{"thinking", "streaming"} {
		n := 0
		for _, e := range got {
			if e.status == s {
				n++
			}
		}
		if n != 1 {
			t.Errorf("status %s reported %d times, want exactly 1 (sequence %v)", s, n, seq)
		}
	}
}

// The question fixture must surface permission_pending — the PTY-path
// [QUESTION] is the permission signal on that path (ACP has its own
// permission request messages).
func TestReplayQuestionSurfacesPermissionPending(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "e2e-question.script.jsonl"), 3)

	sink.sendTaskAssign("job-q", "task-q", "replay", 1)

	deadline := time.Now().Add(20 * time.Second)
	for {
		hit := false
		for _, ev := range sink.snapshot() {
			if ev.Channel == sinkChannelWS && ev.Type == "agent:status" &&
				payloadString(t, ev, "sessionId") == "task-q" &&
				payloadString(t, ev, "status") == "permission_pending" {
				hit = true
			}
		}
		if hit {
			return
		}
		if time.Now().After(deadline) {
			var seen []string
			for _, ev := range sink.snapshot() {
				if ev.Type == "agent:status" {
					seen = append(seen, payloadString(t, ev, "status"))
				}
			}
			t.Fatalf("no permission_pending for task-q; statuses seen: %v", seen)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
