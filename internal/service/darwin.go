package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.open-agents-bridge</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>start</string>
        <string>-d</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/open-agents-bridge.log</string>
    <key>StandardErrorPath</key>
    <string>%s/open-agents-bridge.log</string>
</dict>
</plist>
`

const label = "com.open-agents-bridge"

type DarwinService struct{}

func (s *DarwinService) plistPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "com.open-agents-bridge.plist")
}

func (s *DarwinService) logDir() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "Logs", "open-agents-bridge")
}

func (s *DarwinService) Install(device string) error {
	if device == "" {
		return fmt.Errorf("device name is required: -d <device>")
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, _ = filepath.Abs(exePath)

	logDir := s.logDir()
	os.MkdirAll(logDir, 0755)
	os.MkdirAll(filepath.Dir(s.plistPath()), 0755)

	content := fmt.Sprintf(launchdPlist, exePath, device, logDir, logDir)
	return os.WriteFile(s.plistPath(), []byte(content), 0644)
}

func (s *DarwinService) Uninstall() error {
	s.Stop()
	if err := os.Remove(s.plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *DarwinService) Start() error {
	return exec.Command("launchctl", "load", s.plistPath()).Run()
}

func (s *DarwinService) Stop() error {
	return exec.Command("launchctl", "unload", s.plistPath()).Run()
}

func (s *DarwinService) Status() (string, error) {
	out, _ := exec.Command("launchctl", "list", label).Output()
	if strings.Contains(string(out), label) {
		return "running", nil
	}
	return "stopped", nil
}
