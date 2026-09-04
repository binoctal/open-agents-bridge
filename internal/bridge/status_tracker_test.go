package bridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
	sessionpkg "github.com/binoctal/open-agents-bridge/internal/session"
)

func contentMsg() protocol.Message {
	return protocol.Message{Type: protocol.MessageTypeContent, Content: "chunk"}
}

func thoughtMsg() protocol.Message {
	return protocol.Message{Type: protocol.MessageTypeThought, Content: "hm"}
}

func toolCallMsg() protocol.Message {
	return protocol.Message{Type: protocol.MessageTypeToolCall, Content: protocol.ToolCall{ID: "t1"}}
}

func typedStatusMsg(s protocol.AgentStatus) protocol.Message {
	return protocol.Message{Type: protocol.MessageTypeStatus, Content: s}
}

func labelStatusMsg(label string) protocol.Message {
	return protocol.Message{Type: protocol.MessageTypeStatus, Content: label}
}

// The spec scenario: prompt -> continuous stdout -> process exit reports
// thinking, streaming, idle — each exactly once, even when the burst is
// shorter than the dwell window (forward progress is never throttled).
func TestTrackerPTYLifecycleSequence(t *testing.T) {
	tr := &statusTracker{}

	if s, changed := tr.observePrompt(); !changed || s != protocol.StatusThinking {
		t.Fatalf("prompt: got (%v,%v), want (thinking,true)", s, changed)
	}
	if _, changed := tr.observePrompt(); changed {
		t.Fatal("duplicate prompt must not re-report thinking")
	}
	if s, changed := tr.observe(contentMsg()); !changed || s != protocol.StatusStreaming {
		t.Fatalf("first content: got (%v,%v), want (streaming,true) — forward progress is immediate", s, changed)
	}
	if _, changed := tr.observe(contentMsg()); changed {
		t.Fatal("repeated content must not re-report streaming")
	}
	if s, changed := tr.observe(typedStatusMsg(protocol.StatusIdle)); !changed || s != protocol.StatusIdle {
		t.Fatalf("exit: got (%v,%v), want (idle,true)", s, changed)
	}
	if _, changed := tr.observe(typedStatusMsg(protocol.StatusIdle)); changed {
		t.Fatal("post-exit idle must not re-report")
	}
}

func TestTrackerACPDerivations(t *testing.T) {
	tr := &statusTracker{}
	tr.observePrompt()

	if s, changed := tr.observe(toolCallMsg()); !changed || s != protocol.StatusToolExecuting {
		t.Fatalf("tool call: got (%v,%v), want (tool_executing,true)", s, changed)
	}
	perm := protocol.Message{Type: protocol.MessageTypePermission, Content: protocol.PermissionRequest{}}
	if s, changed := tr.observe(perm); !changed || s != protocol.StatusPermissionPending {
		t.Fatalf("permission: got (%v,%v), want (permission_pending,true)", s, changed)
	}
	// permission_pending is immediate — never throttled, even back-to-back.
	if s, changed := tr.observe(typedStatusMsg(protocol.StatusThinking)); !changed || s != protocol.StatusThinking {
		t.Fatalf("turn resume: got (%v,%v), want (thinking,true)", s, changed)
	}
}

// Backward flapping inside the dwell window is suppressed: the thought-chunk
// / content-chunk alternation must not oscillate the reported status.
func TestTrackerBackwardFlapSuppressed(t *testing.T) {
	tr := &statusTracker{}
	tr.observePrompt()       // thinking
	tr.observe(contentMsg()) // streaming (forward, immediate)
	if _, changed := tr.observe(thoughtMsg()); changed {
		t.Fatal("thought right after streaming must be suppressed (backward within dwell)")
	}
	if got := tr.current(); got != protocol.StatusStreaming {
		t.Fatalf("current after suppressed flap: got %v, want streaming", got)
	}
}

