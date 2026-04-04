package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Config represents a single device's configuration.
// Used by bridge.go at runtime.
type Config struct {
	UserID      string `json:"userId"`
	DeviceID    string `json:"deviceId"`
	DeviceToken string `json:"deviceToken"`
	ServerURL   string `json:"serverUrl"`
	PublicKey   string `json:"publicKey,omitempty"`
	PrivateKey  string `json:"privateKey,omitempty"`
	WebPubKey   string `json:"webPubKey,omitempty"`

	// v1.1: Device config synced from Web
	EnvVars     map[string]string `json:"envVars,omitempty"`
	CLIEnabled  map[string]bool   `json:"cliEnabled,omitempty"`
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
	ModelFallbacks []ModelFallback `json:"modelFallbacks,omitempty"`

	// v2.3: Security scanner
	ScannerEnabled *bool `json:"scannerEnabled,omitempty"` // nil = default (true)

	// v2.4: Environment setting (optional, auto-detected if not set)
	Environment string `json:"environment,omitempty"`

	// v2.5: Device name (key in the devices map)
	DeviceName string `json:"-"`

	// v2.6: I/O Logging for debugging and auditing
	IOLogging *IOLoggingConfig `json:"ioLogging,omitempty"`
}

// fileConfig is the top-level structure of ~/.open-agents-bridge/config.json
type fileConfig struct {
	Devices map[string]*Config `json:"devices"`
}

// GetEnvironment returns the environment setting.
func (c *Config) GetEnvironment() string {
	if c.Environment != "" {
		return c.Environment
	}
	if c.ServerURL == "" {
		return "unknown"
	}
	if strings.Contains(c.ServerURL, "staging") ||
		strings.Contains(c.ServerURL, "preview") ||
		strings.Contains(c.ServerURL, "-staging") {
		return "staging"
	}
	if strings.Contains(c.ServerURL, "localhost") ||
		strings.Contains(c.ServerURL, "127.0.0.1") {
		return "development"
	}
	return "production"
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
			return &fileConfig{Devices: make(map[string]*Config)}, nil
		}
		return nil, err
	}

	// Detect format: check if "devices" key exists
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	if _, ok := raw["devices"]; ok {
		// New unified format
		var fc fileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			return nil, err
		}
		if fc.Devices == nil {
			fc.Devices = make(map[string]*Config)
		}
		// Set DeviceName on each device
		for name, cfg := range fc.Devices {
			cfg.DeviceName = name
		}
		return &fc, nil
	}

	// Old flat format: migrate to new format
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	name := cfg.DeviceName
	if name == "" {
		name = "default"
	}
	cfg.DeviceName = name

	fc := &fileConfig{
		Devices: map[string]*Config{name: &cfg},
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
func initConfig(cfg *Config) {
	if cfg.EnvVars == nil {
		cfg.EnvVars = make(map[string]string)
	}
	if cfg.CLIEnabled == nil {
		cfg.CLIEnabled = map[string]bool{"kiro": true, "claude": true, "cline": true, "codex": true, "gemini": true}
	}
	if cfg.Permissions == nil {
		cfg.Permissions = map[string]bool{"fs_read": true, "fs_write": true, "execute_bash": true, "network": false}
	}
}

// ============================================
// Public API
// ============================================

// Save persists a device's config into the unified file.
// Uses cfg.DeviceName as the key.
func Save(cfg *Config) error {
	fc, err := loadFile()
	if err != nil {
		fc = &fileConfig{Devices: make(map[string]*Config)}
	}
	if fc.Devices == nil {
		fc.Devices = make(map[string]*Config)
	}

	name := cfg.DeviceName
	if name == "" {
		name = "default"
	}
	cfg.DeviceName = name

	fc.Devices[name] = cfg

	return saveFile(fc)
}

// LoadDevice loads a specific device's config.
func LoadDevice(name string) (*Config, error) {
	fc, err := loadFile()
	if err != nil {
		return nil, err
	}

	cfg, ok := fc.Devices[name]
	if !ok {
		return nil, fmt.Errorf("device '%s' not found", name)
	}

	cfg.DeviceName = name
	initConfig(cfg)
	return cfg, nil
}

// SaveDevice saves a device's config with an explicit name.
func SaveDevice(name string, cfg *Config) error {
	cfg.DeviceName = name
	return Save(cfg)
}

// DeleteDevice removes a device from the config file.
func DeleteDevice(name string) error {
	fc, err := loadFile()
	if err != nil {
		return err
	}

	delete(fc.Devices, name)

	return saveFile(fc)
}

// ListDevices returns all device names, sorted.
func ListDevices() ([]string, error) {
	fc, err := loadFile()
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	names := make([]string, 0, len(fc.Devices))
	for name := range fc.Devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// DeviceExists checks if a device config exists.
func DeviceExists(name string) bool {
	fc, err := loadFile()
	if err != nil {
		return false
	}
	_, ok := fc.Devices[name]
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
