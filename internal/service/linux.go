package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const systemdUnit = `[Unit]
Description=Open Agents Bridge
After=network.target

[Service]
Type=simple
ExecStart=%s start
Restart=always
RestartSec=5
Environment=HOME=%s

[Install]
WantedBy=multi-user.target
`

const serviceName = "open-agents-bridge"
const unitPath = "/etc/systemd/system/open-agents-bridge.service"

type LinuxService struct{}

func (s *LinuxService) Install() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, _ = filepath.Abs(exePath)

	content := fmt.Sprintf(systemdUnit, exePath, os.Getenv("HOME"))

	if err := os.WriteFile(unitPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write unit file (try with sudo): %w", err)
	}

	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return err
	}

	return exec.Command("systemctl", "enable", serviceName).Run()
}

func (s *LinuxService) Uninstall() error {
	exec.Command("systemctl", "stop", serviceName).Run()
	exec.Command("systemctl", "disable", serviceName).Run()

	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	return exec.Command("systemctl", "daemon-reload").Run()
}

func (s *LinuxService) Start() error {
	return exec.Command("systemctl", "start", serviceName).Run()
}

func (s *LinuxService) Stop() error {
	return exec.Command("systemctl", "stop", serviceName).Run()
}

func (s *LinuxService) Status() (string, error) {
	out, err := exec.Command("systemctl", "is-active", serviceName).Output()
	status := strings.TrimSpace(string(out))
	if err != nil {
		return status, nil
	}
	return status, nil
}
