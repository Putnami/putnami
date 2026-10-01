// EXACTLY ONCE: the finalizer runs once whenever its producer STARTED, and
// never when it did not.
//
// The invariant this file owns is a LIFETIME rather than a value, and that is
// why the finalizer is not a DAG node. Its trigger is producer START and its
// completion signal is the consumers' TERMINAL states — neither of which a
// dependency graph can express, since a graph only knows "ran successfully".
// Dispatch is therefore explicit, and it has exactly two entries:
//
//   - the FRONTIER path, on the coordinator, at the moment every consumer of a
//     relation has reached a terminal state;
//   - the SWEEP, at the end of the run, for the relations whose frontier will
//     never become terminal because the run was canceled or failed. Cleanup
//     that waits for a state that will never arrive is a leak.
//
// Both fold through claimFinalizer, the single exactly-once gate, which is what
// keeps them from racing into a double teardown. A canceled run additionally
// gives the finalizer an INDEPENDENT bounded context: a teardown that inherits
// the cancellation which triggered it cannot run at all, and that is precisely
// the case where the resource leaks.
//
// Relations that never armed are SKIPPED with a terminal row rather than
// canceled — there was nothing to tear down — so every finalizer in the plan
// still reaches every surface an ordinary task reaches, whichever way the run
// ended.
package jobs

import (
	"context"
	"sync"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
)

// frontierTerminal reports whether every consumer of the relation has reached a
// terminal state in results. The caller holds the results mutex.
func (rel *invocationRelation) frontierTerminal(results map[string]*JobResult) bool {
	for _, consumer := range rel.consumers {
		if results[consumer.Key()] == nil {
			return false
		}
	}
	return true
}

// pendingFinalizers returns the relations whose finalizer must still run: armed,
// not yet finalized, and — when requireFrontier is set — with a terminal
// consumer frontier.
//
// The end-of-run sweep passes requireFrontier=false: a canceled run leaves
// consumers without results forever, and cleanup that waits for a state that
// will never arrive is a leak.
func (rt *invocationRuntime) pendingFinalizers(
	results map[string]*JobResult,
	requireFrontier bool,
) []*invocationRelation {
	if rt == nil {
		return nil
	}
	var ready []*invocationRelation
	for _, rel := range rt.relations {
		rel.mu.Lock()
		eligible := rel.armed && !rel.finalized
		rel.mu.Unlock()
		if !eligible {
			continue
		}
		if requireFrontier && !rel.frontierTerminal(results) {
			continue
		}
		ready = append(ready, rel)
	}
	return ready
}

// claimFinalizer takes the exactly-once right to run the relation's finalizer.
func (rel *invocationRelation) claimFinalizer() bool {
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if !rel.armed || rel.finalized {
		return false
	}
	rel.finalized = true
	return true
}

// finalizerCleanupContext returns the context the finalizer runs under.
//
// While the run is live the finalizer shares its cancellation, exactly like any
// other task. Once the run has been canceled — a signal, a failure that stopped
// the world — cleanup gets an INDEPENDENT context: a teardown that inherits the
// cancellation that triggered it cannot run at all, which is precisely the case
// where the resource leaks. It is bounded, so a wedged provider cannot hold the
// process open forever.
func finalizerCleanupContext(ctx context.Context, job *ScheduledJob) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	budget := DefaultTimeoutMs
	if resolved := job.EffectiveTimeoutMs(); resolved > 0 {
		budget = resolved
	}
	return context.WithTimeout(context.WithoutCancel(ctx), time.Duration(boundedDeadlineMs(budget, 1))*time.Millisecond)
}

// runReadyFinalizers runs the finalizer of every relation whose consumer
// frontier has become terminal.
//
// It runs on the coordinator, synchronously, at the moment the frontier closes.
// Blocking dispatch for the duration of a teardown is deliberate: the frontier
// is terminal, so nothing that depends on the torn-down resource is waiting, and
// a finalizer that ran on a background goroutine would need its own lifetime
// accounting to keep the exactly-once guarantee across an abort.
func (s *Scheduler) runReadyFinalizers(ctx context.Context, results map[string]*JobResult, mu *sync.Mutex) {
	if s.invocations == nil {
		return
	}
	mu.Lock()
	ready := s.invocations.pendingFinalizers(results, true)
	mu.Unlock()
	for _, rel := range ready {
		s.runFinalizer(ctx, rel, results, mu)
	}
}

