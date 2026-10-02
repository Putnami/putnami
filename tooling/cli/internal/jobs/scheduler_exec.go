package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/hooks"
	"go.putnami.dev/tooling/cli/internal/store"
)

// runPreBuildHookFunc is the function used to invoke the preBuild hook
// subprocess. Overridable in tests to exercise the dedup/wait logic without
// actually spawning subprocesses.
var runPreBuildHookFunc = hooks.RunPreBuildHook

// ONE runner, 1..n work items.
//
// A batched dispatch used to run an ALTERNATE lifecycle — whole-batch retry, a
// deadline widened ×n for everyone, per-project results reconstructed from an
// aggregate, and a rejoin with the ordinary path only at finalizeExecutedJob.
// There is now ONE lifecycle for every scheduled task, and the size of the
// dispatch group is the only thing batching changes:
//
//	openTask     (per task) cache lookup / restore / claim, shared-execution
//	                        rendezvous
//	prepareTask  (per task) inside the group's task-output locks: output
//	                        preparation, version refresh, preBuild hook, JobStart
//	runWorkItems (1..n)     the runner API: ONE shared subprocess for the whole
//	                        group, then PER-TASK re-execution of failures
//	closeTask    (per task) finalizeExecutedJob — run history, source-mutation
//	                        marking, task-owned capture, and the remote upload —
//	                        plus the scheduler-level wall
//
// A singleton dispatch is the n == 1 case of exactly that code, so there is no
// second lifecycle left to drift: a batch member's cache key, lease, hooks,
// capture model, entry format and finalization are the ones it would get alone.
// What a batch still shares is the SUBPROCESS (a measured win) and the
// CPU grant that bounds it.

// taskWork is one work item of a runner invocation: the plan node plus the
// per-task lifecycle state the scheduler holds from the moment its cache
// lookup said "miss" until its result is published.
type taskWork struct {
	job             *ScheduledJob
	cacheEnabled    bool
	cacheHash       string
	sourceInputHash string
	release         func()
	started         time.Time
	// shared is this task's participation in a plan-level shared node, or nil
	// when the node has no content-identical peer. The LEADER publishes its
	// terminal result through it; a follower carries the adopted result below
	// and never spawns (see scheduler_shared.go).
	shared *sharedExecutionLease
	// sharedResult is the leader's result this follower adopts. Non-nil only
	// for a follower, and its presence is what replaces the subprocess.
	sharedResult *JobResult
	// drift is the pre-write snapshot of every declared output carrying a drift
	// policy, taken before the hook and the subprocess (task_drift.go). Nil when
	// the task polices nothing or adopts a leader's result.
	drift *driftReference
}

// executeJobGroup runs one dispatch group — a singleton or a batch — through
// the task lifecycle. Every per-task boundary (hit, coalesced result,
// preparation failure, cache entry, terminal row) stays individual; only the
// subprocess and its CPU grant are shared.
func (s *Scheduler) executeJobGroup(
	ctx context.Context,
	group []*ScheduledJob,
	mu *sync.Mutex,
	hashes map[string]string,
	results map[string]*JobResult,
) []jobDone {
	if len(group) == 0 {
		return nil
	}
	var internalRunner InternalJobRunner
	var internalJob *ScheduledJob
	for _, job := range group {
		if runner := s.internalJobs[job.Key()]; runner != nil {
			internalRunner = runner
			internalJob = job
			break
		}
	}
	if internalRunner != nil {
		if len(group) != 1 || internalJob != group[0] {
			failed := make([]jobDone, 0, len(group))
			for _, job := range group {
				failed = append(failed, jobDone{job: job, result: &JobResult{
					Status: "failed",
					Error:  &JobError{Message: "framework-owned jobs cannot share a physical dispatch"},
				}})
			}
			return failed
		}
		// Framework-owned nodes bypass cache, hooks, task context construction and
		// subprocess execution. Readiness and completion remain scheduler-owned,
		// so a failure blocks dependents exactly like an extension task failure.
		started := time.Now()
		s.renderer.JobStart(internalJob)
		mu.Lock()
		snapshot := make(map[string]*JobResult, len(results))
		for key, result := range results {
			snapshot[key] = result
		}
		mu.Unlock()
		result := internalRunner(ctx, snapshot)
		if result == nil {
			result = &JobResult{
				Status: "failed",
				Error:  &JobError{Message: "framework-owned job returned no result"},
			}
		}
		// The renderers see a framework-owned node's events under its own
		// identity, as they see an executed task's.
		for _, event := range result.Events {
			s.renderer.JobEvent(internalJob, event)
		}
		result.TaskWall = time.Since(started)
		return []jobDone{{job: internalJob, result: result}}
	}

	completed := make([]jobDone, 0, len(group))
	opened := make([]taskWork, 0, len(group))
	for _, job := range group {
		item, terminal := s.openTask(ctx, job, mu, hashes)
		if item == nil {
			completed = append(completed, terminal)
			continue
		}
		opened = append(opened, *item)
	}
	if len(opened) == 0 {
		return completed
	}

	// Every member that reached execution takes its task-output locks here, in
	// ONE sorted acquisition for the whole group, after every lock-free wait of
	// openTask and before the first write on its behalf. The hold spans
	// preparation, the subprocess and its retries, and closeTask's capture; the
	// keys only a preparation needs end with it (task_output_lock.go).
	lockJobs := make([]*ScheduledJob, len(opened))
	for i := range opened {
		lockJobs[i] = opened[i].job
	}
	ctx, outputs, err := s.lockTaskOutputs(ctx, lockJobs, s.preparationWritesProjectTree)
	if err != nil {
		for i := range opened {
			result := spawnFailureResult(ctx, err)
			result.CacheKey = opened[i].cacheHash
			result.TaskWall = time.Since(opened[i].started)
			completed = append(completed, s.abandonTask(opened[i], result))
		}
		return completed
	}
	defer outputs.release()

	work := make([]taskWork, 0, len(opened))
	for i := range opened {
		item, terminal := s.prepareTask(ctx, opened[i], mu, hashes)
		if item == nil {
			completed = append(completed, terminal)
			continue
		}
		work = append(work, *item)
	}
	ctx = outputs.endPreparation(ctx)
	if len(work) == 0 {
		return completed
	}
	for i := range work {
		defer work[i].release()
	}
	s.acquireGroupCPUBudget(work)

	runs := s.runWorkItems(ctx, work)
	for i := range work {
		completed = append(completed, s.closeTask(ctx, work[i], runs[work[i].job.Key()], mu, hashes))
	}
	return completed
}

