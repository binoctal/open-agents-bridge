package workflows

import (
	"os"
	"testing"
)

// New worktrees live under the bridge config dir (~/.open-agents-bridge);
// point HOME at a scratch dir so tests never touch the real one.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "oa-wf-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
