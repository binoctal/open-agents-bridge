package scanner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Scanner orchestrates all scanner plugins.
type Scanner struct {
	mu       sync.RWMutex
	enabled  bool
	plugins  []ScannerPlugin
	disabled map[string]bool // disabled plugin names
}

// New creates a Scanner with all built-in plugins enabled.
func New() *Scanner {
	s := &Scanner{
		enabled:  true,
		disabled: make(map[string]bool),
		plugins: []ScannerPlugin{
			&SecretsScanner{},
			&PIIScanner{},
			&CodeShieldScanner{},
			&DangerousCmdScanner{},
			&PathScanner{},
		},
	}
	return s
}

func (s *Scanner) SetEnabled(enabled bool) {
	s.mu.Lock()
	s.enabled = enabled
	s.mu.Unlock()
}

func (s *Scanner) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// SetPluginEnabled enables/disables a specific scanner plugin by name.
func (s *Scanner) SetPluginEnabled(name string, enabled bool) {
	s.mu.Lock()
	if enabled {
		delete(s.disabled, name)
	} else {
		s.disabled[name] = true
	}
	s.mu.Unlock()
}

// LoadCustomRules loads user-defined rules from a JSON file and adds as plugin.
func (s *Scanner) LoadCustomRules(configDir string) {
	path := filepath.Join(configDir, "scanner-rules.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return // no custom rules file, that's fine
	}
	var cfg struct {
		CustomRules []CustomRuleDef `json:"customRules"`
	}
	if json.Unmarshal(data, &cfg) != nil || len(cfg.CustomRules) == 0 {
		return
	}
	s.mu.Lock()
	s.plugins = append(s.plugins, NewCustomRuleScanner(cfg.CustomRules))
	s.mu.Unlock()
}

// Scan runs all enabled plugins against the text.
func (s *Scanner) Scan(text string) []Alert {
	return s.ScanWithDirection(text, DirOutput)
}

// ScanWithDirection runs all enabled plugins with a specific direction.
func (s *Scanner) ScanWithDirection(text string, dir Direction) []Alert {
	s.mu.RLock()
	if !s.enabled || len(text) == 0 {
		s.mu.RUnlock()
		return nil
	}
	plugins := make([]ScannerPlugin, 0, len(s.plugins))
	for _, p := range s.plugins {
		if !s.disabled[p.Name()] {
			plugins = append(plugins, p)
		}
	}
	s.mu.RUnlock()

	var alerts []Alert
	for _, p := range plugins {
		if a := p.Scan(text, dir); len(a) > 0 {
			alerts = append(alerts, a...)
		}
	}
	return alerts
}

// ReplaceCustomRules hot-reloads custom rules from web push.
func (s *Scanner) ReplaceCustomRules(defs []CustomRuleDef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Remove existing custom scanner
	filtered := s.plugins[:0]
	for _, p := range s.plugins {
		if p.Name() != "custom" {
			filtered = append(filtered, p)
		}
	}
	s.plugins = filtered
	if len(defs) > 0 {
		s.plugins = append(s.plugins, NewCustomRuleScanner(defs))
	}
}

// PluginNames returns names of all registered plugins and their enabled status.
func (s *Scanner) PluginNames() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]bool, len(s.plugins))
	for _, p := range s.plugins {
		result[p.Name()] = !s.disabled[p.Name()]
	}
	return result
}
