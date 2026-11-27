package machineproc

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// socketpair is a pair of connected Unix stream sockets, close-on-exec.
// macOS has no SOCK_CLOEXEC: the fork lock keeps a worker started meanwhile
// from inheriting either end before it is marked.
func socketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}
