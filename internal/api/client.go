package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/config"
)

type Client struct {
	baseURL     string
	deviceToken string
	httpClient  *http.Client
}

func NewClient(cfg *config.Config) *Client {
	// Derive API URL from WebSocket URL
	baseURL := cfg.ServerURL
	if len(baseURL) > 3 && baseURL[:3] == "wss" {
		baseURL = "https" + baseURL[3:]
	} else if len(baseURL) > 2 && baseURL[:2] == "ws" {
		baseURL = "http" + baseURL[2:]
	}
	// Remove /ws suffix if present
	if len(baseURL) > 3 && baseURL[len(baseURL)-3:] == "/ws" {
		baseURL = baseURL[:len(baseURL)-3]
	}

	return &Client{
		baseURL:     baseURL,
		deviceToken: cfg.DeviceToken,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) request(method, path string, body interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+c.deviceToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// Permission Rules

type PermissionRule struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	Tool    string `json:"tool"`
	Action  string `json:"action"`
}

func (c *Client) GetPermissionRules(project string) ([]PermissionRule, error) {
	path := "/api/bridge/permission-rules"
	if project != "" {
		path += "?project=" + project
	}

	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Rules []PermissionRule `json:"rules"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	return resp.Rules, nil
}

// Agent Config

type AgentConfig struct {
	SystemPrompt string     `json:"systemPrompt"`
	Steering     []Steering `json:"steering"`
	AllowedTools []string   `json:"allowedTools"`
	DeniedTools  []string   `json:"deniedTools"`
}

type Steering struct {
	Type string `json:"type"`
	Rule string `json:"rule"`
}

func (c *Client) GetAgentConfig(agentID string) (*AgentConfig, error) {
	data, err := c.request("GET", "/api/bridge/agents/"+agentID, nil)
	if err != nil {
		return nil, err
	}

	var cfg AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Session Reporting

type SessionReport struct {
	SessionID string `json:"sessionId"`
	CLIType   string `json:"cliType"`
	WorkDir   string `json:"workDir"`
	Status    string `json:"status"`
}

func (c *Client) ReportSession(report SessionReport) error {
	_, err := c.request("POST", "/api/bridge/sessions", report)
	return err
}

// Message Storage

type MessageReport struct {
	SessionID string                 `json:"sessionId"`
	Role      string                 `json:"role"`
	Content   string                 `json:"content"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

func (c *Client) StoreMessage(msg MessageReport) (string, error) {
	data, err := c.request("POST", "/api/bridge/messages", msg)
	if err != nil {
		return "", err
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}

	return resp.ID, nil
}
