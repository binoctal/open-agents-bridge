package bridge

import (
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/protocol"
)

// Content batching (staging forensics 2026-09-23, "two sessions show
// incoherent dialogue"): batches used to key on sessionID alone while
// carrying a single msgType, so interleaved agent_thought_chunk and
// agent_message_chunk streams merged into one flush labelled with the
// first type — the agent's actual reply went out as chat:thought and the
// chat bubble never got a chat:response. A markdown-split remainder could
// also outlive its flush timer (never re-armed), surfacing minutes later
// under an unrelated session's activity.

func newBatchBridge(t *testing.T) *Bridge {
	t.Helper()
	return &Bridge{
		config:   &config.Config{DeviceID: "dev-batch"},
		batchBuf: make(map[string]*contentBatch),
	}
}

// batchedOfType returns the concatenated offline content of all flushed
// frames of one type.
func batchedOfType(t *testing.T, b *Bridge, msgType string) string {
	t.Helper()
	var sb strings.Builder
	for _, m := range offlineMessages(t, b) {
		if m.Type != msgType {
			continue
		}
		payload, ok := m.Payload.(map[string]interface{})
		if !ok {
			t.Fatalf("%s payload is %T, want map", msgType, m.Payload)
		}
		sb.WriteString(payload["content"].(string))
	}
	return sb.String()
}

// Thought chunks followed by reply chunks in the same flush window must
// produce TWO frames of the right types — not one merged chat:thought.
func TestBatchSeparatesThoughtAndContent(t *testing.T) {
	b := newBatchBridge(t)

	b.batchContent("s1", "acp", "The user wants a file. ", protocol.MessageTypeThought)
	b.batchContent("s1", "acp", "Created permtest.txt", protocol.MessageTypeContent)
	b.doFlushLocked()

	if got := batchedOfType(t, b, "chat:thought"); got != "The user wants a file. " {
		t.Fatalf("thought frame content = %q", got)
	}
	if got := batchedOfType(t, b, "chat:response"); got != "Created permtest.txt" {
		t.Fatalf("response frame content = %q, want the reply as its own chat:response", got)
	}
	// The reply must not leak into the thought frame either.
	if strings.Contains(batchedOfType(t, b, "chat:thought"), "permtest") {
		t.Fatal("reply content merged into the thought frame")
	}
}

// Different sessions never shared a batch, and still must not.
func TestBatchSeparatesSessions(t *testing.T) {
	b := newBatchBridge(t)

	b.batchContent("s1", "acp", "from-one", protocol.MessageTypeContent)
	b.batchContent("s2", "acp", "from-two", protocol.MessageTypeContent)
	b.doFlushLocked()

	// Find each session's own frame with its own content.
	sawOne, sawTwo := false, false
	for _, m := range offlineMessages(t, b) {
		if m.Type != "chat:response" {
			continue
		}
		payload := m.Payload.(map[string]interface{})
		if payload["sessionId"] == "s1" && payload["content"] == "from-one" {
			sawOne = true
		}
		if payload["sessionId"] == "s2" && payload["content"] == "from-two" {
			sawTwo = true
		}
	}
	if !sawOne || !sawTwo {
		t.Fatalf("per-session frames missing: s1=%v s2=%v", sawOne, sawTwo)
	}
}

// A flush that leaves a remainder (markdown split) must re-arm the timer:
// the stale content may not wait for an unrelated session's next batch.
func TestFlushRearmsTimerForRemainder(t *testing.T) {
	b := newBatchBridge(t)
	// "before-fence" ends with a newline so the cut lands before the
	// unclosed code fence; everything from the fence on is the remainder.
	b.batchContent("s1", "acp", "before-fence\n```\ncode-no-close", protocol.MessageTypeContent)

	b.batchMu.Lock()
	b.doFlushLocked()
	rearmed := b.batchTimer != nil
	b.batchMu.Unlock()

	if !rearmed {
		t.Fatal("flush left a remainder but did not re-arm the batch timer")
	}

	// And the re-armed flush drains the remainder on its own: stop the
	// timer (as Stop() does) and flush manually — no content may be lost.
	b.batchMu.Lock()
	if b.batchTimer != nil {
		if b.batchTimer.Stop() {
			b.batchWait.Done()
		}
		b.doFlushLocked()
		b.batchTimer = nil
	}
	drained := len(b.batchBuf)
	b.batchMu.Unlock()

	if drained != 0 {
		t.Fatalf("batchBuf still holds %d batch(es) after final flush", drained)
	}
	merged := batchedOfType(t, b, "chat:response")
	if !strings.Contains(merged, "before-fence") || !strings.Contains(merged, "code-no-close") {
		t.Fatalf("content lost across the split flushes: %q", merged)
	}
}

// Empty flush (no batches) must not re-arm — the timer idles again.
func TestFlushIdlesWhenEmpty(t *testing.T) {
	b := newBatchBridge(t)

	// flushBatches is normally a timer callback owning one batchWait count;
	// simulate that ownership so its Done() stays balanced. It takes batchMu
	// itself, so call it WITHOUT holding the lock.
	b.batchWait.Add(1)
	b.flushBatches()

	b.batchMu.Lock()
	idle := b.batchTimer == nil
	b.batchMu.Unlock()
	if !idle {
		t.Fatal("timer re-armed with nothing pending")
	}
}