func TestTrackerBackwardAllowedAfterDwell(t *testing.T) {
	tr := &statusTracker{}
	tr.observePrompt()
	tr.observe(toolCallMsg()) // tool_executing
	// Simulate dwell elapsed by rewinding the last report time.
	tr.mu.Lock()
	tr.lastReport = time.Now().Add(-2 * statusDwell)
	tr.mu.Unlock()
	if s, changed := tr.observe(thoughtMsg()); !changed || s != protocol.StatusThinking {
		t.Fatalf("backward after dwell: got (%v,%v), want (thinking,true)", s, changed)
	}
}

// Straggler after idle: the ACP prompt response (stopReason → idle) can beat
// the agent's final content chunks, so active observations landing inside the
// dwell window after an idle report belong to the ended turn and must be
// swallowed — otherwise the session sticks in streaming forever
// (live-observed on staging with opencode). A new prompt forces through.
func TestTrackerPostIdleStragglerSuppressed(t *testing.T) {
	tr := &statusTracker{}
	tr.observePrompt()       // thinking
	tr.observe(contentMsg()) // streaming

	// Turn end races ahead of the last chunks.
	if s, changed := tr.observe(typedStatusMsg(protocol.StatusIdle)); !changed || s != protocol.StatusIdle {
		t.Fatalf("turn end: got (%v,%v), want (idle,true)", s, changed)
	}
	if _, changed := tr.observe(thoughtMsg()); changed {
		t.Fatal("thought straggler right after idle must be suppressed")
	}
	if _, changed := tr.observe(contentMsg()); changed {
		t.Fatal("content straggler right after idle must be suppressed")
	}
	if got := tr.current(); got != protocol.StatusIdle {
		t.Fatalf("current after stragglers: got %v, want idle", got)
	}

	// A genuinely new prompt restarts the turn immediately.
	if s, changed := tr.observePrompt(); !changed || s != protocol.StatusThinking {
		t.Fatalf("new prompt after idle: got (%v,%v), want (thinking,true)", s, changed)
	}

	// Once the dwell window has passed, late content is treated as a real
	// transition again (rewind lastReport to simulate elapsed time).
	tr.observe(typedStatusMsg(protocol.StatusIdle))
	tr.mu.Lock()
	tr.lastReport = time.Now().Add(-2 * statusDwell)
	tr.mu.Unlock()
	if s, changed := tr.observe(contentMsg()); !changed || s != protocol.StatusStreaming {
		t.Fatalf("content after dwell: got (%v,%v), want (streaming,true)", s, changed)
	}
}

func TestStatusFromMessageLabelVsTyped(t *testing.T) {
	if _, ok := statusFromMessage(labelStatusMsg("mode_update")); ok {
		t.Fatal("label-class status must not map to an agent status")
	}
	if label, ok := statusLabel(labelStatusMsg("commands_update")); !ok || label != "commands_update" {
		t.Fatalf("statusLabel: got (%q,%v)", label, ok)
	}
	if _, ok := statusLabel(typedStatusMsg(protocol.StatusIdle)); ok {
		t.Fatal("typed status must not be classified as a label")
	}
	if s, ok := statusFromMessage(typedStatusMsg(protocol.StatusIdle)); !ok || s != protocol.StatusIdle {
		t.Fatalf("typed status: got (%v,%v), want (idle,true)", s, ok)
	}
}

// Tracker cleanup: the removed-callback must drop the entry so the map
// cannot grow with session churn. Uses the G17 replay shim as the CLI
// process (the established hermetic pattern — no real CLI involved).
func TestStatusTrackerRemovedOnSessionRemoval(t *testing.T) {
	dir := FixtureWorkspace(t)
	script := `{"frames": []}`
	scriptPath := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", scriptPath)

	b := &Bridge{
		sessions:       sessionpkg.NewManager(),
		statusTrackers: make(map[string]*statusTracker),
	}
	b.sessions.SetRemovedCallback(b.removeStatusTracker)

	sess, err := b.sessions.Create("replay", dir)
	if err != nil {
		t.Fatalf("create replay session: %v", err)
	}
	b.statusTrackerFor(sess.ID)
	if err := b.sessions.Stop(sess.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.statusTrackersMu.Lock()
		_, leaked := b.statusTrackers[sess.ID]
		b.statusTrackersMu.Unlock()
		if !leaked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("status tracker leaked after session removal")
}
