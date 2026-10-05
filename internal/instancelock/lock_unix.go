//go:build !windows

package instancelock

import (
	"errors"
	"os"
	"syscall"
)

var errWouldBlock = errors.New("lock held")

func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errWouldBlock
	}
	return err
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
