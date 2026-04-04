package bridge

import (
	"testing"
)

func TestMessageBuffer_PushAndCount(t *testing.T) {
	buf := NewMessageBuffer(5)
	if buf.Count() != 0 {
		t.Fatalf("expected 0, got %d", buf.Count())
	}

	buf.Push([]byte("msg1"), 1)
	buf.Push([]byte("msg2"), 2)
	if buf.Count() != 2 {
		t.Fatalf("expected 2, got %d", buf.Count())
	}
}

func TestMessageBuffer_RingOverflow(t *testing.T) {
	buf := NewMessageBuffer(3)

	buf.Push([]byte("msg1"), 1)
	buf.Push([]byte("msg2"), 2)
	buf.Push([]byte("msg3"), 3)
	buf.Push([]byte("msg4"), 4) // overwrites msg1

	if buf.Count() != 3 {
		t.Fatalf("expected 3, got %d", buf.Count())
	}

	// msg1 (ID 1) should be gone, msg2-4 should exist
	replay := buf.ReplayAfter(0)
	if len(replay) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(replay))
	}
	if replay[0].ID != 2 {
		t.Fatalf("expected first replay ID=2, got %d", replay[0].ID)
	}
}

func TestMessageBuffer_ReplayAfter(t *testing.T) {
	buf := NewMessageBuffer(10)

	buf.Push([]byte("msg1"), 1) // ID 1
	buf.Push([]byte("msg2"), 2) // ID 2
	buf.Push([]byte("msg3"), 3) // ID 3

	replay := buf.ReplayAfter(1)
	if len(replay) != 2 {
		t.Fatalf("expected 2, got %d", len(replay))
	}
	if replay[0].ID != 2 || replay[1].ID != 3 {
		t.Fatalf("expected IDs [2,3], got [%d,%d]", replay[0].ID, replay[1].ID)
	}
}

func TestMessageBuffer_ReplayAfterEmpty(t *testing.T) {
	buf := NewMessageBuffer(10)
	replay := buf.ReplayAfter(0)
	if replay != nil {
		t.Fatalf("expected nil, got %v", replay)
	}
}

func TestMessageBuffer_ReplayAfterAllAcknowledged(t *testing.T) {
	buf := NewMessageBuffer(10)
	buf.Push([]byte("msg1"), 1) // ID 1
	buf.Push([]byte("msg2"), 2) // ID 2

	replay := buf.ReplayAfter(2)
	if len(replay) != 0 {
		t.Fatalf("expected 0, got %d", len(replay))
	}
}

func TestMessageBuffer_SetLastAck(t *testing.T) {
	buf := NewMessageBuffer(10)
	buf.Push([]byte("msg1"), 1) // ID 1

	if buf.LastAck() != 0 {
		t.Fatalf("expected 0, got %d", buf.LastAck())
	}

	buf.SetLastAck(1)
	if buf.LastAck() != 1 {
		t.Fatalf("expected 1, got %d", buf.LastAck())
	}

	// Should not decrease
	buf.SetLastAck(0)
	if buf.LastAck() != 1 {
		t.Fatalf("expected 1 (no decrease), got %d", buf.LastAck())
	}
}

func TestMessageBuffer_PushReturnsIncrementingID(t *testing.T) {
	buf := NewMessageBuffer(10)

	id1 := buf.Push([]byte("a"), 1)
	id2 := buf.Push([]byte("b"), 2)
	id3 := buf.Push([]byte("c"), 3)

	if id1 != 1 || id2 != 2 || id3 != 3 {
		t.Fatalf("expected [1,2,3], got [%d,%d,%d]", id1, id2, id3)
	}
}

func TestCloseCodeClassification(t *testing.T) {
	b := &Bridge{}

	// Permanent codes
	permanentCodes := []int{1002, 1008, 4001, 4003}
	for _, code := range permanentCodes {
		if !b.isPermanentCloseCode(code) {
			t.Errorf("expected code %d to be permanent", code)
		}
	}

	// Temporary codes
	temporaryCodes := []int{1001, 1005, 1006}
	for _, code := range temporaryCodes {
		if !isTemporaryCloseCode(code) {
			t.Errorf("expected code %d to be temporary", code)
		}
	}

	// Unknown codes
	unknownCodes := []int{1011, 1012, 9999}
	for _, code := range unknownCodes {
		if b.isPermanentCloseCode(code) {
			t.Errorf("expected code %d to NOT be permanent", code)
		}
		if isTemporaryCloseCode(code) {
			t.Errorf("expected code %d to NOT be temporary", code)
		}
	}
}
