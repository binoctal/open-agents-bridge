package config

import "testing"

func TestFromEnvFailsLoudWithoutSecret(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "")
	t.Setenv("API_BASE_URL", "https://api.example.com")
	if _, err := FromEnv(); err == nil {
		t.Fatal("missing INTERNAL_SECRET must fail loud at boot")
	}
}

func TestFromEnvFailsWithoutAPIBaseURL(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "s3cret")
	t.Setenv("API_BASE_URL", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("missing API_BASE_URL must fail loud at boot")
	}
}

func TestFromEnvPortRangeOrder(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "s3cret")
	t.Setenv("API_BASE_URL", "https://api.example.com")
	t.Setenv("GIT_HTTP_PORT_MIN", "24200")
	t.Setenv("GIT_HTTP_PORT_MAX", "24100")
	if _, err := FromEnv(); err == nil {
		t.Fatal("inverted port range must be rejected")
	}
}

func TestFromEnvRuntimePreviewPortRangeOrder(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "s3cret")
	t.Setenv("API_BASE_URL", "https://api.example.com")
	t.Setenv("RUNTIME_PREVIEW_PORT_MIN", "32199")
	t.Setenv("RUNTIME_PREVIEW_PORT_MAX", "32100")
	if _, err := FromEnv(); err == nil {
		t.Fatal("inverted runtime preview port range must be rejected")
	}
}

func TestFromEnvLegacyShorthandIsOneEndpoint(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "s3cret")
	t.Setenv("API_BASE_URL", "https://api.example.com/")
	t.Setenv("COOLIFY_PROJECT_UUID", "proj-global")
	t.Setenv("API_ENDPOINTS", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if len(cfg.Endpoints) != 1 {
		t.Fatalf("legacy shorthand must yield exactly one endpoint, got %d", len(cfg.Endpoints))
	}
	ep := cfg.Endpoints[0]
	if ep.BaseURL != "https://api.example.com" || ep.InternalSecret != "s3cret" || ep.CoolifyProjectUUID != "proj-global" {
		t.Fatalf("legacy endpoint wrong: %+v", ep)
	}
}

func TestFromEnvEndpointsList(t *testing.T) {
	t.Setenv("API_ENDPOINTS",
		"https://api.openagents.top|prod-secret|k3nxla; https://api-staging.openagents.top/|staging-secret|dinu1e")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if len(cfg.Endpoints) != 2 {
		t.Fatalf("want 2 endpoints, got %d", len(cfg.Endpoints))
	}
	if cfg.Endpoints[0].BaseURL != "https://api.openagents.top" || cfg.Endpoints[0].InternalSecret != "prod-secret" || cfg.Endpoints[0].CoolifyProjectUUID != "k3nxla" {
		t.Fatalf("endpoint 0 wrong: %+v", cfg.Endpoints[0])
	}
	if cfg.Endpoints[1].BaseURL != "https://api-staging.openagents.top" || cfg.Endpoints[1].CoolifyProjectUUID != "dinu1e" {
		t.Fatalf("endpoint 1 wrong (trailing slash must trim): %+v", cfg.Endpoints[1])
	}
}

func TestFromEnvEndpointsOptionalProject(t *testing.T) {
	t.Setenv("API_ENDPOINTS", "https://api.example.com|s3cret")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("two-field entry (no project) must parse: %v", err)
	}
	if cfg.Endpoints[0].CoolifyProjectUUID != "" {
		t.Fatalf("absent project field must stay empty, got %q", cfg.Endpoints[0].CoolifyProjectUUID)
	}
}

func TestFromEnvEndpointsRejectsBadEntries(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"duplicate URL", "https://api.example.com|a;https://api.example.com|b"},
		{"missing secret", "https://api.example.com|"},
		{"missing url", "|secret"},
		{"too many fields", "https://api.example.com|s|p|extra"},
		{"one field", "https://api.example.com"},
		{"only separators", "; ;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("API_ENDPOINTS", tc.raw)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("API_ENDPOINTS=%q must be rejected", tc.raw)
			}
		})
	}
}

func TestFromEnvLegacyMissingSecretStillFails(t *testing.T) {
	t.Setenv("API_ENDPOINTS", "")
	t.Setenv("INTERNAL_SECRET", "")
	t.Setenv("API_BASE_URL", "https://api.example.com")
	if _, err := FromEnv(); err == nil {
		t.Fatal("legacy form without a secret must fail loud at boot")
	}
}

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("INTERNAL_SECRET", "s3cret")
	t.Setenv("API_BASE_URL", "https://api.example.com/")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.AgentID != "primary" || cfg.CacheDir != "/var/lib/hosting-agent" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.APIBaseURL != "https://api.example.com" {
		t.Fatalf("trailing slash must be trimmed, got %q", cfg.APIBaseURL)
	}
	if cfg.GitPortMin != 24100 || cfg.GitPortMax != 24199 {
		t.Fatalf("default git port range wrong: %d-%d", cfg.GitPortMin, cfg.GitPortMax)
	}
	if cfg.RuntimePreviewPortMin != 32100 || cfg.RuntimePreviewPortMax != 32199 {
		t.Fatalf("default runtime preview port range wrong: %d-%d", cfg.RuntimePreviewPortMin, cfg.RuntimePreviewPortMax)
	}
	if cfg.RuntimePreviewIdleTTLMinutes != 30 {
		t.Fatalf("default idle TTL wrong: %d", cfg.RuntimePreviewIdleTTLMinutes)
	}
	if cfg.RuntimePreviewLogDir != "/var/lib/caddy/oa-runtime" {
		t.Fatalf("default runtime log dir wrong: %q", cfg.RuntimePreviewLogDir)
	}
	if cfg.CoolifyBaseURL != "http://127.0.0.1:8000" {
		t.Fatalf("default Coolify base URL wrong: %q", cfg.CoolifyBaseURL)
	}
	if cfg.WorkPollIntervalSec != 10 {
		t.Fatalf("default work poll interval wrong: %d", cfg.WorkPollIntervalSec)
	}
	if cfg.WorkPollIdleMaxSec != 60 {
		t.Fatalf("default idle backoff cap wrong: %d", cfg.WorkPollIdleMaxSec)
	}
}