// openTask is the read side of ONE task's lifecycle: cache lookup, restore or
// claim, and the shared-execution rendezvous. It returns a work item when the
// task must execute, or nil plus the terminal row when it must not — a cache
// hit or a pruned producer. It holds no task-output lock while it waits: a
// restore takes and releases its own (restoreDeclaredCacheHit), and the
// execution's locks are taken after it returns.
func (s *Scheduler) openTask(
	ctx context.Context,
	job *ScheduledJob,
	mu *sync.Mutex,
	hashes map[string]string,
) (*taskWork, jobDone) {
	started := time.Now()

	// A relation whose whole consumer frontier was already cached provisions
	// nothing: the producer is pruned here, and its finalizer is never armed
	// (settleFinalizers records the skipped row). The CONSUMERS are not pruned —
	// they are the reason the relation was, so each one falls through to the
	// lookup below and is served the entry that made the pruning decision.
	if s.invocations.isPruned(job) {
		s.renderer.JobStart(job)
		return nil, jobDone{job: job, result: &JobResult{
			Status:   "skipped",
			Error:    &JobError{Message: "every consumer of this invocation-scoped resource was served from cache"},
			TaskWall: time.Since(started),
		}}
	}

	cacheEnabled := isCacheEnabled(job, s.bypass)
	cacheHash, cachedResult, release := s.lookupRestoreOrClaim(ctx, job, cacheEnabled, mu, hashes)
	if cachedResult != nil {
		cachedResult.CacheKey = cacheHash
		cachedResult.TaskWall = clampWallToFirstEvent(
			time.Since(started), cachedResult.SpawnToFirstEvent, cachedResult.FirstEventObserved)
		return nil, jobDone{job: job, result: cachedResult}
	}

	// This task's own entry could not serve it. A plan-level shared node is the
	// second place its work may already exist: another command scheduled the
	// same manifest task over the same project with the same declared inputs.
	// The first member to arrive here leads and
	// executes; the rest wait for it and adopt its result.
	//
	// It is consulted AFTER the lookup on purpose. A member whose own entry hits
	// costs nothing and needs no peer, and it must not take the slot: a hit
	// spawns nothing, so a follower parked on it would be waiting for work that
	// is never going to happen.
	shared := s.enterSharedExecution(job)
	var sharedResult *JobResult
	if shared != nil && !shared.leader {
		sharedResult = adoptSharedResult(shared.await(ctx))
	}
	return &taskWork{
		job:          job,
		cacheEnabled: cacheEnabled,
		cacheHash:    cacheHash,
		release:      release,
		started:      started,
		shared:       shared,
		sharedResult: sharedResult,
	}, jobDone{}
}

// abandonTask ends an opened task that will not execute: it releases the
// cache lease, opens the row and publishes result as the task's terminal
// result.
func (s *Scheduler) abandonTask(item taskWork, result *JobResult) jobDone {
	item.release()
	s.renderer.JobStart(item.job)
	// A leader that dies before its subprocess exists still owes its followers
	// an answer, or they wait for the run's whole duration.
	item.shared.publish(result)
	return jobDone{job: item.job, result: result}
}

// prepareTask is the pre-execution side of ONE task's lifecycle, run while the
// task holds its task-output locks: drift reference, output preparation,
// version refresh, preBuild hook, invocation arming and JobStart. It returns
// the work item to execute, or nil plus the terminal row of a preparation
// failure.
func (s *Scheduler) prepareTask(
	ctx context.Context,
	item taskWork,
	mu *sync.Mutex,
	hashes map[string]string,
) (*taskWork, jobDone) {
	job := item.job
	fail := func(err error) (*taskWork, jobDone) {
		return nil, s.abandonTask(item, &JobResult{
			Status: "failed",
			Error:  &JobError{Message: err.Error()},
			// The key was computed before preparation failed, and the record
			// names it like any other keyed verdict.
			CacheKey: item.cacheHash,
			TaskWall: time.Since(item.started),
		})
	}

	// The drift reference is taken here, after every way this task could still
	// be served without writing (a hit, an adopted result) and before anything
	// that writes on its behalf: the preBuild hook and the subprocess are both
	// the run, from the worktree's point of view (task_drift.go).
	if item.sharedResult == nil {
		item.drift = s.snapshotExecutedDrift(job)
	}

	// Detach a prior session's command-output symlink before an executing task
	// can write its declared subtree. The task-owned entry model never relinks a
	// whole command directory, so this is the only shared-output coordination
	// needed after the inferred-capture path is gone.
	if declaresCommandOutput(job) {
		if err := s.ensureCommandOutputWritable(job); err != nil {
			return fail(fmt.Errorf("prepare command output: %w", err))
		}
	}

	// This project is about to execute rather than reuse an existing artifact.
	// Refresh its version metadata once, using the scheduler-wide build time.
	// All-hit projects deliberately keep the build time stored with their cached
	// output and therefore avoid invalidating an otherwise identical .gen tree.
	s.refreshVersionFile(job)

	// Run preBuild hook (once per extension+project pair)
	if err := s.runPreBuildHook(ctx, job); err != nil {
		return fail(fmt.Errorf("preBuild hook: %w", err))
	}

	// A producer of an invocation-scoped resource is STARTING. Creating its
	// private scratch and publishing its lease here — after every way this task
	// could still fail to reach the subprocess, and before the subprocess exists
	// — is what makes "the finalizer runs whenever the producer started" the
	// same statement as "a lease exists on disk".
	if err := s.armInvocation(job, mu, hashes); err != nil {
		return fail(err)
	}

	s.renderer.JobStart(job)
	if item.cacheEnabled && declaredSourceRewriter(job) {
		// computeJobCacheHash already populated this exact memoized digest during
		// lookup. Keep the value across execution because another project's
		// source fixer may invalidate the shared memo concurrently.
		item.sourceInputHash, _ = sourceInputDigest(s.ws, job, s.commandParams, s.cache)
	}
	return &item, jobDone{}
}

