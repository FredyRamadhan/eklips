//go:build unix

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/frdn/eklips/internal/apperr"
)

// InstanceLock enforces that only one daemon owns a database and playback
// engine at a time using an exclusive flock on `<db>.lock`.
type InstanceLock struct {
	file *os.File
	path string
}

// AcquireLock acquires an exclusive non-blocking file lock at lockPath.
func AcquireLock(lockPath string) (*InstanceLock, error) {
	if lockPath == "" {
		return &InstanceLock{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, apperr.Wrap(apperr.DBError, err, "create lock directory")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, apperr.Wrap(apperr.DBError, err, "open lock file %s", lockPath)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, apperr.New(apperr.AlreadyRunning,
			"another eklips instance is already running (lock held on %s)", lockPath)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &InstanceLock{file: f, path: lockPath}, nil
}

// Release unlocks and closes the lock file.
func (l *InstanceLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
