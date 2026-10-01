//go:build darwin || linux

package output

import (
	"os"
	"syscall"
	"unsafe"
)

// isTTY returns true if the given file descriptor is a terminal.
func isTTY(fd int) bool {
	var termios [256]byte // large enough for any platform's termios
	_, _, err := syscall.Syscall6(
		syscall.SYS_IOCTL,
		uintptr(fd),
		ioctlGetTermios,
		uintptr(unsafe.Pointer(&termios[0])),
		0, 0, 0,
	)
	return err == 0
}

// termSize returns terminal width and height, defaulting to 80×24 if unknown.
func termSize() (width, height int) {
	type winsize struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}
	var ws winsize
	_, _, err := syscall.Syscall6(
		syscall.SYS_IOCTL,
		os.Stderr.Fd(),
		syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws)),
		0, 0, 0,
	)
	if err != 0 || ws.Col == 0 {
		return 80, 24
	}
	h := ws.Row
	if h == 0 {
		h = 24
	}
	return int(ws.Col), int(h)
}
