package command

import (
	"fmt"
	"path/filepath"
	"strings"
)

// allowedCommands is the whitelist of base commands that can be executed.
var allowedCommands = map[string]bool{
	// File listing & navigation
	"ls": true, "cat": true, "pwd": true, "head": true, "tail": true,
	"wc": true, "sort": true, "uniq": true, "diff": true,
	// Search
	"grep": true, "find": true, "rg": true, "fd": true,
	// File operations
	"mkdir": true, "touch": true, "cp": true, "mv": true,
	// Text processing
	"echo": true, "sed": true, "awk": true, "tr": true, "cut": true,
	"tee": true, "xargs": true,
	// Version control
	"git": true,
	// Node.js
	"npm": true, "npx": true, "node": true, "yarn": true, "pnpm": true,
	"bun": true,
	// Go
	"go": true,
	// Build tools
	"make": true, "cargo": true, "bazel": true, "gradle": true,
	// Python
	"python3": true, "python": true, "pip": true, "pip3": true,
	"uv": true, "poetry": true,
	// Container
	"docker": true, "kubectl": true, "helm": true, "terraform": true,
	// Data formats
	"jq": true, "yq": true,
	// Archiving
	"tar": true, "gzip": true, "gunzip": true, "zip": true, "unzip": true,
	// Network (read-only usage)
	"curl": true, "wget": true,
	// File viewing
	"bat": true, "eza": true, "less": true, "more": true,
	// Misc dev tools
	"which": true, "env": true, "printenv": true, "date": true,
	"uname": true, "df": true, "du": true, "free": true,
	"test": true, "true": true, "false": true, "expr": true,
	"basename": true, "dirname": true, "realpath": true,
	"sha256sum": true, "md5sum": true, "base64": true,
}

// shellMetacharacters are characters that enable command chaining,
// piping, redirection, and substitution — all bypass vectors.
var shellMetacharacters = []struct {
	char string
	name string
}{
	{"|", "pipe"},
	{";", "semicolon"},
	{"&", "ampersand"},
	{"$(", "command_substitution"},
	{"`", "backtick"},
	{">>", "redirect_append"},
	{">", "redirect_write"},
	{"<", "redirect_read"},
}

// HasShellMetacharacters checks if the command contains shell metacharacters
// outside of single or double quotes. Returns the name of the first metacharacter found.
func HasShellMetacharacters(cmd string) (bool, string) {
	// Scan character by character, tracking quote state
	inSingle := false
	inDouble := false

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]

		// Handle quote toggling
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}

		// Skip characters inside quotes
		if inSingle || inDouble {
			continue
		}

		// Check for escape character
		if ch == '\\' && i+1 < len(cmd) {
			i++ // skip escaped char
			continue
		}

		// Check multi-char metacharacters first
		remaining := cmd[i:]
		for _, mc := range shellMetacharacters {
			if strings.HasPrefix(remaining, mc.char) {
				return true, mc.name
			}
		}
	}

	return false, ""
}

// ExtractBaseCommand extracts the base command name from a command string.
// For absolute paths like /usr/bin/git, it returns "git".
func ExtractBaseCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}

	// Split on whitespace to get the first token
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return ""
	}

	base := parts[0]
	// Handle absolute/relative paths: extract just the binary name
	return filepath.Base(base)
}

// IsAllowed checks if a base command is in the whitelist.
func IsAllowed(baseCmd string) bool {
	return allowedCommands[baseCmd]
}

// ValidateCommand performs full command validation:
// 1. Check for shell metacharacters (outside quotes)
// 2. Extract the base command
// 3. Verify it's in the whitelist
func ValidateCommand(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("empty command")
	}

	// Step 1: Check for shell metacharacters
	if has, name := HasShellMetacharacters(cmd); has {
		return fmt.Errorf("shell metacharacter detected: %s", name)
	}

	// Step 2: Extract base command
	baseCmd := ExtractBaseCommand(cmd)
	if baseCmd == "" {
		return fmt.Errorf("could not extract base command")
	}

	// Step 3: Check whitelist
	if !IsAllowed(baseCmd) {
		return fmt.Errorf("command not allowed: %s", baseCmd)
	}

	return nil
}
