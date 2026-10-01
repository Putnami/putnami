package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
)

// fileLock is a held exclusive lock on the store's lock file. The operating
// system releases it when the process dies, so a crashed writer never wedges
// the store.
type fileLock struct {
	file *os.File
}

// lockPoll is the interval between two attempts to take a held lock.
const lockPoll = 5 * time.Millisecond

// acquire takes the exclusive lock, waiting at most timeout; a lock still
// held after that is ErrBusy. The lock file is never removed: another process
// may hold the same path open, and unlinking it would let a third process
// lock a different inode.
func acquire(path string, timeout time.Duration) (*fileLock, error) {
	file, err := filelock.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err = filelock.LockFile(file, true, true)
		switch {
		case err == nil:
			return &fileLock{file: file}, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, filelock.ErrBusy):
			_ = file.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("%w for more than %s; nothing was written", ErrBusy, timeout)
		}
		time.Sleep(lockPoll)
	}
}

func (l *fileLock) release() {
	_ = filelock.UnlockFile(l.file)
	_ = l.file.Close()
}
