// Which cache keys a task's writes can move.
//
// A cache key reads file bytes through the CacheManager's memo. A run computes a
// job's key before any task runs — PrecomputeKeys, for the remote negotiation
// and the invocation pruning — and again when the job starts, in
// lookupRestoreOrClaim. Only the second one addresses a lookup, a restore, a
// capture and an upload, and between the two, earlier tasks write their
// declared outputs. Two rules keep it the key of the bytes the job reads:
//
//   - every write drops the memoized digests it can move before the writing
//     task's result is published, so before any task that reads the paths can
//     start: finalizeExecutedJob for an execution, restoreDeclaredCacheHit for
//     a restore;
//   - a decision taken on a precomputed key is trusted only for a job whose
//     key no planned write can move (cachekey.PlanKeyMovement): the negotiation records
//     no other job's key, and a pruning that rests on a movable consumer key
//     is confirmed from the key the consumer computes once every write that
//     can move it happened (invocationRuntime.isPruned).
package scheduler

import (
	"sync"

	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
)

// cachedAtCurrentKey reports whether the local store holds job's entry at the
// key it computes now, from the dependency keys this run published and the
// tree as it stands: the key lookupRestoreOrClaim computes when the job starts,
// once nothing that can move it is left to run.
func (s *Scheduler) cachedAtCurrentKey(job *ScheduledJob, mu *sync.Mutex, hashes map[string]string) bool {
	if s.cache == nil {
		return false
	}
	mu.Lock()
	hashCopy := make(map[string]string, len(job.DependsOn))
	for _, depKey := range cachekey.CacheKeyDependencies(job) {
		if value, ok := hashes[depKey]; ok {
			hashCopy[depKey] = value
		}
	}
	mu.Unlock()
	hash, err := cachekey.ComputeJobCacheHash(s.ws, job, s.commandParams, s.cfg.VersionInfo, s.cache, hashCopy)
	return err == nil && cachedEntryPresent(s.cache, job, hash)
}
