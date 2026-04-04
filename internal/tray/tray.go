package tray

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Tray provides system tray functionality
// Note: Full implementation requires cgo and platform-specific libraries
// This is a simplified version that opens a status window

type Tray struct {
	title   string
	tooltip string
	running bool
}

func New(title string) *Tray {
	return &Tray{
		title:   title,
		tooltip: "Open Agents Bridge",
	}
}

func (t *Tray) SetTooltip(tooltip string) {
	t.tooltip = tooltip
}

func (t *Tray) SetRunning(running bool) {
	t.running = running
}

// ShowNotification shows a system notification
func (t *Tray) ShowNotification(title, message string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification "%s" with title "%s"`, message, title)
		return exec.Command("osascript", "-e", script).Run()
	case "linux":
		return exec.Command("notify-send", title, message).Run()
	case "windows":
		// PowerShell notification
		script := fmt.Sprintf(`
			[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
			$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
			$textNodes = $template.GetElementsByTagName("text")
			$textNodes.Item(0).AppendChild($template.CreateTextNode("%s")) | Out-Null
			$textNodes.Item(1).AppendChild($template.CreateTextNode("%s")) | Out-Null
			$toast = [Windows.UI.Notifications.ToastNotification]::new($template)
			[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier("Open Agents").Show($toast)
		`, title, message)
		return exec.Command("powershell", "-Command", script).Run()
	}
	return nil
}

// OpenStatusPage opens the web dashboard
func (t *Tray) OpenStatusPage() error {
	url := "http://localhost:8080/status" // Local status page
	
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return fmt.Errorf("unsupported platform")
	}
	
	return cmd.Start()
}

// PrintStatus prints status to console (fallback for no GUI)
func (t *Tray) PrintStatus() {
	status := "stopped"
	if t.running {
		status = "running"
	}
	fmt.Printf("\n=== %s ===\n", t.title)
	fmt.Printf("Status: %s\n", status)
	fmt.Printf("Tooltip: %s\n", t.tooltip)
	fmt.Println("\nPress Ctrl+C to stop")
}

// IsSupported checks if system tray is supported
func IsSupported() bool {
	// Full tray support requires additional dependencies
	// For now, return false to use console mode
	if os.Getenv("OPEN_AGENTS_GUI") == "1" {
		return true
	}
	return false
}
