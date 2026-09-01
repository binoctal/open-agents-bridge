package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Recorder incrementally writes a replay script as raw wire frames cross
// the ACP adapter's stdio boundary. Recording is off by default: a nil
// *Recorder is valid and every method is a no-op, so the adapter hot path
// pays nothing when no --record-replay-dir was configured.
//
// While recording, the recorder derives `after` gates automatically: an
// out frame that carries an "id" is a JSON-RPC response, so it is gated on
// the method of the matching inbound request. That keeps the handshake
// order (initialize -> session/new -> session/prompt) stable across
// replays without any hand-editing of recorded scripts.
type Recorder struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	seq    int
	closed bool
	// pending maps a JSON-RPC request id (as decoded by probeRequestID) to
	// the method of the inbound request awaiting its response.
	pending map[any]string
}

// NewRecorder creates the script file and writes the header line.
func NewRecorder(path string, h Header) (*Recorder, error) {
	h.Kind = KindHeader
	if h.RecordedAt == "" {
		h.RecordedAt = nowUTC()
	}
	line, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("replay: marshal header: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("replay: create script: %w", err)
	}
	r := &Recorder{
		f:       f,
		w:       bufio.NewWriter(f),
		pending: make(map[any]string),
	}
	if _, err := r.w.Write(append(line, '\n')); err != nil {
		f.Close()
		return nil, fmt.Errorf("replay: write header: %w", err)
	}
	return r, nil
}

// Frame appends one wire frame to the script. dir is relative to the CLI
// process: DirectionIn for bytes written to its stdin, DirectionOut for
// lines read from its stdout. Nil-safe no-op.
func (r *Recorder) Frame(dir Direction, frame json.RawMessage) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("replay: recorder is closed")
	}

	fr := Frame{
		Kind:  KindFrame,
		Seq:   r.seq,
		Dir:   dir,
		Frame: frame,
	}
	if dir == DirectionIn {
		if method, id, ok := probeRequestMethod(frame); ok {
			r.pending[id] = method
		}
	} else if id, ok := probeResponseID(frame); ok {
		// A response is emitted only after the bridge sent the request it
		// answers; record that dependency as an `after` gate.
		if method, pending := r.pending[id]; pending {
			fr.After = method
			delete(r.pending, id)
		}
	}

	line, err := json.Marshal(fr)
	if err != nil {
		return fmt.Errorf("replay: marshal frame %d: %w", r.seq, err)
	}
	if _, err := r.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("replay: write frame %d: %w", r.seq, err)
	}
	// Flush per frame: recording is a diagnostic mode, and a bridge crash
	// mid-session must leave every frame already observed on disk.
	if err := r.w.Flush(); err != nil {
		return fmt.Errorf("replay: flush frame %d: %w", r.seq, err)
	}
	r.seq++
	return nil
}

// Close flushes and closes the script file. Nil-safe no-op; subsequent
// Frame calls on a closed recorder return an error.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if err := r.w.Flush(); err != nil {
		r.f.Close()
		return fmt.Errorf("replay: flush script: %w", err)
	}
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("replay: close script: %w", err)
	}
	return nil
}

// probeRequestMethod extracts the request method and id from an inbound
// frame. Returns ok=false for notifications (no id) and non-requests.
func probeRequestMethod(frame json.RawMessage) (method string, id any, ok bool) {
	var probe struct {
		Method string `json:"method"`
		ID     any    `json:"id"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil || probe.Method == "" {
		return "", nil, false
	}
	if probe.ID == nil {
		return "", nil, false // notification, nothing to gate a response on
	}
	return probe.Method, normalizedID(probe.ID), true
}

// probeResponseID extracts the id of an outbound response frame. Returns
// ok=false for notifications and server-initiated requests.
func probeResponseID(frame json.RawMessage) (id any, ok bool) {
	var probe struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return nil, false
	}
	if probe.ID == nil || probe.Method != "" {
		// A frame with a method is a request/notification from the CLI
		// (e.g. session/update, fs/read), not a response to gate.
		return nil, false
	}
	return normalizedID(probe.ID), true
}

// normalizedID makes request/response ids comparable regardless of whether
// the JSON encoded the id as a number or a string (both are legal in
// JSON-RPC 2.0 and the two sides of a recording may differ).
func normalizedID(v any) any {
	if s, isStr := v.(string); isStr {
		return "s:" + s
	}
	if f, isNum := v.(float64); isNum {
		return f
	}
	return fmt.Sprintf("raw:%v", v)
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
