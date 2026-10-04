package bridge

import (
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// engine-explicit-auth (D2): claude-agent-acp surfaces the CLI's OAuth
// failures as JSON-RPC internal errors with no structured marker, so the
// bridge sniffs the text at the single choke point (forwardSessionOutput)
// and stamps a code on both session:error and the derived agent status.
const errorCodeAuthExpired = "AUTH_EXPIRED"

// authFailurePhrases are OAuth-failure markers seen in real adapter errors.
// A phrase alone never classifies — classifyAuthError also requires the HTTP
// status, which keeps unrelated mentions (docs, tool output quoting the
// phrase) from firing.
var authFailurePhrases = []string{
	"oauth access token has been revoked",
	"oauth access token has expired",
	"oauth token",
	"failed to authenticate",
}

// classifyAuthError maps adapter error text to a structured error code, or ""
// when no auth-failure pattern matches. The empty result is the
// no-degradation path: the error forwards verbatim with no code attached.
func classifyAuthError(text string) string {
	lower := strings.ToLower(text)
	// The adapter surfaces missing/garbage credentials as a bare JSON-RPC
	// -32000 "Authentication required" with no HTTP status at all
	// (live-observed in the 2026-10-04 local e2e). That exact marker
	// classifies on its own — it only ever rides an error message.
	if strings.Contains(lower, "authentication required") {
		return errorCodeAuthExpired
	}
	if !strings.Contains(text, "401") {
		return ""
	}
	for _, p := range authFailurePhrases {
		if strings.Contains(lower, p) {
			return errorCodeAuthExpired
		}
	}
	return ""
}

// authErrorCodeOf classifies a protocol message if it is an auth-class error.
func authErrorCodeOf(msg protocol.Message) string {
	if msg.Type != protocol.MessageTypeError {
		return ""
	}
	text, ok := msg.Content.(string)
	if !ok {
		return ""
	}
	return classifyAuthError(text)
}
