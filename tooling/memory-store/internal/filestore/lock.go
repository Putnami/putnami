package filestore

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/tooling/memory-store/internal/store"
)

// fileLock is a held exclusive lock on the store's lock file. The lock
// belongs to the open handle, so two openers exclude each other whether they
// are two processes or two goroutines, and the operating system releases it
// when its holder dies: a crashed writer never wedges the store.
type fileLock struct {
	file *os.File
}

// lockPoll is the interval between two attempts to take a held lock.
const lockPoll = 5 * time.Millisecond

// acquire takes the exclusive lock, waiting at most timeout. A lock still
// held after that is unavailable, and nothing is written. The lock file is
// never removed: another process may hold the same path open, and unlinking
// it would let a third process lock a different inode.
func acquire(ctx context.Context, path string, timeout time.Duration) (*fileLock, error) {
	file, err := filelock.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, store.Failf(store.Unavailable, "store.unwritable", false, "open the memory store lock: %v", err)
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
			return nil, store.Failf(store.Unavailable, "store.unwritable", false, "lock the memory store: %v", err)
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			_ = file.Close()
			return nil, store.Failf(store.Unavailable, "store.busy", true,
				"another writer held the memory store lock for more than %s; nothing was written", timeout)
		}
		time.Sleep(lockPoll)
	}
}

func (l *fileLock) release() {
	_ = filelock.UnlockFile(l.file)
	_ = l.file.Close()
}
