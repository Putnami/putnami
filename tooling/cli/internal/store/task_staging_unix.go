//go:build unix

package store

import (
	"errors"
	"os"
	"syscall"
)

// sameFilesystem reports whether a and b, followed through symlinks, live on
// the same device. It reports false when either cannot be inspected.
func sameFilesystem(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false
	}
	statA, okA := infoA.Sys().(*syscall.Stat_t)
	statB, okB := infoB.Sys().(*syscall.Stat_t)
	return okA && okB && statA.Dev == statB.Dev
}

// crossDevice reports whether err is a rename that could not cross from one
// mount to another.
func crossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
