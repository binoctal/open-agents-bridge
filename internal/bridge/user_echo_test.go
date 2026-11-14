package bridge

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// User-message duplication (reported 2026-11-14): claude-agent-acp echoes the
// user's prompt back as a user_message_chunk session/update. The adapter tags
// that message meta.role == "user", but forwardSessionOutput used to batch it
// like any assistant content, so the web client rendered the text a second
// time (as an assistant bubble) and the DO persisted it as an assistant
// history row. isUserEcho gates that path.

func TestIsUserEcho(t *testing.T) {
	cases := []struct {
		name string
		msg  protocol.Message
		want bool
	}{
		{
			"user_message_chunk echo",
			protocol.Message{
				Type:    protocol.MessageTypeContent,
				Content: "fix the login bug",
				Meta:    map[string]interface{}{"protocol": "acp", "role": "user"},
			},
			true,
		},
		{
			"assistant content carries no role",
			protocol.Message{
				Type:    protocol.MessageTypeContent,
				Content: "Here is the fix…",
				Meta:    map[string]interface{}{"protocol": "acp"},
			},
			false,
		},
		{
			"nil meta (nil-map read must not panic)",
			protocol.Message{Type: protocol.MessageTypeContent, Content: "x"},
			false,
		},
		{
			"non-user role is not an echo",
			protocol.Message{
				Type:    protocol.MessageTypeContent,
				Content: "x",
				Meta:    map[string]interface{}{"role": "assistant"},
			},
			false,
		},
		{
			"non-string role is not an echo",
			protocol.Message{
				Type:    protocol.MessageTypeContent,
				Content: "x",
				Meta:    map[string]interface{}{"role": 42},
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUserEcho(tc.msg); got != tc.want {
				t.Fatalf("isUserEcho(%v) = %v, want %v", tc.msg.Meta, got, tc.want)
			}
		})
	}
}
