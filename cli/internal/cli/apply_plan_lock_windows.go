//go:build windows

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// A LockFileEx region on a dedicated file; FAIL_IMMEDIATELY makes it
// non-blocking. Windows releases the region when the last handle closes,
// including on process termination, so a killed apply cannot hold it.
func tryLockFile(file *os.File) error {
	var ol windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, ^uint32(0), 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockBusy
	}
	return err
}

func unlockFile(file *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, ^uint32(0), 0, &ol)
}
