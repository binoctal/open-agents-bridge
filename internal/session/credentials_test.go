package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCreds(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
}

func credsPath(dir string) string {
	return filepath.Join(dir, ".credentials.json")
}

// Judgment matrix from the spec: dead / expiring / healthy / missing (and the
// unreadable variants that collapse to missing). Dead also deletes the file.
func TestCheckCredentialHealthMatrix(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		body     string
		want     CredentialHealth
		wantFile bool // whether the credentials file should exist after
	}{
		{
			name:     "dead: refresh window ended",
			body:     fmt.Sprintf(`{"expiresAt":%d,"refreshTokenExpiresAt":%d}`, now.Add(-time.Hour).UnixMilli(), now.Add(-time.Minute).UnixMilli()),
			want:     CredentialDead,
			wantFile: false,
		},
		{
			name:     "expiring soon: refresh window inside 48h",
			body:     fmt.Sprintf(`{"expiresAt":%d,"refreshTokenExpiresAt":%d}`, now.Add(time.Hour).UnixMilli(), now.Add(2*time.Hour).UnixMilli()),
			want:     CredentialExpiringSoon,
			wantFile: true,
		},
		{
			name:     "healthy: refresh window far out (access token stale is fine)",
			body:     fmt.Sprintf(`{"expiresAt":%d,"refreshTokenExpiresAt":%d}`, now.Add(-time.Hour).UnixMilli(), now.Add(7*24*time.Hour).UnixMilli()),
			want:     CredentialHealthy,
			wantFile: true,
		},
		{
			name:     "no refresh field: falls back to expiresAt (dead)",
			body:     fmt.Sprintf(`{"expiresAt":%d}`, now.Add(-time.Hour).UnixMilli()),
			want:     CredentialDead,
			wantFile: false,
		},
		{
			name:     "garbage file collapses to missing",
			body:     `not json`,
			want:     CredentialMissing,
			wantFile: true,
		},
		{
			name:     "zero timestamps collapse to missing",
			body:     `{}`,
			want:     CredentialMissing,
			wantFile: true,
		},
		// The REAL Claude Code shape: fields nested under "claudeAiOauth".
		// A flat-only reader judges these Missing forever (2026-10-04 e2e
		// finding) — every case below must read through the nesting.
		{
			name: "real shape: dead nested under claudeAiOauth",
			body: fmt.Sprintf(
				`{"claudeAiOauth":{"expiresAt":%d,"refreshTokenExpiresAt":%d,"scopes":["scope"]}}`,
				now.Add(-time.Hour).UnixMilli(), now.Add(-time.Minute).UnixMilli()),
			want:     CredentialDead,
			wantFile: false,
		},
		{
			name: "real shape: expiring soon nested under claudeAiOauth",
			body: fmt.Sprintf(
				`{"claudeAiOauth":{"expiresAt":%d,"refreshTokenExpiresAt":%d,"scopes":["scope"]}}`,
				now.Add(time.Hour).UnixMilli(), now.Add(2*time.Hour).UnixMilli()),
			want:     CredentialExpiringSoon,
			wantFile: true,
		},
		{
			name: "real shape: healthy nested under claudeAiOauth",
			body: fmt.Sprintf(
				`{"claudeAiOauth":{"expiresAt":%d,"refreshTokenExpiresAt":%d,"scopes":["scope"]}}`,
				now.Add(-time.Hour).UnixMilli(), now.Add(7*24*time.Hour).UnixMilli()),
			want:     CredentialHealthy,
			wantFile: true,
		},
		{
			name:     "real shape: oauth object without timestamps collapses to missing",
			body:     `{"claudeAiOauth":{"scopes":["scope"]}}`,
			want:     CredentialMissing,
			wantFile: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCreds(t, dir, c.body)
			if got := CheckCredentialHealth(dir, now); got != c.want {
				t.Fatalf("verdict = %d, want %d", got, c.want)
			}
			_, err := os.Stat(credsPath(dir))
			exists := err == nil
			if exists != c.wantFile {
				t.Fatalf("credentials file exists after check = %v, want %v (stat err: %v)", exists, c.wantFile, err)
			}
		})
	}
}

// Missing file is silent — the clean "not logged in" state.
func TestCheckCredentialHealthMissing(t *testing.T) {
	if got := CheckCredentialHealth(t.TempDir(), time.Now()); got != CredentialMissing {
		t.Fatalf("absent file: got %d, want CredentialMissing", got)
	}
}

// Re-login race: after a dead verdict removed the file, a freshly written
// login (valid window) is judged healthy and survives — the deletion never
// touches a second-generation file.
func TestCheckCredentialHealthReloginAfterDead(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	writeCreds(t, dir, fmt.Sprintf(`{"refreshTokenExpiresAt":%d}`, now.Add(-time.Minute).UnixMilli()))
	if got := CheckCredentialHealth(dir, now); got != CredentialDead {
		t.Fatalf("first check: got %d, want CredentialDead", got)
	}
	if _, err := os.Stat(credsPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("dead file should be removed, stat err: %v", err)
	}

	fresh := fmt.Sprintf(`{"expiresAt":%d,"refreshTokenExpiresAt":%d}`, now.Add(time.Hour).UnixMilli(), now.Add(30*24*time.Hour).UnixMilli())
	writeCreds(t, dir, fresh)
	if got := CheckCredentialHealth(dir, now); got != CredentialHealthy {
		t.Fatalf("post-relogin check: got %d, want CredentialHealthy", got)
	}
	raw, err := os.ReadFile(credsPath(dir))
	if err != nil || string(raw) != fresh {
		t.Fatalf("fresh credentials must survive, err=%v", err)
	}
}

// Boundary: exactly at the deadline counts as dead (now is not before it).
func TestCheckCredentialHealthBoundary(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	deadline := now.Add(time.Minute)
	writeCreds(t, dir, fmt.Sprintf(`{"refreshTokenExpiresAt":%d}`, deadline.UnixMilli()))
	if got := CheckCredentialHealth(dir, deadline); got != CredentialDead {
		t.Fatalf("at-deadline: got %d, want CredentialDead", got)
	}
}
