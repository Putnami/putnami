package store

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// CacheManager's task-owned surface.
//
// The scheduler talks to the cache through CacheManager, never through
// LocalStore, so the declared-capture path reaches every verb it needs — look
// up, claim, wait, publish, restore — here. These are deliberately thin: every rule
// (address derivation, complete-capture validation, explicit empty, atomic
// materialize) lives in task_entry.go / task_ingest.go / task_materialize.go,
// and duplicating any of it here would give the scheduler a second opinion.

// LookupTaskEntry resolves a cache key to its task-owned entry, or (nil, nil)
// for a miss. A format-1 entry under the same key reads as absent: the address
// binds the entry format.
func (cm *CacheManager) LookupTaskEntry(hash string) (*TaskEntry, error) {
	return cm.store.LookupTaskEntry(hash)
}

// LookupResultOnlyTaskEntry returns the result-only entry recorded for hash,
// or nil for a miss. It is the result of a remote hit restored without its
// files, so only a caller that does not need the task's files may serve it
// (task_result_only.go).
func (cm *CacheManager) LookupResultOnlyTaskEntry(hash string) *ResultOnlyTaskEntry {
	return cm.store.LookupResultOnlyTaskEntry(hash)
}

// ServeWitnessed serves again the entries witnesses were taken of, as a hit's
// lookup does, or returns the index of one that moved, and serves none
// (LocalStore.ServeWitnessed).
func (cm *CacheManager) ServeWitnessed(witnesses []EntryWitness) (int, func()) {
	return cm.store.ServeWitnessed(witnesses)
}

// PublishResultOnlyTaskEntry records the result of a task-owned provider hit
// restored without its files, or fails without publishing anything.
func (cm *CacheManager) PublishResultOnlyTaskEntry(hit RemoteTaskEntryHit) (*ResultOnlyTaskEntry, error) {
	return cm.store.PublishResultOnlyTaskEntry(hit)
}

// IngestTaskEntry publishes a task-owned entry from a staging root laid out by
// TaskStagingPath, or fails without publishing anything.
func (cm *CacheManager) IngestTaskEntry(stagingRoot string, spec TaskEntrySpec) (*TaskEntry, error) {
	return cm.store.IngestTaskEntry(stagingRoot, spec)
}

// MaterializeTaskOutput places one recorded output at dest and reports whether
// the restore must count as a write of dest: true when it wrote bytes, and
// when dest already held the output but something there changed since this
// run's reads of the tree began (SetTreeReadsSince), which another session may
// have written after a read this run memoized (ADR 0119). A dest that held the
// output, unchanged since then, is left untouched and reports false. An output
// recorded empty leaves dest untouched. The
// replacement is staged in stagingRoot when rename(2) can reach dest from
// there, and beside dest otherwise. For a directory output that cedes the
// output-relative subpaths in keep, whatever dest already holds at each of
// them survives the swap (see LocalStore.MaterializeTaskOutput).
func (cm *CacheManager) MaterializeTaskOutput(entry *TaskEntry, id, dest, stagingRoot string, keep ...string) (bool, error) {
	return cm.store.materializeTaskOutput(entry, id, dest, stagingRoot, cm.quietBefore(), keep)
}

// treeReadsMargin widens the start of a run's reads backward, so a write the
// filesystem's clock stamps a little before the instant it happened still
// falls after it.
const treeReadsMargin = 2 * time.Second

// processStart bounds every read this process made of the tree from below.
var processStart = time.Now()

// SetTreeReadsSince moves the instant from which this manager's run read the
// tree, which is the start of the process unless set. A held destination that
// changed at or after it counts as written (MaterializeTaskOutput).
func (cm *CacheManager) SetTreeReadsSince(since time.Time) {
	cm.readsSince.Store(since.UnixNano())
}

// AbsentBeforeTreeReads reports whether dest is absent, and was already
// absent when this manager's run began to read the tree: the closest
// directory above it that exists has not changed since (SetTreeReadsSince).
// Removing dest, or a directory above it, changes the directory that held it,
// so an absence another session made after a read this run memoized reports
// false, as do a link above dest and a platform that gives no change time
// (ADR 0119).
func (cm *CacheManager) AbsentBeforeTreeReads(dest string) bool {
	if _, err := os.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	for dir := filepath.Dir(dest); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err == nil {
			// A removal through a link changes the directory it leads to, not
			// the link: a link above dest proves nothing.
			if info.Mode()&os.ModeSymlink != 0 {
				return false
			}
			ctime, _, _, _, ok := fileChangeTime(info)
			return ok && ctime < cm.quietBefore()
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(dir) == dir {
			return false
		}
	}
}

func (cm *CacheManager) quietBefore() int64 {
	if since := cm.readsSince.Load(); since != 0 {
		return since
	}
	return processStart.Add(-treeReadsMargin).UnixNano()
}

// TryClaimTaskEntry claims the computation of key's task-owned entry. See
// LocalStore.TryClaimTaskEntry.
func (cm *CacheManager) TryClaimTaskEntry(key string, estimatedCost time.Duration) (winner bool, release func()) {
	return cm.store.TryClaimTaskEntry(key, estimatedCost)
}

// WaitForTaskEntry waits for a sibling process to publish key's task-owned
// entry while staying responsive to scheduler cancellation.
// LocalStore.WaitForTaskEntry owns the lease-state semantics; the short wait
// slices only add context cancellation without spawning a goroutine that could
// outlive the job.
func (cm *CacheManager) WaitForTaskEntry(ctx context.Context, key string, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if timeout <= 0 {
		return cm.store.WaitForTaskEntry(key, timeout)
	}

	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrLeaseWaitTimeout
		}
		wait := min(remaining, leaseWaitCancelPoll)
		err := cm.store.WaitForTaskEntry(key, wait)
		if !errors.Is(err, ErrLeaseWaitTimeout) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// --- negative (failure) entries ---
//
// The same three verbs for the failure cache. They route at the DISTINCT
// negative address (task_failure.go), which is why no remote code path can
// reach them: remote.go and task_remote.go only ever derive TaskEntryAddress.

// LookupTaskFailure returns the negative entry recorded for hash without
// counting an observation, or nil for a miss.
func (cm *CacheManager) LookupTaskFailure(hash string) *TaskFailure {
	return cm.store.LookupTaskFailure(hash)
}

// ReplayTaskFailure consumes the negative entry recorded for hash, returning it
// with this observation already counted, or nil for a miss.
func (cm *CacheManager) ReplayTaskFailure(hash string) *TaskFailure {
	return cm.store.ReplayTaskFailure(hash)
}

// RecordTaskFailure records a freshly executed failure at hash, preserving any
// existing first-failure time and advancing the attempt counter.
func (cm *CacheManager) RecordTaskFailure(hash string, failure TaskFailure) (*TaskFailure, error) {
	return cm.store.RecordTaskFailure(hash, failure)
}

// ForgetTaskFailure removes any negative entry at hash, which is what a success
// at the same key does to the verdict recorded before it.
func (cm *CacheManager) ForgetTaskFailure(hash string) {
	cm.store.ForgetTaskFailure(hash)
}
