package session

import "testing"

func TestClaudeNpxNeverPromptsForInstall(t *testing.T) {
	_, args, err := NewManager().getCLICommand("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) == 0 || args[0] != "--yes" {
		t.Fatalf("args = %v, want --yes first so npx cannot block on its install prompt", args)
	}
}