// sweepFinalizers runs every armed finalizer that has not run yet, whatever
// state its frontier reached, and then destroys the private scratch of every
// relation.
//
// This is the path a canceled or failed run takes. Its frontier will never
// become terminal — the consumers were skipped or killed — and "the finalizer
// runs exactly once whenever its producer started" has no exception for that.
func (s *Scheduler) sweepFinalizers(ctx context.Context, results map[string]*JobResult, mu *sync.Mutex) {
	if s.invocations == nil {
		return
	}
	mu.Lock()
	pending := s.invocations.pendingFinalizers(results, false)
	mu.Unlock()
	for _, rel := range pending {
		s.runFinalizer(ctx, rel, results, mu)
	}
	for _, rel := range s.invocations.relations {
		rel.discard()
	}
}

// runFinalizer executes one relation's finalizer at most once and records its
// terminal row on every surface an ordinary task reaches.
func (s *Scheduler) runFinalizer(
	ctx context.Context,
	rel *invocationRelation,
	results map[string]*JobResult,
	mu *sync.Mutex,
) {
	if !rel.claimFinalizer() {
		return
	}
	job := rel.finalizer
	cleanupCtx, cancel := finalizerCleanupContext(ctx, job)
	defer cancel()

	started := time.Now()
	s.renderer.JobStart(job)
	result := s.runTaskAttempt(cleanupCtx, job)
	// Finalizers bypass runWorkItems, the ordinary attempt lifecycle that files
	// every completed subprocess in the physical ledger. Record immediately
	// after waiting so this execution keeps its rusage even if later result
	// handling turns it into a finalizer-specific failure.
	s.executions.record(result)
	if result == nil {
		result = &JobResult{Status: "failed"}
	}
	result = s.invocations.guardResult(job, result)
	if result.Status == "failed" && (result.Error == nil || result.Error.Code == "") {
		message := "invocation finalizer failed"
		if result.Error != nil && result.Error.Message != "" {
			message = result.Error.Message
		}
		result.Error = &JobError{Code: extensionproto.FailureSensitiveFinalizerFailed, Message: message}
	}
	result.TaskWall = time.Since(started)

	mu.Lock()
	results[job.Key()] = result
	mu.Unlock()
	s.renderer.JobComplete(job, result)
	s.emitSessionJobEnd(job, result)
}

// settleFinalizers records a terminal row for every finalizer that never ran:
// its producer was pruned by an all-hit consumer frontier, or never started at
// all. They are SKIPPED rather than canceled — there was nothing to clean up.
func (s *Scheduler) settleFinalizers(results map[string]*JobResult, mu *sync.Mutex) {
	if s.invocations == nil {
		return
	}
	var settled []jobDone
	mu.Lock()
	for _, rel := range s.invocations.relations {
		key := rel.finalizer.Key()
		if _, exists := results[key]; exists {
			continue
		}
		rel.mu.Lock()
		armed := rel.armed
		rel.mu.Unlock()
		if armed {
			continue
		}
		skipped := &JobResult{
			Status: "skipped",
			Error:  &JobError{Message: "no invocation-scoped resource was provisioned"},
		}
		results[key] = skipped
		settled = append(settled, jobDone{job: rel.finalizer, result: skipped})
	}
	mu.Unlock()

	for _, done := range settled {
		s.renderer.JobStart(done.job)
		s.renderer.JobComplete(done.job, done.result)
		s.emitSessionJobEnd(done.job, done.result)
	}
}

// "A runOn: finally step failed … the invocation's outcome is unchanged" is
// enforced in the ONE reduction every surface folds through (votesOnTheVerdict
// in result_reduce.go), not by handing the scheduler's own reduction a filtered
// result map. The renderers' Finish and the machine projection re-reduce the raw
// map, so a filter applied here alone made a failed finalizer count Failed in
// the displayed and machine summaries while the exit code said success.

// closeInvocations ends every relation's lifecycle once the workers are done
// and each consumer frontier is as terminal as it will ever get.
//
// It runs each armed finalizer that has not run — a canceled or failed run
// never closes its frontier, and "exactly once whenever the producer started"
// has no exception for that — with its own bounded context after cancellation,
// destroys the private scratch, and records a terminal row for the finalizers
// that were never armed.
func (s *Scheduler) closeInvocations(ctx context.Context, results map[string]*JobResult, mu *sync.Mutex) {
	s.sweepFinalizers(ctx, results, mu)
	s.settleFinalizers(results, mu)
}
