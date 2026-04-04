package test

import (
	"os"
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/session"
)

func TestSessionManagerCreate(t *testing.T) {
	mgr := session.NewManager()

	// Use temp dir that exists
	tmpDir := t.TempDir()
	sess, err := mgr.Create("kiro", tmpDir)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if sess.ID == "" {
		t.Error("Session ID is empty")
	}
	if sess.CLIType != "kiro" {
		t.Errorf("CLIType = %s, want kiro", sess.CLIType)
	}
	if sess.WorkDir != tmpDir {
		t.Errorf("WorkDir = %s, want %s", sess.WorkDir, tmpDir)
	}
	if sess.Status != "active" {
		t.Errorf("Status = %s, want active", sess.Status)
	}
}

func TestSessionManagerGet(t *testing.T) {
	mgr := session.NewManager()
	tmpDir := t.TempDir()

	sess, err := mgr.Create("kiro", tmpDir)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	retrieved := mgr.Get(sess.ID)
	if retrieved == nil {
		t.Fatal("Get returned nil")
	}
	if retrieved.ID != sess.ID {
		t.Errorf("ID = %s, want %s", retrieved.ID, sess.ID)
	}
}

func TestSessionManagerGetNonExistent(t *testing.T) {
	mgr := session.NewManager()

	retrieved := mgr.Get("nonexistent")
	if retrieved != nil {
		t.Error("Expected nil for nonexistent session")
	}
}

func TestSessionManagerList(t *testing.T) {
	mgr := session.NewManager()
	tmpDir := t.TempDir()

	mgr.Create("kiro", tmpDir)
	
	// Create another temp dir for second session
	tmpDir2, _ := os.MkdirTemp("", "session2")
	defer os.RemoveAll(tmpDir2)
	mgr.Create("cline", tmpDir2)

	sessions := mgr.List()
	if len(sessions) != 2 {
		t.Errorf("List returned %d sessions, want 2", len(sessions))
	}
}

func TestSessionManagerStop(t *testing.T) {
	mgr := session.NewManager()
	tmpDir := t.TempDir()

	sess, err := mgr.Create("kiro", tmpDir)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	err = mgr.Stop(sess.ID)
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if mgr.Get(sess.ID) != nil {
		t.Error("Session still exists after Stop")
	}
}

func TestSessionManagerStopNonExistent(t *testing.T) {
	mgr := session.NewManager()

	err := mgr.Stop("nonexistent")
	if err != nil {
		t.Errorf("Stop returned error for nonexistent: %v", err)
	}
}

func TestSessionManagerStopAll(t *testing.T) {
	mgr := session.NewManager()
	tmpDir := t.TempDir()

	mgr.Create("kiro", tmpDir)

	mgr.StopAll()

	if len(mgr.List()) != 0 {
		t.Error("Sessions still exist after StopAll")
	}
}

func TestSessionManagerCreateUnknownAdapter(t *testing.T) {
	mgr := session.NewManager()
	tmpDir := t.TempDir()

	_, err := mgr.Create("unknown_cli", tmpDir)
	if err == nil {
		t.Error("Expected error for unknown adapter")
	}
}
