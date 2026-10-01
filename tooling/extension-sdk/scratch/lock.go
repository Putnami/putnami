package scratch

import (
	"os"

	"go.putnami.dev/sdk/extension/filelock"
)

var errBusy = filelock.ErrBusy

// openLock opens an existing owner lock without following a symlink: on a
// shared temporary directory another user could plant one pointing anywhere.
func openLock(path string) (*os.File, error) {
	return filelock.OpenFile(path, os.O_RDWR|filelock.NoFollow, 0)
}

// lockFile takes an exclusive lock on file. With nonBlocking set, a lock held
// by another handle answers errBusy instead of waiting.
func lockFile(file *os.File, nonBlocking bool) error {
	return filelock.LockFile(file, true, nonBlocking)
}
