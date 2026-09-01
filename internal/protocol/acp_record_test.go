package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/replay"
)

// Spec scenario "Recording captures turn-end metadata" (bridge-replay-testing):
// a session/prompt response carrying stopReason must be recorded verbatim at
// the wire-frame layer. This is the evidence the whole suite exists to
// protect — taskTurnExitCode consumes msg.Meta.stopReason, so a recorder that
// dropped Meta (as IOLogger would) makes replay unable to reproduce turn-end.
func TestACPRecordingCapturesStopReason(t *testing.T) {
	// Scripted agent: answers initialize, session/new, then the first
	// session/prompt with stopReason=end_turn and stays alive (the bridge,
	// not the agent, decides when the process exits). Ids echo the bridge's
	// actual request ids (nextRequestID -> "bridge_N") so the recorder can
	// derive the after gate, exactly as a real agent echoes them.
	agent := `read l; echo '{"jsonrpc":"2.0","id":"bridge_1","result":{"protocolVersion":1,"agentInfo":{"name":"rec","version":"1"}}}'; read l; echo '{"jsonrpc":"2.0","id":"bridge_2","result":{"sessionId":"s1"}}'; read l; echo '{"jsonrpc":"2.0","id":"bridge_3","result":{"stopReason":"end_turn"}}'; sleep 30`

	scriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	rec, err := replay.NewRecorder(scriptPath, replay.Header{
		CLIType:        "claude",
		AdapterVersion: "test",
		Recipe:         "unit",
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}

	a := NewACPAdapter()
	a.SetRecorder(rec)
	defer a.Disconnect() // also flushes and closes the recorder

	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	if err := a.SendMessage(Message{Type: MessageTypeContent, Content: "hello"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Wait for the stopReason frame to hit the script (the readMessages
	// goroutine records asynchronously).
	deadline := time.After(10 * time.Second)
	for {
		data, err := os.ReadFile(scriptPath)
		if err == nil && containsStopReasonEndTurn(string(data)) {
			break
		}
		select {
		case <-deadline:
			data, _ := os.ReadFile(scriptPath)
			t.Fatalf("stopReason frame never recorded; script so far:\n%s", data)
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Disconnect (deferred) closes the recorder; force it here so the file
	// can be validated as a complete, reloadable script.
	a.Disconnect()

	script, err := replay.LoadScript(scriptPath)
	if err != nil {
		t.Fatalf("recorded script must reload: %v", err)
	}

	var sawPromptGate bool
	for _, fr := range script.Frames {
		if fr.Dir != replay.DirectionOut {
			continue
		}
		var probe struct {
			Result struct {
				StopReason string `json:"stopReason"`
			} `json:"result"`
		}
		if err := json.Unmarshal(fr.Frame, &probe); err != nil {
			continue
		}
		if probe.Result.StopReason == "end_turn" {
			if fr.After != "session/prompt" {
				t.Errorf("end_turn frame after = %q, want session/prompt gate", fr.After)
			}
			sawPromptGate = true
		}
	}
	if !sawPromptGate {
		t.Error("no recorded frame carries stopReason end_turn")
	}

	// Both sides must be in the script: the bridge's session/prompt request
	// (in) and the agent's response (out).
	methods := make(map[string]bool)
	for _, fr := range script.Frames {
		if fr.Dir != replay.DirectionIn {
			continue
		}
		var probe struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(fr.Frame, &probe); err == nil && probe.Method != "" {
			methods[probe.Method] = true
		}
	}
	for _, want := range []string{"initialize", "session/new", "session/prompt"} {
		if !methods[want] {
			t.Errorf("recorded script missing inbound %s request", want)
		}
	}
}

// Spec scenario "off-by-default": with no recorder attached the adapter
// behaves exactly as before — no script file is created anywhere. (The nil
// path itself is asserted in the replay package; here we prove the adapter
// does not conjure a recorder on its own.)
func TestACPRecordingOffByDefaultCreatesNoScript(t *testing.T) {
	dir := t.TempDir()
	agent := `read l; echo '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentInfo":{"name":"off","version":"1"}}}'; sleep 5`

	a := NewACPAdapter()
	defer a.Disconnect()
	// No SetRecorder call: recording must stay off.
	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: dir}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("workdir polluted with %d entries, want 0", len(entries))
	}
}

func containsStopReasonEndTurn(s string) bool {
	return strings.Contains(s, `"stopReason":"end_turn"`)
}
