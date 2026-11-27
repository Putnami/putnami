//go:build linux || darwin

package machineproc

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// mirrorTerminal gives the pseudo-terminal of master the settings and the
// size of the terminal from, with suspend disabled: a worker's terminal never
// stops its process group, the client suspends itself instead (CLI ADR 0116
// §4). It returns the characters from had for suspend, interrupt and quit,
// 0 for one it had not.
func mirrorTerminal(master, from *os.File) (suspend, intr, quit byte, err error) {
	t, err := unix.IoctlGetTermios(int(from.Fd()), ioctlGetTermios)
	if err != nil {
		return 0, 0, 0, err
	}
	suspend, intr, quit = t.Cc[unix.VSUSP], t.Cc[unix.VINTR], t.Cc[unix.VQUIT]
	t.Cc[unix.VSUSP] = 0 // _POSIX_VDISABLE
	if err := unix.IoctlSetTermios(int(master.Fd()), ioctlSetTermios, t); err != nil {
		return 0, 0, 0, err
	}
	return suspend, intr, quit, copySize(master, from)
}

// copySize gives master's terminal the size of from's.
func copySize(master, from *os.File) error {
	ws, err := unix.IoctlGetWinsize(int(from.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return err
	}
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws)
}

// makeRaw puts the terminal f in raw mode and returns what restores it: every
// key, Ctrl-C included, goes through to the worker's terminal, whose line
// discipline then acts as the client's would.
func makeRaw(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }, nil
}

// takeTerminal makes the slave of a pseudo-terminal this process's
// controlling terminal, with this process's group in its foreground: the
// worker is a session leader with none. release gives it back.
func takeTerminal(slave *os.File) (release func(), err error) {
	fd := int(slave.Fd())
	if err := unix.IoctlSetInt(fd, unix.TIOCSCTTY, 0); err != nil {
		return nil, err
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, unix.Getpgrp()); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetInt(fd, unix.TIOCNOTTY, 0) }, nil
}
