package machineproc

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// openPTY opens a pseudo-terminal: its master and its slave, both
// close-on-exec.
func openPTY() (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master = os.NewFile(uintptr(mfd), "pty-master")
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	n, err := unix.IoctlGetUint32(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	sfd, err := unix.Open("/dev/pts/"+strconv.Itoa(int(n)), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return master, os.NewFile(uintptr(sfd), "pty-slave"), nil
}
