package instancelock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecondAcquireOnSameMachineFails(t *testing.T) {
	path := PathFor(t.TempDir(), "dev-a")
	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Close()

	_, err = Acquire(path)
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire err = %v, want ErrHeld", err)
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Errorf("error should name the owner pid, got %q", err)
	}
	// The loser must not clobber the owner's pid.
	b, _ := os.ReadFile(path)
	if strings.TrimSpace(string(b)) == "" {
		t.Error("lock file lost the owner pid")
	}
}

func TestDifferentMachinesDoNotExclude(t *testing.T) {
	dir := t.TempDir()
	a, err := Acquire(PathFor(dir, "dev-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Acquire(PathFor(dir, "dev-b"))
	if err != nil {
		t.Fatalf("different machine must not be blocked: %v", err)
	}
	b.Close()
}

func TestReleaseAllowsReacquire(t *testing.T) {
	path := PathFor(t.TempDir(), "dev-a")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	l.Close() // idempotent
	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	l2.Close()
}

func TestPathForSanitizesMachineID(t *testing.T) {
	p := PathFor("/cfg", "../evil/id")
	if filepath.Dir(p) != "/cfg" {
		t.Fatalf("machine id escaped the config dir: %s", p)
	}
}

// A killed holder must free the lock immediately (kernel lock, no pid file).
func TestKillNineFreesTheLock(t *testing.T) {
	if os.Getenv("INSTANCELOCK_HOLDER") != "" {
		l, err := Acquire(os.Getenv("INSTANCELOCK_PATH"))
		if err != nil {
			os.Exit(3)
		}
		defer l.Close()
		time.Sleep(time.Minute)
		return
	}
	path := PathFor(t.TempDir(), "dev-a")
	cmd := exec.Command(os.Args[0], "-test.run=TestKillNineFreesTheLock")
	cmd.Env = append(os.Environ(), "INSTANCELOCK_HOLDER=1", "INSTANCELOCK_PATH="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := Acquire(path)
		if errors.Is(err, ErrHeld) {
			break
		}
		// We won the race before the holder started; release and retry.
		l.Close()
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("holder never took the lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("lock still held after kill -9: %v", err)
	}
	l.Close()
}
