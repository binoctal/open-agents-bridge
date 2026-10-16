package bridge

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

func TestLoopCallKeyCountsOnlyAnnouncements(t *testing.T) {
	start := protocol.ToolCall{ID: "t1", Name: "Bash", Input: map[string]interface{}{"command": "ls"}, Status: "pending"}
	if n, k, ok := loopCallKey(start); !ok || n != "Bash" || k == "" {
		t.Fatalf("announcement must count: %q %q %v", n, k, ok)
	}
	for _, c := range []protocol.ToolCall{
		{ID: "t1", Status: "in_progress"},
		{ID: "t1", Name: "Bash", Status: "completed"},
		{ID: "t1"},
	} {
		if _, _, ok := loopCallKey(c); ok {
			t.Fatalf("update must not count: %+v", c)
		}
	}
	if _, _, ok := loopCallKey("not a tool call"); ok {
		t.Fatal("non tool-call content must not count")
	}
	a := protocol.ToolCall{Name: "Read", Input: map[string]interface{}{"p": "a"}, Status: "pending"}
	b := protocol.ToolCall{Name: "Read", Input: map[string]interface{}{"p": "b"}, Status: "pending"}
	_, ka, _ := loopCallKey(a)
	_, kb, _ := loopCallKey(b)
	if ka == kb {
		t.Fatal("different inputs must have different fingerprints")
	}
}
