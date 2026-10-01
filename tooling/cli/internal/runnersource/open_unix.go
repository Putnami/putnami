//go:build !windows

package runnersource

import (
	"os"
	"syscall"
)

// Nonblocking open prevents a file-to-FIFO race from hanging before fstat can
// reject the replacement. It does not change regular-file read semantics.
const sourceReadFlags = os.O_RDONLY | syscall.O_NONBLOCK
