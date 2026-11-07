//go:build !windows

package agents

import "syscall"

// lockExclusive takes an exclusive advisory lock on the provisioning lock
// file. Unix only: BSD flock semantics.
func lockExclusive(fd int) error {
	return syscall.Flock(fd, syscall.LOCK_EX)
}

// lockExclusive releases the advisory lock.
func lockRelease(fd int) error {
	return syscall.Flock(fd, syscall.LOCK_UN)
}
