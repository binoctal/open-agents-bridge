package bridge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fix-bridge-session-integrity 4.2: a default-mode session's mid-turn
// session/request_permission must reach the web as permission:request (with
// the tool name and options intact), and the web's approval must flow back
// to the adapter without disturbing the turn — the task still completes.
// Before the CLAUDE_CONFIG_DIR isolation a default-mode claude session on a
// host with its own ~/.claude/settings.json never asked at all.
func TestReplayDefaultModePermissionFlow(t *testing.T) {
	sink := newReplaySink(t)
	startReplayBridge(t, sink, fixtureScript(t, "perm-default.script.jsonl"), 1)

	sink.sendTaskAssign("job-perm", "task-perm", "replay", 1)

	// 1. The request reaches the web, preserved.
	req := sink.waitFor(20*time.Second, "WS permission:request for task-perm",
		func(ev sinkEvent) bool {
			return ev.Channel == sinkChannelWS && ev.Type == "permission:request"
		})
	var p struct {
		SessionID string         `json:"sessionId"`
		ToolName  string         `json:"toolName"`
		Options   []string       `json:"options"`
		Risk      string         `json:"risk"`
		ID        interface{}    `json:"id"`
		ToolInput map[string]any `json:"toolInput"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		t.Fatalf("unmarshal permission:request: %v (%s)", err, req.Payload)
	}
	if !strings.Contains(p.ToolName, "rm -rf") {
		t.Errorf("toolName = %q, want the rm -rf title", p.ToolName)
	}
	if len(p.Options) != 3 || p.Options[0] != "allow_always" {
		t.Errorf("options = %v, want [allow_always allow_once reject]", p.Options)
	}
	if p.Risk != "high" {
		t.Errorf("risk = %q, want high (rm -rf is on the dangerous list)", p.Risk)
	}
	if p.ID == nil {
		t.Error("id missing — the web cannot address its approval without it")
	}

	// 2. The user approves. This must reach the adapter (JSON-RPC result on
	// its stdin) without side effects on the session.
	sink.sendPermissionResponse(p.ID, "allow_once", true)

	// 3. The turn completes normally: exactly one terminal report for the
	// task, and no session:error from the approval path.
	terminal := sink.assertExactlyOneTerminal(t, "task-perm")
	if terminal.Type != "workflow:task_result" {
		t.Fatalf("terminal = %s, want workflow:task_result", terminal.Type)
	}
	for _, ev := range sink.snapshot() {
		if ev.Channel == sinkChannelWS && ev.Type == "session:error" {
			t.Fatalf("unexpected session:error during the permission flow: %s", ev.Payload)
		}
	}
}
