package jobs

import (
	"log/slog"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
)

// The failure cache: replaying a failure whose inputs have not changed.
//
// Success has always been cached; failure was not. So the cheapest possible
// outcome — "nothing changed since you last failed" — used to be the most
// expensive one to observe: a full replan and a full re-execution to reach the
// verdict the previous run already reached. A negative entry closes that gap.
//
// # What is recorded, and what is refused
//
// A negative entry is recorded ONLY when every one of these holds, because each
// one is a way the verdict could otherwise depend on something the cache key
// does not describe:
//
//   - CACHING IS ON FOR THIS JOB AND THE KEY IS NON-EMPTY. A non-cacheable task
//     is excluded for free: it has no key at all. A bypassed one (--no-cache,
//     --no-cache-projects) does have a key — the bypass suppresses ENTRIES, not
//     identity — and is refused explicitly below.
//   - THE STATUS IS "failed". A canceled task says nothing about its inputs
//     (the session was cut short), and a skipped one already has its own
//     caching rule.
//   - THE JOB'S OWN LOGIC RAN (resultEmittedMeta). A wrapper bail-out — an
//     unavailable Go toolchain, an empty project — fails for a reason that is a
//     property of THIS HOST, not of the task's inputs. isCacheableResult
//     applies the same guard to a cached skip.
//   - THE FAILURE IS NOT A TIMEOUT. A deadline is a function of host load, not
//     of the cache key, so pinning one would turn a busy laptop into a
//     permanent verdict. JobResult.TimedOut is the structural marker every
//     deadline-to-failure site sets; the message is never string-matched.
//   - THE RUN IS NOT HOSTED (runcredential.Hosted). A hosted run installs
//     offline, and a failure from a missing dependency is not in the key of a
//     task that does not declare the offline signal.
//   - THE FAILURE IS NOT A SENSITIVE LEAK. FailureSensitiveLeakDetected rests
//     on an invocation-scoped artifact's path and bytes, and on the process
//     capabilities a run provisions — per-run values the key does not describe,
//     which is the same reason the timeout is refused.
//
// # What invalidates it
//
// Exactly what invalidates a positive entry: the v5 cache key. Plus two
// explicit escapes — --retry-failed, and a later SUCCESS at the same key, which
// DELETES the record however the run was configured, including under a cache
// bypass. There is no TTL and no clock: a failure the inputs cannot explain is
// a determinism bug, and smoothing it over with an expiry would hide it. See
// tooling/cli/doc/adr/0030.
//
// --no-cache and --no-cache-projects only suppress the READ. They are not
// invalidation on their own: a bypassed run that fails again leaves the
// previous record exactly as it found it, and only a bypassed SUCCESS removes
// it. See recordExecutedOutcome for that asymmetry.

// replayFailedEntry serves a recorded failure for this task's cache key, or nil
// when there is none to serve.
//
// It is consulted on the read path AFTER the local and remote positive lookups
// and immediately BEFORE the lease/coalesce step, so a positive entry anywhere
// still wins and every existing hit path is unchanged. The replayed result is
// an ordinary failed result — same Status, same effect on dependents, same
// non-zero exit — carrying only the provenance renderers annotate it with.
func (s *Scheduler) replayFailedEntry(job *ScheduledJob, hash string) *JobResult {
	if s.cache == nil || hash == "" || s.cfg.RetryFailed || s.cfg.NoCache {
		return nil
	}
	record := s.replayRecordOnce(hash)
	if record == nil {
		return nil
	}

	result := jobResultFromEntryResult(record.Result)
	// Every caller at this key shares one memoized record, so give each result
	// its own top-level payload map rather than an alias of the record's.
	// Events are already cloned on the way out of the entry.
	result.Data = cloneEventData(result.Data)
	if result.Status != string(TaskStatusFailed) {
		// Defense in depth: the store already refuses a record that does not
		// state a failure. A non-failure here must never be replayed as one.
		return nil
	}
	result.ExitCode = record.ExitCode
	result.ReplayedFailure = &ReplayedFailure{
		FirstFailedAt: record.FirstFailedAt,
		Attempts:      record.Attempts,
	}

	// The task's cache key is deliberately NOT published into the shared hashes
	// map: a cold failure does not publish one either, and a dependent must key
	// exactly as it would have after a fresh failure.
	if s.cfg.Debug {
		slog.Info("failure replayed from the local cache",
			"job", job.Key(), "entry", record.Address, "attempts", record.Attempts)
	}
	s.replayCacheEvents(job, result)
	return result
}

// replayRecordOnce consumes the negative entry at hash exactly once per run and
// serves that same record to every later caller at the same key.
//
// The memo is what keeps the attempt counter meaning "how many times has this
// workspace been told about this failure". A plan-level shared node's members
// all look up before enterSharedExecution decides which of them leads, so they
// all arrive here with one key; counting each would inflate the number a user
// reads to decide whether they are looping.
//
// The lock is held ACROSS the store call on purpose: those members arrive
// concurrently, and releasing between the memo check and the store's
// read-modify-write would let two of them count an observation. The serialized
// section is one small local record read reached only on a negative hit, which
// is the case this whole path exists to make cheap. A MISS is memoized too, so
// a key with no record costs one store read per run rather than one per node.
func (s *Scheduler) replayRecordOnce(hash string) *store.TaskFailure {
	s.replayedFailuresMu.Lock()
	defer s.replayedFailuresMu.Unlock()
	if record, seen := s.replayedFailures[hash]; seen {
		return record
	}
	record := s.cache.ReplayTaskFailure(hash)
	if s.replayedFailures == nil {
		s.replayedFailures = make(map[string]*store.TaskFailure)
	}
	s.replayedFailures[hash] = record
	return record
}

