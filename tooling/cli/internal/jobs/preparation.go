package jobs

import (
	"os"
	"sync"
	"time"
)

// Where the dependency-preparation stage's time goes, and why it is NOT an
// entry in the physical execution ledger.
//
// A baseline measurement attributed 3% of physical time to
// "preparation/metadata" and could say nothing further about it: the stage runs
// before the scheduler exists, so none of it appears in executions[] — the
// ledger execution.go accumulates. Two facts make reusing that ledger the wrong
// answer rather than merely an inconvenient one:
//
//   - An Execution is ONE SUBPROCESS and is joined to by TaskRecord.executionId.
//     Most of this stage is IN-PROCESS work (hashing input trees, walking
//     replacement closures, copying files into an isolated source view) that no
//     subprocess ever performed and no task record could reference. Filing it as
//     an execution would put rows in the run's spawn ledger for spawns that did
//     not happen, and run.cpu.actualMs — which sums that ledger — would start
//     reporting CLI-process CPU as child-process CPU.
//   - The ledger is scheduler-owned and only exists once execution starts. This
//     stage runs in engine phase 1c/1d, BEFORE project selection and planning,
//     precisely so a runtime compile is not paid under a task-scoped timeout.
//
// So preparation gets its OWN additive block, reported beside executions[]
// rather than inside it, and the two never double-count: nothing in
// preparation is an execution, and nothing in executions is preparation.
// (The one place they touch is honest and stated: where a preparation phase DID
// spawn a subprocess, its measured child CPU is recorded on the phase, so the
// stage's machine cost is not silently zero.)

// preparationPhaseOrder is the canonical emission order: the classes as the
// epic names them, so two sessions list them the same way regardless of which
// phase a given run happened to enter first.
var preparationPhaseOrder = []PreparationPhase{
	PreparationNetwork,
	PreparationResolution,
	PreparationVerification,
	PreparationGeneration,
	PreparationMutation,
}

// preparationSpans accumulates one independent step's spans WITHOUT
// synchronization. Each parallel worker owns exactly one, so the hot path takes
// no lock; the finished accumulator is merged into the shared report once, by
// the goroutine that joins the workers. A nil *preparationSpans records nothing,
// which is what lets every instrumented call site stay unconditional and lets
// the non-session callers (cache, extensions, projects sync) opt out by passing
// nil rather than by growing a second code path.
type preparationSpans struct {
	totals map[PreparationPhase]PreparationPhaseTotals
	total  time.Duration
}

// add files one span. A negative wall (a clock that went backwards) is clamped
// to zero rather than subtracted from the phase.
func (s *preparationSpans) add(phase PreparationPhase, wall, cpu time.Duration) {
	if s == nil {
		return
	}
	if wall < 0 {
		wall = 0
	}
	if cpu < 0 {
		cpu = 0
	}
	if s.totals == nil {
		s.totals = make(map[PreparationPhase]PreparationPhaseTotals, len(preparationPhaseOrder))
	}
	entry := s.totals[phase]
	entry.Phase = phase
	entry.Wall += wall
	entry.CPU += cpu
	entry.Steps++
	s.totals[phase] = entry
	s.total += wall
}

// since files a span that started at start and ended now.
func (s *preparationSpans) since(phase PreparationPhase, start time.Time) {
	if s == nil {
		return
	}
	s.add(phase, time.Since(start), 0)
}

// elapsed is the wall summed over every span filed so far. Its only use is the
// residual in prepareOrLoadExtensionRuntimeWithDigest: the spans filed inside a
// store admit are disjoint sub-intervals of the admit itself, so
// admitWall - (elapsed after - elapsed before) is the admit's own overhead —
// the ownership-lock wait and the atomic publish — and is non-negative by
// construction.
func (s *preparationSpans) elapsed() time.Duration {
	if s == nil {
		return 0
	}
	return s.total
}

// spawnCPU is the child CPU one finished subprocess consumed, read from the
// same ProcessState the execution ledger reads. It is zero for a process that
// never started, which is a measurement that does not exist rather than a
// measured zero.
func spawnCPU(ps *os.ProcessState) time.Duration {
	if ps == nil {
		return 0
	}
	cpu := ps.UserTime() + ps.SystemTime()
	if cpu < 0 {
		return 0
	}
	return cpu
}

// PreparationReport accumulates the preparation stage across every
// synchronization call a run makes — engine phase 1c's command-scoped pass and
// phase 1d's per-provider probe resolutions are the same stage, split by when
// the runtime is first demanded, and a reader wants the run's total.
//
// The zero value is ready. A nil *PreparationReport records nothing, so a
// caller with no session to publish into passes nil instead of a throwaway.
type PreparationReport struct {
	mu          sync.Mutex
	wall        time.Duration
	parallelism int
	totals      map[PreparationPhase]PreparationPhaseTotals
}

// merge folds one worker's finished accumulator into the report.
func (r *PreparationReport) merge(spans *preparationSpans) {
	if r == nil || spans == nil || len(spans.totals) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.totals == nil {
		r.totals = make(map[PreparationPhase]PreparationPhaseTotals, len(preparationPhaseOrder))
	}
	for phase, add := range spans.totals {
		entry := r.totals[phase]
		entry.Phase = phase
		entry.Wall += add.Wall
		entry.CPU += add.CPU
		entry.Steps += add.Steps
		r.totals[phase] = entry
	}
}

// observeStage records one top-level synchronization call: its own wall, and
// the fan-out it was permitted. Calls are sequential within a run, so the walls
// add; the parallelism is the WIDEST any call was granted, because that is the
// bound under which the reported phase walls may have overlapped.
func (r *PreparationReport) observeStage(wall time.Duration, parallelism int) {
	if r == nil {
		return
	}
	if wall < 0 {
		wall = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wall += wall
	if parallelism > r.parallelism {
		r.parallelism = parallelism
	}
}

// Snapshot copies the report in canonical phase order. It returns nil when the
// stage did nothing, so a run that prepared no runtime publishes no preparation
// block at all rather than an empty one.
func (r *PreparationReport) Snapshot() *PreparationSummary {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.totals) == 0 {
		return nil
	}
	summary := &PreparationSummary{Wall: r.wall, Parallelism: max(r.parallelism, 1)}
	for _, phase := range preparationPhaseOrder {
		entry, entered := r.totals[phase]
		if !entered {
			continue
		}
		summary.Phases = append(summary.Phases, entry)
	}
	return summary
}
