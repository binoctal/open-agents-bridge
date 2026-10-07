package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

func TestConfigStruct(t *testing.T) {
	cfg := &config.Config{
		UserID:      "user_123",
		MachineID:    "machine_456",
		MachineToken: "token_789",
		ServerURL:   "wss://test.example.com",
		PublicKey:   "pubkey_base64",
		PrivateKey:  "privkey_base64",
	}

	// Test JSON marshaling
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var loaded config.Config
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if loaded.UserID != cfg.UserID {
		t.Errorf("UserID = %s, want %s", loaded.UserID, cfg.UserID)
	}
	if loaded.MachineID != cfg.MachineID {
		t.Errorf("MachineID = %s, want %s", loaded.MachineID, cfg.MachineID)
	}
	if loaded.ServerURL != cfg.ServerURL {
		t.Errorf("ServerURL = %s, want %s", loaded.ServerURL, cfg.ServerURL)
	}
}

func TestConfigDir(t *testing.T) {
	dir := config.ConfigDir()
	if dir == "" {
		t.Error("ConfigDir returned empty string")
	}
}

func TestConfigPath(t *testing.T) {
	path := config.ConfigPath()
	if filepath.Base(path) != "config.json" {
		t.Errorf("ConfigPath = %s, want config.json as filename", path)
	}
}

func TestConfigLoadNotExist(t *testing.T) {
	// Temporarily change HOME to non-existent dir
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", "/nonexistent/path/that/does/not/exist")
	defer os.Setenv("HOME", origHome)

	_, err := config.LoadMachine("default")
	if err == nil {
		t.Error("Expected error when config doesn't exist")
	}
}