// plannedCeiling returns one job's fixed pre-dispatch recommendation and the
// ceiling its subprocess may use. prepareScheduling plans every scheduled job,
// so the recompute is only for a job that reached execution without a plan; it
// reproduces the same deterministic inputs rather than inventing a fallback.
// A missing class entry reads as zero, which max leaves alone.
func (s *Scheduler) plannedCeiling(job *ScheduledJob) (cpuRecommendation, int) {
	classBudget := s.batchCPUBudgetByJob[job.Key()]
	if plan, ok := s.resourcePlanByJob[job.Key()]; ok {
		return plan.recommendation, max(plan.ceiling, classBudget)
	}
	recommendation := s.cpuAlloc.recommend(
		job.EffectiveCPUWeight(),
		job.ExpectedCPUWorkMs,
		float64(job.ExpectedWallMs),
		job.HistoricalCPUCeiling,
		s.criticalityCeilingByJob[job.Key()],
	)
	return recommendation, max(recommendation.budget, classBudget)
}

// acquireGroupCPUBudget takes the ONE CPU ceiling the work items share and
// stamps it on every member. Ceilings are not aggregate reservations: there is
// no live claim to release, so peer lifetime cannot affect a grant.
//
// The ceiling stays group-scoped for a batch, deliberately:
// what a budget bounds is a SUBPROCESS — it is exported as PUTNAMI_CPU_BUDGET
// and, for Go tooling, as GOMAXPROCS — and a batch is one subprocess. Splitting
// it per member would either hand n budgets to a single process tree (each of
// which it would read as its whole allowance) or shrink the shared tool to one
// member's share of the machine while it does n members' work. Every compatible
// planned member instead receives its class's fixed maximum recommendation, so
// the shared tool has enough threads for its most demanding member without
// making physical concurrency depend on runtime group membership.
func (s *Scheduler) acquireGroupCPUBudget(work []taskWork) {
	if s.cpuAlloc == nil || len(work) == 0 {
		return
	}
	key := work[0].job.Key()
	if len(work) == 1 {
		job := work[0].job
		recommendation, budget := s.plannedCeiling(job)
		job.CPUBudget = s.cpuAlloc.recordGrant(key, job.EffectiveCPUWeight(), job.ExpectedCPUWorkMs, budget)
		job.CPUBudgetHistoryEligible = recommendation.fromHistory
		job.CPUBudgetUnweightedCeiling = recommendation.unweightedCeiling
		return
	}

	weight := 0.0
	weightedWork := 0.0
	budget := 0
	key += "#batch"
	// A batch is one subprocess with one tool-native thread pool. Take the fixed
	// class ceiling (defensively, the max of every member's class) rather than
	// summing the runtime cohort, whose membership varies with cache hits and
	// dependency timing.
	for i := range work {
		job := work[i].job
		memberWeight := job.EffectiveCPUWeight()
		weight += memberWeight
		weightedWork += memberWeight * job.ExpectedCPUWorkMs
		_, memberBudget := s.plannedCeiling(job)
		budget = max(budget, memberBudget)
	}
	expectedCPU := 0.0
	if weight > 0 {
		expectedCPU = weightedWork / weight
	}
	budget = s.cpuAlloc.recordGrant(key, weight, expectedCPU, budget)
	for i := range work {
		work[i].job.CPUBudget = budget
		// The shared class ceiling is not evidence that any one member needs that
		// concurrency when it next runs alone.
		work[i].job.CPUBudgetHistoryEligible = false
		work[i].job.CPUBudgetUnweightedCeiling = 0
	}
}

// taskRun is what the runner returns for ONE work item: the result its last
// execution produced and the instant that execution finished. The instant is
// per item rather than per group so a member that needed no retry is not
// charged for a batch-mate's, which would inflate exactly the walls B5b
// measures the batching economics from.
type taskRun struct {
	result *JobResult
	ranAt  time.Time
}

