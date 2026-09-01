package replay

import (
	"path/filepath"
	"testing"
)

func TestGoldenRecorderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uplink.golden.jsonl")
	g, err := NewGoldenRecorder(path, Header{CLIType: "bridge", AdapterVersion: "test"})
	if err != nil {
		t.Fatalf("NewGoldenRecorder: %v", err)
	}
	if err := g.Event(ChannelWS, "workflow:task_started", map[string]any{"taskId": "t1"}); err != nil {
		t.Fatalf("event ws: %v", err)
	}
	if err := g.Event(ChannelCallback, "workflow:task_result", map[string]any{"taskId": "t1"}); err != nil {
		t.Fatalf("event callback: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := g.Event(ChannelWS, "late", nil); err == nil {
		t.Error("event after close must error")
	}

	golden, err := LoadGolden(path)
	if err != nil {
		t.Fatalf("LoadGolden: %v", err)
	}
	if len(golden.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(golden.Events))
	}
	if golden.Events[0].Channel != ChannelWS || golden.Events[0].Type != "workflow:task_started" {
		t.Errorf("event 0 = %s/%s", golden.Events[0].Channel, golden.Events[0].Type)
	}
	if golden.Events[1].Channel != ChannelCallback || golden.Events[1].Type != "workflow:task_result" {
		t.Errorf("event 1 = %s/%s", golden.Events[1].Channel, golden.Events[1].Type)
	}
	// Merge order is the point of the file: seq must be dense.
	if golden.Events[1].Seq != 1 {
		t.Errorf("event 1 seq = %d, want 1", golden.Events[1].Seq)
	}
}

func TestNilGoldenRecorderIsNoOp(t *testing.T) {
	var g *GoldenRecorder
	if err := g.Event(ChannelWS, "workflow:task_started", nil); err != nil {
		t.Errorf("nil Event: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

func TestLoadGoldenRejectsBadChannel(t *testing.T) {
	path := writeScript(t,
		`{"kind":"header","cliType":"bridge"}`,
		`{"kind":"event","seq":0,"channel":"carrier-pigeon","type":"x","payload":{}}`,
	)
	if _, err := LoadGolden(path); err == nil {
		t.Error("expected error for invalid channel")
	}
}
