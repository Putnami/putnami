//go:build !windows

package infra

import "os"

// renameFile renames once: a Unix rename does not fail because another
// process holds the destination open.
func renameFile(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

// removeFile removes once, for the same reason.
func removeFile(path string) error {
	return os.Remove(path)
}

// readFile reads once: a Unix open does not fail because a rename replaces
// the file.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
