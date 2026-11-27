package machineproc

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// openPTY opens a pseudo-terminal: its master and its slave, both
// close-on-exec.
func openPTY() (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master = os.NewFile(uintptr(mfd), "pty-master")
	fail := func(err error) (*os.File, *os.File, error) {
		_ = master.Close()
		return nil, nil, err
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), unix.TIOCPTYGRANT, 0); errno != 0 {
		return fail(errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), unix.TIOCPTYUNLK, 0); errno != 0 {
		return fail(errno)
	}
	name := make([]byte, 128)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		return fail(errno)
	}
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	sfd, err := unix.Open(string(name), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	return master, os.NewFile(uintptr(sfd), "pty-slave"), nil
}