// runWorkItems is THE runner API: it executes 1..n work items and returns one
// taskRun per item, keyed by job key.
//
// # Retry semantics
//
// Attempt 1 is the SHARED invocation — one subprocess for the whole group.
// Every retry is PER TASK and SOLO: only the members whose own result failed
// re-execute, each on its own plan node, with its own per-task deadline and its
// own live event stream. The whole-batch retry this replaces was wrong in three
// independent ways:
//
//   - it re-ran members that had already succeeded, which is both wasted work
//     and a chance to turn a green member red on a later attempt;
//   - it could not retry a member that failed INSIDE a successful aggregate (a
//     project with lint errors), so `--retry` meant something different
//     depending on whether a task happened to be batched — the exact
//     solo↔batch divergence this slice removes;
//   - a retried batch re-entered the alternate path, so a member that passed on
//     attempt 2 was stored from a slice of an aggregate rather than from its own
//     execution.
//
// The cost of the change is bounded and only on the failure path: with the
// default --retry=0 there are no retries at all and this is byte-identical to
// before, and a leader that dies with --retry=n costs at most n solo attempts
// per member instead of n batches. Everything else in the task's lifecycle
// (lease, hooks, capture model, entry) is already open and is NOT redone: a
// retry is another attempt inside one task, exactly as in the singleton path.
func (s *Scheduler) runWorkItems(ctx context.Context, work []taskWork) map[string]taskRun {
	results := s.runSharedAttempt(ctx, work)
	sharedEnded := time.Now()

	// File the shared attempt's physical execution BEFORE any retry can replace
	// the result that carries it. A batch contributes one entry (every member
	// holds the same execution), and it stays in the ledger even when the split
	// failed and all n members go on to re-execute solo — that subprocess ran,
	// and the run's cost includes it whether or not a surviving record still
	// names it.
	for i := range work {
		s.executions.record(results[work[i].job.Key()])
	}

	maxAttempts := s.cfg.Retry + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	runs := make(map[string]taskRun, len(work))
	for i := range work {
		key := work[i].job.Key()
		run := taskRun{result: results[key], ranAt: sharedEnded}
		for attempt := 1; attempt < maxAttempts; attempt++ {
			// An adopted result has no attempt of its own to repeat: retrying it
			// would spawn the subprocess the shared node exists to avoid, and the
			// leader has already applied this run's retry budget to the identical
			// work.
			if work[i].sharedResult != nil {
				break
			}
			if run.result == nil || run.result.Status != "failed" || ctx.Err() != nil {
				break
			}
			run.result = retainFirstEventLatency(run.result, s.runTaskAttempt(ctx, work[i].job))
			// Each retry is another physical execution. The task record will name
			// only the final one, so recording here is what keeps the superseded
			// attempts' wall, CPU and RSS in the run's cost.
			s.executions.record(run.result)
			run.ranAt = time.Now()
		}
		runs[key] = run
	}
	return runs
}

// runSharedAttempt performs the ONE subprocess invocation the group shares.
//
// n == 1 is the ordinary singleton spawn, streaming its events live. n > 1
// spawns the batch leader and splits its typed wire back onto the members
// (scheduler_batch_exec.go); either way the caller receives one result per work
// item and cannot tell which shape produced it.
func (s *Scheduler) runSharedAttempt(ctx context.Context, work []taskWork) map[string]*JobResult {
	if len(work) == 1 {
		// A follower of a plan-level shared node already holds the result its
		// leader produced. This is the ONE place the subprocess is skipped; every
		// other step of the lifecycle — lease, hooks, capture, entry, session row
		// — runs exactly as it would for a task that executed.
		if work[0].sharedResult != nil {
			s.replayResultEvents(work[0].job, work[0].sharedResult)
			return map[string]*JobResult{work[0].job.Key(): work[0].sharedResult}
		}
		return map[string]*JobResult{work[0].job.Key(): s.runTaskAttempt(ctx, work[0].job)}
	}
	return s.runBatchAttempt(ctx, work)
}

// runTaskAttempt is ONE ordinary subprocess attempt for ONE task: the job's own
// definition, its own per-task deadline, its own live event stream. It is what
// a singleton dispatch runs and what a failed batch member re-executes as.
func (s *Scheduler) runTaskAttempt(ctx context.Context, job *ScheduledJob) *JobResult {
	ctx = s.processCapabilities.contextForJob(ctx, job)
	var before verificationTreeSnapshot
	var observationErr error
	if s.cfg.CacheVerification {
		before, observationErr = snapshotJobWorkspaceTree(s.ws, job)
	}
	result, err := runJob(
		ctx,
		s.ws,
		job,
		s.taskParams,
		s.commandDefaults(job),
		s.cfg.VersionInfo,
		s.jobProcessEnv(),
		s.invocations.eventSink(job, func(event RawJobEvent) { s.renderer.JobEvent(job, event) }),
	)
	if err != nil {
		result = spawnFailureResult(ctx, err)
	}
	if s.cfg.CacheVerification {
		after, err := snapshotJobWorkspaceTree(s.ws, job)
		if observationErr == nil {
			observationErr = err
		}
		if observationErr != nil {
			result.VerificationError = observationErr.Error()
		} else {
			result.VerificationWrites = diffVerificationTreeSnapshots(before, after)
		}
	}
	return result
}

