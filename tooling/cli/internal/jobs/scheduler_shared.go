package jobs

import (
	"context"
	"sync"
)

// The RUNTIME half of a plan-level shared node. The
// plan half is plan_shared.go, which decides which nodes are one shared
// execution; this file makes exactly one of them spawn a subprocess.
//
// The shape is deliberately the one runPreBuildHook already uses: the first
// member to reach the point of executing takes the slot and becomes the LEADER;
// every later member blocks on the leader's result and adopts it. Nothing about
// the plan decides who leads — whichever member the DAG makes ready first does,
// which is what keeps the mechanism correct when a member is skipped, canceled
// or served from its own cache entry. If the leader is skipped by a failed
// dependency it never takes the slot at all, so the other command's node
// executes exactly as it would have alone.
//
// What a follower is, precisely: a task whose own cache lookup MISSED and whose
// work another node in this same run is doing or has done. That is the same
// sentence ReuseCoalesced already describes, so a follower reports as
// coalesced. It then continues through the ordinary task lifecycle — it
// publishes its OWN cache entry at its OWN unchanged key, registers its own
// hash for its dependents' keys, and emits its own session row. The steady
// (warm) state of the workspace is therefore bit-identical to the state before
// this slice; only a COLD run loses the duplicate subprocess.
//
// Three properties are load-bearing and pinned by tests:
//
//   - The follower's result carries the LEADER'S Execution pointer, so the
//     physical ledger records that subprocess ONCE (executionLedger.record
//     de-duplicates on the id) while both logical rows still name it.
//   - The follower's CPUTime is zero. Wall is an attribution of the logical
//     task, exactly as a cache hit reports the producing run's duration, but CPU
//     is the quantity a run-scope roll-up sums, and one subprocess's CPU must
//     not be counted twice.
//   - A follower adopts a FAILURE too. Two nodes over identical inputs fail
//     identically, and adopting keeps "a describe failure fails both commands'
//     dependents" true without spending the failing minute twice.

// sharedExecutionSlot is the leader/follower rendezvous for one shared node.
// The leader closes done exactly once, after which result is immutable and
// readable without the lock.
type sharedExecutionSlot struct {
	done   chan struct{}
	result *JobResult
}

// sharedExecutionLease is one job's participation in a shared node. Only the
// leader may publish, and publishing is idempotent so every terminal path of
// the leader's lifecycle can call it unconditionally.
type sharedExecutionLease struct {
	slot   *sharedExecutionSlot
	leader bool
	once   sync.Once
}

// enterSharedExecution registers this job with its shared node, returning nil
// when the job has no shared peers.
func (s *Scheduler) enterSharedExecution(job *ScheduledJob) *sharedExecutionLease {
	if job == nil || job.SharedExecutionID == "" {
		return nil
	}
	s.sharedMu.Lock()
	defer s.sharedMu.Unlock()
	if s.sharedSlots == nil {
		s.sharedSlots = make(map[string]*sharedExecutionSlot)
	}
	if slot, ok := s.sharedSlots[job.SharedExecutionID]; ok {
		return &sharedExecutionLease{slot: slot}
	}
	slot := &sharedExecutionSlot{done: make(chan struct{})}
	s.sharedSlots[job.SharedExecutionID] = slot
	return &sharedExecutionLease{slot: slot, leader: true}
}

// publish hands the leader's terminal result to the followers. It is safe to
// call from any exit path and from a non-leader (where it does nothing), which
// is what lets the lifecycle call it in one place per outcome instead of
// reasoning about which path ran.
func (l *sharedExecutionLease) publish(result *JobResult) {
	if l == nil || !l.leader {
		return
	}
	l.once.Do(func() {
		l.slot.result = result
		close(l.slot.done)
	})
}

// await blocks until the leader publishes, and returns the result this follower
// adopts — or nil when there is nothing to adopt, in which case the caller
// executes normally.
//
// Waiting here cannot deadlock the pool. A follower exists only because a
// leader already took the slot, and a leader takes the slot on the worker that
// is running it, so at most workers-1 goroutines can ever be parked on a leader
// that is itself making progress. A follower holds no CPU grant while it waits:
// enterSharedExecution is consulted before the group's budget is acquired.
func (l *sharedExecutionLease) await(ctx context.Context) *JobResult {
	if l == nil || l.leader {
		return nil
	}
	select {
	case <-l.slot.done:
		return l.slot.result
	case <-ctx.Done():
		return nil
	}
}

// adoptSharedResult is the follower's own result, derived from the leader's.
//
// Status, error, data and the event stream are carried over verbatim: from each
// command's perspective the task produced exactly what it would have produced
// alone, so its dependents skip, its diagnostics land and its artifacts are
// reported identically. The timings are the one thing that changes, and they
// change in the direction of the truth — see the file header.
func adoptSharedResult(leader *JobResult) *JobResult {
	if leader == nil {
		return nil
	}
	adopted := *leader
	// The follower spawned nothing, so it has no startup latency of its own and
	// must not report the leader's as if it did.
	adopted.SpawnToFirstEvent = 0
	adopted.FirstEventObserved = false
	// CPU is the extensive quantity a run-scope roll-up sums. The physical total
	// lives on the shared Execution below; a second claim on it would be the
	// double count this scheme exists to prevent.
	adopted.CPUTime = 0
	// TaskWall is the scheduler-level wall around THIS task and closeTask sets
	// it; the leader's would describe a different span.
	adopted.TaskWall = 0
	// SourceMutated is a verdict about this task's own keyed source inputs,
	// recomputed by finalizeExecutedJob when the task rewrites sources.
	adopted.SourceMutated = false
	// Canonical is the typed record a batch wire produced, and it carries the
	// producing task's IDENTITY. Adopting it would attribute the leader's key,
	// project and job name to this row; the ordinary event-walk projection
	// rebuilds the follower's own record from the shared event stream.
	adopted.Canonical = nil
	// The PHYSICAL execution both logical rows came out of. The ledger keys on
	// its id, so the subprocess is recorded once and referenced twice.
	adopted.Execution = leader.Execution
	adopted.MarkReuse(ReuseCoalesced)
	return &adopted
}
