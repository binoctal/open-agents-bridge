package session

import (
	"strings"
	"testing"
)

// Spec scenario "Missing shim command fails loud": a session with
// cliType "replay" and no OA_REPLAY_SHIM in the environment must be a hard
// error at command resolution — never a silent exec of an empty command
// (which would look like a working session that dies instantly).
func TestGetCLICommandReplayMissingEnvFailsLoud(t *testing.T) {
	t.Setenv("OA_REPLAY_SHIM", "")
	t.Setenv("OA_REPLAY_SHIM_ARGS", "")

	m := NewManager()
	cmd, _, err := m.getCLICommand("replay")
	if err == nil {
		t.Fatalf("expected error, got command %q", cmd)
	}
	if want := "OA_REPLAY_SHIM"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q must name %s so the operator knows what to set", err.Error(), want)
	}
}

func TestGetCLICommandReplayUsesEnv(t *testing.T) {
	t.Setenv("OA_REPLAY_SHIM", "/tmp/test-shim")
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")

	m := NewManager()
	cmd, args, err := m.getCLICommand("replay")
	if err != nil {
		t.Fatalf("getCLICommand: %v", err)
	}
	if cmd != "/tmp/test-shim" {
		t.Errorf("cmd = %q", cmd)
	}
	if len(args) != 1 || args[0] != "-test.run=TestReplayAgentHelper" {
		t.Errorf("args = %v", args)
	}
}
