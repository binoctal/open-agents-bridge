//go:build windows

package instancelock

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var (
	errWouldBlock = errors.New("lock held")

	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x2
	lockfileFailImmediately = 0x1
	errorLockViolation      = syscall.Errno(33)
)

func lockFile(f *os.File) error {
	var ol syscall.Overlapped
	r, _, e := procLockFileEx.Call(
		f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0, uintptr(unsafe.Pointer(&ol)),
	)
	if r != 0 {
		return nil
	}
	if errno, ok := e.(syscall.Errno); ok && errno == errorLockViolation {
		return errWouldBlock
	}
	return e
}

func unlockFile(f *os.File) {
	var ol syscall.Overlapped
	_, _, _ = procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
}
