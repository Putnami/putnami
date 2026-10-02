//go:build unix

package regularfile

import (
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens name for reading without following a symbolic link at
// name and without waiting for a writer when name is a FIFO.
func openNoFollow(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// setBlocking clears the non-blocking flag openNoFollow set, so reads of the
// regular file it opened block as reads of any regular file do.
func setBlocking(file *os.File) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := conn.Control(func(fd uintptr) {
		if fd > uintptr(^uint(0)>>1) {
			setErr = fmt.Errorf("file descriptor %d exceeds int range", fd)
			return
		}
		setErr = syscall.SetNonblock(int(fd), false)
	}); err != nil {
		return err
	}
	return setErr
}
