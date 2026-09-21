package bridge

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	scannerpkg "github.com/binoctal/open-agents-bridge/internal/scanner"
	sessionpkg "github.com/binoctal/open-agents-bridge/internal/session"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

// fix-bridge-session-integrity defect A (bridge half): the auto-recreate
// branch of handleSessionSend used to fall back to workDir="." when the
// payload carried none, landing a restarted bridge's recreated agent in the
// bridge process cwd. It must now refuse with session:error/PARAM_MISSING;
// with params present (DO-side completion) the recreate must land in the
// original workDir.
//
// Hermetic: the "replay" cliType + G17 shim stands in for the CLI process
// (same pattern as TestStatusTrackerRemovedOnSessionRemoval).

func newSendBridge(t *testing.T) *Bridge {
	t.Helper()
	return &Bridge{
		config:         &config.Config{DeviceID: "dev-recreate"},
		scanner:        scannerpkg.New(),
		sessions:       sessionpkg.NewManager(),
		msgBuffer:      NewMessageBuffer(DefaultBufferCapacity),
		statusTrackers: make(map[string]*statusTracker),
	}
}

// offlineMessages drains the offline buffer into decoded Messages — with no
// WS connection, sendMessage lands every frame here.
func offlineMessages(t *testing.T, b *Bridge) []Message {
	t.Helper()
	b.offlineMu.Lock()
	defer b.offlineMu.Unlock()
	var out []Message
	for _, m := range b.offlineBuf {
		out = append(out, m)
	}
	return out
}

func findOffline(t *testing.T, b *Bridge, msgType string) (map[string]interface{}, bool) {
	t.Helper()
	for _, m := range offlineMessages(t, b) {
		if m.Type != msgType {
			continue
		}
		payload, ok := m.Payload.(map[string]interface{})
		if !ok {
			t.Fatalf("%s payload is %T, want map", msgType, m.Payload)
		}
		return payload, true
	}
	return nil, false
}

func TestSessionSendAutoRecreateNoWorkDirRefused(t *testing.T) {
	b := newSendBridge(t)

	// No session registered (bridge "restarted") and no workDir in the
	// payload (DO completion missed too — both restarted together).
	b.handleSessionSend(Message{
		Type: "session:send",
		Payload: map[string]interface{}{
			"sessionId": "sess-noparams",
			"deviceId":  "dev-recreate",
			"content":   "hello",
		},
		Timestamp: time.Now().UnixMilli(),
	})

	payload, ok := findOffline(t, b, "session:error")
	if !ok {
		t.Fatal("expected session:error for param-less auto-recreate, got none")
	}
	if code, _ := payload["code"].(string); code != "PARAM_MISSING" {
		t.Errorf("session:error code = %v, want PARAM_MISSING", payload["code"])
	}
	if sid, _ := payload["sessionId"].(string); sid != "sess-noparams" {
		t.Errorf("session:error sessionId = %v, want sess-noparams", payload["sessionId"])
	}
	if sess := b.sessions.Get("sess-noparams"); sess != nil {
		t.Error("session must NOT be recreated without a workDir (would land in bridge cwd)")
	}
	if _, started := findOffline(t, b, "session:started"); started {
		t.Error("no session:started may be emitted for a refused recreate")
	}
}

func TestSessionSendAutoRecreateWithParamsLandsInWorkDir(t *testing.T) {
	dir := FixtureWorkspace(t)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)

	// Params as the DO-side completion would inject them.
	b.handleSessionSend(Message{
		Type: "session:send",
		Payload: map[string]interface{}{
			"sessionId":      "sess-withparams",
			"deviceId":       "dev-recreate",
			"content":        "hello",
			"cliType":        "replay",
			"workDir":        dir,
			"permissionMode": "default",
		},
		Timestamp: time.Now().UnixMilli(),
	})

	payload, ok := findOffline(t, b, "session:started")
	if !ok {
		t.Fatal("expected session:started for param-carrying auto-recreate, got none")
	}
	if wd, _ := payload["workDir"].(string); wd != dir {
		t.Errorf("session:started workDir = %q, want %q", payload["workDir"], dir)
	}
	sess := b.sessions.Get("sess-withparams")
	if sess == nil {
		t.Fatal("session should exist after recreate")
	}
	if sess.WorkDir != dir {
		t.Errorf("recreated session WorkDir = %q, want %q", sess.WorkDir, dir)
	}
	if _, refused := findOffline(t, b, "session:error"); refused {
		t.Error("param-carrying recreate must not be refused")
	}

	if err := b.sessions.Stop("sess-withparams"); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// Compile-time guard that the error payload stays JSON-marshalable with its
// code field (the web branch reads payload.code).
func TestSessionRecreateErrorPayloadShape(t *testing.T) {
	_, err := json.Marshal(map[string]interface{}{
		"sessionId": "s", "deviceId": "d", "error": "boom", "code": "PARAM_MISSING",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
}

// Defect B: a resume against a bridge that lost the session must recreate it
// from the DO-completed params and answer session:resumed — not fail and let
// the next session:send recreate it blind (the old masked-swap path).
func TestSessionResumeControlledRecreate(t *testing.T) {
	dir := FixtureWorkspace(t)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)

	b.handleSessionResume(Message{
		Type: "session:resume",
		Payload: map[string]interface{}{
			"sessionId":      "sess-resume",
			"deviceId":       "dev-recreate",
			"cliType":        "replay",
			"workDir":        dir,
			"permissionMode": "default",
		},
		Timestamp: time.Now().UnixMilli(),
	})

	payload, ok := findOffline(t, b, "session:resumed")
	if !ok {
		t.Fatal("expected session:resumed from controlled recreate, got none")
	}
	if wd, _ := payload["workDir"].(string); wd != dir {
		t.Errorf("session:resumed workDir = %q, want %q", payload["workDir"], dir)
	}
	if recreated, _ := payload["recreated"].(bool); !recreated {
		t.Error("controlled recreate must mark recreated:true so the web can show it")
	}
	if sess := b.sessions.Get("sess-resume"); sess == nil || sess.WorkDir != dir {
		t.Errorf("recreated session missing or wrong workDir: %+v", sess)
	}
	if _, failed := findOffline(t, b, "session:resume:failed"); failed {
		t.Error("param-carrying resume recreate must not report failure")
	}

	if err := b.sessions.Stop("sess-resume"); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// Without params the resume must still fail explicitly (not_found) — a blind
// recreate would repeat the defect-A cwd swap under a different message.
func TestSessionResumeNoParamsStillFails(t *testing.T) {
	b := newSendBridge(t)

	b.handleSessionResume(Message{
		Type: "session:resume",
		Payload: map[string]interface{}{
			"sessionId": "sess-resume-bare",
			"deviceId":  "dev-recreate",
		},
		Timestamp: time.Now().UnixMilli(),
	})

	payload, ok := findOffline(t, b, "session:resume:failed")
	if !ok {
		t.Fatal("expected session:resume:failed for param-less resume")
	}
	if reason, _ := payload["reason"].(string); reason != "not_found" {
		t.Errorf("reason = %v, want not_found", payload["reason"])
	}
	if sess := b.sessions.Get("sess-resume-bare"); sess != nil {
		t.Error("no session may be created from a param-less resume")
	}
}
