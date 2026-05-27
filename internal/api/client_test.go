package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/config"
)

func TestNewClient_WSSConversion(t *testing.T) {
	cfg := &config.Config{ServerURL: "wss://api.example.com/ws"}
	c := NewClient(cfg)
	if c.baseURL != "https://api.example.com" {
		t.Errorf("expected https://api.example.com, got %s", c.baseURL)
	}
}

func TestNewClient_WSConversion(t *testing.T) {
	cfg := &config.Config{ServerURL: "ws://localhost:8787/ws"}
	c := NewClient(cfg)
	if c.baseURL != "http://localhost:8787" {
		t.Errorf("expected http://localhost:8787, got %s", c.baseURL)
	}
}

func TestNewClient_WSSuffixStripped(t *testing.T) {
	cfg := &config.Config{ServerURL: "https://api.example.com/ws", DeviceToken: "tok123"}
	c := NewClient(cfg)
	if c.baseURL != "https://api.example.com" {
		t.Errorf("expected https://api.example.com, got %s", c.baseURL)
	}
}

func TestClient_GetPermissionRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bridge/permission-rules" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"rules": []map[string]string{
				{"id": "r1", "pattern": "*", "tool": "fs_read", "action": "auto-approve"},
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	rules, err := c.GetPermissionRules("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].ID != "r1" {
		t.Errorf("expected r1, got %s", rules[0].ID)
	}
}

func TestClient_GetAgentConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(AgentConfig{
			SystemPrompt: "You are helpful",
			AllowedTools: []string{"fs_read", "fs_write"},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok"}
	c := NewClient(cfg)

	agentCfg, err := c.GetAgentConfig("agent1")
	if err != nil {
		t.Fatal(err)
	}
	if agentCfg.SystemPrompt != "You are helpful" {
		t.Errorf("unexpected system prompt: %s", agentCfg.SystemPrompt)
	}
}

func TestClient_ReportSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok"}
	c := NewClient(cfg)

	err := c.ReportSession(SessionReport{SessionID: "s1", CLIType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_ApiError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "bad"}
	c := NewClient(cfg)

	_, err := c.GetPermissionRules("")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}
