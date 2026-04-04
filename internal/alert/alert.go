package alert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// Level represents alert severity level
type Level string

const (
	LevelInfo    Level = "info"
	LevelWarning Level = "warning"
	LevelError   Level = "error"
	LevelCritical Level = "critical"
)

// Alert represents an alert event
type Alert struct {
	ID        string                 `json:"id"`
	Level     Level                  `json:"level"`
	Type      string                 `json:"type"`
	Title     string                 `json:"title"`
	Message   string                 `json:"message"`
	Source    string                 `json:"source"`
	Timestamp int64                  `json:"timestamp"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// Handler handles alert delivery
type Handler interface {
	Send(alert Alert) error
	Name() string
}

// Config for alert manager
type Config struct {
	Enabled     bool          `json:"enabled"`
	Cooldown    time.Duration `json:"cooldown"`
	MaxAlerts   int           `json:"maxAlerts"`
	WebhookURL  string        `json:"webhookUrl"`
	SlackToken  string        `json:"slackToken"`
	EmailConfig *EmailConfig  `json:"emailConfig,omitempty"`
}

// EmailConfig for email alerts
type EmailConfig struct {
	SMTPHost     string   `json:"smtpHost"`
	SMTPPort     int      `json:"smtpPort"`
	Username     string   `json:"username"`
	Password     string   `json:"password"`
	From         string   `json:"from"`
	Recipients   []string `json:"recipients"`
}

// Manager manages alert delivery
type Manager struct {
	config    Config
	handlers  []Handler
	alerts    []Alert
	cooldowns map[string]int64 // alert type -> last sent timestamp
	mu        sync.RWMutex
}

// NewManager creates a new alert manager
func NewManager(config Config) *Manager {
	m := &Manager{
		config:    config,
		handlers:  make([]Handler, 0),
		alerts:    make([]Alert, 0),
		cooldowns: make(map[string]int64),
	}

	// Register default handlers
	if config.WebhookURL != "" {
		m.RegisterHandler(NewWebhookHandler(config.WebhookURL))
	}

	return m
}

// RegisterHandler registers an alert handler
func (m *Manager) RegisterHandler(handler Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, handler)
}

// Send sends an alert through all registered handlers
func (m *Manager) Send(alert Alert) error {
	if !m.config.Enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Check cooldown
	if lastSent, ok := m.cooldowns[alert.Type]; ok {
		if time.Now().UnixMilli()-lastSent < m.config.Cooldown.Milliseconds() {
			return nil // Skip due to cooldown
		}
	}

	// Set defaults
	if alert.Timestamp == 0 {
		alert.Timestamp = time.Now().UnixMilli()
	}
	if alert.ID == "" {
		alert.ID = fmt.Sprintf("%s-%d", alert.Type, alert.Timestamp)
	}

	// Store alert
	m.alerts = append(m.alerts, alert)
	if len(m.alerts) > m.config.MaxAlerts {
		m.alerts = m.alerts[1:]
	}

	// Update cooldown
	m.cooldowns[alert.Type] = alert.Timestamp

	// Send through all handlers
	var lastErr error
	for _, handler := range m.handlers {
		if err := handler.Send(alert); err != nil {
			lastErr = err
			fmt.Fprintf(os.Stderr, "[Alert] Failed to send via %s: %v\n", handler.Name(), err)
		}
	}

	return lastErr
}

// GetAlerts returns recent alerts
func (m *Manager) GetAlerts(limit int) []Alert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 || limit > len(m.alerts) {
		return m.alerts
	}

	return m.alerts[len(m.alerts)-limit:]
}

// Clear clears all stored alerts
func (m *Manager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts = make([]Alert, 0)
}

// WebhookHandler sends alerts to a webhook URL
type WebhookHandler struct {
	url string
}

// NewWebhookHandler creates a new webhook handler
func NewWebhookHandler(url string) *WebhookHandler {
	return &WebhookHandler{url: url}
}

// Name returns the handler name
func (h *WebhookHandler) Name() string {
	return "webhook"
}

// Send sends an alert to the webhook
func (h *WebhookHandler) Send(alert Alert) error {
	data, err := json.Marshal(alert)
	if err != nil {
		return err
	}

	resp, err := http.Post(h.url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	return nil
}

// LogHandler logs alerts to console
type LogHandler struct{}

// NewLogHandler creates a new log handler
func NewLogHandler() *LogHandler {
	return &LogHandler{}
}

// Name returns the handler name
func (h *LogHandler) Name() string {
	return "log"
}

// Send logs an alert to console
func (h *LogHandler) Send(alert Alert) error {
	fmt.Printf("[%s] [%s] %s: %s\n", 
		alert.Level,
		alert.Source,
		alert.Title,
		alert.Message,
	)
	return nil
}

// SlackHandler sends alerts to Slack
type SlackHandler struct {
	webhookURL string
}

// NewSlackHandler creates a new Slack handler
func NewSlackHandler(webhookURL string) *SlackHandler {
	return &SlackHandler{webhookURL: webhookURL}
}

// Name returns the handler name
func (h *SlackHandler) Name() string {
	return "slack"
}

// Send sends an alert to Slack
func (h *SlackHandler) Send(alert Alert) error {
	color := "#36a64f" // green for info
	switch alert.Level {
	case LevelWarning:
		color = "#ff9800" // orange
	case LevelError:
		color = "#f44336" // red
	case LevelCritical:
		color = "#9c27b0" // purple
	}

	payload := map[string]interface{}{
		"attachments": []map[string]interface{}{
			{
				"color":  color,
				"title":  alert.Title,
				"text":   alert.Message,
				"fields": []map[string]interface{}{
					{"title": "Level", "value": alert.Level, "short": true},
					{"title": "Source", "value": alert.Source, "short": true},
					{"title": "Type", "value": alert.Type, "short": true},
				},
				"ts": alert.Timestamp / 1000,
			},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	resp, err := http.Post(h.webhookURL, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("slack returned status %d", resp.StatusCode)
	}

	return nil
}

// Global alert manager
var globalManager *Manager
var globalMu sync.RWMutex

// Init initializes the global alert manager
func Init(config Config) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalManager = NewManager(config)
	globalManager.RegisterHandler(NewLogHandler())
}

// GetManager returns the global alert manager
func GetManager() *Manager {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalManager
}

// Send sends an alert through the global manager
func Send(alert Alert) error {
	globalMu.RLock()
	defer globalMu.RUnlock()
	if globalManager == nil {
		return fmt.Errorf("alert manager not initialized")
	}
	return globalManager.Send(alert)
}

// Info sends an info level alert
func Info(alertType, title, message string, metadata map[string]interface{}) error {
	return Send(Alert{
		Level:    LevelInfo,
		Type:     alertType,
		Title:    title,
		Message:  message,
		Source:   "bridge",
		Metadata: metadata,
	})
}

// Warning sends a warning level alert
func Warning(alertType, title, message string, metadata map[string]interface{}) error {
	return Send(Alert{
		Level:    LevelWarning,
		Type:     alertType,
		Title:    title,
		Message:  message,
		Source:   "bridge",
		Metadata: metadata,
	})
}

// Error sends an error level alert
func Error(alertType, title, message string, metadata map[string]interface{}) error {
	return Send(Alert{
		Level:    LevelError,
		Type:     alertType,
		Title:    title,
		Message:  message,
		Source:   "bridge",
		Metadata: metadata,
	})
}

// Critical sends a critical level alert
func Critical(alertType, title, message string, metadata map[string]interface{}) error {
	return Send(Alert{
		Level:    LevelCritical,
		Type:     alertType,
		Title:    title,
		Message:  message,
		Source:   "bridge",
		Metadata: metadata,
	})
}

// SessionError sends a session error alert
func SessionError(sessionID, cliType, errorMsg string) {
	Error("session_error", "Session Error", 
		fmt.Sprintf("Session %s (%s) encountered an error: %s", sessionID, cliType, errorMsg),
		map[string]interface{}{
			"sessionId": sessionID,
			"cliType":   cliType,
		},
	)
}

// HighMemoryUsage sends a high memory usage alert
func HighMemoryUsage(currentMB, thresholdMB float64) {
	Warning("high_memory", "High Memory Usage",
		fmt.Sprintf("Memory usage (%.2f MB) exceeds threshold (%.2f MB)", currentMB, thresholdMB),
		map[string]interface{}{
			"currentMB":  currentMB,
			"thresholdMB": thresholdMB,
		},
	)
}

// WebSocketDisconnected sends a WebSocket disconnection alert
func WebSocketDisconnected(reason string) {
	Warning("ws_disconnected", "WebSocket Disconnected",
		fmt.Sprintf("WebSocket connection lost: %s", reason),
		map[string]interface{}{
			"reason": reason,
		},
	)
}

// WebSocketReconnected sends a WebSocket reconnection alert
func WebSocketReconnected() {
	Info("ws_reconnected", "WebSocket Reconnected",
		"WebSocket connection has been restored",
		nil,
	)
}

// PermissionDenied sends a permission denied alert
func PermissionDenied(toolName, description string) {
	Info("permission_denied", "Permission Denied",
		fmt.Sprintf("Permission denied for %s: %s", toolName, description),
		map[string]interface{}{
			"toolName":    toolName,
			"description": description,
		},
	)
}
