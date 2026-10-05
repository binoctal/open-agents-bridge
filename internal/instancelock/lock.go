// Package instancelock gives a bridge process exclusive ownership of one
// device on this machine. The lock is a kernel-level file lock, so it is
// released automatically when the process dies (including kill -9) and never
// leaves a stale lock behind the way a pid file would.
package instancelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrHeld is returned when another process already owns the lock.
var ErrHeld = errors.New("another bridge instance is already running for this device")

// Lock is a held instance lock. Close releases it.
type Lock struct {
	f *os.File
}

// PathFor returns the lock file path for a device inside dir. The device id is
// sanitized because it ends up in a file name.
func PathFor(dir, deviceID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, deviceID)
	return filepath.Join(dir, "bridge-"+safe+".lock")
}

// Acquire takes the exclusive lock at path, creating the file (and its
// directory) if needed, and records the owner pid for diagnostics. It returns
// ErrHeld, wrapped with the owner pid when readable, if the lock is taken.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		owner, _ := os.ReadFile(path)
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			if pid := strings.TrimSpace(string(owner)); pid != "" {
				return nil, fmt.Errorf("%w (pid %s)", ErrHeld, pid)
			}
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Truncate only after owning the lock so a loser never clobbers the pid.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return &Lock{f: f}, nil
}

// Close releases the lock. Safe on a nil Lock and safe to call twice.
func (l *Lock) Close() {
	if l == nil || l.f == nil {
		return
	}
	unlockFile(l.f)
	_ = l.f.Close()
	l.f = nil
}
