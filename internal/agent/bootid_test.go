package agent

import (
	"regexp"
	"testing"
)

// NewBootID is the node-rebuilt signal the API diffs (design D8): it must be
// well-formed hex and change across calls, or a restart would look like the
// same process and live deployments would never be re-offered from R2.
func TestNewBootIDUniqueAndWellFormed(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewBootID()
		if !re.MatchString(id) {
			t.Fatalf("boot id %q is not 32 hex chars", id)
		}
		if seen[id] {
			t.Fatalf("boot id %q repeated", id)
		}
		seen[id] = true
	}
}
