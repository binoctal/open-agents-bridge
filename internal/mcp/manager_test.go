package mcp

import (
	"path/filepath"
	"testing"
)

func TestManager_AddAndListServers(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	err := m.AddServer("filesystem", ServerConfig{
		Command: "npx",
		Args:    []string{"-y", "@modelcontextprotocol/server-filesystem"},
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	servers := m.ListServers()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers["filesystem"].Command != "npx" {
		t.Errorf("expected npx, got %s", servers["filesystem"].Command)
	}
}

func TestManager_RemoveServer(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	m.AddServer("test", ServerConfig{Command: "test-cmd", Enabled: true})
	m.RemoveServer("test")

	servers := m.ListServers()
	if len(servers) != 0 {
		t.Errorf("expected 0 servers after removal, got %d", len(servers))
	}
}

func TestManager_ToggleServer(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	m.AddServer("test", ServerConfig{Command: "cmd", Enabled: true})
	m.ToggleServer("test", false)

	servers := m.ListServers()
	if servers["test"].Enabled {
		t.Error("expected server to be disabled")
	}
}

func TestManager_GetEnabledServers(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	m.AddServer("on", ServerConfig{Command: "a", Enabled: true})
	m.AddServer("off", ServerConfig{Command: "b", Enabled: false})

	enabled := m.GetEnabledServers()
	if len(enabled) != 1 {
		t.Fatalf("expected 1 enabled, got %d", len(enabled))
	}
	if _, ok := enabled["on"]; !ok {
		t.Error("expected 'on' in enabled servers")
	}
}

func TestManager_GenerateClaudeConfig(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	m.AddServer("fs", ServerConfig{Command: "npx", Args: []string{"fs-server"}, Enabled: true})
	m.AddServer("off", ServerConfig{Command: "skip", Enabled: false})

	data, err := m.GenerateClaudeConfig()
	if err != nil {
		t.Fatal(err)
	}

	str := string(data)
	if len(str) == 0 {
		t.Error("expected non-empty config")
	}
	// Should include the enabled server
	if !contains(str, "fs-server") {
		t.Error("expected enabled server in config output")
	}
}

func TestManager_SyncFromRemote(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	remote := map[string]ServerConfig{
		"remote-srv": {Command: "remote-cmd", Enabled: true},
	}
	m.SyncFromRemote(remote)

	servers := m.ListServers()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server from remote, got %d", len(servers))
	}
	if servers["remote-srv"].Command != "remote-cmd" {
		t.Errorf("expected remote-cmd, got %s", servers["remote-srv"].Command)
	}
}

func TestManager_Persistence(t *testing.T) {
	dir := t.TempDir()
	m1 := NewManager(dir)
	m1.AddServer("persist", ServerConfig{Command: "saved", Enabled: true})

	// Load from same dir
	m2 := NewManager(dir)
	servers := m2.ListServers()
	if len(servers) != 1 {
		t.Fatalf("expected 1 server loaded from disk, got %d", len(servers))
	}
	if servers["persist"].Command != "saved" {
		t.Errorf("expected saved, got %s", servers["persist"].Command)
	}
}

func TestManager_Load_NonexistentDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no-such-dir")
	m := NewManager(dir)
	servers := m.ListServers()
	if len(servers) != 0 {
		t.Error("expected empty servers for nonexistent dir")
	}
}

func TestManager_ToJSON(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.AddServer("test", ServerConfig{Command: "cmd", Enabled: true})

	data, err := m.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty JSON")
	}
}

func TestValidateServerConfig(t *testing.T) {
	if err := ValidateServerConfig(ServerConfig{}); err == nil {
		t.Error("expected error for empty command")
	}
	if err := ValidateServerConfig(ServerConfig{Command: "valid"}); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
