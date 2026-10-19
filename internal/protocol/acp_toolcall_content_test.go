package protocol

import (
	"strings"
	"testing"
)

// Tool call payload fidelity (prod 2026-10-19): the expanded detail of a tool
// row rendered empty because the adapter dropped both content fields
// claude-code-acp actually sends — rawInput (structured arguments) on
// tool_call and rawOutput (engine content blocks) on tool_call_update. The
// old code read a top-level "result" key that never exists.

func toolCallUpdateFrame(update map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params":  map[string]interface{}{"update": update},
	}
}

func collectToolCalls(t *testing.T, frames ...map[string]interface{}) []ToolCall {
	t.Helper()
	a := NewACPAdapter()
	var calls []ToolCall
	done := make(chan struct{})
	a.Subscribe(func(m Message) {
		if m.Type == MessageTypeToolCall {
			calls = append(calls, m.Content.(ToolCall))
		}
		close(done)
	})
	for _, f := range frames {
		a.handleMessage(f)
	}
	<-done
	return calls
}

func TestToolCallCarriesRawInput(t *testing.T) {
	calls := collectToolCalls(t, toolCallUpdateFrame(map[string]interface{}{
		"sessionUpdate": "tool_call",
		"toolCallId":    "t1",
		"title":         "Read file",
		"status":        "pending",
		"rawInput": map[string]interface{}{
			"file_path": "/repo/src/app.ts",
		},
	}))
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	if got := calls[0].Input["file_path"]; got != "/repo/src/app.ts" {
		t.Fatalf("input.file_path = %v, want /repo/src/app.ts", got)
	}
}

func TestToolCallInputTruncatesOversizedValues(t *testing.T) {
	big := strings.Repeat("x", 5000)
	calls := collectToolCalls(t, toolCallUpdateFrame(map[string]interface{}{
		"sessionUpdate": "tool_call",
		"toolCallId":    "t2",
		"title":         "Write file",
		"status":        "pending",
		"rawInput": map[string]interface{}{
			"file_path": "/repo/out.ts",
			"content":   big,
		},
	}))
	s, ok := calls[0].Input["content"].(string)
	if !ok {
		t.Fatal("input.content missing")
	}
	if len(s) > toolCallInputValueMax+16 {
		t.Fatalf("input.content len = %d, want bounded by %d (+ellipsis)", len(s), toolCallInputValueMax)
	}
	if fp := calls[0].Input["file_path"]; fp != "/repo/out.ts" {
		t.Fatalf("small sibling value clobbered: file_path = %v", fp)
	}
}

func TestToolCallUpdateFlattensRawOutput(t *testing.T) {
	calls := collectToolCalls(t, toolCallUpdateFrame(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "t1",
		"status":        "completed",
		"rawOutput": []interface{}{
			map[string]interface{}{"type": "text", "text": "     1\tpackage main"},
			map[string]interface{}{"type": "text", "text": "     2\tfunc main() {}"},
		},
	}))
	res, ok := calls[0].Result.(string)
	if !ok {
		t.Fatalf("result = %#v, want flattened string", calls[0].Result)
	}
	if !strings.Contains(res, "package main") || !strings.Contains(res, "func main() {}") {
		t.Fatalf("result missing block texts: %q", res)
	}
}

func TestToolCallUpdateResultBounded(t *testing.T) {
	wall := strings.Repeat("y", 100*1024)
	calls := collectToolCalls(t, toolCallUpdateFrame(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "t3",
		"status":        "completed",
		"rawOutput": []interface{}{
			map[string]interface{}{"type": "text", "text": wall},
		},
	}))
	res, ok := calls[0].Result.(string)
	if !ok {
		t.Fatalf("result = %#v, want string", calls[0].Result)
	}
	if len(res) > toolCallResultMax+16 {
		t.Fatalf("result len = %d, want bounded by %d (+ellipsis)", len(res), toolCallResultMax)
	}
}

func TestToolCallUpdateWithoutRawOutputKeepsNilResult(t *testing.T) {
	// The post-tool-use hook variant carries only _meta; result must stay nil
	// so the web keeps whatever the terminal update stored.
	calls := collectToolCalls(t, toolCallUpdateFrame(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "t4",
	}))
	if calls[0].Result != nil {
		t.Fatalf("result = %#v, want nil", calls[0].Result)
	}
}
