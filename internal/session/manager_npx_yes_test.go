package session

import (
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/agents"
)

func TestClaudeNpxNeverPromptsForInstall(t *testing.T) {
	spec, err := NewManager().getCLICommand("claude")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ACP == nil {
		t.Fatal("claude must declare an ACP command")
	}
	if len(spec.ACP.Args) == 0 || spec.ACP.Args[0] != "--yes" {
		t.Fatalf("args = %v, want --yes first so npx cannot block on its install prompt", spec.ACP.Args)
	}
	// The 2026-11-07 incident: a bare --yes re-resolved registry latest on
	// every spawn (0.91.0 -> 0.101.0 drift) and cold install exceeded the
	// handshake timeout. The spec must be pinned exactly.
	want := agents.ClaudeAgentSpec()
	if got := strings.Join(spec.ACP.Args, " "); !strings.Contains(got, want) {
		t.Fatalf("claude ACP args = %q, want pinned spec %q", got, want)
	}
}

// Known ACP-only wrappers must declare NO PTY equivalent: driving a JSON-RPC
// server with keystrokes answers every user message with -32700 (zombie
// session). ACP failure for these types must fail loud instead.
func TestACPOnlyCLITypesHaveNoPTYFallback(t *testing.T) {
	m := NewManager()
	for _, cliType := range []string{"claude", "qwen", "goose", "gemini", "opencode", "dsh", "cline", "codex"} {
		spec, err := m.getCLICommand(cliType)
		if err != nil {
			t.Fatalf("%s: %v", cliType, err)
		}
		if spec.ACP == nil {
			t.Errorf("%s: expected an ACP command", cliType)
		}
		if spec.PTY != nil {
			t.Errorf("%s: must not declare a PTY equivalent (PTY would drive an ACP-only server)", cliType)
		}
	}
}

// PTY-only types must skip ACP auto-detection entirely.
func TestPTYOnlyCLITypesSkipACP(t *testing.T) {
	m := NewManager()
	for _, cliType := range []string{"claude-pty", "aider"} {
		spec, err := m.getCLICommand(cliType)
		if err != nil {
			t.Fatalf("%s: %v", cliType, err)
		}
		if spec.ACP != nil {
			t.Errorf("%s: must not declare an ACP command", cliType)
		}
		if spec.PTY == nil {
			t.Errorf("%s: expected a PTY command", cliType)
		}
	}
}

// Unknown/custom commands keep both paths: PTY is a legitimate guess when the
// command may be an interactive REPL.
func TestUnknownCLITypeKeepsAutoDetect(t *testing.T) {
	spec, err := NewManager().getCLICommand("some-custom-repl")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ACP == nil || spec.PTY == nil {
		t.Fatalf("unknown type must keep auto-detect, got %+v", spec)
	}
}
