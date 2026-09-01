package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Channel of a golden event: where the uplink message left the bridge.
const (
	ChannelWS       = "ws"       // WebSocket message to the orchestrator
	ChannelCallback = "callback" // HTTP callback to the orchestrator API
)

// KindEvent is the entry kind of golden sequence lines.
const KindEvent = "event"

// GoldenEvent is one bridge uplink event — a WS message or an HTTP callback
// — recorded in merge order. The golden sequence is the second face of a
// recorded session (the ACP script is the first); replay asserts the bridge
// still produces this sequence, modulo unstable fields.
type GoldenEvent struct {
	Kind    string          `json:"kind"`
	Seq     int             `json:"seq"`
	Channel string          `json:"channel"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Golden is a loaded golden sequence file.
type Golden struct {
	Header Header
	Events []GoldenEvent
}

// GoldenRecorder writes the merged uplink sequence. Nil-safe: a nil
// *GoldenRecorder is a no-op, so hooks can stay unconditionally in the
// production path with zero cost when recording is off.
type GoldenRecorder struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	seq    int
	closed bool
}

// NewGoldenRecorder creates the golden file and writes its header line.
func NewGoldenRecorder(path string, h Header) (*GoldenRecorder, error) {
	h.Kind = KindHeader
	if h.RecordedAt == "" {
		h.RecordedAt = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("replay: marshal golden header: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("replay: create golden: %w", err)
	}
	g := &GoldenRecorder{f: f, w: bufio.NewWriter(f)}
	if _, err := g.w.Write(append(line, '\n')); err != nil {
		f.Close()
		return nil, fmt.Errorf("replay: write golden header: %w", err)
	}
	if err := g.w.Flush(); err != nil {
		f.Close()
		return nil, fmt.Errorf("replay: flush golden header: %w", err)
	}
	return g, nil
}

// Event appends one uplink event. payload is marshaled as-is. Nil-safe.
func (g *GoldenRecorder) Event(channel, typ string, payload any) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("replay: golden recorder is closed")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("replay: marshal golden event %d: %w", g.seq, err)
	}
	ev := GoldenEvent{Kind: KindEvent, Seq: g.seq, Channel: channel, Type: typ, Payload: raw}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("replay: marshal golden event %d: %w", g.seq, err)
	}
	if _, err := g.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("replay: write golden event %d: %w", g.seq, err)
	}
	// Flush per event, same crash-durability rationale as the frame recorder.
	if err := g.w.Flush(); err != nil {
		return fmt.Errorf("replay: flush golden event %d: %w", g.seq, err)
	}
	g.seq++
	return nil
}

// Close flushes and closes the golden file. Nil-safe, idempotent.
func (g *GoldenRecorder) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	g.closed = true
	if err := g.w.Flush(); err != nil {
		g.f.Close()
		return fmt.Errorf("replay: flush golden: %w", err)
	}
	return g.f.Close()
}

// LoadGolden reads and validates a golden sequence file.
func LoadGolden(path string) (*Golden, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open golden: %w", err)
	}
	defer f.Close()

	var g Golden
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &probe); err != nil {
			return nil, fmt.Errorf("replay: golden line %d: not JSON: %w", line, err)
		}
		switch probe.Kind {
		case KindHeader:
			if line != 1 {
				return nil, fmt.Errorf("replay: golden line %d: header must be first", line)
			}
			if err := json.Unmarshal(scanner.Bytes(), &g.Header); err != nil {
				return nil, fmt.Errorf("replay: golden line %d: bad header: %w", line, err)
			}
		case KindEvent:
			var ev GoldenEvent
			if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
				return nil, fmt.Errorf("replay: golden line %d: bad event: %w", line, err)
			}
			if ev.Channel != ChannelWS && ev.Channel != ChannelCallback {
				return nil, fmt.Errorf("replay: golden line %d: invalid channel %q", line, ev.Channel)
			}
			if ev.Seq != len(g.Events) {
				return nil, fmt.Errorf("replay: golden line %d: seq %d, want %d", line, ev.Seq, len(g.Events))
			}
			g.Events = append(g.Events, ev)
		default:
			return nil, fmt.Errorf("replay: golden line %d: unknown kind %q", line, probe.Kind)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("replay: read golden: %w", err)
	}
	if g.Header.Kind != KindHeader {
		return nil, fmt.Errorf("replay: golden missing header")
	}
	return &g, nil
}
