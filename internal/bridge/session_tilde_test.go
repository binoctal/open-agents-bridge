package bridge

import (
	"os"
	"testing"
	"time"
)

// fix-session-workdir-tilde: the default workDir sent by web is `~`, which
// bridge must expand to the device home before spawn — otherwise cmd.Dir
// chdirs into a literal "~" directory and creation always fails with a
// misleading "fork/exec <cmd>: no such file or directory".
//
// Hermetic: same "replay" cliType + G17 shim pattern as
// TestSessionSendAutoRecreateWithParamsLandsInWorkDir. os.UserHomeDir reads
// $HOME on linux, so pointing HOME at the fixture workspace makes the
// expansion result observable.

func TestSessionStartExpandsTildeWorkDir(t *testing.T) {
	home := FixtureWorkspace(t)
	t.Setenv("HOME", home)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)

	b.handleSessionStart(Message{
		Type: "session:start",
		Payload: map[string]interface{}{
			"sessionId": "sess-tilde",
			"deviceId":  "dev-recreate",
			"cliType":   "replay",
			"workDir":   "~",
		},
		Timestamp: time.Now().UnixMilli(),
	})

	// The ack must echo the expanded path, never the literal "~".
	payload, ok := findOffline(t, b, "session:started")
	if !ok {
		t.Fatal("expected session:started for workDir ~")
	}
	if wd, _ := payload["workDir"].(string); wd != home {
		t.Errorf("session:started workDir = %q, want expanded home %q", payload["workDir"], home)
	}
	sess := b.sessions.Get("sess-tilde")
	if sess == nil {
		t.Fatal("session should exist after start")
	}
	if sess.WorkDir != home {
		t.Errorf("session WorkDir = %q, want %q", sess.WorkDir, home)
	}
	if _, errored := findOffline(t, b, "session:error"); errored {
		t.Error("workDir ~ must not produce session:error")
	}

	if err := b.sessions.Stop("sess-tilde"); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A `~/...` workDir under a home that exists creates in the joined path;
// a `~` pointing at a home that does not exist must fail with the truthful
// reason instead of the fork/exec red herring.
func TestSessionStartTildeSubpathAndMissingHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OA_REPLAY_SHIM", os.Args[0])
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")
	t.Setenv("OA_REPLAY_SCRIPT", fixtureScript(t, "success.script.jsonl"))

	b := newSendBridge(t)

	// ~/work exists → session lands there.
	work := home + "/work"
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b.handleSessionStart(Message{
		Type: "session:start",
		Payload: map[string]interface{}{
			"sessionId": "sess-tilde-sub",
			"cliType":   "replay",
			"workDir":   "~/work",
		},
		Timestamp: time.Now().UnixMilli(),
	})
	sess := b.sessions.Get("sess-tilde-sub")
	if sess == nil {
		t.Fatal("session should exist for ~/work")
	}
	if sess.WorkDir != work {
		t.Errorf("session WorkDir = %q, want %q", sess.WorkDir, work)
	}
	if err := b.sessions.Stop("sess-tilde-sub"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// ~/missing does not exist → truthful session:error, no session.
	b2 := newSendBridge(t)
	b2.handleSessionStart(Message{
		Type: "session:start",
		Payload: map[string]interface{}{
			"sessionId": "sess-tilde-missing",
			"cliType":   "replay",
			"workDir":   "~/missing-0922",
		},
		Timestamp: time.Now().UnixMilli(),
	})
	payload, ok := findOffline(t, b2, "session:error")
	if !ok {
		t.Fatal("expected session:error for non-existent ~/missing-0922")
	}
	if errStr, _ := payload["error"].(string); errStr == "" {
		t.Error("session:error must carry the error text")
	}
	if _, started := findOffline(t, b2, "session:started"); started {
		t.Error("no session:started may be emitted for a failed start")
	}
	if sess := b2.sessions.Get("sess-tilde-missing"); sess != nil {
		t.Error("session must not exist after failed start")
	}
}

