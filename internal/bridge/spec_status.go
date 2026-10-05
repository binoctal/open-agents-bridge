package bridge

import (
	"os"
	"sync"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/protocol"
	"github.com/binoctal/open-agents-bridge/internal/session"
)

// session-spec-status: D1 holds the desired session parameters (spec); the
// bridge applies them and reports what is actually in force (status).

const (
	applyStateApplied         = "applied"
	applyStateRestartRequired = "restart_required"
	applyStateFailed          = "failed"
)

// specTracker remembers the newest spec version applied per session so that
// out-of-order spec updates are ignored.
type specTracker struct {
	mu      sync.Mutex
	version map[string]int64
}

func newSpecTracker() *specTracker { return &specTracker{version: map[string]int64{}} }

// accept records v and reports whether it is newer than the last one seen.
func (t *specTracker) accept(sessionID string, v int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v <= t.version[sessionID] {
		return false
	}
	t.version[sessionID] = v
	return true
}

func (t *specTracker) get(sessionID string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.version[sessionID]
}

func (t *specTracker) forget(sessionID string) {
	t.mu.Lock()
	delete(t.version, sessionID)
	t.mu.Unlock()
}

// bypassUnavailable reports whether accept-all (bypassPermissions) cannot run:
// the claude CLI refuses it for root unless IS_SANDBOX is set.
func bypassUnavailable() bool {
	return os.Geteuid() == 0 && os.Getenv("IS_SANDBOX") == ""
}

// applyPermissionSpec applies a desired permission mode to a live session and
// returns the resulting applyState and a failure reason.
func applyPermissionSpec(sess *session.Session, mode string) (state, reason string) {
	if sess.Protocol == nil {
		return applyStateFailed, "session protocol not ready"
	}
	acp, isACP := sess.Protocol.GetAdapter().(*protocol.ACPAdapter)
	if sess.CLIType != "claude" || !isACP {
		// Engines that bake the mode into env/args at spawn cannot change it
		// in place; the user confirms a stop->resume rebuild.
		return applyStateRestartRequired, ""
	}
	if mode == "accept-all" && bypassUnavailable() {
		return applyStateFailed, "bypassPermissions is unavailable when the bridge runs as root"
	}
	if err := acp.SetMode(session.ClaudeACPModeID(mode), mode); err != nil {
		return applyStateFailed, err.Error()
	}
	sess.PermissionMode = mode
	return applyStateApplied, ""
}

// handleSessionSpecUpdate handles session:spec_update (web -> bridge).
func (b *Bridge) handleSessionSpecUpdate(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	sessionID, _ := payload["sessionId"].(string)
	mode, _ := payload["permissionMode"].(string)
	version := int64(0)
	if f, ok := payload["specVersion"].(float64); ok {
		version = int64(f)
	}
	sess := b.sessions.Get(sessionID)
	if sess == nil {
		b.logDebug("[%s] spec_update for unknown session %s", logger.ModSession, sessionID)
		return
	}
	if !b.specTracker().accept(sessionID, version) {
		// Stale or duplicate: do not apply, just restate the truth.
		b.sendSessionStatus(sess, applyStateApplied, "")
		return
	}
	state, reason := applyStateApplied, ""
	if mode != "" && mode != sess.PermissionMode {
		state, reason = applyPermissionSpec(sess, mode)
	}
	b.sendSessionStatus(sess, state, reason)
}

// sendSessionStatus reports the actual (not the desired) state of a session.
func (b *Bridge) sendSessionStatus(sess *session.Session, applyState, reason string) {
	payload := map[string]interface{}{
		"sessionId":          sess.ID,
		"deviceId":           b.config.DeviceID,
		"appliedSpecVersion": b.specTracker().get(sess.ID),
		"permissionMode":     sess.PermissionMode,
		"applyState":         applyState,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if err := b.sendMessage(Message{Type: "session:status", Payload: payload, Timestamp: time.Now().UnixMilli()}); err != nil {
		b.logDebug("[%s] session:status send failed: %v", logger.ModSession, err)
	}
}

// sendAllSessionStatus re-reports every running session (after a reconnect).
func (b *Bridge) sendAllSessionStatus() {
	for _, sess := range b.sessions.List() {
		if sess.Status == "active" {
			b.sendSessionStatus(sess, applyStateApplied, "")
		}
	}
}

// reportEngineMode turns an engine-pushed mode change (current_mode_update or
// config_option_update) into a status report.
func (b *Bridge) reportEngineMode(sessionID string, msg protocol.Message) {
	sess := b.sessions.Get(sessionID)
	if sess == nil || sess.CLIType != "claude" {
		return
	}
	modeID, _ := msg.Meta["modeId"].(string)
	if modeID == "" {
		modeID = modeFromConfigOptions(msg.Meta["configOptions"])
	}
	if modeID == "" {
		return
	}
	actual, known := session.BridgeModeFromACP(modeID)
	if !known {
		return
	}
	sess.PermissionMode = actual
	b.sendSessionStatus(sess, applyStateApplied, "")
}

// modeFromConfigOptions finds the current value of the "mode" config option.
func modeFromConfigOptions(raw interface{}) string {
	opts, _ := raw.([]interface{})
	for _, o := range opts {
		m, _ := o.(map[string]interface{})
		if m == nil {
			continue
		}
		if id, _ := m["id"].(string); id == "mode" {
			if v, ok := m["currentValue"].(string); ok {
				return v
			}
		}
		if cat, _ := m["category"].(string); cat == "mode" {
			if v, ok := m["currentValue"].(string); ok {
				return v
			}
		}
	}
	return ""
}

// specTracker returns the tracker, creating it on first use so Bridge values
// built without the constructor (tests) still work.
func (b *Bridge) specTracker() *specTracker {
	b.specsOnce.Do(func() {
		if b.specs == nil {
			b.specs = newSpecTracker()
		}
	})
	return b.specs
}
