// Package flock is the CLI's name for go.putnami.dev/sdk/extension/filelock,
// the advisory file lock every Putnami binary shares. Many worktrees of a repo
// share machine-global stores (~/.putnami/store, ~/.putnami/artifacts), so an
// in-process mutex is not enough: writers take the lock SHARED (they coexist)
// while garbage collection takes it EXCLUSIVE (it waits for and excludes all
// writers). The lock excludes other processes on every platform the CLI
// builds for.
package flock

import (
	"os"

	"go.putnami.dev/sdk/extension/filelock"
)

// Lock is a held advisory file lock. Release it with Release.
type Lock = filelock.Lock

// ErrBusy is returned by a non-blocking request when the lock is held in a
// conflicting mode by another holder.
var ErrBusy = filelock.ErrBusy

// Inheritable reports whether a held lock passes to a child process through
// exec.Cmd.ExtraFiles: true on Unix, false on Windows.
const Inheritable = filelock.Inheritable

// Acquire opens (creating if needed) path and locks it. exclusive selects an
// exclusive lock (e.g. GC) over a shared one (writers). When nonBlocking is
// set it returns ErrBusy instead of waiting if the lock is contended.
func Acquire(path string, exclusive, nonBlocking bool) (*Lock, error) {
	return filelock.Acquire(path, exclusive, nonBlocking)
}

// OpenFile opens a file to lock with LockFile; see filelock.OpenFile.
func OpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	return filelock.OpenFile(path, flag, perm)
}

// LockFile locks a file OpenFile opened; see filelock.LockFile.
func LockFile(f *os.File, exclusive, nonBlocking bool) error {
	return filelock.LockFile(f, exclusive, nonBlocking)
}

// UnlockFile releases the lock LockFile took on f.
func UnlockFile(f *os.File) error {
	return filelock.UnlockFile(f)
}

// SyncDir makes a rename into dir durable; see filelock.SyncDir.
func SyncDir(dir string) error {
	return filelock.SyncDir(dir)
}

// RemoveDir removes dir, which holds the lock file lockName, and ends the
// caller's hold on it with release before it deletes that file; see
// filelock.RemoveDir.
func RemoveDir(dir, lockName string, release func() error) error {
	return filelock.RemoveDir(dir, lockName, release)
}

// RemoveDirLocks removes dir, which holds the lock files lockNames, and ends
// the caller's holds on them with release before it deletes those files; see
// filelock.RemoveDirLocks.
func RemoveDirLocks(dir string, lockNames []string, release func() error) error {
	return filelock.RemoveDirLocks(dir, lockNames, release)
}
