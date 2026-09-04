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
	data, _, err := c.requestWithStatus(method, path, body)
	return data, err
}

// requestWithStatus is like request but also returns the HTTP status code,
// which preview-hosting error handling needs to distinguish error codes
// (e.g. PREVIEW_QUOTA_EXCEEDED) without re-parsing the wrapped error string.
func (c *Client) requestWithStatus(method, path string, body interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", "Bearer "+c.deviceToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	if resp.StatusCode >= 400 {
		return respBody, resp.StatusCode, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, resp.StatusCode, nil
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

// Preview hosting (add-preview-hosting, task 4.3)
//
// The bridge is the only party that ever touches source: it builds the
// merged worktree locally, hashes the static output, and uploads just the
// bytes over these three calls. Every method here is best-effort by design
// from the caller's point of view — see preview.RunAndUpload, which is the
// only place that calls them and never lets a failure reach the mission.

// PreviewFile is one entry in an upload manifest: a relative, forward-slash
// path plus its content hash and byte size. Mirrors the platform's
// ManifestFile (apps/api/src/services/preview-deployments.ts).
type PreviewFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// PreviewUpload is one presigned PUT target the platform wants filled in.
// Files already present in the bucket (a revive hitting the skip-list) are
// simply absent from this list — nothing to compare against, just upload
// exactly what comes back.
type PreviewUpload struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

// DeclarePreviewResponse is the 201 body from POST .../previews.
type DeclarePreviewResponse struct {
	PreviewID string          `json:"previewId"`
	Subdomain string          `json:"subdomain"`
	URL       string          `json:"url"`
	Revived   bool            `json:"revived"`
	Uploads   []PreviewUpload `json:"uploads"`
}

// PreviewAPIError is the {"error":{"code","message"}} shape every
// preview-hosting failure response carries. Codes include
// PREVIEW_QUOTA_EXCEEDED, PREVIEW_DAILY_LIMIT_EXCEEDED, PREVIEW_TAKEN_DOWN,
// PREVIEW_UPLOAD_UNAVAILABLE, PREVIEW_PLATFORM_BUSY, PREVIEW_INVALID_MANIFEST.
// The bridge never branches on the code — every one of them means "log and
// stop" — but callers may want it for the log line.
type PreviewAPIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *PreviewAPIError) Error() string {
	return fmt.Sprintf("preview API error %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// parsePreviewError turns a non-2xx preview response body into a
// PreviewAPIError when it parses as the documented error shape, otherwise
// returns the original error unchanged.
func parsePreviewError(status int, body []byte, orig error) error {
	if len(body) == 0 {
		return orig
	}
	var wrapped struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil || wrapped.Error.Code == "" {
		return orig
	}
	return &PreviewAPIError{StatusCode: status, Code: wrapped.Error.Code, Message: wrapped.Error.Message}
}

// CreatePreview declares (or revives) a preview deployment for a mission
// from a file manifest. jobId here is the bridge-side name for what the
// platform calls missionId.
func (c *Client) CreatePreview(jobID string, files []PreviewFile) (*DeclarePreviewResponse, error) {
	body := struct {
		Files []PreviewFile `json:"files"`
	}{Files: files}

	data, status, err := c.requestWithStatus("POST", "/api/missions/internal/"+jobID+"/previews", body)
	if err != nil {
		return nil, parsePreviewError(status, data, err)
	}

	var resp DeclarePreviewResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CompletePreview tells the platform every presigned upload for previewID
// landed, flipping the preview to ready.
func (c *Client) CompletePreview(jobID, previewID string) error {
	data, status, err := c.requestWithStatus("POST", "/api/missions/internal/"+jobID+"/previews/"+previewID+"/complete", nil)
	if err != nil {
		return parsePreviewError(status, data, err)
	}
	return nil
}

// PendingRevive is one mission whose preview the user asked to regenerate
// while this device was offline or busy.
type PendingRevive struct {
	MissionID string `json:"missionId"`
	PreviewID string `json:"previewId"`
}

// GetPendingRevives polls for user-requested preview rebuilds. Piggybacked
// on whatever periodic poll loop the bridge already runs (task 4.4).
func (c *Client) GetPendingRevives() ([]PendingRevive, error) {
	data, err := c.request("GET", "/api/missions/internal/previews/pending-revives", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Revives []PendingRevive `json:"revives"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Revives, nil
}

// UploadPreviewFile PUTs raw file bytes to a presigned R2 upload URL. The
// signature covers UNSIGNED-PAYLOAD with SignedHeaders=host only, so this
// deliberately sends no headers beyond what net/http sets automatically
// (Content-Length) — an extra header (e.g. Content-Type) can invalidate the
// signature.
func (c *Client) UploadPreviewFile(url string, data []byte) error {
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	// Use a client with no default headers of its own; c.httpClient carries
	// only a timeout, so it's safe to reuse here too.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("preview upload PUT failed %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