// recordExecutedOutcome is the negative-entry write side, called from
// finalizeExecutedJob for EVERY executed task — batched or singleton, after all
// --retry attempts — right after the positive entry is published.
//
// A success at the key deletes any recorded failure there: a genuinely fixed
// input must not leave a stale verdict behind for a later run to replay.
//
// # Why the two directions are not symmetric under a cache bypass
//
// A bypassed run (--no-cache, or --no-cache-projects over this task's closure)
// must NOT write a verdict at the key it refused to read: that is the whole
// point of the bypass, which says "this run's outcome is not describable by
// that key". So no positive entry, and no negative one either.
//
// A SUCCESS is different in kind. It is not a verdict being published, it is
// PROOF that the recorded failure at this key is stale — the task just ran, at
// these inputs, and passed. Deleting stale state is always safe: the worst a
// wrong delete can cost is one re-execution. Without this, --no-cache — the
// flag a user reaches for precisely to get past a stuck verdict — was the one
// path that provably could not clear it, and a flake recorded once replayed on
// every later cached run until someone found --retry-failed.
//
// A task with no identity to compute (CanUseCache false, hash "") never
// recorded a failure, so there is nothing to forget and this stays a no-op.
func (s *Scheduler) recordExecutedOutcome(job *ScheduledJob, result *JobResult, cacheEnabled bool, hash string) {
	if s.cache == nil || hash == "" || result == nil {
		return
	}
	if result.Status == "success" {
		// Unconditional and cheap: a key with no record removes a directory that
		// is not there. Reading first to log the difference would cost a store
		// read on every successful task, which is the hot path this whole file
		// exists to keep cheap.
		s.cache.ForgetTaskFailure(hash)
		return
	}
	if !cacheEnabled {
		return
	}
	if !recordableFailure(job, result) {
		return
	}
	// One run records one observation per key, however many plan nodes reached
	// it: a plan-level shared node's leader and its followers all finalize
	// against the same key, and counting each of them would inflate the attempt
	// number a user reads as "how many times have I run this".
	if !s.claimFailureRecord(hash) {
		return
	}
	record, err := s.cache.RecordTaskFailure(hash, store.TaskFailure{
		ExitCode:      result.ExitCode,
		FirstFailedAt: time.Now(),
		Result:        failureEntryResultFromJobResult(result),
	})
	if err != nil {
		slog.Debug("failure not recorded for replay",
			"project", job.Project.Name, "job", job.JobDef.Name, "error", err)
		return
	}
	if s.cfg.Debug {
		slog.Info("failure recorded for replay",
			"job", job.Key(), "entry", record.Address, "attempts", record.Attempts)
	}
}

// recordableFailure applies the correctness carve-outs: only a failure the
// task's own logic produced, and never one a deadline produced.
func recordableFailure(job *ScheduledJob, result *JobResult) bool {
	if result.Status != string(TaskStatusFailed) {
		return false
	}
	if result.TimedOut {
		return false
	}
	// A hosted run is the same class: it runs offline, so a task that needs a
	// dependency the fetch did not bring fails for a reason its key does not
	// describe unless the task declares the offline signal as an input.
	// Recording it would replay the failure on a run that may download.
	if runcredential.Hosted() {
		return false
	}
	// A sensitive-leak verdict is the same class as a timeout: its cause is not
	// in the key. The guard matches on an invocation-scoped artifact's absolute
	// path and byte samples (invocation_redact.go) and on the process
	// capabilities a run provisions (process_capabilities.go) — values the key
	// deliberately does not describe, and which the next run at this very key
	// provisions afresh. Recording one would replay "you leaked" at a key whose
	// inputs cannot say whether that is still true.
	if result.Error != nil && result.Error.Code == extensionproto.FailureSensitiveLeakDetected {
		return false
	}
	// A drift verdict is the same class again: it compares the task's output
	// with the WORKTREE the task ran in, which the key does not describe. The
	// scheduler judges drift after this record is written (closeTask), so this
	// arm is the guard for any other path that hands a drift failure here.
	if result.Error != nil && result.Error.Code == extensionproto.OutputDriftDiagnosticCode {
		return false
	}
	if !resultEmittedMeta(result) {
		return false
	}
	return CanUseCache(job)
}

// claimFailureRecord reports whether this run has not yet recorded a failure at
// hash, and claims it if so.
func (s *Scheduler) claimFailureRecord(hash string) bool {
	s.failureRecordsMu.Lock()
	defer s.failureRecordsMu.Unlock()
	if s.failureRecords == nil {
		s.failureRecords = make(map[string]bool)
	}
	if s.failureRecords[hash] {
		return false
	}
	s.failureRecords[hash] = true
	return true
}
