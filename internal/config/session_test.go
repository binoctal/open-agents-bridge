package config

import (
	"os"
	"testing"
)

func setSessionEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{envSessionToken, envSessionID, envUserID, envShadowDevice, envAPIBaseURL} {
		t.Setenv(k, kv[k])
	}
}

func TestFromSessionEnvBuildsConfigAndGatewayCredential(t *testing.T) {
	setSessionEnv(t, map[string]string{
		envSessionToken: "sst_abc", envSessionID: "s1", envUserID: "u1",
		envShadowDevice: "cse_1", envAPIBaseURL: "https://api.example/",
	})
	if !SessionEnvActive() {
		t.Fatal("expected session mode")
	}
	cfg, err := FromSessionEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeviceID != "cse_1" || cfg.UserID != "u1" || cfg.DeviceToken != "sst_abc" || cfg.ServerURL != "wss://api.example" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if got := os.Getenv("ANTHROPIC_BASE_URL"); got != "https://api.example/api/sandbox/llm" {
		t.Fatalf("gateway url = %q", got)
	}
	if os.Getenv("ANTHROPIC_AUTH_TOKEN") != "sst_abc" {
		t.Fatal("gateway token not set")
	}
}

func TestFromSessionEnvFailsLoudlyWhenIncomplete(t *testing.T) {
	setSessionEnv(t, map[string]string{envSessionToken: "sst_abc"})
	if _, err := FromSessionEnv(); err == nil {
		t.Fatal("expected error for incomplete environment")
	}
	setSessionEnv(t, map[string]string{
		envSessionToken: "not-sst", envSessionID: "s", envUserID: "u", envShadowDevice: "d", envAPIBaseURL: "https://x",
	})
	if _, err := FromSessionEnv(); err == nil {
		t.Fatal("expected error for non-session token")
	}
}

func TestSessionEnvInactiveWithoutToken(t *testing.T) {
	setSessionEnv(t, map[string]string{})
	if SessionEnvActive() {
		t.Fatal("must not be active without SESSION_TOKEN")
	}
}
