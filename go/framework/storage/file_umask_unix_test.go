//go:build unix

package storage

import (
	"syscall"
	"testing"
)

// setUmask sets the process umask for the rest of the test and restores it on
// cleanup. Tests that call it must not run in parallel.
func setUmask(t *testing.T, mask int) {
	t.Helper()
	previous := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(previous) })
}