// closeTask is the write side of ONE task's lifecycle: publication through
// finalizeExecutedJob and the scheduler-level wall.
//
// The wall is measured from run.ranAt — when THIS task's execution finished —
// rather than from "now", so member k's wall is free of members 0..k-1's
// finalization and a batch member's reported wall stays comparable to the same
// task's wall when it runs alone.
func (s *Scheduler) closeTask(
	ctx context.Context,
	item taskWork,
	run taskRun,
	mu *sync.Mutex,
	hashes map[string]string,
) jobDone {
	finalizeStarted := time.Now()
	result := run.result
	if result != nil {
		// Confinement runs BEFORE publication: a sensitive path or its bytes must
		// never reach the cache entry, the session record, or the renderer's
		// completion row, and the producer's declared artifacts must exist before
		// the frontier is allowed to consume them.
		//
		// VALIDATION FIRST. provisioned is what derives the needles, from the
		// artifacts the task just wrote, so running the guard ahead of it matched
		// the PRODUCER's own result — its error, its data, its events — against an
		// empty set and published whatever it had echoed. Ordering it after is the
		// only way the producer is guarded with the needles derived from what it
		// produced.
		cause := s.invocations.provisioned(item.job, result)
		result = s.invocations.guardResult(item.job, result)
		// The producer's live stream was withheld until this moment for the same
		// reason; it is redacted with the same needle set and released now, after
		// the guard has read the events it shares its payload maps with.
		s.invocations.releaseProducerEvents(item.job, func(event RawJobEvent) {
			s.renderer.JobEvent(item.job, event)
		})
		// Producer replay observes the complete spooled stream, including events
		// omitted from bounded JobResult.Events. Fold its leak side channel into
		// the verdict before any cache/session/result publication.
		result = s.invocations.guardResult(item.job, result)
		if cause != nil && (result.Error == nil || result.Error.Code != extensionproto.FailureSensitiveLeakDetected) {
			result.Status = "failed"
			result.Error = cause
		}
	}
	if result == nil {
		// Unreachable by construction: a split that cannot name every member
		// fails the whole group instead (sharedFailureResults). Kept so a future
		// runner shape cannot silently publish a nil row.
		result = &JobResult{
			Status: "failed",
			Error:  &JobError{Message: "runner returned no result for " + item.job.Key()},
		}
	}
	result = s.finalizeExecutedJob(
		ctx, item.job, result, item.cacheEnabled, item.cacheHash, item.sourceInputHash, mu, hashes)
	// Drift is judged AFTER publication on purpose: the entry holds the
	// successful run the task did, and the failure cache never sees a verdict
	// that depends on the worktree rather than on the key (task_drift.go). A
	// follower adopts its leader's already-judged result.
	if item.sharedResult == nil {
		result = s.judgeExecutedDrift(item.job, result, item.drift)
	}
	result.CacheKey = item.cacheHash
	result.TaskWall = clampWallToFirstEvent(
		run.ranAt.Sub(item.started)+time.Since(finalizeStarted),
		result.SpawnToFirstEvent,
		result.FirstEventObserved,
	)
	// The leader of a plan-level shared node releases its followers here, once
	// its outputs are on disk, its entry is published and its result is final —
	// so a follower captures the same tree the leader was cached from and can
	// never observe a half-written one. Non-leaders and unshared tasks no-op.
	item.shared.publish(result)
	return jobDone{job: item.job, result: result}
}

// spawnFailureResult classifies a RunJob error that never produced a result: a
// cancellation that is not a deadline is the session being cut short, anything
// else is this task failing.
func spawnFailureResult(ctx context.Context, err error) *JobResult {
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &JobResult{Status: "canceled"}
	}
	// TimedOut marks the deadline structurally so no consumer has to read the
	// message: a timeout is a function of host load, not of the cache key, and
	// the failure cache must never pin one as this key's verdict.
	return &JobResult{
		Status:   "failed",
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		Error:    &JobError{Message: err.Error()},
	}
}

// retainFirstEventLatency carries the FIRST attempt's startup latency onto a
// retry's result. The measurement answers "how long did this task take to say
// anything", which the first attempt already answered; the singleton retry loop
// has always kept it and a per-task batch retry must not report differently.
func retainFirstEventLatency(previous, next *JobResult) *JobResult {
	if previous != nil && previous.FirstEventObserved && next != nil {
		next.SpawnToFirstEvent = previous.SpawnToFirstEvent
		next.FirstEventObserved = true
	}
	return next
}

// jobProcessEnv returns the run-level environment every job subprocess of this
// run receives on top of its own: this run's session id, and — when a remote
// cache is in force — the provider's object-cache socket and the resolved trust
// policy (protocol/cache ObjectCacheSocketEnv, CacheTrustEnv).
//
// It is RUN-level on purpose — the same values for every job — and
// execution-only: it reaches cmd.Env and nothing else, so it cannot move a cache
// key, a run marker, or the task params a job's identity is computed from.
//
// The two groups have DIFFERENT gates, and the difference matters. The cache
// entries are withheld under --no-cache, because a run that consults no cache
// must not hand its jobs a cache to consult either. The session id is not: it
// describes who spawned the process, a fact that stays true however the run is
// cached, and gating it would make nested runs invisible to accounting exactly
// on the uncached runs whose cost is worth the most.
//
// Withholding means ABSENT in the job, not "whatever this process inherited":
// buildJobInvocation strips the outer run's pair from os.Environ before
// appending this slice, so a nested --no-cache run never hands its jobs its
// parent's socket.
func (s *Scheduler) jobProcessEnv() []string {
	var env []string
	if s.cfg.SessionID != "" {
		env = append(env, protocolcli.ParentSessionEnv+"="+s.cfg.SessionID)
	}
	if s.remote != nil && !s.cfg.NoCache {
		env = append(env, s.remote.objectCacheJobEnv()...)
	}
	return env
}

// commandDefaults resolves the workspace config defaults for a job's command.
func (s *Scheduler) commandDefaults(job *ScheduledJob) map[string]any {
	if s.ws == nil || s.ws.Config == nil {
		return map[string]any{}
	}
	return s.ws.Config.GetCommandDefaults(jobCommandName(job), job.Extension.Name)
}

