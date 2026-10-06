package store

import (
	"context"
	"errors"
	"time"
)

// CacheManager's task-owned surface.
//
// The scheduler talks to the cache through CacheManager, never through
// LocalStore, so the declared-capture path needs the same four verbs the legacy
// path has — look up, claim, wait, publish, restore — routed at the task-owned
// address instead of the legacy one. These are deliberately thin: every rule
// (address derivation, complete-capture validation, explicit empty, atomic
// materialize) lives in task_entry.go / task_ingest.go / task_materialize.go,
// and duplicating any of it here would give the scheduler a second opinion.

// LookupTaskEntry resolves a cache key to its task-owned entry, or (nil, nil)
// for a miss. A legacy entry under the same key is invisible here by
// construction — the two models occupy disjoint addresses.
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
// any bytes were written. An output recorded empty leaves dest untouched. The
// replacement is staged in stagingRoot when rename(2) can reach dest from
// there, and beside dest otherwise. For a directory output that cedes the
// output-relative subpaths in keep, whatever dest already holds at each of
// them survives the swap (see LocalStore.MaterializeTaskOutput).
func (cm *CacheManager) MaterializeTaskOutput(entry *TaskEntry, id, dest, stagingRoot string, keep ...string) (bool, error) {
	return cm.store.MaterializeTaskOutput(entry, id, dest, stagingRoot, keep...)
}

// TryClaimTaskEntry is TryClaim for the task-owned model: the lease is taken on
// the entry's derived address, so a legacy publisher and a task-owned publisher
// of the same cache key never wait on each other.
func (cm *CacheManager) TryClaimTaskEntry(key string, estimatedCost time.Duration) (winner bool, release func()) {
	return cm.store.TryClaimTaskEntry(key, estimatedCost)
}

// WaitForTaskEntry waits for a sibling process to publish key's task-owned
// entry while staying responsive to scheduler cancellation. It mirrors
// WaitForPublish exactly — same slicing, same ErrLeaseExpired /
// ErrLeaseWaitTimeout contract — so callers keep one coalescing loop.
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
