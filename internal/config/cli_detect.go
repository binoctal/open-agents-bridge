package config

import (
	"os/exec"
	"sort"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// cliDetectMap maps CLI type names to the executable they need on PATH.
// Must stay in sync with session/manager.go:getCLICommand.
var cliDetectMap = map[string]string{
	"claude":     "claude",     // claude-pty mode needs the claude binary
	"claude-pty": "claude",     // same binary
	"qwen":       "qwen-code",
	"goose":      "goose",
	"gemini":     "gemini-cli",
	"kiro":       "kiro",
	"cline":      "cline",
	"codex":      "codex",
	"aider":      "aider",
}

// DetectInstalledCLIs checks which CLI tools are available on this machine.
// Returns a map of cliType -> installed (true/false).
func DetectInstalledCLIs() map[string]bool {
	result := make(map[string]bool, len(cliDetectMap))

	for cliType, binary := range cliDetectMap {
		_, err := exec.LookPath(binary)
		result[cliType] = err == nil
	}

	// Log summary
	var installed []string
	for cli, ok := range result {
		if ok {
			installed = append(installed, cli)
		}
	}
	sort.Strings(installed)
	logger.Info("[CLI Detect] Found %d installed CLIs: %v", len(installed), installed)

	return result
}

// HasNpx checks whether npx is available (needed for claude ACP mode).
func HasNpx() bool {
	_, err := exec.LookPath("npx")
	return err == nil
}

// DefaultFallbackChain returns built-in fallback mappings for common CLI types.
// These are used when the user has not configured custom ModelFallbacks.
func DefaultFallbackChain() []ModelFallback {
	return []ModelFallback{
		{CLIType: "claude", Fallback: "codex", OnError: "any"},
		{CLIType: "claude-pty", Fallback: "aider", OnError: "any"},
		{CLIType: "kiro", Fallback: "claude", OnError: "any"},
		{CLIType: "codex", Fallback: "claude", OnError: "any"},
		{CLIType: "gemini", Fallback: "claude", OnError: "any"},
		{CLIType: "qwen", Fallback: "claude", OnError: "any"},
		{CLIType: "goose", Fallback: "claude", OnError: "any"},
		{CLIType: "cline", Fallback: "codex", OnError: "any"},
		{CLIType: "aider", Fallback: "codex", OnError: "any"},
	}
}
