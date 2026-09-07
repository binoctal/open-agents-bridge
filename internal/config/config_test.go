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
}
