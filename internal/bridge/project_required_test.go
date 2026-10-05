package bridge

import (
	"testing"
	"time"
)

// workspace-checkout: a session entry point without a project path must be
// rejected with PROJECT_REQUIRED instead of silently running in the bridge's
// launch directory ("."), which a service-run bridge never chose for the user.

func assertProjectRequired(t *testing.T, b *Bridge, sessionID string) {
	t.Helper()
	payload, ok := findOffline(t, b, "session:error")
	if !ok {
		t.Fatal("expected session:error for a missing project path")
	}
	if code, _ := payload["code"].(string); code != "PROJECT_REQUIRED" {
		t.Errorf("code = %q, want PROJECT_REQUIRED", code)
	}
	if sid, _ := payload["sessionId"].(string); sid != sessionID {
		t.Errorf("sessionId = %q, want %q", sid, sessionID)
	}
	if _, started := findOffline(t, b, "session:started"); started {
		t.Error("no session:started may be emitted without a project path")
	}
	if sess := b.sessions.Get(sessionID); sess != nil {
		t.Error("no session may be created without a project path")
	}
}

func TestSessionStartWithoutWorkDirIsRejected(t *testing.T) {
	b := newSendBridge(t)
	b.handleSessionStart(Message{
		Type: "session:start",
		Payload: map[string]interface{}{
			"sessionId": "sess-no-dir",
			"cliType":   "replay",
		},
		Timestamp: time.Now().UnixMilli(),
	})
	assertProjectRequired(t, b, "sess-no-dir")
}

func TestResumeWithContextWithoutWorkDirIsRejected(t *testing.T) {
	b := newSendBridge(t)
	b.handleResumeWithContext(Message{
		Type: "session:resume-with-context",
		Payload: map[string]interface{}{
			"originalSessionId": "sess-resume-no-dir",
			"cliType":           "replay",
		},
		Timestamp: time.Now().UnixMilli(),
	})
	assertProjectRequired(t, b, "sess-resume-no-dir")
}

func TestChatSendAutoCreateWithoutWorkDirIsRejected(t *testing.T) {
	b := newSendBridge(t)
	b.handleChatSend(Message{
		Type: "chat:send",
		Payload: map[string]interface{}{
			"sessionId": "sess-chat-no-dir",
			"content":   "hello",
		},
		Timestamp: time.Now().UnixMilli(),
	})
	assertProjectRequired(t, b, "sess-chat-no-dir")
}