// finalizeExecutedJob applies the per-project history, source-mutation, cache
// publication, and remote-upload semantics after a real execution. It is
// reached through closeTask by EVERY executed task, batched or not, with that
// task's own result — so a batch member publishes the same task-owned entry
// and remote upload it would have published alone.
func (s *Scheduler) finalizeExecutedJob(
	ctx context.Context,
	job *ScheduledJob,
	result *JobResult,
	cacheEnabled bool,
	cacheHash string,
	sourceInputHash string,
	mu *sync.Mutex,
	hashes map[string]string,
) *JobResult {
	// A real task may update tracked source bytes even when those sidecars cannot
	// be declared as task-owned outputs (Go describe/generate are the canonical
	// examples). The next version-stamp refresh must therefore recompute this
	// project's binding rather than reuse the pre-execution value.
	s.invalidateCapabilitySourceBindingsForProject(job)

	// Fold the execution into the run history so future runs can boost tasks
	// that are CPU-unbalanced against their same-named peers and dispatch
	// long poles first. Only successful real executions teach the store.
	s.taskStats.record(job, result)

	// A task that may have rewritten the worktree's sources invalidates the
	// memoized input digests for every later job. When caching is active,
	// recompute its keyed project-source digest: only unchanged source bytes
	// prove that a stored green verdict can be reused without skipping edits.
	if result.Status == "success" && rewritesSourceTree(job) && s.cache != nil {
		s.cache.InvalidateFileHashes()
		if cacheEnabled && cacheHash != "" {
			afterSourceHash, err := sourceInputDigest(s.ws, job, s.commandParams, s.cache)
			result.SourceMutated = sourceInputHash == "" || err != nil ||
				afterSourceHash == "" || afterSourceHash != sourceInputHash
		}
	}

	// Publish this task's IDENTITY for its dependents, whether or not this run
	// may read or write an entry at it. A bypassed job's dependents are bypassed
	// too (NewCacheBypass walks the cache-key closure), so this can only ever
	// feed another bypassed key — and without it a --no-cache run would derive a
	// different key for the same inputs, which is the address the forget below
	// has to name to be worth anything.
	if cacheHash != "" && isCacheableResult(job, result) {
		mu.Lock()
		hashes[job.Key()] = cacheHash
		mu.Unlock()
	}

	// Cache store (on success, or a deterministic in-job skip) always publishes
	// a task-owned entry. isCacheEnabled admits only usesDeclaredCapture jobs.
	if cacheEnabled && s.cache != nil && cacheHash != "" && isCacheableResult(job, result) {
		stored := s.storeDeclaredCapture(job, result, cacheHash)
		if stored && !result.SourceMutated && s.remote != nil {
			s.remote.UploadTaskEntry(ctx, cacheHash, job, s.cache)
		}
	}

	// The negative side of the same publication: record a failure this key's
	// inputs fully explain, or delete the one a success at this key has just
	// disproved. Local only — the record has no remote address to travel to.
	s.recordExecutedOutcome(job, result, cacheEnabled, cacheHash)

	return result
}

// lookupRestoreOrClaim handles the complete read side before openTask mutates
// outputs or starts hooks: local lookup, remote restoration, then per-key lease
// ownership for a genuine miss. The returned release is always non-nil.
func (s *Scheduler) lookupRestoreOrClaim(
	ctx context.Context,
	job *ScheduledJob,
	cacheEnabled bool,
	mu *sync.Mutex,
	hashes map[string]string,
) (string, *JobResult, func()) {
	noop := func() {}
	if s.cache == nil {
		return "", nil, noop
	}

	// computeJobCacheHash only mixes in the cache keys of this job's direct
	// dependencies, so copy just those entries under the lock rather than the
	// whole (growing) hashes map. This keeps the per-job copy O(deps) instead
	// of O(N) and shortens the critical section every worker contends on.
	mu.Lock()
	hashCopy := make(map[string]string, len(job.DependsOn))
	for _, depKey := range cacheKeyDependencies(job) {
		if value, ok := hashes[depKey]; ok {
			hashCopy[depKey] = value
		}
	}
	mu.Unlock()

	// A BYPASSED task (--no-cache, or --no-cache-projects over its closure) is
	// refused every entry, but it still has an IDENTITY, and this is where it
	// is computed. finalizeExecutedJob needs it for the only two things a
	// bypassed run may do with a key: publish it for the (also bypassed)
	// dependents that fold it into their own, and delete a recorded failure
	// that this run's SUCCESS has just disproved — see recordExecutedOutcome
	// for why deleting is the one store write a bypass permits.
	//
	// A task that CANNOT use the cache has no identity and nothing to forget,
	// so it keeps the empty key and the whole path stays a no-op. The
	// computation is deliberately NOT recorded in CacheStats: nothing was
	// served, and a bypassed run must not report cache wall it never spent
	// serving anything.
	if !cacheEnabled {
		if !CanUseCache(job) {
			return "", nil, noop
		}
		hash, err := computeJobCacheHash(s.ws, job, s.commandParams, s.cfg.VersionInfo, s.cache, hashCopy)
		if err != nil {
			return "", nil, noop
		}
		return hash, nil, noop
	}
	return s.lookupDeclaredEntry(ctx, job, hashCopy, mu, hashes)
}

// cacheCoalescer is the three store operations the claim-or-wait loop needs,
// so every task-owned entry shares one implementation of lease semantics.
type cacheCoalescer interface {
	claim(hash string, estimatedCost time.Duration) (bool, func())
	wait(ctx context.Context, hash string, timeout time.Duration) error
	// restore consumes an entry the lease winner published, or returns nil when
	// there is none this job can use.
	restore(ctx context.Context, hash string) *JobResult
}

