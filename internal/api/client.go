package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
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

// Scanner Rules

// ScannerRule is an organization-wide scanner rule managed in the admin panel.
// The field names match scanner.CustomRuleDef so the two sets can be merged
// without a translation step; the conversion still happens in the bridge, to
// keep this transport package free of a dependency on the scanner.
type ScannerRule struct {
	ID       string `json:"id"`
	Pattern  string `json:"pattern"`
	Category string `json:"category"`
	Level    string `json:"level"`
	Title    string `json:"title"`
	Desc     string `json:"desc"`
}

func (c *Client) GetScannerRules() ([]ScannerRule, error) {
	data, err := c.request("GET", "/api/bridge/scanner-rules", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Rules []ScannerRule `json:"rules"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	return resp.Rules, nil
}

// Command Alert Rules

// CommandAlertRule is an organization-wide rule matched against the command a
// permission request is asking to run.
type CommandAlertRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Pattern     string `json:"pattern"`
	Severity    string `json:"severity"`
	Action      string `json:"action"`
	Description string `json:"description"`
}

func (c *Client) GetCommandAlertRules() ([]CommandAlertRule, error) {
	data, err := c.request("GET", "/api/bridge/command-alert-rules", nil)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Rules []CommandAlertRule `json:"rules"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	return resp.Rules, nil
}

// SecurityAlert is an alert this device raised. The server takes the device and
// the user from the token, so neither is sent here.
type SecurityAlert struct {
	RuleID      string `json:"ruleId"`
	Severity    string `json:"severity"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Command     string `json:"command,omitempty"`
	SessionID   string `json:"sessionId,omitempty"`
}

func (c *Client) ReportSecurityAlert(alert SecurityAlert) error {
	_, err := c.request("POST", "/api/bridge/security-alert", alert)
	return err
}

// PermissionDecision is a permission request this device's own rules engine
// resolved without ever asking the server. Those requests never reach the
// WebSocket path that records every other approval, so without this report the
// audit table would show a device's human approvals and silently omit
// everything its rules decided.
//
// As with SecurityAlert, the device and the user come from the token — this is
// the table whose entire purpose is saying who let something through, so the
// sender does not get to name itself.
type PermissionDecision struct {
	ID             string         `json:"id"`
	Approved       bool           `json:"approved"`
	RuleID         string         `json:"ruleId,omitempty"`
	SessionID      string         `json:"sessionId,omitempty"`
	PermissionType string         `json:"permissionType,omitempty"`
	ToolName       string         `json:"toolName,omitempty"`
	Description    string         `json:"description,omitempty"`
	Detail         map[string]any `json:"detail,omitempty"`
	Risk           string         `json:"risk,omitempty"`
	// DecidedAt is when the bridge decided, not when the server heard about it:
	// network latency is not part of how long an approval took.
	DecidedAt string `json:"decidedAt,omitempty"`
}

func (c *Client) ReportPermissionDecision(d PermissionDecision) error {
	_, err := c.request("POST", "/api/bridge/permission-decision", d)
	return err
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

// Session List (for restore on startup)

type SessionInfo struct {
	ID              string `json:"id"`
	CLIType         string `json:"cliType"`
	WorkDir         string `json:"workDir"`
	Status          string `json:"status"`
	EffectiveStatus string `json:"effectiveStatus"`
	StartedAt       int64  `json:"startedAt"`
	EndedAt         int64  `json:"endedAt,omitempty"`
}

func (c *Client) ListSessions(deviceID string, limit int) ([]SessionInfo, error) {
	path := fmt.Sprintf("/api/sessions?deviceId=%s&limit=%d&status=all", deviceID, limit)

	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var sessions []SessionInfo
	if err := json.Unmarshal(data, &sessions); err != nil {
		// API may wrap in { sessions: [...] }
		var resp struct {
			Sessions []SessionInfo `json:"sessions"`
		}
		if err2 := json.Unmarshal(data, &resp); err2 != nil {
			return nil, err
		}
		sessions = resp.Sessions
	}

	return sessions, nil
}

// Session Messages (for resume context)

type MessageInfo struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp int64  `json:"timestamp"`
}

func (c *Client) GetSessionMessages(sessionID string, limit int) ([]MessageInfo, error) {
	path := fmt.Sprintf("/api/sessions/%s/messages?limit=%d", sessionID, limit)

	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var messages []MessageInfo
	if err := json.Unmarshal(data, &messages); err != nil {
		var resp struct {
			Messages []MessageInfo `json:"messages"`
		}
		if err2 := json.Unmarshal(data, &resp); err2 != nil {
			return nil, err
		}
		messages = resp.Messages
	}

	return messages, nil
}

// Session Reporting

type SessionReport struct {
	SessionID string `json:"sessionId"`
	CLIType   string `json:"cliType"`
	WorkDir   string `json:"workDir"`
	Status    string `json:"status"`
	Protocol  string `json:"protocol,omitempty"`
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
