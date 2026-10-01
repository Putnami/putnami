//go:build windows

package output

import (
	"os"

	"golang.org/x/sys/windows"
)

// Standard output and standard error render ANSI escape sequences when they
// are consoles, so colored output reads the same as on Unix.
func init() {
	isTTY(int(os.Stdout.Fd()))
	isTTY(int(os.Stderr.Fd()))
}

// isTTY returns true if the given file descriptor is a console that renders
// ANSI escape sequences, turning virtual terminal processing on when it is
// off. A console that refuses it (Windows before 10 1511) prints escapes
// verbatim, so it is not a terminal for this package.
func isTTY(fd int) bool {
	handle := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}

// termSize returns the visible console window's width and height, defaulting
// to 80×24 if unknown.
func termSize() (width, height int) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stderr.Fd()), &info); err != nil {
		return 80, 24
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	if w <= 0 {
		return 80, 24
	}
	h := int(info.Window.Bottom-info.Window.Top) + 1
	if h <= 0 {
		h = 24
	}
	return w, h
}
