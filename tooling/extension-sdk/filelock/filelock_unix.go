//go:build unix

package filelock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Inheritable reports whether a held lock passes to a child process through
// exec.Cmd.ExtraFiles. flock(2) belongs to the open file description, which a
// child shares with its parent.
const Inheritable = true

// NoFollow is the OpenFile flag that refuses a symbolic link at the path.
const NoFollow = syscall.O_NOFOLLOW

func openFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		if flag&NoFollow != 0 && errors.Is(err, syscall.ELOOP) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
		}
		return nil, err
	}
	return f, nil
}

func lockFile(f *os.File, exclusive, nonBlocking bool) error {
	fd, err := descriptor(f.Fd())
	if err != nil {
		return err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if nonBlocking {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(fd, how); err != nil {
		if nonBlocking && errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrBusy
		}
		return err
	}
	return nil
}

func unlockFile(f *os.File) error {
	fd, err := descriptor(f.Fd())
	if err != nil {
		return err
	}
	return syscall.Flock(fd, syscall.LOCK_UN)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// descriptor converts an os.File descriptor to the int syscall.Flock takes.
func descriptor(fd uintptr) (int, error) {
	if fd > uintptr(^uint(0)>>1) {
		return 0, fmt.Errorf("file descriptor %d exceeds int range", fd)
	}
	return int(fd), nil
}
