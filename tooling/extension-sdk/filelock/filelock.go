// Package filelock is the cross-process advisory file lock that the CLI, the
// SDK and the language extensions share, so every Putnami binary locks one way
// on every platform.
//
// A lock is shared or exclusive: shared locks coexist, an exclusive lock
// excludes every other one. A lock belongs to one open handle, so two Acquire
// calls in one process contend like two processes. A non-blocking request that
// meets a conflicting lock answers ErrBusy; a blocking one waits without a time
// bound, and a caller that must stay cancelable polls without blocking. The
// operating system releases a lock when its holder dies, however it dies.
//
// A lock never covers the file's content: any handle, in the holder's process
// or another, reads, writes and truncates a locked file. On Unix the lock is
// flock(2) on a close-on-exec descriptor. On Windows it is one byte at offset
// 2^62 taken with LockFileEx, a range no Putnami file reaches.
//
// A holder may rename its lock file while it holds the lock, and removes the
// directory that contains it with RemoveDir. Nobody renames another file over
// a lock file or deletes one a live holder may hold: a newcomer would lock a
// different file than the one its waiters hold.
//
// Only Unix passes a held lock to a child process; Inheritable says which.
// A platform without an implementation does not compile.
package filelock

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

var (
	// ErrBusy is returned by a non-blocking request when another handle holds
	// the lock in a conflicting mode.
	ErrBusy = errors.New("file lock busy")

	// ErrDirectory is returned when the file to lock is a directory: only a
	// regular file carries a lock on every platform.
	ErrDirectory = errors.New("file lock on a directory")
)

// openFlags are the flags OpenFile accepts. Any other flag is refused on
// every platform, so a caller never gets different open semantics on Windows.
const openFlags = os.O_RDONLY | os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_EXCL | os.O_TRUNC | NoFollow

// Lock is a held file lock. Release it with Release.
type Lock struct {
	f *os.File
}

// Acquire opens path, creating it if needed, and locks it. exclusive selects
// an exclusive lock over a shared one. With nonBlocking set, a conflicting
// lock answers ErrBusy instead of waiting.
func Acquire(path string, exclusive, nonBlocking bool) (*Lock, error) {
	f, err := OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := LockFile(f, exclusive, nonBlocking); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// OpenFile is os.OpenFile for a file this package locks. The handle is never
// inherited by a child process, and on Windows it lets other handles delete
// and rename the file. It accepts the access modes, os.O_CREATE, os.O_EXCL,
// os.O_TRUNC and NoFollow. With NoFollow, a symbolic link at path (on Windows,
// any reparse point) answers an error that matches os.ErrPermission.
func OpenFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	if flag&^openFlags != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: errors.ErrUnsupported}
	}
	return openFile(path, flag, perm)
}

// LockFile locks f, which callers open with OpenFile. exclusive and
// nonBlocking mean what they mean for Acquire. A directory answers
// ErrDirectory.
func LockFile(f *os.File, exclusive, nonBlocking bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return ErrDirectory
	}
	return lockFile(f, exclusive, nonBlocking)
}

// UnlockFile releases the lock LockFile took on f.
func UnlockFile(f *os.File) error {
	return unlockFile(f)
}

// SyncDir makes a rename into dir durable. A holder that replaces a file under
// its lock writes a temporary file, syncs it, renames it over the name, and
// calls SyncDir last. On Windows there is nothing to flush and SyncDir returns
// nil: a directory opened for reading cannot be flushed, and NTFS journals the
// rename, so after a crash the name holds the old file or the new one.
func SyncDir(dir string) error {
	return syncDir(dir)
}

// RemoveDir removes dir, the directory that holds lockName, the lock file
// whose lock the caller holds, and calls release to end that hold; see
// RemoveDirLocks.
func RemoveDir(dir, lockName string, release func() error) error {
	return RemoveDirLocks(dir, []string{lockName}, release)
}

// RemoveDirLocks removes dir, the directory that holds lockNames, the lock
// files whose locks the caller holds, and calls release to end those holds. It
// removes every other entry under the locks, then calls release, then removes
// the lock files and dir. A holder never deletes a lock file it holds: Windows
// 10 before version 1809 only marks an open file for deletion, and then
// refuses to remove its directory. When another entry cannot be removed,
// RemoveDirLocks still calls release but keeps the lock files and dir, so
// whoever reclaims the directory later still finds its locks. It returns every
// error.
func RemoveDirLocks(dir string, lockNames []string, release func() error) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		releaseErr := release()
		return errors.Join(releaseErr, os.RemoveAll(dir))
	}
	var errs []error
	for _, entry := range entries {
		if !slices.Contains(lockNames, entry.Name()) {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(append(errs, release())...)
	}
	releaseErr := release()
	return errors.Join(releaseErr, os.RemoveAll(dir))
}

// Release releases the lock and closes its file. Safe on a nil Lock and after
// Release or Close.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unlockFile(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}

// File returns the locked file, for a holder record written into it and, where
// Inheritable, for explicit transfer through exec.Cmd.ExtraFiles. After
// starting such a child, use Close, not Release: unlocking would also unlock
// the child's inherited descriptor.
func (l *Lock) File() *os.File {
	return l.f
}

// Close drops this handle. Where Inheritable, it does not unlock: a child that
// inherited the descriptor keeps the lock until its last copy closes, and
// without one the lock ends now. Elsewhere Close is Release. Safe on a nil
// Lock and after Release or Close.
func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	if !Inheritable {
		return l.Release()
	}
	err := l.f.Close()
	l.f = nil
	return err
}
