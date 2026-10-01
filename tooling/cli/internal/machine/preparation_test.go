package machine

import (
	"encoding/json"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// The dependency-preparation stage's projection.
//
// The block reaches the session having been measured before the scheduler
// existed, so unlike every other member of the document there is nothing in the
// task list or the execution ledger that could cross-check it. These tests pin
// the two things that make it trustworthy anyway: the projection is a pure unit
// conversion that invents nothing, and a stage that measured nothing publishes
// nothing.

func preparedRun() Run {
	run := batchedRun()
	run.Session.Duration = 12 * time.Second
	run.Session.Preparation = &jobs.PreparationSummary{
		Wall:        2700 * time.Millisecond,
		Parallelism: 3,
		Phases: []jobs.PreparationPhaseTotals{
			{Phase: jobs.PreparationResolution, Wall: 800 * time.Millisecond, Steps: 6},
			{Phase: jobs.PreparationVerification, Wall: 340 * time.Millisecond, CPU: 122 * time.Millisecond, Steps: 12},
			{Phase: jobs.PreparationGeneration, Wall: 3281 * time.Millisecond, CPU: 2526 * time.Millisecond, Steps: 3},
			{Phase: jobs.PreparationMutation, Wall: 1098 * time.Millisecond, Steps: 12},
		},
	}
	return run
}

// TestPreparationProjectsEveryPhaseVerbatim: the projection converts units and
// nothing else. In particular it preserves the producer's canonical ORDER and
// does not re-sort by cost — a reader comparing two sessions phase by phase is
// the whole use of the block.
func TestPreparationProjectsEveryPhaseVerbatim(t *testing.T) {
	preparation := preparedRun().Preparation()
	if preparation == nil {
		t.Fatal("a run that prepared runtimes projected no preparation block")
	}
	want := protocolcli.SessionPreparation{
		WallMs:      2700,
		Parallelism: 3,
		Phases: []protocolcli.PreparationPhaseRecord{
			{Phase: protocolcli.PreparationPhaseResolution, WallMs: 800, Steps: 6},
			{Phase: protocolcli.PreparationPhaseVerification, WallMs: 340, Steps: 12, CPUMs: 122},
			{Phase: protocolcli.PreparationPhaseGeneration, WallMs: 3281, Steps: 3, CPUMs: 2526},
			{Phase: protocolcli.PreparationPhaseMutation, WallMs: 1098, Steps: 12},
		},
	}
	got, err := json.Marshal(preparation)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(&want)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(expected) {
		t.Errorf("preparation projection drift:\n got: %s\nwant: %s", got, expected)
	}
}

// TestPreparationRidesTheSessionDocumentAndValidates checks the member is
// actually published — a projection nothing calls is a measurement nobody sees
// — and that the resulting document satisfies the contract, overlap rule
// included: 3.3 s of generation inside a 2.7 s stage is legitimate at
// parallelism 3 and would be a violation at 1.
func TestPreparationRidesTheSessionDocumentAndValidates(t *testing.T) {
	run := preparedRun()
	file := run.SessionFile([]string{"build"}, nil, nil)
	file.SessionID = "20260806-091500-a1b2c3"
	file.StartTime = "2026-08-06T09:15:00.000Z"
	if file.Preparation == nil {
		t.Fatal("session document dropped the preparation block")
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
		t.Fatalf("recorded session does not conform: %v", violations)
	}
}

// TestPreparationAbsentWhenNothingWasPrepared: a run that prepared no runtime —
// every extension already resolved, or a caller that recorded no attribution —
// publishes no block at all. An empty one would claim the stage was measured
// and found to cost nothing.
func TestPreparationAbsentWhenNothingWasPrepared(t *testing.T) {
	for name, run := range map[string]Run{
		"no report":  batchedRun(),
		"no session": {},
		"no phases": func() Run {
			run := batchedRun()
			run.Session.Preparation = &jobs.PreparationSummary{Wall: time.Second, Parallelism: 1}
			return run
		}(),
		"phase without a step": func() Run {
			run := batchedRun()
			run.Session.Preparation = &jobs.PreparationSummary{
				Wall:        time.Second,
				Parallelism: 1,
				Phases:      []jobs.PreparationPhaseTotals{{Phase: jobs.PreparationNetwork}},
			}
			return run
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if got := run.Preparation(); got != nil {
				t.Errorf("Preparation() = %+v, want nil", got)
			}
		})
	}
}
