package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Config represents a single machine's configuration.
// Used by bridge.go at runtime.
type Config struct {
	UserID      string `json:"userId"`
	MachineID    string `json:"machineId"`
	MachineToken string `json:"machineToken"`
	ServerURL   string `json:"serverUrl"`
	PublicKey   string `json:"publicKey,omitempty"`
	PrivateKey  string `json:"privateKey,omitempty"`
	WebPubKey   string `json:"webPubKey,omitempty"`

	// v1.1: Machine config synced from Web
	EnvVars     map[string]string `json:"envVars,omitempty"`
	CLIEnabled  map[string]bool   `json:"cliEnabled,omitempty"`
	CLIDetected map[string]bool   `json:"cliDetected,omitempty"` // auto-detected installed CLIs
	Permissions map[string]bool   `json:"permissions,omitempty"`

	// v1.1: Auto-approval rules
	Rules []AutoApprovalRule `json:"rules,omitempty"`

	// v1.2: Storage settings
	StorageType string    `json:"storageType,omitempty"` // saas, s3, local
	S3Config    *S3Config `json:"s3Config,omitempty"`

	// v1.3: Logging settings
	LogLevel string `json:"logLevel,omitempty"` // debug, info, warn, error (default: info)

	// v1.4: Synced prompts from Web
	Prompts interface{} `json:"prompts,omitempty"`

	// v2.2: Model fallback chain
	FallbackEnabled bool            `json:"fallbackEnabled,omitempty"`
	ModelFallbacks  []ModelFallback `json:"modelFallbacks,omitempty"`

	// v2.3: Security scanner
	ScannerEnabled *bool `json:"scannerEnabled,omitempty"` // nil = default (true)

	// v2.7: Command whitelist (user-configured extras merged with defaults)
	CommandWhitelist []string `json:"commandWhitelist,omitempty"`

	// v2.4: Environment setting (optional, auto-detected if not set)
	Environment string `json:"environment,omitempty"`

	// v2.5: Machine name (key in the machines map)
	MachineName string `json:"-"`

	// v2.6: I/O Logging for debugging and auditing
	IOLogging *IOLoggingConfig `json:"ioLogging,omitempty"`

	// preview-hosting-ux-parity: default ON. A pointer so an absent key is
	// distinct from explicit false — the old opt-in zero value was the
	// biggest silent switch (previews quietly never happened). Explicit
	// false keeps the zero-side-effects contract; read it ONLY through
	// PreviewBuildEffective, never the raw pointer.
	PreviewBuildEnabled *bool `json:"previewBuildEnabled,omitempty"`
}

// PreviewBuildEffective resolves the three-state preview toggle: unset = ON
// (the preview-hosting-ux-parity default), explicit false honored. The old
// opt-out users who wrote `false` are unaffected; everyone else stops
// needing to know the key exists.
func (c *Config) PreviewBuildEffective() bool {
	return c.PreviewBuildEnabled == nil || *c.PreviewBuildEnabled
}

// fileConfig is the top-level structure of ~/.open-agents-bridge/config.json
type fileConfig struct {
	Machines map[string]*Config `json:"machines"`
}

// GetEnvironment returns the environment setting.
func (c *Config) GetEnvironment() string {
	if c.Environment != "" {
		return c.Environment
	}
	if c.ServerURL == "" {
		return "unknown"
	}
	return EnvironmentForURL(c.ServerURL)
}

// officialHosts maps the official control-plane hosts to their environment.
// Detection matches the parsed host exactly: a substring test let
// "https://evil.com/?staging" read as staging and anything else as
// production.
var officialHosts = map[string]string{
	"api.openagents.top":         "production",
	"api-staging.openagents.top": "staging",
}

// EnvironmentForURL classifies a server URL: an official host maps to its
// environment, a loopback host to "development", and anything else to
// "custom" (a self-hosted or unofficial control plane).
func EnvironmentForURL(raw string) string {
	host := urlHost(raw)
	if env, ok := officialHosts[host]; ok {
		return env
	}
	if isLoopbackHost(host) {
		return "development"
	}
	return "custom"
}

// IsLoopbackURL reports whether the URL's host is this machine (localhost or
// a loopback IP), so "https://localhost.evil.com" does not qualify.
func IsLoopbackURL(raw string) bool {
	return isLoopbackHost(urlHost(raw))
}

func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// GetEffectiveFallbacks returns custom ModelFallbacks if fallback is enabled
// and configured. Returns empty slice when fallback is disabled (default).
func (c *Config) GetEffectiveFallbacks() []ModelFallback {
	if !c.FallbackEnabled {
		return nil
	}
	if len(c.ModelFallbacks) > 0 {
		return c.ModelFallbacks
	}
	defaults := DefaultFallbackChain()
	if len(c.CLIDetected) == 0 {
		return defaults
	}
	var result []ModelFallback
	for _, fb := range defaults {
		if c.CLIDetected[fb.Fallback] {
			result = append(result, fb)
		}
	}
	return result
}

