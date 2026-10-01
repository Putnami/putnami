//go:build linux

package output

import "syscall"

// ioctlGetTermios is the ioctl request code for getting terminal attributes on Linux.
const ioctlGetTermios = syscall.TCGETS
