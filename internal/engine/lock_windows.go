//go:build windows

package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/frdn/eklips/internal/apperr"
)

// InstanceLock is a best-effort single-instance lock on Windows.
type InstanceLock struct {
	file *os.File
	path string
}

// AcquireLock creates an exclusive lock file on Windows.
func AcquireLock(lockPath string) (*InstanceLock, error) {
	if lockPath == "" {
		return &InstanceLock{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, apperr.Wrap(apperr.DBError, err, "create lock directory")
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, apperr.New(apperr.AlreadyRunning,
			"another eklips instance is already running (lock held on %s)", lockPath)
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return &InstanceLock{file: f, path: lockPath}, nil
}

// Release closes and removes the lock file.
func (l *InstanceLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	_ = os.Remove(l.path)
	l.file = nil
	return err
}
