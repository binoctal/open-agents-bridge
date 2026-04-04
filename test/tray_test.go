package test

import (
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/tray"
)

func TestTrayNew(t *testing.T) {
	tr := tray.New("Test App")
	if tr == nil {
		t.Error("New returned nil")
	}
}

func TestTraySetTooltip(t *testing.T) {
	tr := tray.New("Test App")
	// Should not panic
	tr.SetTooltip("New tooltip")
}

func TestTraySetRunning(t *testing.T) {
	tr := tray.New("Test App")
	// Should not panic
	tr.SetRunning(true)
	tr.SetRunning(false)
}

func TestTrayShowNotification(t *testing.T) {
	tr := tray.New("Test App")
	// This may fail in headless environment, but should not panic
	err := tr.ShowNotification("Test Title", "Test Message")
	if err != nil {
		t.Logf("ShowNotification returned error (expected in headless env): %v", err)
	}
}

func TestTrayPrintStatus(t *testing.T) {
	tr := tray.New("Test App")
	tr.SetRunning(true)
	// Should not panic
	tr.PrintStatus()
}

func TestIsSupported(t *testing.T) {
	// Should return a boolean without panicking
	supported := tray.IsSupported()
	t.Logf("Tray supported: %v", supported)
}
