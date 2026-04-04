package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const serviceName_win = "OpenAgentsBridge"

type WindowsService struct{}

func (s *WindowsService) Install() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, _ = filepath.Abs(exePath)

	// Use sc.exe to create service
	cmd := exec.Command("sc", "create", serviceName_win,
		"binPath=", fmt.Sprintf(`"%s" start`, exePath),
		"start=", "auto",
		"DisplayName=", "Open Agents Bridge")

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", string(out), err)
	}
	return nil
}

func (s *WindowsService) Uninstall() error {
	s.Stop()
	cmd := exec.Command("sc", "delete", serviceName_win)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w", string(out), err)
	}
	return nil
}

func (s *WindowsService) Start() error {
	return exec.Command("sc", "start", serviceName_win).Run()
}

func (s *WindowsService) Stop() error {
	return exec.Command("sc", "stop", serviceName_win).Run()
}

func (s *WindowsService) Status() (string, error) {
	out, err := exec.Command("sc", "query", serviceName_win).Output()
	if err != nil {
		return "not installed", nil
	}
	if strings.Contains(string(out), "RUNNING") {
		return "running", nil
	}
	return "stopped", nil
}
