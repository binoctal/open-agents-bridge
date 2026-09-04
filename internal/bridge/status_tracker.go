package bridge

import (
	"sync"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// G18: the AgentStatus vocabulary already existed on the wire and in the web
// UI, but nothing produced it — the PTY path only reported idle on process
// exit, and ACP label updates (mode_update & friends) traveled as strings the
// web mapped back to idle. This tracker is the first real producer: it derives
// a session's status from the protocol message stream, protocol-agnostically,
// with no per-CLI output parsing (the [QUESTION] detector stays the only
// pattern matcher and is reused, not duplicated).

// statusDwell throttles *backward* active-state transitions (streaming back
// to thinking, tool_executing back to streaming/thinking). Forward progress
// (thinking → streaming → tool_executing) reports immediately so a short PTY
// burst still surfaces the full sequence; the flap risk is thought-chunks and
// content-chunks alternating, which is exactly the backward direction.
const statusDwell = time.Second

// activeStatusRank orders the active states along the "progress" axis.
// Higher rank = further along a turn. Backward moves (lower rank) are subject
// to the dwell throttle; permission_pending and idle are always immediate.
var activeStatusRank = map[protocol.AgentStatus]int{
	protocol.StatusThinking:      1,
	protocol.StatusStreaming:     2,
	protocol.StatusToolExecuting: 3,
}

// statusTracker is per-session state deriving AgentStatus transitions.
type statusTracker struct {
	mu          sync.Mutex
	reported    protocol.AgentStatus // last value sent to the web
	hasReported bool
	lastReport  time.Time // when the current reported value was sent
}

// observe maps one protocol message to a status transition. It returns the
// new status and whether it should be reported (a real transition that
// survived the dwell throttle and the post-idle straggler window).
func (t *statusTracker) observe(msg protocol.Message) (protocol.AgentStatus, bool) {
	next, ok := statusFromMessage(msg)
	if !ok {
		return t.reported, false
	}
	return t.transition(next, false)
}

// observePrompt reports thinking right after a prompt was handed to the CLI.
// Forced: a new prompt always restarts the turn, bypassing the post-idle
// straggler suppression.
func (t *statusTracker) observePrompt() (protocol.AgentStatus, bool) {
	return t.transition(protocol.StatusThinking, true)
}

// current returns the last reported status (idle before anything reported).
func (t *statusTracker) current() protocol.AgentStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.hasReported {
		return protocol.StatusIdle
	}
	return t.reported
}

func (t *statusTracker) transition(next protocol.AgentStatus, force bool) (protocol.AgentStatus, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.hasReported && next == t.reported {
		return t.reported, false
	}
	if t.hasReported && !force {
		_, nextActive := activeStatusRank[next]
		// A backward move within the active states inside the dwell window
		// is a flap — keep showing the more advanced state.
		nextRank, _ := activeStatusRank[next]
		curRank, curActive := activeStatusRank[t.reported]
		if nextActive && curActive && nextRank < curRank && now.Sub(t.lastReport) < statusDwell {
			return t.reported, false
		}
		// Straggler after idle: ACP agents can emit the prompt response
		// (stopReason → idle) ahead of their final session/update chunks, so
		// content arriving just after an idle report belongs to the turn
		// that already ended. Swallow it or the session sticks in streaming
		// forever (live-observed on staging with opencode). A forced
		// transition (new prompt) always passes.
		if nextActive && t.reported == protocol.StatusIdle && now.Sub(t.lastReport) < statusDwell {
			return t.reported, false
		}
	}
	t.reported = next
	t.hasReported = true
	t.lastReport = now
	return next, true
}

// statusFromMessage maps a protocol message to the status it implies. Only
// structured signals count: a MessageTypeStatus whose Content is a typed
// AgentStatus is a lifecycle signal (PTY exit, ACP turn end); a plain-string
// Content is a label-class update (mode/commands/config/session_info) and
// carries no status at all — see statusLabel.
func statusFromMessage(msg protocol.Message) (protocol.AgentStatus, bool) {
	switch msg.Type {
	case protocol.MessageTypeContent:
		return protocol.StatusStreaming, true
	case protocol.MessageTypeThought:
		return protocol.StatusThinking, true
	case protocol.MessageTypeToolCall:
		return protocol.StatusToolExecuting, true
	case protocol.MessageTypePermission:
		return protocol.StatusPermissionPending, true
	case protocol.MessageTypeStatus:
		if s, ok := msg.Content.(protocol.AgentStatus); ok {
			return s, true
		}
		return "", false
	}
	return "", false
}

// statusLabel extracts a label-class update ("commands_update", "mode_update",
// ...) from a status message. Empty when the message carries a real status.
func statusLabel(msg protocol.Message) (string, bool) {
	if msg.Type != protocol.MessageTypeStatus {
		return "", false
	}
	if s, ok := msg.Content.(string); ok {
		return s, true
	}
	return "", false
}

// ── Bridge plumbing ─────────────────────────────────────────────────────────

// statusTrackerFor returns (creating on demand) the tracker for a session.
func (b *Bridge) statusTrackerFor(sessionID string) *statusTracker {
	b.statusTrackersMu.Lock()
	defer b.statusTrackersMu.Unlock()
	t, ok := b.statusTrackers[sessionID]
	if !ok {
		t = &statusTracker{}
		b.statusTrackers[sessionID] = t
	}
	return t
}

// removeStatusTracker drops a session's tracker. Registered as the session
// manager's removed-callback so every delete path (stop, replace, idle
// cleanup, create-failure rollback) cleans up — the map must not leak.
func (b *Bridge) removeStatusTracker(sessionID string) {
	b.statusTrackersMu.Lock()
	defer b.statusTrackersMu.Unlock()
	delete(b.statusTrackers, sessionID)
}

// observeSessionStatus runs every forwarded protocol message through the
// session's tracker and broadcasts agent:status on a real transition.
func (b *Bridge) observeSessionStatus(sessionID, protocolName string, msg protocol.Message) {
	if s, changed := b.statusTrackerFor(sessionID).observe(msg); changed {
		b.sendStatus(sessionID, protocolName, s, "")
	}
}

// notePromptSent reports thinking after a prompt was successfully delivered
// to the CLI. This is the server-side fact the web used to fake locally.
func (b *Bridge) notePromptSent(sessionID, protocolName string) {
	if s, changed := b.statusTrackerFor(sessionID).observePrompt(); changed {
		b.sendStatus(sessionID, protocolName, s, "")
	}
}

// sendStatusLabel forwards a label-class update: the status field stays a
// valid enum value (the tracker's current one) and the label rides in detail.
func (b *Bridge) sendStatusLabel(sessionID, protocolName, label string) {
	b.sendStatus(sessionID, protocolName, b.statusTrackerFor(sessionID).current(), label)
}

// sendStatus is the single agent:status emitter. status MUST be a valid
// AgentStatus enum value — never a label string (the web maps unknown values
// to nothing and old builds mapped them to idle, resetting the UI).
func (b *Bridge) sendStatus(sessionID, protocolName string, status protocol.AgentStatus, detail string) {
	payload := map[string]interface{}{
		"sessionId": sessionID,
		"deviceId":  b.config.DeviceID,
		"status":    status,
		"protocol":  protocolName,
	}
	if detail != "" {
		payload["detail"] = detail
	}
	b.sendMessage(Message{
		Type:      "agent:status",
		Payload:   payload,
		Timestamp: time.Now().UnixMilli(),
	})
	logger.Debug("[%s] agent status -> %s (session %s)", logger.ModBridge, status, sessionID)
}
