package replay

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.jsonl")
	r, err := NewRecorder(path, Header{CLIType: "claude", AdapterVersion: "0.6.2"})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return r, path
}

func TestRecorderWritesHeaderAndDenseFrames(t *testing.T) {
	r, path := newTestRecorder(t)
	if err := r.Frame(DirectionIn, []byte(`{"id":1,"method":"initialize"}`)); err != nil {
		t.Fatalf("frame in: %v", err)
	}
	if err := r.Frame(DirectionOut, []byte(`{"id":1,"result":{}}`)); err != nil {
		t.Fatalf("frame out: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	script, err := LoadScript(path)
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	if script.Header.CLIType != "claude" {
		t.Errorf("CLIType = %q", script.Header.CLIType)
	}
	if script.Header.RecordedAt == "" {
		t.Error("RecordedAt should default to now, got empty")
	}
	if len(script.Frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(script.Frames))
	}
	if script.Frames[0].Seq != 0 || script.Frames[1].Seq != 1 {
		t.Errorf("seq = %d,%d, want 0,1", script.Frames[0].Seq, script.Frames[1].Seq)
	}
	// Round-trip: what the recorder wrote must reload without validation
	// errors — writer and reader share one contract.
}

func TestRecorderDerivesAfterGateFromResponseID(t *testing.T) {
	r, path := newTestRecorder(t)
	mustFrame := func(dir Direction, raw string) {
		t.Helper()
		if err := r.Frame(dir, []byte(raw)); err != nil {
			t.Fatalf("frame %s: %v", raw, err)
		}
	}
	mustFrame(DirectionIn, `{"id":"bridge_1","method":"session/prompt","params":{}}`)
	mustFrame(DirectionOut, `{"jsonrpc":"2.0","method":"session/update","params":{}}`) // notification: no gate
	mustFrame(DirectionOut, `{"id":"bridge_1","result":{"stopReason":"end_turn"}}`)    // response: gated
	mustFrame(DirectionOut, `{"id":999,"result":{}}`)                                  // unmatched id: no gate
	mustFrame(DirectionOut, `{"id":"bridge_1","method":"fs/read","params":{}}`)        // server request: no gate
	r.Close()

	script, err := LoadScript(path)
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	want := []string{"", "", "session/prompt", "", ""}
	for i, fr := range script.Frames {
		if fr.After != want[i] {
			t.Errorf("frame %d after = %q, want %q", i, fr.After, want[i])
		}
	}
}

func TestRecorderCloseIsIdempotentAndSealsFrames(t *testing.T) {
	r, path := newTestRecorder(t)
	if err := r.Frame(DirectionIn, []byte(`{}`)); err != nil {
		t.Fatalf("frame: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := r.Frame(DirectionIn, []byte(`{}`)); err == nil {
		t.Error("frame after close must error")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 2 lines: header + 1 frame; everything must be flushed by Close.
	if got := len(splitLines(string(data))); got != 2 {
		t.Errorf("script lines = %d, want 2", got)
	}
}

func TestNilRecorderIsNoOp(t *testing.T) {
	// Recording is off by default: a nil *Recorder must be callable with
	// zero setup and zero effect — that is the adapter's default state.
	var r *Recorder
	if err := r.Frame(DirectionIn, []byte(`{}`)); err != nil {
		t.Errorf("nil Frame: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

func TestNewRecorderFailsOnBadPath(t *testing.T) {
	if _, err := NewRecorder(filepath.Join(t.TempDir(), "missing-dir", "s.jsonl"), Header{}); err == nil {
		t.Error("expected error for unwritable path")
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
