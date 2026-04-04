package test

import (
	"os"
	"testing"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/storage"
)

func TestStorageCreateSession(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := storage.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	session := store.CreateSession("session_1", "device_1", "kiro", "/home/user/project")

	if session.SessionID != "session_1" {
		t.Errorf("SessionID = %s, want session_1", session.SessionID)
	}
	if session.CLIType != "kiro" {
		t.Errorf("CLIType = %s, want kiro", session.CLIType)
	}

	// Verify file was created
	files, _ := os.ReadDir(tmpDir)
	if len(files) != 1 {
		t.Errorf("Expected 1 file, got %d", len(files))
	}
}

func TestStorageAddMessage(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := storage.NewStore(tmpDir)

	store.CreateSession("session_1", "device_1", "kiro", "/project")

	store.AddMessage("session_1", storage.Message{
		ID:      "msg_1",
		Role:    "user",
		Content: "Hello",
	})

	store.AddMessage("session_1", storage.Message{
		ID:      "msg_2",
		Role:    "assistant",
		Content: "Hi there!",
	})

	messages := store.GetMessages("session_1", 10)
	if len(messages) != 2 {
		t.Errorf("Expected 2 messages, got %d", len(messages))
	}

	if messages[0].Content != "Hello" {
		t.Errorf("First message content = %s, want Hello", messages[0].Content)
	}
}

func TestStorageGetMessagesLimit(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := storage.NewStore(tmpDir)

	store.CreateSession("session_1", "device_1", "kiro", "/project")

	for i := 0; i < 10; i++ {
		store.AddMessage("session_1", storage.Message{
			ID:      "msg_" + string(rune('0'+i)),
			Role:    "user",
			Content: "Message",
		})
	}

	messages := store.GetMessages("session_1", 3)
	if len(messages) != 3 {
		t.Errorf("Expected 3 messages, got %d", len(messages))
	}
}

func TestStoragePersistence(t *testing.T) {
	tmpDir := t.TempDir()

	// Create and populate store
	store1, _ := storage.NewStore(tmpDir)
	store1.CreateSession("session_1", "device_1", "kiro", "/project")
	store1.AddMessage("session_1", storage.Message{
		ID:        "msg_1",
		Role:      "user",
		Content:   "Persisted message",
		Timestamp: time.Now(),
	})

	// Create new store instance (simulates restart)
	store2, _ := storage.NewStore(tmpDir)

	session := store2.GetSession("session_1")
	if session == nil {
		t.Fatal("Session not loaded from disk")
	}

	messages := store2.GetMessages("session_1", 10)
	if len(messages) != 1 {
		t.Errorf("Expected 1 message, got %d", len(messages))
	}

	if messages[0].Content != "Persisted message" {
		t.Errorf("Message content = %s, want 'Persisted message'", messages[0].Content)
	}
}

func TestStorageListSessions(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := storage.NewStore(tmpDir)

	store.CreateSession("session_1", "device_1", "kiro", "/project1")
	store.CreateSession("session_2", "device_1", "cline", "/project2")

	sessions := store.ListSessions()
	if len(sessions) != 2 {
		t.Errorf("Expected 2 sessions, got %d", len(sessions))
	}
}

func TestStorageNonExistentSession(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := storage.NewStore(tmpDir)

	session := store.GetSession("nonexistent")
	if session != nil {
		t.Error("Expected nil for nonexistent session")
	}

	messages := store.GetMessages("nonexistent", 10)
	if messages != nil {
		t.Error("Expected nil for nonexistent session messages")
	}
}
