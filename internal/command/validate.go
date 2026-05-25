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

// IsAllowed checks if a base command is in the default whitelist.
func IsAllowed(baseCmd string) bool {
	return allowedCommands[baseCmd]
}

// relaxedBlacklist are the metacharacters still banned in relaxed (ACP) mode.
// Pipe | and logical AND && are allowed; everything else is blocked.
// Note: single & (background execution) is blocked because it acts as an
// implicit semicolon, allowing command chaining that bypasses per-segment
// validation.  && is safe because both segments are independently validated.
var relaxedBlacklist = []struct {
	char             string
	name             string
	allowWhenDoubled bool // if true, skip when the char appears twice (e.g. &&)
}{
	{"$(", "command_substitution", false},
	{"`", "backtick", false},
	{">>", "redirect_append", false},
	{">", "redirect_write", false},
	{"<", "redirect_read", false},
	{";", "semicolon", false},
	{"&", "ampersand", true}, // single & blocked (background), && allowed (logical AND)
}

// DefaultWhitelistCount returns the number of commands in the default whitelist.
func DefaultWhitelistCount() int {
	return len(allowedCommands)
}

// BuildEffectiveWhitelist merges the default whitelist with user-configured extras.
func BuildEffectiveWhitelist(extras []string) map[string]bool {
	effective := make(map[string]bool, len(allowedCommands)+len(extras))
	for cmd := range allowedCommands {
		effective[cmd] = true
	}
	for _, cmd := range extras {
		if cmd = strings.TrimSpace(cmd); cmd != "" {
			effective[cmd] = true
		}
	}
	return effective
}

// effectiveWhitelist is the runtime whitelist used by ValidateCommandRelaxed.
// Initialized to the default whitelist; updated via SetEffectiveWhitelist.
var effectiveWhitelist = BuildEffectiveWhitelist(nil)

// SetEffectiveWhitelist replaces the runtime effective whitelist.
func SetEffectiveWhitelist(wl map[string]bool) {
	effectiveWhitelist = wl
}

// ValidateCommandRelaxed performs ACP-mode validation:
//  1. Check for dangerous metacharacters (pipe is allowed, but $(), >, <, ;, & are blocked)
//  2. Split by pipe and validate each segment's base command against the effective whitelist
func ValidateCommandRelaxed(cmd string) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("empty command")
	}

	// Step 1: Check dangerous metacharacters (pipe NOT included)
	if has, name := hasRelaxedMetacharacters(cmd); has {
		return fmt.Errorf("shell metacharacter detected: %s", name)
	}

	// Step 2: Split by pipe and &&, validate each segment
	segments := splitByOperators(cmd)
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		baseCmd := ExtractBaseCommand(seg)
		if baseCmd == "" {
			return fmt.Errorf("could not extract base command")
		}
		if !effectiveWhitelist[baseCmd] {
			return fmt.Errorf("command not allowed: %s", baseCmd)
		}
	}

	return nil
}

// hasRelaxedMetacharacters checks for metacharacters banned in relaxed mode (pipe allowed).
func hasRelaxedMetacharacters(cmd string) (bool, string) {
	inSingle := false
	inDouble := false

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]

		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if inSingle || inDouble {
			continue
		}
		if ch == '\\' && i+1 < len(cmd) {
			i++
			continue
		}

		remaining := cmd[i:]
		for _, mc := range relaxedBlacklist {
			if strings.HasPrefix(remaining, mc.char) {
				// Allow doubled form (e.g. "&&") for chars with allowWhenDoubled=true.
				// Only the single form (e.g. "&" for background execution) is dangerous.
				if mc.allowWhenDoubled && strings.HasPrefix(remaining, mc.char+mc.char) {
					i++ // skip both chars (outer loop increments past the second)
					break
				}
				return true, mc.name
			}
		}
	}

	return false, ""
}

// splitByPipe splits a command by pipe | outside of quotes.
func splitByPipe(cmd string) []string {
	return splitByOperators(cmd)
}

// splitByOperators splits a command by pipe | and logical AND && outside of quotes.
func splitByOperators(cmd string) []string {
	var segments []string
	var current strings.Builder
	inSingle := false
	inDouble := false

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]

		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			current.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			current.WriteByte(ch)
			continue
		}
		if ch == '\\' && i+1 < len(cmd) {
			current.WriteByte(ch)
			i++
			current.WriteByte(cmd[i])
			continue
		}

		if !inSingle && !inDouble {
			// Check for pipe |
			if ch == '|' {
				segments = append(segments, current.String())
				current.Reset()
				continue
			}
			// Check for logical AND &&
			if ch == '&' && i+1 < len(cmd) && cmd[i+1] == '&' {
				segments = append(segments, current.String())
				current.Reset()
				i++ // skip second &
				continue
			}
		}

		current.WriteByte(ch)
	}
	segments = append(segments, current.String())

	return segments
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
