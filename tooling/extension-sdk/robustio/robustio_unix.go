//go:build !windows

package robustio

import "os"

// rename renames once: a Unix rename does not fail because another process
// holds the file open.
func rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// remove removes once, for the same reason.
func remove(path string) error {
	return os.Remove(path)
}
