// Which cache keys a task's writes can move.
//
// A cache key reads file bytes through the CacheManager's memo. A run computes a
// job's key before any task runs — PrecomputeKeys, for the remote
// negotiation — and again when the job starts, in
// lookupRestoreOrClaim. Only the second one addresses a lookup, a restore, a
// capture and an upload, and between the two, earlier tasks write their
// declared outputs. Two rules keep it the key of the bytes the job reads:
//
//   - every write drops the memoized digests it can move before the writing
//     task's result is published, so before any task that reads the paths can
//     start: finalizeExecutedJob for an execution, restoreDeclaredCacheHit for
//     a restore;
//   - a decision taken on a precomputed key is trusted only for a job whose
//     key no planned write can move (cachekey.PlanKeyMovement): the negotiation
//     records no other job's key. A lazy invocation producer is decided by the
//     key each consumer computes when it starts (invocation_lazy.go).
package scheduler

import (
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// cachedEntryPresent reports whether the local store already holds the job's
// entry at hash, using the same address the execution path would look it up at.
// A source-bound task's entry counts absent here (cachekey.CacheEntryPresent).
func cachedEntryPresent(cache *store.CacheManager, job *ScheduledJob, hash string) bool {
	present, err := cachekey.CacheEntryPresent(cache, job, hash)
	return err == nil && present
}
