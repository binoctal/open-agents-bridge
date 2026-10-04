package bridge

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
	"github.com/binoctal/open-agents-bridge/internal/session"
)

// The production anchor: exact error text observed on 2026-10-04 when the
// claude ACP engine's OAuth credential copy had been revoked by refresh-token
// rotation. The mapping must keep recognizing it verbatim.
const prodRevokedError = "Error -32603: Internal error: Failed to authenticate. API Error: 401 OAuth access token has been revoked."

func TestClassifyAuthError(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"production revoked anchor", prodRevokedError, errorCodeAuthExpired},
		{"expired variant", "API Error: 401 OAuth access token has expired.", errorCodeAuthExpired},
		{"oauth token variant", "401 unauthorized: invalid oauth token", errorCodeAuthExpired},
		{"failed to authenticate variant", "Error: Failed to authenticate. API Error: 401", errorCodeAuthExpired},
		// Local-e2e anchor (2026-10-04): the adapter reports missing/garbage
		// credentials as a bare -32000 with no HTTP status at all.
		{"adapter -32000 bare marker", "Error -32000: Authentication required", errorCodeAuthExpired},
		// No-degradation matrix: misses return "" and the error forwards
		// verbatim with no code attached.
		{"non-auth error", "ENOENT: no such file or directory", ""},
		{"auth phrase without 401", "docs say the CLI may print Failed to authenticate somewhere", ""},
		{"401 without auth phrase", "API Error: 401 quota exceeded for project", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := classifyAuthError(c.text); got != c.want {
			t.Fatalf("%s: classifyAuthError(%q) = %q, want %q", c.name, c.text, got, c.want)
		}
	}
}

// authErrorCodeOf only classifies error messages with string content.
func TestAuthErrorCodeOf(t *testing.T) {
	errMsg := protocol.Message{Type: protocol.MessageTypeError, Content: prodRevokedError}
	if got := authErrorCodeOf(errMsg); got != errorCodeAuthExpired {
		t.Fatalf("auth error message: got %q, want %q", got, errorCodeAuthExpired)
	}
	if got := authErrorCodeOf(contentMsg()); got != "" {
		t.Fatalf("non-error message must not classify, got %q", got)
	}
	structured := protocol.Message{Type: protocol.MessageTypeError, Content: map[string]interface{}{"code": -32603}}
	if got := authErrorCodeOf(structured); got != "" {
		t.Fatalf("non-string error content must not classify, got %q", got)
	}
}

// Derivation rule: an auth-class error maps to auth_required; any other
// error text implies no status at all (existing behavior unchanged).
func TestStatusFromMessageAuthError(t *testing.T) {
	msg := protocol.Message{Type: protocol.MessageTypeError, Content: prodRevokedError}
	if s, ok := statusFromMessage(msg); !ok || s != protocol.StatusAuthRequired {
		t.Fatalf("auth error: got (%v,%v), want (auth_required,true)", s, ok)
	}
	plain := protocol.Message{Type: protocol.MessageTypeError, Content: "spawn: npx ENOENT"}
	if _, ok := statusFromMessage(plain); ok {
		t.Fatal("non-auth error must imply no status")
	}
}

// auth_required is in the immediate set: it must surface even inside the
// dwell window, both as a "backward" move from an active state and right
// after an idle report (where active stragglers are otherwise swallowed).
func TestTrackerAuthRequiredImmediate(t *testing.T) {
	tr := &statusTracker{}
	tr.observePrompt()       // thinking
	tr.observe(contentMsg()) // streaming

	authMsg := protocol.Message{Type: protocol.MessageTypeError, Content: prodRevokedError}
	if s, changed := tr.observe(authMsg); !changed || s != protocol.StatusAuthRequired {
		t.Fatalf("auth error during active turn: got (%v,%v), want (auth_required,true) — never dwell-throttled", s, changed)
	}

	// After idle, inside the straggler window, an auth error still reports.
	tr.observe(typedStatusMsg(protocol.StatusIdle))
	if s, changed := tr.observe(authMsg); !changed || s != protocol.StatusAuthRequired {
		t.Fatalf("auth error after idle: got (%v,%v), want (auth_required,true) — straggler suppression must not swallow it", s, changed)
	}
	// Not re-reported while it stays the verdict.
	if _, changed := tr.observe(authMsg); changed {
		t.Fatal("repeated auth error must not re-report auth_required")
	}
	// Recovery: a new prompt after the fix restarts the turn normally.
	if s, changed := tr.observePrompt(); !changed || s != protocol.StatusThinking {
		t.Fatalf("prompt after auth_required: got (%v,%v), want (thinking,true)", s, changed)
	}
}

// Field contract of the agent:status payload: status stays a valid enum
// value; detail and code are optional and only present when non-empty.
func TestStatusPayloadCodeField(t *testing.T) {
	p := statusPayload("dev1", "s1", "acp", protocol.StatusAuthRequired, "credential_dead", errorCodeAuthExpired)
	if p["code"] != errorCodeAuthExpired {
		t.Fatalf("payload code: got %v, want %s", p["code"], errorCodeAuthExpired)
	}
	if p["status"] != protocol.StatusAuthRequired {
		t.Fatalf("payload status: got %v, want auth_required", p["status"])
	}

	noCode := statusPayload("dev1", "s1", "acp", protocol.StatusStreaming, "", "")
	if _, present := noCode["code"]; present {
		t.Fatal("empty code must not appear in the payload")
	}
	if _, present := noCode["detail"]; present {
		t.Fatal("empty detail must not appear in the payload")
	}
}

// The creation-time health verdict maps to auth_required with the structured
// code; Missing/Healthy produce nothing (spec zero-report rule).
func TestCredentialHealthDetail(t *testing.T) {
	cases := []struct {
		health     session.CredentialHealth
		wantEmit   bool
		wantDetail string
	}{
		{session.CredentialDead, true, "credential_dead"},
		{session.CredentialExpiringSoon, true, "credential_expiring_soon"},
		{session.CredentialHealthy, false, ""},
		{session.CredentialMissing, false, ""},
	}
	for _, c := range cases {
		detail, emit := credentialHealthDetail(c.health)
		if emit != c.wantEmit || (emit && detail != c.wantDetail) {
			t.Fatalf("health %d: got (%q,%v), want (%q,%v)", c.health, detail, emit, c.wantDetail, c.wantEmit)
		}
	}
}
