package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// identityEnvKeys are the env keys that give a claude process an identity of
// its own (API key / token), making any login state irrelevant.
var identityEnvKeys = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// hostClaudeConfigDir is where plain `claude` keeps its login: the bridge
// process's CLAUDE_CONFIG_DIR, else ~/.claude. The bool reports whether the
// default (~/.claude) is in use, in which case the variable must be UNSET for
// the engine (claude then also finds ~/.claude.json next to it).
func hostClaudeConfigDir() (dir string, isDefault bool) {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v, false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", true
	}
	return filepath.Join(home, ".claude"), true
}

func hasIdentityEnv(env map[string]string) bool {
	for _, k := range identityEnvKeys {
		if env[k] != "" {
			return true
		}
	}
	return false
}

// projectSettingsHasIdentity reports whether workDir's .claude/settings(.local).json
// env block carries an identity key.
func projectSettingsHasIdentity(workDir string) bool {
	for _, name := range []string{"settings.json", "settings.local.json"} {
		raw, err := os.ReadFile(filepath.Join(workDir, ".claude", name))
		if err != nil {
			continue
		}
		var s struct {
			Env map[string]any `json:"env"`
		}
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		for _, k := range identityEnvKeys {
			if v, ok := s.Env[k].(string); ok && v != "" {
				return true
			}
		}
	}
	return false
}

// applyHostIdentityFallback implements the global-identity fallback: when no
// identity layer (project settings env, bridge env, isolated-dir OAuth) exists
// and the host has a login, the engine uses the host config dir LIVE — never a
// copy, so the main login's refresh-token rotation cannot orphan it. Returns
// whether the fallback was applied. Call after mergeProfileEnv.
func applyHostIdentityFallback(config *protocol.AdapterConfig, workDir, isolatedDir string) bool {
	if hasIdentityEnv(config.CustomEnv) || projectSettingsHasIdentity(workDir) {
		return false
	}
	if CheckCredentialHealth(isolatedDir, time.Now()) != CredentialMissing {
		return false
	}
	hostDir, isDefault := hostClaudeConfigDir()
	if hostDir == "" || !hostLoginUsable(hostDir) {
		return false
	}
	if isDefault {
		config.CustomEnv["CLAUDE_CONFIG_DIR"] = "" // empty = unset for the child
	} else {
		config.CustomEnv["CLAUDE_CONFIG_DIR"] = hostDir
	}
	logger.Info("[%s] no engine identity configured; claude uses host login at %s", logger.ModSession, hostDir)
	return true
}

// hostLoginUsable: the host has a login whose refresh window has not ended.
// Read-only — a dead host file is left for the user to deal with.
func hostLoginUsable(dir string) bool {
	h := checkCredentialHealth(dir, time.Now(), false)
	return h == CredentialHealthy || h == CredentialExpiringSoon
}
