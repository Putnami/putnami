package jobs

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The PHYSICAL execution, and what a LOGICAL task record may claim from it.
//
// The CLI's task list is logical and fans out: a
// batched dispatch runs n projects in ONE subprocess, so n task records
// describe work that was measured once. Before this, nothing recorded the
// physical side at all — the spawn site computed a CPU total and dropped it
// before persistence, peak RSS and the IO counters sitting in the same rusage
// were never read, and each batch member recorded the leader's FULL wall while
// its CPU was cut into equal shares of a number nobody could reconcile against
// a measurement. Summing task records therefore over-stated cost by the batch
// factor (the epic's baseline: 796 logical task records against 57.1
// de-duplicated physical task-minutes), and no later slice could have proven a
// saving against numbers with that property.
//
// What this file establishes:
//
//   - ONE physical execution is ONE Execution value with a stable id. Every
//     logical result produced by that subprocess points at the SAME value, so a
//     consumer aggregates physical cost by walking executions (each once) while
//     still attributing per project by walking tasks.
//   - A batch member's wall and CPU become an explicit SHARE of that shared
//     attempt rather than a claim of the whole or an unreconcilable fiction:
//     the shares sum back to the attempt's own measured total EXACTLY,
//     remainder included. The truth stays on the execution; the share exists so
//     per-project attribution, the learned CPU weights and the cached entry's
//     recorded duration keep a plausible per-task number instead of n copies of
//     the batch's.
//
//     What is divided is the TASK-level wall (JobResult.Duration), not
//     Execution.Wall. They are different quantities — the task wall starts
//     before the context file is written, the execution wall at fork/exec — and
//     the task-level one is chosen because it is what a SOLO task's duration
//     also reports, which keeps a batched member's number comparable with the
//     same task's solo number. A consumer must therefore not reconcile the
//     members' published durations against executions[].wallMs; the millisecond
//     truncation each record undergoes separately would lose up to n-1 ms
//     anyway. protocols/cli/doc/02-result-v2.md states this for readers.
//
// Reuse produces no Execution at all, which is the point: a cache hit spends
// no machine time in this run, and its absence from the ledger is what makes
// "count physical work exactly once" true rather than aspirational.

// executionSeq numbers this process's executions. Ids need only be stable and
// unique WITHIN one recorded session — they are a join key inside one document,
// not a global identifier — so a counter is enough and stays readable in a
// session file a human is reading.
var executionSeq atomic.Uint64

// nextExecutionID mints the next physical execution id.
func nextExecutionID() string {
	return "exec-" + strconv.FormatUint(executionSeq.Add(1), 10)
}

// executionLedger accumulates every physical execution a run spawns, in the
// order they finished.
//
// It is APPENDED TO as executions happen rather than derived from the surviving
// task results, and that distinction is the whole point. A JobResult holds only
// its LAST attempt, so a task-derived ledger silently loses:
//
//   - attempt 1 of any retried task — its wall, CPU and peak RSS were spent and
//     would vanish from the run's cost;
//   - the ENTIRE batch leader when a split could not be attributed and every
//     member then retried solo (scheduler_exec.go): no surviving result points
//     at it, so a derived ledger would not emit a multi-second subprocess at all.
//
// Both are exactly the case protocols/cli permits and documents — an execution
// nothing references is real work — so the producer must be able to emit it.
// Under-counting is the one error a cost baseline cannot absorb,
// because every later saving is measured against it.
//
// Recording is keyed on the execution id, so a batch whose n members all carry
// the same shared execution contributes it ONCE.
type executionLedger struct {
	mu    sync.Mutex
	seen  map[string]bool
	order []Execution
}

// record files the physical execution behind one result, if it had one. It is
// safe to call for every result of every attempt: nil executions and repeats
// are no-ops, which is what lets the caller record unconditionally instead of
// reasoning about which attempt was the last.
func (l *executionLedger) record(result *JobResult) {
	if l == nil || result == nil || result.Execution == nil || result.Execution.ID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[result.Execution.ID] {
		return
	}
	if l.seen == nil {
		l.seen = make(map[string]bool)
	}
	l.seen[result.Execution.ID] = true
	l.order = append(l.order, *result.Execution)
}

// snapshot copies the ledger in completion order.
func (l *executionLedger) snapshot() []Execution {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.order) == 0 {
		return nil
	}
	return append([]Execution(nil), l.order...)
}

// captureExecution reads everything one finished subprocess can still tell us,
// at the only moment it can be asked: after wait, while ProcessState is alive.
//
// It never fails. A platform that exposes no rusage yields an execution with
// the times it does have and zeroes elsewhere, because a run must not break
// over a counter it cannot read.
func captureExecution(ps *os.ProcessState, wall time.Duration, concurrency int) *Execution {
	if ps == nil {
		return nil
	}
	maxRSSBytes, inBlocks, outBlocks := processRusage(ps)
	return &Execution{
		ID:          nextExecutionID(),
		Wall:        max(wall, 0),
		UserCPU:     max(ps.UserTime(), 0),
		SystemCPU:   max(ps.SystemTime(), 0),
		MaxRSSBytes: maxRSSBytes,
		IOInBlocks:  inBlocks,
		IOOutBlocks: outBlocks,
		Concurrency: max(concurrency, 0),
		BatchSize:   1,
	}
}

// executionShare divides a measured total across the n logical records one
// physical execution produced, giving element index its part.
//
// The remainder is distributed over the first records rather than dropped, so
// the shares sum to the total EXACTLY, in the nanoseconds this operates on.
// That is the property the whole arrangement rests on: a batch's logical rows
// add up to the one measurement they divide, instead of each row claiming all
// of it. (The published records truncate to milliseconds independently, so the
// exactness is the model's, not the wire's — see attributeSharedExecution.)
func executionShare(total time.Duration, n, index int) time.Duration {
	if n <= 1 || total <= 0 {
		return total
	}
	share := total / time.Duration(n)
	if index < int(total%time.Duration(n)) {
		share++
	}
	return share
}

// attributeSharedExecution points every member of a batch at the ONE physical
// execution they shared, and replaces the wall and CPU each of them would
// otherwise claim with its share of that execution.
//
// Members are walked in work order, not map order, so the remainder lands
// deterministically. It runs after BOTH the split and the fail-closed path: a
// group whose aggregate could not be attributed still ran as one process, and
// reporting n copies of its wall would over-state a failure exactly as it
// over-stated a success.
//
// Wall and CPU are shared because they are EXTENSIVE — the members' shares add
// up to the measurement. SpawnToFirstEvent is deliberately left whole: it is a
// LATENCY of the shared process, and a fifth of a startup latency is not a
// quantity anything can be said about.
func attributeSharedExecution(work []taskWork, results map[string]*JobResult, aggregate *JobResult) {
	if aggregate == nil || len(work) < 2 {
		return
	}
	if aggregate.Execution != nil {
		aggregate.Execution.BatchSize = len(work)
	}
	for i := range work {
		result := results[work[i].job.Key()]
		if result == nil {
			continue
		}
		result.Execution = aggregate.Execution
		result.Duration = executionShare(aggregate.Duration, len(work), i)
		result.CPUTime = executionShare(aggregate.CPUTime, len(work), i)
	}
}
