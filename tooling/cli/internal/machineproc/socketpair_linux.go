package machineproc

import "golang.org/x/sys/unix"

// socketpair is a pair of connected Unix stream sockets, close-on-exec from
// their creation.
func socketpair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}
