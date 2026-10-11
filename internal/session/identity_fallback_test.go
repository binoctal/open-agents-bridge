package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

func writeCred(t *testing.T, dir string, expiry time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"claudeAiOauth":{"refreshTokenExpiresAt":` + itoa(expiry.UnixMilli()) + `}}`)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func fallbackEnv(t *testing.T) (host, iso, work string) {
	t.Helper()
	root := t.TempDir()
	host = filepath.Join(root, "host")
	iso = filepath.Join(root, "iso")
	work = filepath.Join(root, "work")
	for _, d := range []string{iso, work} {
		_ = os.MkdirAll(d, 0o755)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", host)
	return
}

func newCfg() *protocol.AdapterConfig {
	return &protocol.AdapterConfig{CustomEnv: map[string]string{"CLAUDE_CONFIG_DIR": "isolated"}}
}

func TestHostIdentityFallback_AppliesWhenNoIdentity(t *testing.T) {
	host, iso, work := fallbackEnv(t)
	writeCred(t, host, time.Now().Add(30*24*time.Hour))
	cfg := newCfg()
	if !applyHostIdentityFallback(cfg, work, iso) {
		t.Fatal("expected fallback")
	}
	if cfg.CustomEnv["CLAUDE_CONFIG_DIR"] != host {
		t.Fatalf("got %q", cfg.CustomEnv["CLAUDE_CONFIG_DIR"])
	}
	if _, err := os.Stat(filepath.Join(iso, ".credentials.json")); err == nil {
		t.Fatal("credentials must not be copied")
	}
}

func TestHostIdentityFallback_SkippedWhenIdentityExists(t *testing.T) {
	host, iso, work := fallbackEnv(t)
	writeCred(t, host, time.Now().Add(30*24*time.Hour))

	cfg := newCfg()
	cfg.CustomEnv["ANTHROPIC_API_KEY"] = "k"
	if applyHostIdentityFallback(cfg, work, iso) {
		t.Fatal("envVars identity must win")
	}

	_ = os.MkdirAll(filepath.Join(work, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(work, ".claude", "settings.local.json"), []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"t"}}`), 0o644)
	if applyHostIdentityFallback(newCfg(), work, iso) {
		t.Fatal("project settings identity must win")
	}
	_ = os.Remove(filepath.Join(work, ".claude", "settings.local.json"))

	writeCred(t, iso, time.Now().Add(30*24*time.Hour))
	if applyHostIdentityFallback(newCfg(), work, iso) {
		t.Fatal("isolated OAuth must win")
	}
}

func TestHostIdentityFallback_SkippedWithoutHostLogin(t *testing.T) {
	host, iso, work := fallbackEnv(t)
	if applyHostIdentityFallback(newCfg(), work, iso) {
		t.Fatal("no host login → no fallback")
	}
	writeCred(t, host, time.Now().Add(-time.Hour))
	if applyHostIdentityFallback(newCfg(), work, iso) {
		t.Fatal("dead host login → no fallback")
	}
	if _, err := os.Stat(filepath.Join(host, ".credentials.json")); err != nil {
		t.Fatal("host credentials must never be deleted")
	}
}