type ModelFallback struct {
	CLIType  string `json:"cliType"`
	Fallback string `json:"fallback"`
	OnError  string `json:"onError,omitempty"`
}

type AutoApprovalRule struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	Tool    string `json:"tool"`
	Action  string `json:"action"`
}

type S3Config struct {
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	Endpoint  string `json:"endpoint,omitempty"`
}

type IOLoggingConfig struct {
	Enabled    bool     `json:"enabled"`
	Types      []string `json:"types"`
	MaxSizeMB  int      `json:"maxSizeMB,omitempty"`
	MaxBackups int      `json:"maxBackups,omitempty"`
}

// ============================================
// Path helpers
// ============================================

func ConfigDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "open-agents-bridge")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".open-agents-bridge")
	}
}

func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

// ============================================
// File I/O
// ============================================

// loadFile reads and parses the unified config file.
// Handles migration from the old flat format automatically.
func loadFile() (*fileConfig, error) {
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &fileConfig{Machines: make(map[string]*Config)}, nil
		}
		return nil, err
	}

	// engine-explicit-auth (D5): the file carries envVars that may hold
	// credentials; tighten legacy copies that predate the 0600 write policy.
	// Best-effort — a chmod failure never blocks startup.
	_ = os.Chmod(ConfigPath(), 0600)

	// Detect format: check if "machines" key exists
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	if _, ok := raw["machines"]; ok {
		// New unified format
		var fc fileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			return nil, err
		}
		if fc.Machines == nil {
			fc.Machines = make(map[string]*Config)
		}
		// Set MachineName on each machine
		for name, cfg := range fc.Machines {
			cfg.MachineName = name
		}
		return &fc, nil
	}

	// Old flat format: migrate to new format
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	name := cfg.MachineName
	if name == "" {
		name = "default"
	}
	cfg.MachineName = name

	fc := &fileConfig{
		Machines: map[string]*Config{name: &cfg},
	}

	// Auto-save in new format
	saveFile(fc)

	return fc, nil
}

// saveFile writes the unified config file.
func saveFile(fc *fileConfig) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(ConfigPath(), data, 0600)
}

// initConfig initializes default maps on a Config.
// CLIEnabled defaults are set from auto-detected installed CLIs,
// not hardcoded to true for all types.
func initConfig(cfg *Config) {
	if cfg.EnvVars == nil {
		cfg.EnvVars = make(map[string]string)
	}
	if cfg.CLIEnabled == nil {
		cfg.CLIEnabled = make(map[string]bool)
	}
	// If cliDetected was auto-detected, use it to initialize cliEnabled
	// so only actually installed CLIs are enabled by default.
	if len(cfg.CLIDetected) > 0 && len(cfg.CLIEnabled) == 0 {
		for cli, installed := range cfg.CLIDetected {
			cfg.CLIEnabled[cli] = installed
		}
	}
	if cfg.Permissions == nil {
		cfg.Permissions = map[string]bool{"fs_read": true, "fs_write": true, "execute_bash": true, "network": false}
	}
}

// ============================================
// Public API
// ============================================

// Save persists a machine's config into the unified file.
// Uses cfg.MachineName as the key.
func Save(cfg *Config) error {
	fc, err := loadFile()
	if err != nil {
		fc = &fileConfig{Machines: make(map[string]*Config)}
	}
	if fc.Machines == nil {
		fc.Machines = make(map[string]*Config)
	}

	name := cfg.MachineName
	if name == "" {
		name = "default"
	}
	cfg.MachineName = name

	fc.Machines[name] = cfg

	return saveFile(fc)
}

// LoadMachine loads a specific machine's config.
func LoadMachine(name string) (*Config, error) {
	fc, err := loadFile()
	if err != nil {
		return nil, err
	}

	cfg, ok := fc.Machines[name]
	if !ok {
		return nil, fmt.Errorf("machine '%s' not found", name)
	}

	cfg.MachineName = name
	initConfig(cfg)
	return cfg, nil
}

// SaveMachine saves a machine's config with an explicit name.
func SaveMachine(name string, cfg *Config) error {
	cfg.MachineName = name
	return Save(cfg)
}

// DeleteMachine removes a machine from the config file.
func DeleteMachine(name string) error {
	fc, err := loadFile()
	if err != nil {
		return err
	}

	delete(fc.Machines, name)

	return saveFile(fc)
}

// ListMachines returns all machine names, sorted.
func ListMachines() ([]string, error) {
	fc, err := loadFile()
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	names := make([]string, 0, len(fc.Machines))
	for name := range fc.Machines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// MachineExists checks if a machine config exists.
func MachineExists(name string) bool {
	fc, err := loadFile()
	if err != nil {
		return false
	}
	_, ok := fc.Machines[name]
	return ok
}

// SaveScannerRules persists custom scanner rules to a separate file.
func SaveScannerRules(rules interface{}) error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	wrapper := map[string]interface{}{"customRules": rules}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "scanner-rules.json"), data, 0600)
}
