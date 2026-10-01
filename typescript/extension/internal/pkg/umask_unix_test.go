//go:build unix

package pkg

import (
	"syscall"
	"testing"
)

// setUmask sets the process umask to mask and returns the function that puts
// the previous one back.
func setUmask(t *testing.T, mask int) (restore func()) {
	t.Helper()
	previous := syscall.Umask(mask)
	return func() { syscall.Umask(previous) }
}
