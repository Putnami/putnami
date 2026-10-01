//go:build darwin

package output

import "syscall"

// ioctlGetTermios is the ioctl request code for getting terminal attributes on macOS.
const ioctlGetTermios = syscall.TIOCGETA
