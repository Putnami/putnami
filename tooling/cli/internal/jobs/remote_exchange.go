package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/store"
)

// Blob-exchange directory lifecycle.
//
// Every provider session hands blobs through its own directory under the
// exchange parent: the store root, or <ws>/.putnami without a store. The session
// deletes it at shutdown. A run that is killed, canceled, or crashes never gets
// there, and its directory stays behind: hardlinks that keep evicted CAS blobs
// alive, plus restored blob copies. Cleaning that residue is core's job
// (protocols/cache ADR 0002), so each session start sweeps it.
//
// Liveness is an advisory lock, as for invocation scratch (store/invocation.go).
// The owning session holds an exclusive flock on <dir>/owner.lock from just after
// it creates the directory until just after it deletes it, and the kernel drops
// that lock however the owner dies. A sweep deletes a sibling only when BOTH hold:
//
//   - its mtime is older than the grace. This covers the one moment a live
//     directory is unlocked: between MkdirTemp and the owner's flock. It is also
//     the only protection a session started by an older CLI has, since that
//     CLI holds no lock.
//   - the sweep can take the owner lock itself. That proves no owner is alive,
//     however long the owner has been running, so age alone never deletes a
//     live session's directory.
//
// The sweep holds the lock while it deletes, and an owner releases its lock only
// after it has deleted its directory. Until the owner starts that deletion, the
// owner.lock a sweep opens is the inode the owner holds, so the sweep finds it
// busy. A sweep that races the owner's own deletion can only help delete a
// directory its owner is already deleting.
//
// A sweep checks whether it should stop before each directory it deletes, and
// shutdown asks it to stop, so shutdown waits for one directory at most. The
// next run's sweep takes the rest. A deletion cut short by a killed process can
// leave a directory without its owner.lock and with a fresh mtime. Once the grace
// has passed, the next sweep recreates owner.lock (flock.Acquire creates the
// file), takes the lock, and deletes what is left.
//
// Exported blobs stay until shutdown. An UploadResult only means the provider
// QUEUED the upload; the bytes are confirmed at OpSummary, which comes right
// before shutdown. An earlier delete would free space only for a run killed in
// that short window.
const (
	// exchangeDirPrefix names a session's exchange directory under the parent.
	exchangeDirPrefix = "cache-provider-exchange-"
	// exchangeOwnerLockName is the file whose flock the owning session holds for
	// the directory's whole life. The name is not a blob shard (two hex
	// characters), so it cannot collide with a BlobExchangePath.
	exchangeOwnerLockName = "owner.lock"
	// minExchangeSweepGrace floors the sweep grace. A zero
	// PUTNAMI_STORE_GC_GRACE must not expose a directory in the instant between
	// its creation and its owner's lock.
	minExchangeSweepGrace = time.Minute
)

// exchangeSweepGrace is the store GC grace (PUTNAMI_STORE_GC_GRACE, one hour by
// default), floored at minExchangeSweepGrace.
func exchangeSweepGrace() time.Duration {
	return max(store.ResolveGCGrace(store.Config{}), minExchangeSweepGrace)
}

// newExchangeDir creates a session's exchange directory under parent and locks
// it before returning. A directory whose lock cannot be taken is deleted and
// reported as an error: a session never runs with a directory a sweep could
// take for dead.
func newExchangeDir(parent string) (string, *flock.Lock, error) {
	dir, err := os.MkdirTemp(parent, exchangeDirPrefix)
	if err != nil {
		return "", nil, err
	}
	owner, err := flock.Acquire(filepath.Join(dir, exchangeOwnerLockName), true, true)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	return dir, owner, nil
}

// removeExchangeDir deletes a session's exchange directory and releases its
// owner lock. Everything but the lock file goes under the lock, so no content
// outlives its owner's hold; the lock file goes after the release, because
// Windows before version 1809 cannot remove a directory that holds an open
// file. It is safe to call twice and on a nil lock.
func removeExchangeDir(dir string, owner *flock.Lock) {
	if dir == "" {
		_ = owner.Release()
		return
	}
	_ = flock.RemoveDir(dir, exchangeOwnerLockName, owner.Release)
}

// sweepStaleExchangeDirs deletes the exchange directories under parent that
// dead sessions left behind, and returns how many it deleted. It is best-effort:
// a directory it cannot prove dead is left for a later run. It calls stopped
// before each directory it would delete, and returns once stopped reports true.
func sweepStaleExchangeDirs(parent string, now time.Time, grace time.Duration, stopped func() bool) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		// IsDir reports the entry itself, not a symlink target, so a link named
		// like an exchange directory is never followed out of parent.
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), exchangeDirPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < grace {
			continue
		}
		if stopped() {
			break
		}
		if removeDeadExchangeDir(filepath.Join(parent, entry.Name())) {
			removed++
		}
	}
	return removed
}

// removeDeadExchangeDir deletes dir only if it can take the owner lock, and
// holds that lock until everything but the lock file is gone. A busy lock is a
// live owner; any other error leaves the directory alone.
func removeDeadExchangeDir(dir string) bool {
	owner, err := flock.Acquire(filepath.Join(dir, exchangeOwnerLockName), true, true)
	if err != nil {
		return false
	}
	return flock.RemoveDir(dir, exchangeOwnerLockName, owner.Release) == nil
}
