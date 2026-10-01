//go:build windows

package filelock

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Inheritable reports whether a held lock passes to a child process. Windows
// handles opened here are never inherited, and os/exec refuses ExtraFiles.
const Inheritable = false

// NoFollow is the OpenFile flag that refuses a reparse point at the path,
// which covers symbolic links and junctions.
const NoFollow = windows.O_FILE_FLAG_OPEN_REPARSE_POINT

// lockOffset is the one byte LockFileEx locks. A Windows byte-range lock is
// mandatory for the bytes it covers, so it sits at 2^62, where no Putnami file
// has content, and every handle reads, writes and truncates the file freely.
const lockOffset = 1 << 62

func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	h, err := open(path, flag, perm)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// open mirrors syscall.Open, which os.OpenFile uses, with two differences: the
// handle shares delete access, so a holder can rename its lock file while it
// holds it, and NoFollow opens a reparse point itself and refuses it.
// A nil security descriptor keeps the handle out of child processes.
func open(path string, flag int, perm os.FileMode) (windows.Handle, error) {
	if path == "" {
		return windows.InvalidHandle, windows.ERROR_FILE_NOT_FOUND
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access = windows.GENERIC_READ
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if flag&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		attrs = windows.FILE_ATTRIBUTE_READONLY
	}
	if access&windows.GENERIC_WRITE == 0 {
		// A read-only open may name a directory, which CreateFile opens
		// only with backup semantics.
		attrs |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	if flag&NoFollow != 0 {
		attrs |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	var disposition uint32
	switch {
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		disposition = windows.CREATE_NEW
		attrs |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	case flag&os.O_CREATE != 0:
		disposition = windows.OPEN_ALWAYS
	default:
		disposition = windows.OPEN_EXISTING
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	h, err := windows.CreateFile(name, access, share, nil, disposition, attrs, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) && attrs&windows.FILE_FLAG_BACKUP_SEMANTICS == 0 {
			if a, statErr := windows.GetFileAttributes(name); statErr == nil && a&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
				err = syscall.EISDIR
			}
		}
		return windows.InvalidHandle, err
	}
	if flag&NoFollow != 0 {
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &info); err != nil {
			_ = windows.CloseHandle(h)
			return windows.InvalidHandle, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			_ = windows.CloseHandle(h)
			return windows.InvalidHandle, os.ErrPermission
		}
	}
	if flag&os.O_TRUNC != 0 && disposition != windows.CREATE_NEW {
		if err := windows.Ftruncate(h, 0); err != nil {
			_ = windows.CloseHandle(h)
			return windows.InvalidHandle, err
		}
	}
	return h, nil
}

func lockFile(f *os.File, exclusive, nonBlocking bool) error {
	var flags uint32
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, lockRange()); err != nil {
		if nonBlocking && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return ErrBusy
		}
		return err
	}
	return nil
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRange())
}

// syncDir has nothing to flush; SyncDir says why.
func syncDir(string) error {
	return nil
}

// lockRange addresses lockOffset. On a synchronous handle the structure only
// carries the offset: the call itself waits.
func lockRange() *windows.Overlapped {
	return &windows.Overlapped{Offset: lockOffset & 0xFFFFFFFF, OffsetHigh: lockOffset >> 32}
}
