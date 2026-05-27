package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_CreateAndGetSession(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	h := s.CreateSession("s1", "d1", "claude", "/home/user/project")
	if h.SessionID != "s1" {
		t.Errorf("expected s1, got %s", h.SessionID)
	}
	if h.DeviceID != "d1" {
		t.Errorf("expected d1, got %s", h.DeviceID)
	}
	if h.CLIType != "claude" {
		t.Errorf("expected claude, got %s", h.CLIType)
	}

	got := s.GetSession("s1")
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.SessionID != "s1" {
		t.Errorf("expected s1, got %s", got.SessionID)
	}
}

func TestStore_AddAndGetMessages(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.AddMessage("s1", Message{ID: "m1", Role: "user", Content: "hello"})
	s.AddMessage("s1", Message{ID: "m2", Role: "assistant", Content: "hi"})

	msgs := s.GetMessages("s1", 0)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "hello" {
		t.Errorf("expected hello, got %s", msgs[0].Content)
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("expected assistant, got %s", msgs[1].Role)
	}
}

func TestStore_GetMessages_WithLimit(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.AddMessage("s1", Message{ID: "m1", Role: "user", Content: "1"})
	s.AddMessage("s1", Message{ID: "m2", Role: "user", Content: "2"})
	s.AddMessage("s1", Message{ID: "m3", Role: "user", Content: "3"})

	msgs := s.GetMessages("s1", 2)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Content != "2" {
		t.Errorf("expected 2, got %s", msgs[0].Content)
	}
}

func TestStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir)
	s1.CreateSession("s1", "d1", "claude", "/tmp")
	s1.AddMessage("s1", Message{ID: "m1", Role: "user", Content: "persist me"})

	// Verify file was written
	data, err := os.ReadFile(filepath.Join(dir, "s1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("expected non-empty file")
	}

	// Load from same dir
	s2, _ := NewStore(dir)
	got := s2.GetSession("s1")
	if got == nil {
		t.Fatal("expected session loaded from disk, got nil")
	}
	msgs := s2.GetMessages("s1", 0)
	if len(msgs) != 1 || msgs[0].Content != "persist me" {
		t.Errorf("expected persisted message, got %v", msgs)
	}
}

func TestStore_ListSessions(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)
	s.CreateSession("s1", "d1", "claude", "/a")
	s.CreateSession("s2", "d1", "gemini", "/b")

	list := s.ListSessions()
	if len(list) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(list))
	}
}

func TestStore_GetMessages_Nonexistent(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	msgs := s.GetMessages("nope", 0)
	if msgs != nil {
		t.Error("expected nil for nonexistent session")
	}
}

func TestStore_AutoTimestamp(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	before := time.Now()
	s.AddMessage("s1", Message{ID: "m1", Role: "user", Content: "hi"})
	after := time.Now()

	msgs := s.GetMessages("s1", 0)
	if msgs[0].Timestamp.Before(before) || msgs[0].Timestamp.After(after) {
		t.Error("expected auto-populated timestamp")
	}
}
