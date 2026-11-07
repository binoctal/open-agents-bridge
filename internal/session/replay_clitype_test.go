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
	spec, err := m.getCLICommand("replay")
	if err == nil {
		t.Fatalf("expected error, got spec %+v", spec)
	}
	if want := "OA_REPLAY_SHIM"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q must name %s so the operator knows what to set", err.Error(), want)
	}
}

func TestGetCLICommandReplayUsesEnv(t *testing.T) {
	t.Setenv("OA_REPLAY_SHIM", "/tmp/test-shim")
	t.Setenv("OA_REPLAY_SHIM_ARGS", "-test.run=TestReplayAgentHelper")

	m := NewManager()
	spec, err := m.getCLICommand("replay")
	if err != nil {
		t.Fatalf("getCLICommand: %v", err)
	}
	if spec.ACP == nil {
		t.Fatal("replay must declare an ACP command")
	}
	if spec.ACP.Command != "/tmp/test-shim" {
		t.Errorf("cmd = %q", spec.ACP.Command)
	}
	if len(spec.ACP.Args) != 1 || spec.ACP.Args[0] != "-test.run=TestReplayAgentHelper" {
		t.Errorf("args = %v", spec.ACP.Args)
	}
}
