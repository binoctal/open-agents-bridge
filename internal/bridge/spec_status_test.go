package bridge

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
	"github.com/binoctal/open-agents-bridge/internal/session"
)

func TestSpecTrackerIgnoresStaleVersions(t *testing.T) {
	tr := newSpecTracker()
	if !tr.accept("s", 5) {
		t.Fatal("first version must be accepted")
	}
	if tr.accept("s", 4) {
		t.Error("out-of-order version 4 accepted after 5")
	}
	if tr.accept("s", 5) {
		t.Error("duplicate version 5 accepted twice")
	}
	if tr.get("s") != 5 {
		t.Errorf("applied version = %d, want 5", tr.get("s"))
	}
	if !tr.accept("other", 1) {
		t.Error("versions are per session")
	}
}

func TestApplyPermissionSpecSpawnBakedEngineNeedsRestart(t *testing.T) {
	sess := &session.Session{ID: "s", CLIType: "gemini", PermissionMode: "accept-all", Protocol: protocol.NewManager()}
	state, _ := applyPermissionSpec(sess, "default")
	if state != applyStateRestartRequired {
		t.Errorf("state = %q, want restart_required", state)
	}
	if sess.PermissionMode != "accept-all" {
		t.Errorf("actual mode changed to %q although engine was not touched", sess.PermissionMode)
	}
}

func TestApplyPermissionSpecWithoutProtocolFails(t *testing.T) {
	sess := &session.Session{ID: "s", CLIType: "claude", PermissionMode: "default"}
	if state, reason := applyPermissionSpec(sess, "plan"); state != applyStateFailed || reason == "" {
		t.Errorf("state=%q reason=%q, want failed with reason", state, reason)
	}
}

func TestModeFromConfigOptions(t *testing.T) {
	opts := []interface{}{
		map[string]interface{}{"id": "model", "currentValue": "x"},
		map[string]interface{}{"id": "mode", "currentValue": "plan"},
	}
	if got := modeFromConfigOptions(opts); got != "plan" {
		t.Errorf("got %q, want plan", got)
	}
	if got := modeFromConfigOptions(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCapabilityReportDeclaresSessionSpec(t *testing.T) {
	r := buildCapabilityReport("v", "ok", "", false)
	if r["sessionSpec"] != 1 {
		t.Errorf("sessionSpec = %v, want 1", r["sessionSpec"])
	}
}
