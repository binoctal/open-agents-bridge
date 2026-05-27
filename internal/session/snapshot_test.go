package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/protocol"
)

func TestSnapshotManager_TakeAndRestore(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	sess := &Session{
		ID:             "snap-1",
		CLIType:        "claude",
		WorkDir:        "/tmp/project",
		PermissionMode: "default",
		Status:         "active",
		CreatedAt:      time.Now(),
	}

	history := []protocol.Message{
		{Type: protocol.MessageTypeContent, Content: "hello"},
	}

	snap, err := sm.TakeSnapshot(sess, history)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SessionID != "snap-1" {
		t.Errorf("expected snap-1, got %s", snap.SessionID)
	}
	if snap.CLIType != "claude" {
		t.Errorf("expected claude, got %s", snap.CLIType)
	}
	if len(snap.History) != 1 {
		t.Errorf("expected 1 history message, got %d", len(snap.History))
	}

	// Verify file exists
	if _, err := os.Stat(filepath.Join(dir, "snap-1.json")); os.IsNotExist(err) {
		t.Error("expected snapshot file to exist")
	}

	// Restore
	restored, err := sm.RestoreSnapshot("snap-1")
	if err != nil {
		t.Fatal(err)
	}
	if restored.SessionID != "snap-1" {
		t.Errorf("expected snap-1, got %s", restored.SessionID)
	}
}

func TestSnapshotManager_RestoreNonexistent(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	_, err := sm.RestoreSnapshot("nope")
	if err == nil {
		t.Error("expected error for nonexistent snapshot")
	}
}

func TestSnapshotManager_DeleteSnapshot(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	sess := &Session{ID: "del-me", CLIType: "claude", WorkDir: "/tmp", CreatedAt: time.Now()}
	sm.TakeSnapshot(sess, nil)

	err := sm.DeleteSnapshot("del-me")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "del-me.json")); !os.IsNotExist(err) {
		t.Error("expected snapshot file to be deleted")
	}
}

func TestSnapshotManager_DeleteNonexistent(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	err := sm.DeleteSnapshot("nope")
	if err != nil {
		t.Errorf("expected nil for deleting nonexistent snapshot, got %v", err)
	}
}

func TestSnapshotManager_ListSnapshots(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	sess1 := &Session{ID: "list-1", CLIType: "claude", WorkDir: "/a", CreatedAt: time.Now()}
	sess2 := &Session{ID: "list-2", CLIType: "gemini", WorkDir: "/b", CreatedAt: time.Now()}
	sm.TakeSnapshot(sess1, nil)
	sm.TakeSnapshot(sess2, nil)

	ids, err := sm.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 snapshots, got %d", len(ids))
	}
}

func TestSnapshotManager_ListSnapshots_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	ids, err := sm.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 snapshots, got %d", len(ids))
	}
}

func TestSnapshotManager_CleanOldSnapshots(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	sess := &Session{ID: "old-snap", CLIType: "claude", WorkDir: "/tmp", CreatedAt: time.Now()}
	sm.TakeSnapshot(sess, nil)

	// Set modification time to 48 hours ago
	oldPath := filepath.Join(dir, "old-snap.json")
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(oldPath, oldTime, oldTime)

	// Create a recent snapshot
	sess2 := &Session{ID: "new-snap", CLIType: "claude", WorkDir: "/tmp", CreatedAt: time.Now()}
	sm.TakeSnapshot(sess2, nil)

	err := sm.CleanOldSnapshots(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	ids, _ := sm.ListSnapshots()
	if len(ids) != 1 {
		t.Fatalf("expected 1 snapshot after cleanup, got %d", len(ids))
	}
}

func TestSnapshotManager_ContextData(t *testing.T) {
	dir := t.TempDir()
	sm := NewSnapshotManager(dir)

	sess := &Session{
		ID:        "ctx-1",
		CLIType:   "claude",
		WorkDir:   "/tmp",
		Status:    "active",
		JobID:     "job-123",
		TaskID:    "task-456",
		CreatedAt: time.Now(),
	}

	snap, _ := sm.TakeSnapshot(sess, nil)
	if snap.Context["status"] != "active" {
		t.Errorf("expected active, got %v", snap.Context["status"])
	}
	if snap.Context["job_id"] != "job-123" {
		t.Errorf("expected job-123, got %v", snap.Context["job_id"])
	}
	if snap.Version != "1.0" {
		t.Errorf("expected 1.0, got %s", snap.Version)
	}
}
