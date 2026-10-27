//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package agentsession

import (
	"errors"
	"os"
	"syscall"
)

// lockSessionFile opens the lock file and takes an exclusive flock on it
// without waiting. The lock belongs to the open file, which Go opens
// close-on-exec, so no child process of the hook inherits it.
func lockSessionFile(path string) (*sessionLock, error) {
	for range 3 {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor fits an int.
			_ = file.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, errSessionBusy
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return nil, err
		}
		// A lock file removed and created again under the same name is
		// another lock: the one this process holds then guards nothing.
		held, heldErr := file.Stat()
		named, namedErr := os.Stat(path)
		if heldErr == nil && namedErr == nil && os.SameFile(held, named) {
			return &sessionLock{file: file}, nil
		}
		_ = file.Close()
	}
	return nil, errSessionBusy
}
