package cli

import (
	"errors"
	"os"
	"path/filepath"
)

// errLockBusy is the platform lock's own "already held" signal, translated by
// acquireApplyPlanLock into an apply-specific message.
var errLockBusy = errors.New("file lock already held")

// applyPlanLock serializes applies of one discobox on this machine. It is an
// OS advisory lock on a file in the plan directory, so it is released by the
// kernel when the holding process exits — a killed apply never wedges the
// next one — and never by stale-pid bookkeeping.
type applyPlanLock struct {
	file *os.File
}

// acquireApplyPlanLock takes the lock non-blocking. A second apply while one
// is running gets an actionable "already running" error rather than two
// processes racing one plan file and one worktree.
func acquireApplyPlanLock(sandboxID string) (*applyPlanLock, error) {
	dir := applyPlanDir(sandboxID)
	if err := ensureStateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := restrictToUser(path); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := tryLockFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, errLockBusy) {
			return nil, fmtApplyInProgress(sandboxID)
		}
		return nil, err
	}
	return &applyPlanLock{file: file}, nil
}

func fmtApplyInProgress(sandboxID string) error {
	return errors.New("an apply of discobox " + sandboxID +
		" is already running on this machine; wait for it to finish, then run apply again")
}

// release unlocks and closes the lock file. The file itself stays: it is the
// lock's address for every future run, and removing it under another waiter
// would reopen the interlock race.
func (l *applyPlanLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unlockFile(l.file)
	err := l.file.Close()
	l.file = nil
	return err
}
