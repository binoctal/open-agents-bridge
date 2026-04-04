package test

import (
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/adapter"
)

func TestAdapterGet(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"kiro", false},
		{"cline", false},
		{"claude", false},
		{"codex", false},
		{"gemini", false},
		{"unknown", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adp, err := adapter.Get(tt.name)
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if adp == nil {
					t.Error("Adapter is nil")
				}
			}
		})
	}
}

func TestAdapterList(t *testing.T) {
	names := adapter.List()

	if len(names) < 5 {
		t.Errorf("Expected at least 5 adapters, got %d", len(names))
	}

	// Check required adapters exist
	required := map[string]bool{"kiro": false, "cline": false, "claude": false, "codex": false, "gemini": false}
	for _, name := range names {
		required[name] = true
	}

	for name, found := range required {
		if !found {
			t.Errorf("Missing required adapter: %s", name)
		}
	}
}

func TestKiroAdapter(t *testing.T) {
	adp, _ := adapter.Get("kiro")

	if adp.Name() != "kiro" {
		t.Errorf("Name = %s, want kiro", adp.Name())
	}
	if adp.DisplayName() == "" {
		t.Error("DisplayName is empty")
	}
	if adp.IsRunning() {
		t.Error("Should not be running initially")
	}
}

func TestClineAdapter(t *testing.T) {
	adp, _ := adapter.Get("cline")

	if adp.Name() != "cline" {
		t.Errorf("Name = %s, want cline", adp.Name())
	}
	if adp.DisplayName() == "" {
		t.Error("DisplayName is empty")
	}
}

func TestAdapterCallbacks(t *testing.T) {
	adp, _ := adapter.Get("kiro")

	// These should not panic
	adp.OnOutput(func(e adapter.OutputEvent) {})
	adp.OnPermission(func(r adapter.PermissionRequest) adapter.PermissionResponse {
		return adapter.PermissionResponse{ID: r.ID, Approved: true}
	})
	adp.OnExit(func(code int) {})
}
