package bridge

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/notify"
	"github.com/binoctal/open-agents-bridge/internal/updater"
)

// Registration handshake (orchestration-parity rule 8): right after the WS
// connection is established the bridge reports what it actually holds —
// its version, whether its API callbacks can reach the orchestrator, and
// whether E2EE keys are loaded — so a mismatch surfaces at connect time
// instead of as a silently dropped task result later.
//
// Every step is best-effort: an old server that has never heard of
// bridge:capability_report just ignores the message, and a report that
// cannot be sent must never take the connection down with it.

// Probe outcome classification. The strings are a wire contract, mirrored
// by evaluateHandshake in apps/api/src/realtime/room.ts — change both or
// neither.
const (
	probeOK                = "ok"
	probeReachableAuthFail = "reachable_auth_failed"
	probeUnreachable       = "unreachable"
)

// probeTimeout is deliberately short: the probe rides along every
// (re)connect, so it must never hold the report hostage for long.
const probeTimeout = 5 * time.Second

// capabilityReportRetries bounds resend attempts after a send failure —
// the report is opportunistic, not transactional.
const capabilityReportRetries = 3

// apiBaseURL derives the HTTP base from the configured WebSocket URL
// (same conversion updateLastSeen uses, kept as one helper for the
// handshake path).
func apiBaseURL(serverURL string) string {
	if strings.HasPrefix(serverURL, "wss://") {
		return "https://" + strings.TrimPrefix(serverURL, "wss://")
	}
	if strings.HasPrefix(serverURL, "ws://") {
		return "http://" + strings.TrimPrefix(serverURL, "ws://")
	}
	return serverURL
}

// classifyCallbackProbe hits a device-token-authenticated API route and
// reduces the outcome to the three-state conclusion the handshake spec
// defines. A dedicated client keeps the transport error (unreachable)
// distinguishable from an HTTP error status (reachable but rejected) —
// the shared api.Client collapses both into one error type.
func classifyCallbackProbe(client *http.Client, apiURL, deviceToken string) (result, detail string) {
	url := strings.TrimRight(apiURL, "/") + "/api/bridge/permission-rules"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return probeUnreachable, fmt.Sprintf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+deviceToken)

	resp, err := client.Do(req)
	if err != nil {
		return probeUnreachable, err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return probeReachableAuthFail, fmt.Sprintf("HTTP %d from %s", resp.StatusCode, url)
	}
	// Any other status still proves the route is reachable and the token
	// was accepted — judging payload details here would make the probe
	// flappy for no scheduling value.
	return probeOK, fmt.Sprintf("HTTP %d", resp.StatusCode)
}

// buildCapabilityReport assembles the wire payload. Pure so tests can pin
// the field set without a live connection.
func buildCapabilityReport(version, probeResult, probeDetail string, e2ee bool) map[string]interface{} {
	return map[string]interface{}{
		"version":       version,
		"callbackProbe": probeResult,
		"e2ee":          e2ee,
		"detail":        probeDetail,
	}
}

// e2eeActive reports whether both key halves are loaded; only then can
// outbound frames actually be encrypted.
func (b *Bridge) e2eeActive() bool {
	b.mu.Lock()
	kp := b.keyPair
	wpk := b.webPubKey
	b.mu.Unlock()
	return kp != nil && wpk != nil
}

// sendCapabilityReport probes the API and delivers the capability report.
// Called as a goroutine after every successful (re)connect: async by
// contract, so neither the probe timeout nor a failed send may block or
// break the connection loop.
func (b *Bridge) sendCapabilityReport() {
	client := &http.Client{Timeout: probeTimeout}
	probeResult, probeDetail := classifyCallbackProbe(client, apiBaseURL(b.config.ServerURL), b.config.DeviceToken)

	report := buildCapabilityReport(updater.Version, probeResult, probeDetail, b.e2eeActive())

	b.deliverCapabilityReport(report, probeResult, func(msg Message) error {
		return b.sendMessage(msg)
	})
}

// deliverCapabilityReport sends the assembled report with bounded retries.
// The send function is injected so tests can exercise the retry loop
// without standing up a WebSocket; the backoff also yields on b.done so a
// shutting-down bridge abandons the report immediately.
func (b *Bridge) deliverCapabilityReport(report map[string]interface{}, probeResult string, send func(Message) error) {
	for attempt := 1; attempt <= capabilityReportRetries; attempt++ {
		err := send(Message{
			Type:      "bridge:capability_report",
			Payload:   report,
			Timestamp: time.Now().UnixMilli(),
		})
		if err == nil {
			b.logInfo("[%s] Capability report sent (version=%s callbackProbe=%s e2ee=%v)",
				logger.ModBridge, report["version"], probeResult, report["e2ee"])
			return
		}
		b.logWarn("[%s] Capability report send failed (attempt %d/%d): %v",
			logger.ModBridge, attempt, capabilityReportRetries, err)
		select {
		case <-b.done:
			return
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}
	b.logError("[%s] Capability report abandoned after %d attempts", logger.ModBridge, capabilityReportRetries)
}

// handleHandshakeMismatch reacts to the server's pairing verdict: the
// device is degraded (logged server-side, shown in the web UI) and the
// human at this machine needs to know why their tasks may not run.
func (b *Bridge) handleHandshakeMismatch(msg Message) {
	payload, _ := msg.Payload.(map[string]interface{})
	detail := "unknown"
	if raw, ok := payload["mismatches"]; ok {
		detail = fmt.Sprintf("%v", raw)
	}
	b.logError("[%s] Handshake mismatch from server: %s", logger.ModBridge, detail)
	// Desktop notification so an unattended machine still surfaces the
	// degradation; a missing notify-send is not worth failing over.
	_ = notify.Send(notify.Notification{
		Title:   "Open Agents Bridge: connection degraded",
		Message: "The server rejected this bridge's capability report: " + detail,
		Urgency: "critical",
	})
}
