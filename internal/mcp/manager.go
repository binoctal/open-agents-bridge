package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// ServerConfig represents an MCP server configuration
type ServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Enabled bool              `json:"enabled"`
}

// Config holds all MCP server configurations
type Config struct {
	Servers map[string]ServerConfig `json:"mcpServers"`
}

// Manager manages MCP server configurations for CLI tools
type Manager struct {
	configDir string
	config    Config
	mu        sync.RWMutex
}

// NewManager creates a new MCP config manager
func NewManager(configDir string) *Manager {
	m := &Manager{
		configDir: configDir,
		config:    Config{Servers: make(map[string]ServerConfig)},
	}
	m.Load()
	return m
}

func (m *Manager) configPath() string {
	return filepath.Join(m.configDir, "mcp-servers.json")
}

// Load reads the MCP config from disk
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	return json.Unmarshal(data, &m.config)
}

// Save writes the MCP config to disk
func (m *Manager) Save() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if err := os.MkdirAll(m.configDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(m.config, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(m.configPath(), data, 0644)
}

// ListServers returns all configured MCP servers
func (m *Manager) ListServers() map[string]ServerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]ServerConfig, len(m.config.Servers))
	for k, v := range m.config.Servers {
		result[k] = v
	}
	return result
}

// AddServer adds or updates an MCP server configuration
func (m *Manager) AddServer(name string, server ServerConfig) error {
	m.mu.Lock()
	m.config.Servers[name] = server
	m.mu.Unlock()

	logger.Info("[MCP] Server added/updated: %s (command: %s)", name, server.Command)
	return m.Save()
}

// RemoveServer removes an MCP server configuration
func (m *Manager) RemoveServer(name string) error {
	m.mu.Lock()
	delete(m.config.Servers, name)
	m.mu.Unlock()

	logger.Info("[MCP] Server removed: %s", name)
	return m.Save()
}

// ToggleServer enables or disables an MCP server
func (m *Manager) ToggleServer(name string, enabled bool) error {
	m.mu.Lock()
	if s, ok := m.config.Servers[name]; ok {
		s.Enabled = enabled
		m.config.Servers[name] = s
	}
	m.mu.Unlock()

	return m.Save()
}

// GetEnabledServers returns only enabled servers
func (m *Manager) GetEnabledServers() map[string]ServerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]ServerConfig)
	for k, v := range m.config.Servers {
		if v.Enabled {
			result[k] = v
		}
	}
	return result
}

// GenerateClaudeConfig generates claude_desktop_config.json format
func (m *Manager) GenerateClaudeConfig() ([]byte, error) {
	servers := m.GetEnabledServers()
	config := map[string]interface{}{
		"mcpServers": servers,
	}
	return json.MarshalIndent(config, "", "  ")
}

// SyncFromRemote updates local config from remote (Web dashboard) config
func (m *Manager) SyncFromRemote(remoteConfig map[string]ServerConfig) error {
	m.mu.Lock()
	m.config.Servers = remoteConfig
	m.mu.Unlock()

	logger.Info("[MCP] Synced %d servers from remote", len(remoteConfig))
	return m.Save()
}

// ToJSON returns the config as JSON bytes
func (m *Manager) ToJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return json.Marshal(m.config)
}

// Validate checks if a server config is valid
func ValidateServerConfig(s ServerConfig) error {
	if s.Command == "" {
		return fmt.Errorf("command is required")
	}
	return nil
}
