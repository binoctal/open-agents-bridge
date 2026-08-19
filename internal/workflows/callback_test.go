package workflows

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractArtifacts(t *testing.T) {
	cfg := DefaultCallbackConfig()
	cm := NewCallbackManager(cfg)

	tests := []struct {
		name           string
		input          string
		wantSummaryLen int
		wantTruncated  bool
	}{
		{
			name:           "short output",
			input:          "Hello World",
			wantSummaryLen: 11,
			wantTruncated:  false,
		},
		{
			name:           "exact 500 chars",
			input:          strings.Repeat("a", 500),
			wantSummaryLen: 500,
			wantTruncated:  false,
		},
		{
			name:           "over 500 chars",
			input:          strings.Repeat("a", 600),
			wantSummaryLen: 500,
			wantTruncated:  false, // summary is truncated, artifacts may be truncated
		},
		{
			name:           "over 100KB",
			input:          strings.Repeat("a", 101*1024),
			wantSummaryLen: 500,
			wantTruncated:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, artifacts := cm.ExtractArtifacts([]byte(tt.input))

			if len(summary) > 500 {
				t.Errorf("summary length = %d, want <= 500", len(summary))
			}

			if tt.wantTruncated && !strings.Contains(artifacts, "truncated") {
				t.Error("expected artifacts to contain truncation notice")
			}

			if !tt.wantTruncated && strings.Contains(artifacts, "truncated") {
				t.Error("unexpected truncation notice in artifacts")
			}
		})
	}
}

func TestCallbackConfigDefaults(t *testing.T) {
	cfg := DefaultCallbackConfig()

	if cfg.Timeout != 30*60*1000*1000*1000 { // 30 minutes
		t.Errorf("default timeout = %v, want 30 minutes", cfg.Timeout)
	}

	if cfg.MaxRetries != 3 {
		t.Errorf("default max retries = %d, want 3", cfg.MaxRetries)
	}

	if cfg.MaxArtifactSize != 100*1024 {
		t.Errorf("default max artifact size = %d, want 100KB", cfg.MaxArtifactSize)
	}
}

func TestNewCallbackManagerNormalizesWSScheme(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ws://localhost:8989", "http://localhost:8989"},
		{"wss://api.example.com", "https://api.example.com"},
		{"http://localhost:8989", "http://localhost:8989"},
		{"https://api.example.com", "https://api.example.com"},
		{"", ""},
	}
	for _, tt := range tests {
		cm := NewCallbackManager(CallbackConfig{APIURL: tt.in, DeviceID: "dev-1"})
		if cm.config.APIURL != tt.want {
			t.Errorf("APIURL %q normalized to %q, want %q", tt.in, cm.config.APIURL, tt.want)
		}
	}
}

func TestSendTaskResultUsesMissionEventRoute(t *testing.T) {
	var gotPath, gotSecret, gotDeviceID string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSecret = r.Header.Get("X-Internal-Secret")
		gotDeviceID = r.Header.Get("X-Device-ID")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cm := NewCallbackManager(CallbackConfig{APIURL: srv.URL, DeviceID: "dev-1", UserID: "user-1", InternalSecret: "s3cret"})
	if err := cm.SendTaskResult(TaskResult{JobID: "j1", TaskID: "t1", Success: true}); err != nil {
		t.Fatalf("SendTaskResult: %v", err)
	}

	if gotPath != "/api/missions/internal/orchestrator/event" {
		t.Errorf("callback path = %q, want /api/missions/internal/orchestrator/event", gotPath)
	}
	if gotSecret != "s3cret" {
		t.Errorf("X-Internal-Secret = %q, want %q", gotSecret, "s3cret")
	}
	if gotDeviceID != "dev-1" {
		t.Errorf("X-Device-ID = %q, want dev-1", gotDeviceID)
	}
	// The internal route has no JWT context; the payload must carry the
	// mission owner so the route can build the user-scoped orchestrator.
	payload, _ := gotBody["payload"].(map[string]any)
	if payload["userId"] != "user-1" {
		t.Errorf("payload.userId = %v, want user-1", payload["userId"])
	}
}
