package cli

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The dependency-preparation stage's ownership attribution.
//
// The execution ledger made physical SUBPROCESS cost countable, and the runner
// environment made it interpretable. Neither could say anything about the
// stage that runs BEFORE the scheduler
// exists: resolving content identities, building extension runtimes, and
// publishing them into the machine-global content-addressed store. That stage
// is on the critical path of every run, so these tests pin the three properties
// that make its decomposition trustworthy — it round-trips whole, it is
// strictly additive for a pre-W3 reader, and its arithmetic is checked exactly
// where the arithmetic is meaningful.

func sessionFileWithPreparation() SessionFile {
	return SessionFile{
		ProtocolVersion: ResultProtocolVersion,
		SessionID:       "20260806-091500-abc123",
		StartTime:       "2026-08-06T09:15:00.000Z",
		EndTime:         "2026-08-06T09:15:12.000Z",
		Commands:        []string{"build"},
		Run: RunSummary{
			Outcome:    RunOutcomeSuccess,
			ExitCode:   ExitSuccess,
			Counts:     RunCounts{Total: 1, Succeeded: 1},
			DurationMs: 12000,
		},
		Preparation: &SessionPreparation{
			WallMs:      4200,
			Parallelism: 4,
			Phases: []PreparationPhaseRecord{
				{Phase: PreparationPhaseResolution, WallMs: 640, Steps: 8},
				{Phase: PreparationPhaseVerification, WallMs: 310, Steps: 9, CPUMs: 120},
				{Phase: PreparationPhaseGeneration, WallMs: 14800, Steps: 4, CPUMs: 51200},
				{Phase: PreparationPhaseMutation, WallMs: 720, Steps: 13},
			},
		},
	}
}

// TestSessionPreparationRoundTrip pins the whole block through the wire.
func TestSessionPreparationRoundTrip(t *testing.T) {
	want := sessionFileWithPreparation()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SessionFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost a member:\n got: %+v\nwant: %+v", got, want)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionPreparationIsAdditive keeps a pre-W3 reader whole: a document with
// no preparation block is exactly the document an earlier producer wrote, and it still
// validates.
func TestSessionPreparationIsAdditive(t *testing.T) {
	file := sessionFileWithPreparation()
	file.Preparation = nil
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytesContain(data, "preparation") {
		t.Errorf("absent block still emits preparation: %s", data)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("ValidateDocument = %v, want none", violations)
	}
}

// TestSessionPreparationOverlapIsOnlyAllowedUnderParallelism is the arithmetic
// rule, from both sides.
//
// The generation phase in the fixture sums to 14.8 s inside a 4.2 s stage,
// which is not a contradiction: four independent extension runtimes compiled
// concurrently, and that gap IS the parallelization this slice bought. Declare
// the same numbers at parallelism 1 and the document is claiming 14.8 seconds of
// non-overlapping work inside 4.2 seconds of wall, which no measurement can
// produce — so exactly there, the validator rejects it.
func TestSessionPreparationOverlapIsOnlyAllowedUnderParallelism(t *testing.T) {
	overlapping := sessionFileWithPreparation()
	data, err := json.Marshal(overlapping)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("parallel phases must be allowed to overlap: %v", violations)
	}

	serial := sessionFileWithPreparation()
	serial.Preparation.Parallelism = 1
	data, err = json.Marshal(serial)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	violations := ValidateDocument(DocumentSessionFile, data)
	if !hasViolation(violations, ViolationCountMismatch, "preparation.wallMs") {
		t.Errorf("serial phases exceeding the stage wall = %v, want %s at preparation.wallMs",
			violations, ViolationCountMismatch)
	}

	// The same serial document with a stage wall that can hold its phases is
	// accepted, so the rule is about the arithmetic and not about parallelism 1.
	serial.Preparation.WallMs = 16470
	data, err = json.Marshal(serial)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if violations := ValidateDocument(DocumentSessionFile, data); len(violations) != 0 {
		t.Errorf("serial phases fitting the stage wall = %v, want none", violations)
	}
}

// TestSessionPreparationPhasesAreAHistogram: the block partitions the stage by
// OWNERSHIP, so one class cannot appear twice. A producer that emitted per-step
// rows would otherwise publish a plausible-looking list from which every ratio
// a reader computes is wrong, with nothing else in the document to reveal it.
func TestSessionPreparationPhasesAreAHistogram(t *testing.T) {
	file := sessionFileWithPreparation()
	file.Preparation.Phases = append(file.Preparation.Phases,
		PreparationPhaseRecord{Phase: PreparationPhaseGeneration, WallMs: 10, Steps: 1})
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	violations := ValidateDocument(DocumentSessionFile, data)
	if !hasViolation(violations, ViolationCountMismatch, "preparation.phases[4].phase") {
		t.Errorf("repeated phase = %v, want %s at preparation.phases[4].phase",
			violations, ViolationCountMismatch)
	}
}

// TestPreparationPhaseVocabularyIsClosed: an unclassified step is a cost nobody
// can act on, so a sixth phase name is a producer bug rather than a forward
// extension.
func TestPreparationPhaseVocabularyIsClosed(t *testing.T) {
	file := sessionFileWithPreparation()
	file.Preparation.Phases = []PreparationPhaseRecord{{Phase: "other", WallMs: 10, Steps: 1}}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	violations := ValidateDocument(DocumentSessionFile, data)
	if !hasViolation(violations, ViolationInvalidEnum, "preparation.phases[0].phase") {
		t.Errorf("unknown phase = %v, want %s", violations, ViolationInvalidEnum)
	}
}

// TestPreparationPhaseStepsAreAtLeastOne: a phase with no span is ABSENT, never
// a zero row. It is the same "measured, never derived" rule the environment
// block follows — a zero here would read as "this class was entered and cost
// nothing", which is a different statement from "never entered".
func TestPreparationPhaseStepsAreAtLeastOne(t *testing.T) {
	file := sessionFileWithPreparation()
	file.Preparation.Phases = []PreparationPhaseRecord{{Phase: PreparationPhaseNetwork, WallMs: 0, Steps: 0}}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	violations := ValidateDocument(DocumentSessionFile, data)
	if !hasViolation(violations, ViolationInvalidValue, "preparation.phases[0].steps") {
		t.Errorf("zero-step phase = %v, want %s", violations, ViolationInvalidValue)
	}
}

func hasViolation(violations []Violation, code, path string) bool {
	for _, violation := range violations {
		if violation.Code == code && violation.Path == path {
			return true
		}
	}
	return false
}
