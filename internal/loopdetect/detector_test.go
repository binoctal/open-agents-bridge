package loopdetect

import "testing"

func TestRecord_NoRepetition(t *testing.T) {
	d := New(10, 3, 5)
	r := d.Record("tool_a", "hash1")
	if r.Level != None {
		t.Errorf("expected None, got %d", r.Level)
	}
}

func TestRecord_WarningThreshold(t *testing.T) {
	d := New(10, 3, 5)
	d.Record("tool_a", "hash1")
	d.Record("tool_a", "hash1")
	r := d.Record("tool_a", "hash1")
	if r.Level != Warning {
		t.Errorf("expected Warning, got %d", r.Level)
	}
}

func TestRecord_CriticalThreshold(t *testing.T) {
	d := New(10, 3, 5)
	for i := 0; i < 5; i++ {
		d.Record("tool_a", "hash1")
	}
	r := d.Record("tool_a", "hash1")
	if r.Level != Critical {
		t.Errorf("expected Critical, got %d", r.Level)
	}
}

func TestRecord_DifferentTools(t *testing.T) {
	d := New(10, 3, 5)
	d.Record("tool_a", "hash1")
	d.Record("tool_b", "hash2")
	d.Record("tool_a", "hash1")
	r := d.Record("tool_c", "hash3")
	if r.Level != None {
		t.Errorf("expected None for different tools, got %d", r.Level)
	}
}

func TestRecord_Reset(t *testing.T) {
	d := New(10, 3, 5)
	for i := 0; i < 3; i++ {
		d.Record("tool_a", "hash1")
	}
	d.Reset()
	r := d.Record("tool_a", "hash1")
	if r.Level != None {
		t.Errorf("expected None after reset, got %d", r.Level)
	}
}

func TestRecord_PingPongPattern(t *testing.T) {
	d := New(10, 3, 5)
	// A -> B -> A -> B -> A -> B triggers ping-pong
	d.Record("tool_a", "h1")
	d.Record("tool_b", "h2")
	d.Record("tool_a", "h1")
	d.Record("tool_b", "h2")
	d.Record("tool_a", "h1")
	r := d.Record("tool_b", "h2")
	if r.Level != Warning {
		t.Errorf("expected Warning for ping-pong, got %d", r.Level)
	}
}

func TestRecord_WindowOverflow(t *testing.T) {
	d := New(5, 3, 5)
	// Fill window beyond capacity
	for i := 0; i < 8; i++ {
		d.Record("tool_a", "hash1")
	}
	// Should detect critical even with circular buffer
	r := d.Record("tool_a", "hash1")
	if r.Level < Warning {
		t.Errorf("expected at least Warning, got %d", r.Level)
	}
}