// coalesceMiss turns a production cache miss into claim-or-wait. A lease
// winner proceeds with execution and holds the returned release function until
// the task lifecycle has either published or abandoned the result. A loser consumes the
// winner's atomically-published entry; expiry retries the claim so exactly one
// waiter takes over, while timeout, cancellation, or storage errors preserve the
// previous reliable behavior by falling through to independent execution.
func (s *Scheduler) coalesceMiss(
	ctx context.Context,
	job *ScheduledJob,
	hash string,
	coalescer cacheCoalescer,
) (*JobResult, func()) {
	noop := func() {}
	estimatedCost := time.Duration(job.ExpectedWallMs) * time.Millisecond
	waitDeadline := time.Now().Add(s.cacheLeaseWaitTimeout(job))

	for ctx.Err() == nil {
		winner, release := coalescer.claim(hash, estimatedCost)
		if winner {
			return nil, release
		}

		remaining := time.Until(waitDeadline)
		if remaining <= 0 {
			return nil, noop
		}
		if s.onCacheLeaseWait != nil {
			s.onCacheLeaseWait(job)
		}
		err := coalescer.wait(ctx, hash, remaining)
		if err == nil {
			if result := coalescer.restore(ctx, hash); result != nil {
				// The entry was absent when this scheduler arrived and was
				// published by the lease owner while we waited. Keep the stored
				// execution status for DAG semantics, but report the reuse as
				// coalesced rather than as an ordinary warm-cache hit.
				result.MarkReuse(ReuseCoalesced)
				return result, noop
			}
			// The winner reported a publish but its entry is unavailable or cannot
			// satisfy this job. Computing locally is the bounded reliability fallback.
			return nil, noop
		}
		if !errors.Is(err, store.ErrLeaseExpired) {
			return nil, noop
		}
	}

	return nil, noop
}

// cacheLeaseWaitTimeout bounds coalescing by the same subprocess timeout the
// owner receives, expanded for configured retries. A crashed owner normally
// wakes waiters much earlier through the lease TTL; this ceiling handles a live
// but wedged owner and keeps coordination strictly best-effort.
func (s *Scheduler) cacheLeaseWaitTimeout(job *ScheduledJob) time.Duration {
	timeoutMs := DefaultTimeoutMs
	if resolved := job.EffectiveTimeoutMs(); resolved > 0 {
		timeoutMs = resolved
	}
	attempts := s.cfg.Retry + 1
	if attempts < 1 {
		attempts = 1
	}
	return time.Duration(boundedDeadlineMs(timeoutMs, attempts)) * time.Millisecond
}

// replayCacheEvents reproduces a cold run's renderer sequence for a cache hit:
// job:start followed by a job:event for each report-relevant record captured
// from the cached execution (result, metrics, diagnostics, summaries, meta,
// artifacts — the set retained by cacheResultEventType). Without this, a warm
// run emitted only job:start and a cache-hit job:end, silently dropping the
// result event that carries testSummary/coverageSummary from the JSONL stream.
// The captured events are display/report-only on every renderer
// (LiveRenderer.JobEvent mutates only display state; metric/diagnostic counters
// are folded in JobComplete's captureMetrics, so replaying does not double
// count), so this stays a faithful, side-effect-free replay.
//
// Cache keys do not include the CLI version, so entries written before result
// events were captured survive an upgrade with result.Data but no cached
// result event. When the replayed set lacks one, synthesizeResultEvent rebuilds
// it from the restored result so those legacy warm streams still carry
// testSummary/coverageSummary and fold like a cold run — emitted last, matching
// the order a cold subprocess streams its result event.
func (s *Scheduler) replayCacheEvents(job *ScheduledJob, result *JobResult) {
	s.renderer.JobStart(job)
	s.replayResultEvents(job, result)
}

// replayResultEvents emits a reused result's captured events as this job's own,
// without opening the job — the half of replayCacheEvents a caller that has
// already emitted job:start needs.
//
// A follower of a plan-level shared node is that caller: it went through
// openTask like any executing task and has already been
// started, but its "execution" was another node's, so its stream has to be
// replayed the way a cache hit's is or the renderers see a task that completed
// having said nothing.
func (s *Scheduler) replayResultEvents(job *ScheduledJob, result *JobResult) {
	if result == nil {
		return
	}
	sawResult := false
	for _, ev := range result.Events {
		if ev.Type == EventTypeResult {
			sawResult = true
		}
		s.renderer.JobEvent(job, ev)
	}
	if !sawResult {
		if ev, ok := synthesizeResultEvent(result); ok {
			s.renderer.JobEvent(job, ev)
		}
	}
}

func (s *Scheduler) refreshVersionFile(job *ScheduledJob) {
	if job == nil || job.Project == nil {
		return
	}
	s.versionFilesMu.Lock()
	defer s.versionFilesMu.Unlock()
	if s.versionFiles[job.Project.Path] {
		return
	}
	s.versionFiles[job.Project.Path] = true
	started := time.Now()
	spawned := generateVersionFilesAtWithSourceBindings(
		s.ws, []*ScheduledJob{job}, s.cfg.VersionInfo, s.versionBuildTime, false, s.capabilitySourceBindings,
	)
	s.cacheStats.recordLocalBindings(started, time.Now(), spawned)
}

