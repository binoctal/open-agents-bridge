package config

import (
	"testing"
)

func TestDetectInstalledCLIs(t *testing.T) {
	result := DetectInstalledCLIs()

	if len(result) != len(cliDetectMap) {
		t.Errorf("expected %d entries, got %d", len(cliDetectMap), len(result))
	}

	// Each key in cliDetectMap should have a result
	for cliType := range cliDetectMap {
		if _, ok := result[cliType]; !ok {
			t.Errorf("missing result for %s", cliType)
		}
	}
}

func TestDefaultFallbackChain(t *testing.T) {
	chain := DefaultFallbackChain()

	if len(chain) == 0 {
		t.Error("expected non-empty fallback chain")
	}

	// Verify each entry has required fields
	for _, fb := range chain {
		if fb.CLIType == "" {
			t.Error("expected non-empty CLIType")
		}
		if fb.Fallback == "" {
			t.Error("expected non-empty Fallback")
		}
		if fb.OnError == "" {
			t.Error("expected non-empty OnError")
		}
	}

	// Check specific mapping exists
	found := false
	for _, fb := range chain {
		if fb.CLIType == "claude" && fb.Fallback == "codex" {
			found = true
		}
	}
	if !found {
		t.Error("expected claude->codex fallback")
	}
}
