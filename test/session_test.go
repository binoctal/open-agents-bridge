package test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/session"
)

// testCLIType is the CLI these tests spawn. "claude-pty" maps to the plain
// `claude` binary with ForceProtocol=pty, so Create finishes as soon as the
// process starts — unlike "claude", which maps to `npx <acp package>` and would
// pull a package off the network and sit through the ACP handshake budget.
const testCLIType = "claude-pty"

// requireCLI skips the test when the binary a session would spawn is missing.
// Manager.Create starts a real process, so on a machine without the CLI these
// tests would only be measuring what happens to be on PATH.
func requireCLI(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not installed; skipping process-spawning session test")
	}
}

func TestSessionManagerCreate(t *testing.T) {
	requireCLI(t)
	mgr := session.NewManager()
	defer mgr.StopAll()

	// Use temp dir that exists
	tmpDir := t.TempDir()
	sess, err := mgr.Create(testCLIType, tmpDir)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if sess.ID == "" {
		t.Error("Session ID is empty")
	}
	if sess.CLIType != testCLIType {
		t.Errorf("CLIType = %s, want %s", sess.CLIType, testCLIType)
	}
	if sess.WorkDir != tmpDir {
		t.Errorf("WorkDir = %s, want %s", sess.WorkDir, tmpDir)
	}
	if sess.Status != "active" {
		t.Errorf("Status = %s, want active", sess.Status)
	}
}

func TestSessionManagerGet(t *testing.T) {
	requireCLI(t)
	mgr := session.NewManager()
	defer mgr.StopAll()
	tmpDir := t.TempDir()

	sess, err := mgr.Create(testCLIType, tmpDir)
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
	requireCLI(t)
	mgr := session.NewManager()
	defer mgr.StopAll()
	tmpDir := t.TempDir()

	if _, err := mgr.Create(testCLIType, tmpDir); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Create another temp dir for second session
	tmpDir2, _ := os.MkdirTemp("", "session2")
	defer os.RemoveAll(tmpDir2)
	if _, err := mgr.Create(testCLIType, tmpDir2); err != nil {
		t.Fatalf("Create (second) failed: %v", err)
	}

	sessions := mgr.List()
	if len(sessions) != 2 {
		t.Errorf("List returned %d sessions, want 2", len(sessions))
	}
}

func TestSessionManagerStop(t *testing.T) {
	requireCLI(t)
	mgr := session.NewManager()
	defer mgr.StopAll()
	tmpDir := t.TempDir()

	sess, err := mgr.Create(testCLIType, tmpDir)
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
	requireCLI(t)
	mgr := session.NewManager()
	tmpDir := t.TempDir()

	if _, err := mgr.Create(testCLIType, tmpDir); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	mgr.StopAll()

	if len(mgr.List()) != 0 {
		t.Error("Sessions still exist after StopAll")
	}
}

func TestSessionManagerCreateUnknownAdapter(t *testing.T) {
	mgr := session.NewManager()
	tmpDir := t.TempDir()

	// An unknown type falls through getCLICommand to itself as the command, so
	// Create must fail at exec rather than silently spawning something.
	_, err := mgr.Create("unknown_cli_that_does_not_exist", tmpDir)
	if err == nil {
		t.Error("Expected error for unknown adapter")
	}
}
