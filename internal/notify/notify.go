package notify

import (
	"fmt"
	"os/exec"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// Notification represents a desktop notification
type Notification struct {
	Title   string
	Message string
	Icon    string // Optional: icon name or path
	 Urgency string // "low", "normal", "critical"
}

// Send sends a desktop notification using notify-send (Linux)
func Send(n Notification) error {
	// Check if notify-send is available
	if _, err := exec.LookPath("notify-send"); err != nil {
		logger.Debug("[Notify] notify-send not available, skipping notification")
		return fmt.Errorf("notify-send not available")
	}

	args := []string{}

	// Add urgency
	if n.Urgency != "" {
		args = append(args, fmt.Sprintf("--urgency=%s", n.Urgency))
	} else {
		args = append(args, "--urgency=normal")
	}

	// Add icon if specified
	if n.Icon != "" {
		args = append(args, fmt.Sprintf("--icon=%s", n.Icon))
	}

	// Add title and message
	args = append(args, n.Title, n.Message)

	cmd := exec.Command("notify-send", args...)
	if err := cmd.Run(); err != nil {
		logger.Error("[Notify] Failed to send notification: %v", err)
		return err
	}

	logger.Debug("[Notify] Notification sent: %s - %s", n.Title, n.Message)
	return nil
}

// AuthRequired sends a notification about authentication requirement
// The notification tells user to authenticate in another terminal, then refresh session
func AuthRequired(agentName, authMethod string) error {
	return Send(Notification{
		Title:   "🔐 Claude Authentication Required",
		Message: "Run 'claude auth login' in terminal, then create a new session.",
		Icon:    "dialog-password",
		Urgency: "critical",
	})
}

// SessionCreated sends a notification about session creation
func SessionCreated(cliType string) error {
	return Send(Notification{
		Title:   "✅ Session Created",
		Message: fmt.Sprintf("%s session is ready", cliType),
		Icon:    "dialog-information",
		Urgency: "low",
	})
}

// Error sends an error notification
func Error(title, message string) error {
	return Send(Notification{
		Title:   title,
		Message: message,
		Icon:    "dialog-error",
		Urgency: "critical",
	})
}

// Info sends an info notification
func Info(title, message string) error {
	return Send(Notification{
		Title:   title,
		Message: message,
		Icon:    "dialog-information",
		Urgency: "normal",
	})
}
