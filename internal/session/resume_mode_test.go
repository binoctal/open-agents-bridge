package session

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// A resume that cannot reach the engine must not claim the new mode: the
// recorded mode is what session:spec_update compares against.
func TestSyncResumedModeKeepsModeWhenEngineUnreachable(t *testing.T) {
	m := &Manager{}
	sess := &Session{ID: "s1", CLIType: "claude", PermissionMode: "default", Protocol: protocol.NewManager()}

	m.syncResumedMode(sess, "accept-edits")

	if sess.PermissionMode != "default" {
		t.Fatalf("PermissionMode = %q, want default (engine never applied accept-edits)", sess.PermissionMode)
	}
}

func TestSyncResumedModeNoopForSameOrEmptyMode(t *testing.T) {
	m := &Manager{}
	sess := &Session{ID: "s1", CLIType: "claude", PermissionMode: "plan", Protocol: protocol.NewManager()}

	m.syncResumedMode(sess, "")
	m.syncResumedMode(sess, "plan")

	if sess.PermissionMode != "plan" {
		t.Fatalf("PermissionMode = %q, want plan", sess.PermissionMode)
	}
}
