//go:build !windows

package cli

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive, non-blocking flock. flock is tied to the
// open file description and released automatically by close or process exit,
// which is the crash semantics the apply plan needs.
func tryLockFile(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLockBusy
	}
	return err
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
