package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// engine-explicit-auth (D3): the claude ACP engine's OAuth fallback lives in
// the isolated config dir (claudeACPModeDir). Copies there die silently when
// the main login's refresh-token rotation revokes them (2026-10-04 incident),
// so every claude session creation judges the file's remaining validity —
// a local file read, never a network request.

// CredentialHealth is the verdict of that check.
type CredentialHealth int

const (
	// CredentialMissing: no readable .credentials.json — a clean "not logged
	// in" state; identity comes from project settings or bridge-injected env.
	// Never reported.
	CredentialMissing CredentialHealth = iota
	// CredentialHealthy: refresh window comfortably in the future. Never
	// reported.
	CredentialHealthy
	// CredentialExpiringSoon: refresh window ends within
	// credentialWarnWindow. Reported as an auth_required pre-warning.
	CredentialExpiringSoon
	// CredentialDead: refresh window already ended. The file cannot recover,
	// so it is deleted on detection — back to "not logged in" instead of
	// holding a corpse that 401s every prompt. Reported as auth_required.
	CredentialDead
)

// credentialWarnWindow is how far before the refresh-token window ends the
// health check starts warning (design D3: 48h).
const credentialWarnWindow = 48 * time.Hour

// credentialsFile mirrors the subset of Claude Code's .credentials.json the
// check needs. The real file nests the fields under "claudeAiOauth"; the
// flat read of a top-level shape stays as a tolerance for hand-forged test
// files. Timestamps are local milliseconds since the Unix epoch.
type credentialsFile struct {
	ClaudeAiOauth         *credentialsFile `json:"claudeAiOauth"`
	ExpiresAt             int64            `json:"expiresAt"`
	RefreshTokenExpiresAt int64            `json:"refreshTokenExpiresAt"`
}

// effective returns the nested oauth payload when present, else the file
// itself — the one shape both real logins and forged test files map onto.
func (c *credentialsFile) effective() *credentialsFile {
	if c.ClaudeAiOauth != nil {
		return c.ClaudeAiOauth
	}
	return c
}

// CheckCredentialHealth judges (and on the dead branch, removes) the OAuth
// credentials in a claude ACP config dir:
//   - file absent/unreadable/garbage → CredentialMissing (silent)
//   - refresh window already ended    → CredentialDead, file deleted
//   - refresh window within warn window → CredentialExpiringSoon
//   - otherwise                       → CredentialHealthy
//
// An access token past expiresAt alone is NOT a warning — refreshing it is
// the normal automatic path; only the refresh window bounds real validity.
// "Revoked" is not detectable from the file at all; the runtime error
// mapping (bridge classifyAuthError) is the backstop for that case. A file
// with no refresh expiry falls back to expiresAt for both checks.
func CheckCredentialHealth(dir string, now time.Time) CredentialHealth {
	return checkCredentialHealth(dir, now, true)
}

// checkCredentialHealth is CheckCredentialHealth with the dead-file removal
// switchable: the host's own login must never be deleted by the bridge.
func checkCredentialHealth(dir string, now time.Time, removeDead bool) CredentialHealth {
	path := filepath.Join(dir, ".credentials.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return CredentialMissing
	}
	var cred credentialsFile
	if err := json.Unmarshal(raw, &cred); err != nil {
		return CredentialMissing
	}
	effective := cred.effective()
	deadline := effective.RefreshTokenExpiresAt
	if deadline == 0 {
		deadline = effective.ExpiresAt
	}
	if deadline == 0 {
		return CredentialMissing
	}
	deadlineTime := time.UnixMilli(deadline)
	if !now.Before(deadlineTime) {
		if !removeDead {
			return CredentialDead
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Warn("[%s] failed to remove dead credential file %s: %v", logger.ModSession, path, err)
		} else if err == nil {
			logger.Info("[%s] removed dead credential file (refresh window ended): %s", logger.ModSession, path)
		}
		return CredentialDead
	}
	if deadlineTime.Sub(now) < credentialWarnWindow {
		return CredentialExpiringSoon
	}
	return CredentialHealthy
}
