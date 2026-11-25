//go:build !linux && !darwin

package store

import (
	"errors"
	"os"
)

// exchangePaths has no one-step form on this platform, so every directory
// swap takes the two-rename path. Windows is such a platform.
func exchangePaths(a, b string) error {
	return &os.LinkError{Op: "exchange", Old: a, New: b, Err: errors.ErrUnsupported}
}

// fileChangeTime returns the change time, device, inode and link count of
// the file info describes, for FileIdentityOf.
func fileChangeTime(os.FileInfo) (ctime int64, dev, ino, links uint64, ok bool) {
	return 0, 0, 0, 0, false
}
