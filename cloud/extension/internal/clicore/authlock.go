package clicore

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
)

// authRefreshLockWait bounds how long a process waits for another one to
// finish refreshing the stored session. A holder that dies releases the lock
// at once, since the operating system drops it with the process; the bound
// only covers a live holder stuck on the network. Past it, the waiter
// refreshes without the lock, as before the lock existed: auth-server answers
// a rotated refresh token with the same successor for a short window, and
// refreshStoredAuthWithRetry still absorbs a lost race.
var authRefreshLockWait = 30 * time.Second

// authRefreshLockPoll is the interval between two attempts at a busy lock.
const authRefreshLockPoll = 20 * time.Millisecond

// authLockPath is the lock file next to auth.json. The lock never sits on
// auth.json itself: WriteAuth renames a new file over it, and a lock belongs
// to the file a handle opened, not to its name.
func authLockPath(env map[string]string) string {
	return authPath(env) + ".lock"
}

// lockAuthRefresh takes the exclusive lock that serializes session refreshes
// across processes and returns its release. A lock the process cannot take,
// because the Putnami home cannot hold the lock file or the holder outlives
// authRefreshLockWait, yields a release that does nothing: the refresh then
// runs unlocked rather than fail the command.
func lockAuthRefresh(env map[string]string) func() {
	path := authLockPath(env)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() {}
	}
	deadline := time.Now().Add(authRefreshLockWait)
	for {
		lock, err := filelock.Acquire(path, true, true)
		if err == nil {
			return func() { _ = lock.Release() }
		}
		if !errors.Is(err, filelock.ErrBusy) || !time.Now().Before(deadline) {
			return func() {}
		}
		time.Sleep(authRefreshLockPoll)
	}
}
