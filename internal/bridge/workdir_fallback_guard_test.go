package bridge

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// workspace-checkout guard: session entry points must not default their
// workDir to the launch directory. The only tolerated "." fallbacks are the
// legacy task-dispatch default (old API without projectPath) and the legacy
// worktree manager; anything new must go through rejectMissingProject.
func TestNoLaunchDirWorkDirFallbackInSessionEntries(t *testing.T) {
	src, err := os.ReadFile("bridge.go")
	if err != nil {
		t.Fatal(err)
	}
	// workDir = "." / sessions.Create("claude", ".") style fallbacks.
	re := regexp.MustCompile(`(?m)^\s*(workDir\s*=\s*"\."|.*sessions\.Create\([^)]*,\s*"\."\))`)
	var hits []string
	for _, line := range strings.Split(string(src), "\n") {
		if re.MatchString(line) {
			hits = append(hits, strings.TrimSpace(line))
		}
	}
	if len(hits) != 0 {
		t.Fatalf("launch-dir workDir fallback reintroduced (use rejectMissingProject): %q", hits)
	}
}
