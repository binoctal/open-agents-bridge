package workflows

import (
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
