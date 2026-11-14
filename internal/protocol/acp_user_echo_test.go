package protocol

import (
	"testing"
)

// Contract for the user-echo filter in bridge/bridge.go (isUserEcho): the
// adapter must keep tagging the ACP user_message_chunk echo with
// meta.role == "user". The bridge relies on that tag to keep the echoed
// prompt out of the chat:response path — dropping the tag would resurface the
// duplicate-user-message bug.

func TestUserMessageChunkTaggedAsUserEcho(t *testing.T) {
	a := NewACPAdapter()
	got := make(chan Message, 1)
	a.Subscribe(func(m Message) {
		if m.Type == MessageTypeContent {
			select {
			case got <- m:
			default:
			}
		}
	})

	a.handleMessage(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]interface{}{
			"update": map[string]interface{}{
				"sessionUpdate": "user_message_chunk",
				"content":       map[string]interface{}{"type": "text", "text": "hello agent"},
			},
		},
	})

	var msg Message
	select {
	case msg = <-got:
	default:
		t.Fatal("user_message_chunk emitted no MessageTypeContent")
	}
	if msg.Content != "hello agent" {
		t.Fatalf("content = %v, want the echoed prompt text", msg.Content)
	}
	role, ok := msg.Meta["role"].(string)
	if !ok || role != "user" {
		t.Fatalf("meta.role = %v (ok=%v), want \"user\" — the bridge echo filter depends on it", msg.Meta["role"], ok)
	}
}
