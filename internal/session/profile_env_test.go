package session

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

func TestMergeProfileEnvSystemKeysWin(t *testing.T) {
	cfg := protocol.AdapterConfig{CustomEnv: map[string]string{"CLAUDECODE": "", "CLAUDE_CONFIG_DIR": "/sys"}}
	mergeProfileEnv(&cfg, map[string]string{"CLAUDE_CONFIG_DIR": "/evil", "ANTHROPIC_API_KEY": "k"}, "s1")
	if cfg.CustomEnv["CLAUDE_CONFIG_DIR"] != "/sys" {
		t.Fatal("system key must win over profile key")
	}
	if cfg.CustomEnv["ANTHROPIC_API_KEY"] != "k" {
		t.Fatal("profile key must be injected")
	}
}

func TestMergeProfileEnvNilMap(t *testing.T) {
	cfg := protocol.AdapterConfig{}
	mergeProfileEnv(&cfg, map[string]string{"A": "1"}, "s1")
	if cfg.CustomEnv["A"] != "1" {
		t.Fatal("nil CustomEnv must be allocated")
	}
	cfg2 := protocol.AdapterConfig{}
	mergeProfileEnv(&cfg2, nil, "s1")
	if cfg2.CustomEnv != nil {
		t.Fatal("no profile = untouched env")
	}
}
