package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 2026-09-26: session/cancel is a NOTIFICATION in the ACP spec. The bridge
// used to send it as a request with an "id"; agents that register the method
// via onNotification only (claude-agent-acp v0.49.0) answer -32601 and drop
// the cancel entirely — the stop button did nothing and the idle watchdog
// later reported "agent turn stalled". The cancel frame must carry no "id".

func TestCancelSentAsNotification(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "stdin-captured.log")

	// Fake agent: answer initialize, then tee every incoming frame to a file.
	agent := "echo '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"agentInfo\":{\"name\":\"t\",\"version\":\"1\"}}}'\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> " + capture + "\n" +
		"done"

	a := NewACPAdapter()
	defer a.Disconnect()

	if err := a.Connect(AdapterConfig{Command: "sh", Args: []string{"-c", agent}, WorkDir: dir}); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	if err := a.SendMessage(Message{Type: MessageTypeCancel, Content: "user_cancelled"}); err != nil {
		t.Fatalf("SendMessage(cancel) failed: %v", err)
	}

	// Wait for the frame to land in the capture file.
	var cancelLine string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(capture)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if strings.Contains(line, "session/cancel") {
					cancelLine = line
					break
				}
			}
		}
		if cancelLine != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if cancelLine == "" {
		t.Fatal("session/cancel frame never reached the agent's stdin")
	}

	var frame map[string]interface{}
	if err := json.Unmarshal([]byte(cancelLine), &frame); err != nil {
		t.Fatalf("cancel frame is not valid JSON: %v\nframe: %s", err, cancelLine)
	}

	if _, hasID := frame["id"]; hasID {
		t.Errorf("session/cancel carried an \"id\" (%v) — agents that register it as a notification drop the cancel with -32601", frame["id"])
	}
	if frame["method"] != "session/cancel" {
		t.Errorf("method = %v, want session/cancel", frame["method"])
	}
	params, _ := frame["params"].(map[string]interface{})
	if params["reason"] != "user_cancelled" {
		t.Errorf("params.reason = %v, want user_cancelled", params["reason"])
	}
}
