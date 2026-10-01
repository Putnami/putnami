package jobs

import (
	"reflect"
	"testing"
	"time"
)

// The preparation report's own accounting.
//
// The stage runs TWICE in one engine run — phase 1c synchronizes the runtimes
// the selected commands demand, phase 1d resolves whatever a workspace probe
// turns out to need — and both are the same stage from a reader's point of
// view. These tests pin what that costs the accounting: the two invocations
// ADD, the widest fan-out wins, and a caller with nothing to publish into is a
// total no-op rather than a second code path.

func TestPreparationReportAccumulatesEveryInvocation(t *testing.T) {
	t.Parallel()
	report := &PreparationReport{}

	first := &preparationSpans{}
	first.add(PreparationResolution, 30*time.Millisecond, 0)
	first.add(PreparationGeneration, 2*time.Second, 5*time.Second)
	report.merge(first)
	report.observeStage(2100*time.Millisecond, 3)

	// Phase 1d: the probe demanded one more runtime, serially.
	second := &preparationSpans{}
	second.add(PreparationResolution, 12*time.Millisecond, 0)
	second.add(PreparationVerification, 8*time.Millisecond, 3*time.Millisecond)
	report.merge(second)
	report.observeStage(40*time.Millisecond, 1)

	summary := report.Snapshot()
	if summary == nil {
		t.Fatal("report with two merged invocations snapshotted nil")
	}
	// The invocations are sequential, so their walls add: the run really did
	// wait for both.
	if want := 2140 * time.Millisecond; summary.Wall != want {
		t.Errorf("stage wall = %s, want %s", summary.Wall, want)
	}
	// The WIDEST fan-out is reported, because it is the bound under which the
	// summed phase walls may have overlapped. Taking the last, or an average,
	// would let a reader treat overlapping sums as elapsed time.
	if summary.Parallelism != 3 {
		t.Errorf("parallelism = %d, want the widest granted (3)", summary.Parallelism)
	}
	want := []PreparationPhaseTotals{
		{Phase: PreparationResolution, Wall: 42 * time.Millisecond, Steps: 2},
		{Phase: PreparationVerification, Wall: 8 * time.Millisecond, CPU: 3 * time.Millisecond, Steps: 1},
		{Phase: PreparationGeneration, Wall: 2 * time.Second, CPU: 5 * time.Second, Steps: 1},
	}
	if !reflect.DeepEqual(summary.Phases, want) {
		t.Errorf("phases = %+v, want %+v in canonical order", summary.Phases, want)
	}
}

// TestPreparationReportOmitsPhasesNothingEntered: the canonical order is a
// filter, not a template. A class the stage never entered is absent, which is
// the difference between "we did not do this" and "we did this for free".
func TestPreparationReportOmitsPhasesNothingEntered(t *testing.T) {
	t.Parallel()
	report := &PreparationReport{}
	spans := &preparationSpans{}
	spans.add(PreparationMutation, time.Millisecond, 0)
	report.merge(spans)
	report.observeStage(2*time.Millisecond, 1)

	summary := report.Snapshot()
	if len(summary.Phases) != 1 || summary.Phases[0].Phase != PreparationMutation {
		t.Fatalf("phases = %+v, want mutation alone", summary.Phases)
	}
}

// TestPreparationReportIsANoOpWithoutARecorder covers the seam the non-session
// callers use. `putnami cache`, `putnami extensions` and `projects sync` all
// synchronize runtimes with nothing to publish into, and they must not need a
// throwaway report or a branch of their own.
func TestPreparationReportIsANoOpWithoutARecorder(t *testing.T) {
	t.Parallel()
	var report *PreparationReport
	var spans *preparationSpans

	// Every entry point tolerates the nil receiver, including the ones the
	// instrumented call sites reach unconditionally.
	spans.add(PreparationNetwork, time.Second, time.Second)
	spans.since(PreparationNetwork, time.Now())
	if got := spans.elapsed(); got != 0 {
		t.Errorf("nil spans accumulated %s", got)
	}
	report.merge(spans)
	report.merge(&preparationSpans{})
	report.observeStage(time.Second, 4)
	if got := report.Snapshot(); got != nil {
		t.Errorf("nil report snapshotted %+v, want nil", got)
	}

	// A live report that recorded nothing is equally silent: a run whose
	// extensions were all already resolved publishes no block.
	if got := (&PreparationReport{}).Snapshot(); got != nil {
		t.Errorf("empty report snapshotted %+v, want nil", got)
	}
}

// TestPreparationSpansClampBackwardsClocks: a span is never allowed to make a
// phase smaller. A wall-clock adjustment mid-stage would otherwise let the
// serial arithmetic the protocol enforces come out true by subtraction.
func TestPreparationSpansClampBackwardsClocks(t *testing.T) {
	t.Parallel()
	spans := &preparationSpans{}
	spans.add(PreparationMutation, 10*time.Millisecond, 4*time.Millisecond)
	spans.add(PreparationMutation, -50*time.Millisecond, -20*time.Millisecond)

	report := &PreparationReport{}
	report.merge(spans)
	report.observeStage(-time.Second, 0)

	summary := report.Snapshot()
	if summary.Wall != 0 {
		t.Errorf("stage wall = %s, want 0 rather than a negative measurement", summary.Wall)
	}
	if summary.Parallelism != 1 {
		t.Errorf("parallelism = %d, want at least 1", summary.Parallelism)
	}
	got := summary.Phases[0]
	if got.Wall != 10*time.Millisecond || got.CPU != 4*time.Millisecond || got.Steps != 2 {
		t.Errorf("phase = %+v, want the forward span alone with both steps counted", got)
	}
}

// TestSpawnCPUReportsNothingForAProcessThatNeverRan pins the "absent, not zero"
// rule at its source: a phase whose subprocess never started must not be able
// to claim it measured no CPU.
func TestSpawnCPUReportsNothingForAProcessThatNeverRan(t *testing.T) {
	t.Parallel()
	if got := spawnCPU(nil); got != 0 {
		t.Errorf("spawnCPU(nil) = %s, want 0", got)
	}
}
