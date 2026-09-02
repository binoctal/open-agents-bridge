package config

import (
	"os"
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

func TestDetectInstalledCLIsReplayFollowsShimEnv(t *testing.T) {
	t.Setenv("OA_REPLAY_SHIM", "")
	if DetectInstalledCLIs()["replay"] {
		t.Error("replay must not be installed when OA_REPLAY_SHIM is unset")
	}

	shimPath := t.TempDir() + "/shim"
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OA_REPLAY_SHIM", shimPath)
	if !DetectInstalledCLIs()["replay"] {
		t.Error("replay must be installed when OA_REPLAY_SHIM points at an existing file")
	}

	// A dangling path is as good as no shim: reporting it installed would
	// let cliEnabled survive the sanitizer for a CLI that cannot start.
	t.Setenv("OA_REPLAY_SHIM", t.TempDir()+"/missing")
	if DetectInstalledCLIs()["replay"] {
		t.Error("replay must not be installed when OA_REPLAY_SHIM points at a missing file")
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
