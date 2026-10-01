package cachepolicy

import (
	"errors"
	"os"
	"path/filepath"

	"go.putnami.dev/sdk/extension/filelock"
)

// ErrBusy is returned by Acquire when nonBlocking is set and another collector
// holds the lock.
var ErrBusy = errors.New("cache collector lock busy")

// Lock is a held advisory whole-file lock. Release it with Release.
type Lock struct {
	file *os.File
}

// Acquire takes the EXCLUSIVE collector lock for a cache root, creating the
// root and the lock file if needed.
//
// Exclusive is the only mode offered: two collectors of the same cache would
// pick overlapping victims and each report freeing bytes the other already
// freed. The lock says nothing at all about the toolchain processes reading the
// same cache — those are the grace window's job, not this one's.
func Acquire(root string, nonBlocking bool) (*Lock, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	file, err := filelock.OpenFile(filepath.Join(root, LockFileName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := filelock.LockFile(file, true, nonBlocking); err != nil {
		_ = file.Close()
		if errors.Is(err, filelock.ErrBusy) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &Lock{file: file}, nil
}

// Release drops the lock. Safe on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	// The lock file is deliberately NOT removed: another process may hold this
	// same path open, and unlinking it would let a third process create a
	// different inode and take a lock that excludes nobody.
	err := filelock.UnlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