// restampRestoredVersionStamp puts THIS run's build stamp back after a cache hit
// replaced a project's .gen subtree with a stored one.
//
// A generation entry is keyed on its content inputs rather than on
// the commit, so one entry is legitimately reused across revisions — and the
// .gen/version.json it carries names the revision that PRODUCED it. Leaving that
// stamp standing means `putnami deploy` reads <workload>/.gen/version.json
// and ships the previous commit's identity under the new release tag. Doing the
// re-stamp here, at the single convergence point of the local, remote and
// coalesced restores (see lookupDeclaredEntry) and BEFORE the hit is published,
// is what makes "the stamp reports the current SHA" hold on all three legs
// instead of on whichever one a fix happened to be written for.
//
// preserveMatching is TRUE, and that is what keeps that class of stale stamp
// shut. When every
// identity field already matches — re-running the same commit, the ordinary case
// — the restored file is left byte-for-byte alone, so buildTime does not churn.
// When it differs, the rewrite lands on exactly the identity
// preserveMatchingVersionFiles seeded before ANY key was computed. Either way the
// run ends holding the same buildTime-free digest (store.versionStampDigest) that
// every key computed in this run hashed, so this write cannot invalidate a key it
// was itself an input to.
//
// It deliberately does NOT claim the versionFiles slot refreshVersionFile owns:
// this is a preserve-style write for a project that reused an artifact, not the
// forced refresh a project gets when it is about to execute.
//
// The mutex serializes this writer against refreshVersionFile, but a cache-key
// READ of the stamp takes no lock, so between the materialize and this call a
// stale identity is briefly on disk. Nothing in the same project can observe it:
// serializeWriteResources puts every declared READER of the `gen` resource
// either after the writer it derives from or before the first writer, so a gen
// reader never runs concurrently with the gen restore — and with a remote cache
// configured, Negotiate's PrecomputeKeys has already memoized every file hash
// before the first task opens. Widening the window would mean giving the stamp a
// lock every key computation has to take, which is a cost paid on every task to
// close a window nothing reaches.
func (s *Scheduler) restampRestoredVersionStamp(job *ScheduledJob, entry *store.TaskEntry) {
	if job == nil || job.Project == nil || !restoreReplacesVersionStamp(entry) {
		return
	}
	s.versionFilesMu.Lock()
	defer s.versionFilesMu.Unlock()
	started := time.Now()
	spawned := generateVersionFilesAtWithSourceBindings(
		s.ws, []*ScheduledJob{job}, s.cfg.VersionInfo, s.versionBuildTime, true, s.capabilitySourceBindings,
	)
	s.cacheStats.recordLocalBindings(started, time.Now(), spawned)
}

// emitSessionJobEnd emits a session event for job completion.
func (s *Scheduler) emitSessionJobEnd(job *ScheduledJob, result *JobResult) {
	if s.onSessionEvent == nil {
		return
	}
	s.sessionEndedMu.Lock()
	if s.sessionEnded[job.Key()] {
		s.sessionEndedMu.Unlock()
		return
	}
	if s.sessionEnded == nil {
		s.sessionEnded = make(map[string]bool)
	}
	s.sessionEnded[job.Key()] = true
	s.sessionEndedMu.Unlock()

	// One projection, two representations: the typed task the in-process
	// consumers read and the untyped payload events.jsonl persists, the latter
	// derived from the former so they cannot drift.
	task := TaskSummaryOf(job, result)
	s.onSessionEvent(SessionRecord{
		JobKey: job.Key(),
		Type:   SessionRecordJobEnd,
		Data:   JobEndPayload(&task, job.CPUBudget),
		Task:   &task,
	})
}

// JobEndPayload renders a task's terminal result as the untyped job:end payload
// events.jsonl persists. It is the ONLY producer of that shape.
//
// The key set and the value types are a wire contract:
// internal/commands/sessions/sessions_helpers.go reads "duration", "cache" and "coalesced"
// off a recorded session, and workspace_state projects the rest into
// session.json's job table. TestJobEndPayload_PinsTheRecordedWire pins them.
func JobEndPayload(task *TaskResult, cpuBudget int) map[string]any {
	data := map[string]any{
		"project":    task.Project,
		"job":        task.Job,
		"taskKind":   task.TaskKind,
		"extension":  task.Extension,
		"status":     string(task.Status),
		"outcome":    task.Outcome(),
		"duration":   task.Timing.Duration.Milliseconds(),
		"taskWallMs": task.Timing.TaskWall.Milliseconds(),
		"cache":      task.Reuse.CacheHit(),
		"coalesced":  task.Reuse == ReuseCoalesced,
	}
	if task.Timing.FirstEventObserved {
		data["spawnToFirstEventMs"] = task.Timing.SpawnToFirstEvent.Milliseconds()
	}
	if cpuBudget > 0 {
		data["cpuBudget"] = cpuBudget
	}
	if task.Timing.CPUTime > 0 {
		data["cpuTimeMs"] = task.Timing.CPUTime.Milliseconds()
	}
	// The physical execution this logical row came out of. Batch members carry
	// the SAME id, so a reader of this log can de-duplicate a batch's cost
	// instead of adding one copy of it per member; it is
	// absent for a row that spawned nothing. Additive: every key above keeps its
	// name, type and meaning.
	if task.Execution != nil && task.Execution.ID != "" {
		data["executionId"] = task.Execution.ID
	}
	if task.Error != nil {
		data["error"] = task.Error.Message
	}
	return data
}

// runPreBuildHook runs the extension's preBuild hook for a job at most once
// per extension+project pair. Concurrent callers with the same key block until
// the in-flight run completes and observe the same error (so a failed hook
// fails every dependent job instead of being silently skipped). The hook runs
// inside its task's preparation hold and receives the held task-output lock
// ids ctx carries, so a nested session it starts never waits for its own task.
//
// A job of a hosted install's dependency fetch (WithDependencyFetch) runs no
// preBuild hook: a fetch runs no workspace code, and a hook is repository code
// that would run while another extension's fetch holds the job credential.
func (s *Scheduler) runPreBuildHook(ctx context.Context, job *ScheduledJob) error {
	if job.Extension.Hooks == nil || job.Extension.Hooks.PreBuild == nil || dependencyFetch(ctx) {
		return nil
	}

	hookKey := preBuildHookKey(job)

	s.hooksMu.Lock()
	slot, exists := s.hookSlots[hookKey]
	if !exists {
		slot = &hookSlot{done: make(chan struct{})}
		s.hookSlots[hookKey] = slot
	}
	s.hooksMu.Unlock()

	if !exists {
		_, slot.err = runPreBuildHookFunc(ctx, s.ws, job.Extension, job.Project, s.cfg.Debug, heldOutputLocksHookEnv(ctx))
		close(slot.done)
		return slot.err
	}

	select {
	case <-slot.done:
		return slot.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
