package darc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	archproto "go.putnami.dev/protocol/architecture"
)

// The cross-language DARC conformance corpus, Go side.
//
// protocols/architecture/fixtures/conformance/*-behavior.json states what a
// contract DOES — ordering, idempotency, late events, freshness, the single
// writer, rebuild, deletion, immutability, fact minimization — and both this
// runner and the TypeScript mirror in `@putnami/application` execute the same
// cases. A behavior that differs between the two languages fails on one side
// rather than shipping as two runtimes that describe the same manifest
// differently.
//
// Refusals are pinned by a stable token, never by a message: each language
// phrases its errors idiomatically, and what has to agree is WHICH clause of the
// contract refused. `refused` is the token for an operation the corpus requires
// to fail without pinning the clause.

// conformanceDir is the corpus location, relative to this package.
const conformanceDir = "../fixtures/conformance"

type behaviorCorpus struct {
	ProtocolVersion    int                               `json:"protocolVersion"`
	Bases              map[string]json.RawMessage        `json:"bases"`
	ConsistencyPresets map[string]*archproto.Consistency `json:"consistencyPresets"`
	Cases              []behaviorCase                    `json:"cases"`
}

type behaviorCase struct {
	Name        string                      `json:"name"`
	Base        string                      `json:"base"`
	Set         map[string]json.RawMessage  `json:"set"`
	Remove      []string                    `json:"remove"`
	Consistency string                      `json:"consistency"`
	Now         string                      `json:"now"`
	Sources     map[string][]behaviorUpdate `json:"sources"`
	Carrier     *bool                       `json:"carrier"`
	ExpectError string                      `json:"expectError"`
	Steps       []behaviorStep              `json:"steps"`
}

type behaviorUpdate struct {
	ID             string `json:"id"`
	Value          string `json:"value"`
	SourceVersion  string `json:"sourceVersion"`
	IdempotencyKey string `json:"idempotencyKey"`
	ObservedAt     string `json:"observedAt"`
}

type behaviorStep struct {
	Op             string         `json:"op"`
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Value          string         `json:"value"`
	Version        string         `json:"version"`
	Payload        string         `json:"payload"`
	SourceVersion  string         `json:"sourceVersion"`
	IdempotencyKey string         `json:"idempotencyKey"`
	ObservedAt     string         `json:"observedAt"`
	Seconds        float64        `json:"seconds"`
	Fail           bool           `json:"fail"`
	Expect         behaviorExpect `json:"expect"`
}

type behaviorExpect struct {
	Changed    *bool    `json:"changed"`
	Found      *bool    `json:"found"`
	Value      *string  `json:"value"`
	Freshness  string   `json:"freshness"`
	Provenance string   `json:"provenance"`
	Error      string   `json:"error"`
	Since      *string  `json:"since"`
	IDs        []string `json:"ids"`
	Versions   []string `json:"versions"`
	Facts      []string `json:"facts"`
	Attempted  *int     `json:"attempted"`
	Failed     *int     `json:"failed"`
	Observed   *int     `json:"observed"`
}

// TestProjectionBehaviorConformance runs the projection corpus.
func TestProjectionBehaviorConformance(t *testing.T) {
	corpus := loadBehaviorCorpus(t, "projection-behavior.json")
	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) { runProjectionCase(t, corpus, testCase) })
	}
}

// TestSnapshotBehaviorConformance runs the snapshot corpus.
func TestSnapshotBehaviorConformance(t *testing.T) {
	corpus := loadBehaviorCorpus(t, "snapshot-behavior.json")
	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) { runSnapshotCase(t, corpus, testCase) })
	}
}

// TestCommandAndReferenceBehaviorConformance runs the two modes that keep no
// local copy.
func TestCommandAndReferenceBehaviorConformance(t *testing.T) {
	corpus := loadBehaviorCorpus(t, "command-reference-behavior.json")
	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			if testCase.Base == "command" {
				runCommandCase(t, corpus, testCase)
				return
			}
			runReferenceCase(t, corpus, testCase)
		})
	}
}

func runProjectionCase(t *testing.T, corpus behaviorCorpus, testCase behaviorCase) {
	t.Helper()
	clock := &testClock{now: behaviorTime(t, testCase.Now)}
	contract := buildBehaviorContract(t, corpus, testCase)
	options := []ProjectionOption[string]{WithClock[string](clock.Now)}
	replaySince := ""
	if updates, declared := testCase.Sources["bootstrap"]; declared {
		options = append(options, WithBootstrap(func(context.Context) ([]Update[string], error) {
			return behaviorUpdates(t, updates), nil
		}))
	}
	if updates, declared := testCase.Sources["replay"]; declared {
		options = append(options, WithReplay(func(_ context.Context, since string) ([]Update[string], error) {
			replaySince = since
			return behaviorUpdates(t, updates), nil
		}))
	}
	projection, err := NewProjection[string](contract, options...)
	if !acceptConstruction(t, testCase, err) {
		return
	}

	// The writer handle is claimed lazily so the cases that prove the
	// single-writer refusal are not defeated by the harness taking it first.
	var writer *Writer[string]
	claim := func() *Writer[string] {
		t.Helper()
		if writer == nil {
			claimed, claimErr := projection.Writer(contract.LocalModel.Writer)
			if claimErr != nil {
				t.Fatalf("claim the declared writer: %v", claimErr)
			}
			writer = claimed
		}
		return writer
	}

	ctx := context.Background()
	for index, step := range testCase.Steps {
		field := fmt.Sprintf("steps[%d] %s", index, step.Op)
		switch step.Op {
		case "apply":
			changed, applyErr := claim().Apply(ctx, Update[string]{
				ID: step.ID, Value: step.Value, SourceVersion: step.SourceVersion,
				IdempotencyKey: step.IdempotencyKey, ObservedAt: behaviorOptionalTime(t, step.ObservedAt),
			})
			checkError(t, field, step.Expect.Error, applyErr)
			checkBool(t, field+" changed", step.Expect.Changed, changed)
		case "delete":
			checkError(t, field, step.Expect.Error, claim().Delete(ctx, step.ID, step.SourceVersion))
		case "get":
			record, found, getErr := projection.Get(ctx, step.ID)
			checkError(t, field, step.Expect.Error, getErr)
			checkRecord(t, field, step.Expect, found, record)
		case "all":
			ids := make([]string, 0, len(projection.All()))
			for _, record := range projection.All() {
				ids = append(ids, record.ID)
			}
			checkStrings(t, field+" ids", step.Expect.IDs, ids)
		case "rebuild":
			checkError(t, field, step.Expect.Error, projection.Rebuild(ctx))
			if step.Expect.Since != nil && replaySince != *step.Expect.Since {
				t.Errorf("%s: replay resumed from %q, want %q", field, replaySince, *step.Expect.Since)
			}
		case "writer":
			_, writerErr := projection.Writer(step.Name)
			checkError(t, field, step.Expect.Error, writerErr)
		case "advance":
			clock.advance(time.Duration(step.Seconds) * time.Second)
		default:
			t.Fatalf("%s: the corpus names an operation this runner does not implement", field)
		}
	}
}

func runSnapshotCase(t *testing.T, corpus behaviorCorpus, testCase behaviorCase) {
	t.Helper()
	clock := &testClock{now: behaviorTime(t, testCase.Now)}
	snapshot, err := NewSnapshot[string](buildBehaviorContract(t, corpus, testCase),
		WithSnapshotClock[string](clock.Now))
	if !acceptConstruction(t, testCase, err) {
		return
	}
	ctx := context.Background()
	for index, step := range testCase.Steps {
		field := fmt.Sprintf("steps[%d] %s", index, step.Op)
		switch step.Op {
		case "attach":
			checkError(t, field, step.Expect.Error,
				snapshot.Attach(step.Version, step.Value, behaviorOptionalTime(t, step.ObservedAt)))
		case "at":
			record, found := snapshot.At(step.Version)
			checkRecord(t, field, step.Expect, found, record)
		case "latest":
			record, found, latestErr := snapshot.Latest(ctx)
			checkError(t, field, step.Expect.Error, latestErr)
			checkRecord(t, field, step.Expect, found, record)
		case "versions":
			checkStrings(t, field, step.Expect.Versions, snapshot.Versions())
		case "advance":
			clock.advance(time.Duration(step.Seconds) * time.Second)
		default:
			t.Fatalf("%s: the corpus names an operation this runner does not implement", field)
		}
	}
}

func runCommandCase(t *testing.T, corpus behaviorCorpus, testCase behaviorCase) {
	t.Helper()
	contract := buildBehaviorContract(t, corpus, testCase)
	failNext := false
	observed := 0
	var carrier SendFunc[string]
	if testCase.Carrier == nil || *testCase.Carrier {
		carrier = func(context.Context, string) error {
			if failNext {
				return errors.New("the carrier refused the payload")
			}
			return nil
		}
	}
	command, err := NewCommand[string](contract, carrier,
		WithFailureObserver[string](func(error) { observed++ }))
	if !acceptConstruction(t, testCase, err) {
		return
	}
	ctx := context.Background()
	for index, step := range testCase.Steps {
		field := fmt.Sprintf("steps[%d] %s", index, step.Op)
		failNext = step.Fail
		switch step.Op {
		case "send":
			checkError(t, field, step.Expect.Error, command.Send(ctx, step.Payload))
		case "emit":
			observed = 0
			command.Emit(ctx, step.Payload)
			checkInt(t, field+" observed", step.Expect.Observed, observed)
		case "stats":
			attempted, failed := command.Stats()
			checkInt(t, field+" attempted", step.Expect.Attempted, attempted)
			checkInt(t, field+" failed", step.Expect.Failed, failed)
		default:
			t.Fatalf("%s: the corpus names an operation this runner does not implement", field)
		}
	}
}

func runReferenceCase(t *testing.T, corpus behaviorCorpus, testCase behaviorCase) {
	t.Helper()
	reference, err := NewReference(buildBehaviorContract(t, corpus, testCase))
	if !acceptConstruction(t, testCase, err) {
		return
	}
	for index, step := range testCase.Steps {
		field := fmt.Sprintf("steps[%d] %s", index, step.Op)
		switch step.Op {
		case "fact":
			provenance, factErr := reference.Fact(step.Name)
			checkError(t, field, step.Expect.Error, factErr)
			if step.Expect.Provenance != "" && provenance != step.Expect.Provenance {
				t.Errorf("%s: provenance = %q, want %q", field, provenance, step.Expect.Provenance)
			}
		case "facts":
			checkStrings(t, field, step.Expect.Facts, reference.Facts())
		default:
			t.Fatalf("%s: the corpus names an operation this runner does not implement", field)
		}
	}
}

// acceptConstruction applies the case's construction verdict and reports
// whether the steps should run.
func acceptConstruction(t *testing.T, testCase behaviorCase, err error) bool {
	t.Helper()
	if testCase.ExpectError != "" {
		if token := behaviorErrorToken(err); token != testCase.ExpectError {
			t.Fatalf("construction error = %v (token %q), want %q", err, token, testCase.ExpectError)
		}
		return false
	}
	if err != nil {
		t.Fatalf("construction failed: %v", err)
	}
	return true
}

// behaviorErrorToken reduces an error to the stable token the corpus pins.
func behaviorErrorToken(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrMissing):
		return "missing"
	case errors.Is(err, ErrStale):
		return "stale"
	case errors.Is(err, ErrLateUpdate):
		return "late-update"
	case errors.Is(err, ErrWriterClaimed):
		return "writer-claimed"
	case errors.Is(err, ErrNotTheWriter):
		return "not-the-writer"
	case errors.Is(err, ErrDeletionNotApplicable):
		return "deletion-not-applicable"
	case errors.Is(err, ErrImmutable):
		return "immutable"
	case errors.Is(err, ErrNotActive):
		return "not-active"
	case errors.Is(err, ErrFactNotImported):
		return "fact-not-imported"
	}
	contractError := &ContractError{}
	if errors.As(err, &contractError) {
		return "contract"
	}
	return "refused"
}

func checkError(t *testing.T, field, want string, err error) {
	t.Helper()
	if got := behaviorErrorToken(err); got != want {
		t.Errorf("%s: error = %v (token %q), want %q", field, err, got, want)
	}
}

func checkRecord(t *testing.T, field string, expect behaviorExpect, found bool, record Record[string]) {
	t.Helper()
	checkBool(t, field+" found", expect.Found, found)
	if expect.Value != nil && record.Value != *expect.Value {
		t.Errorf("%s: value = %q, want %q", field, record.Value, *expect.Value)
	}
	if expect.Freshness != "" && string(record.Freshness) != expect.Freshness {
		t.Errorf("%s: freshness = %q, want %q", field, record.Freshness, expect.Freshness)
	}
	if expect.Provenance != "" && record.Provenance != expect.Provenance {
		t.Errorf("%s: provenance = %q, want %q", field, record.Provenance, expect.Provenance)
	}
}

func checkBool(t *testing.T, field string, want *bool, got bool) {
	t.Helper()
	if want != nil && got != *want {
		t.Errorf("%s = %v, want %v", field, got, *want)
	}
}

func checkInt(t *testing.T, field string, want *int, got int) {
	t.Helper()
	if want != nil && got != *want {
		t.Errorf("%s = %d, want %d", field, got, *want)
	}
}

func checkStrings(t *testing.T, field string, want, got []string) {
	t.Helper()
	if want == nil {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", field, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("%s = %v, want %v", field, got, want)
			return
		}
	}
}

// buildBehaviorContract applies the case's shallow merge and consistency preset
// to its named base.
func buildBehaviorContract(t *testing.T, corpus behaviorCorpus, testCase behaviorCase) archproto.Import {
	t.Helper()
	raw, found := corpus.Bases[testCase.Base]
	if !found {
		t.Fatalf("case %q names base %q, which the corpus does not define", testCase.Name, testCase.Base)
	}
	document := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("base %q does not parse: %v", testCase.Base, err)
	}
	for member, value := range testCase.Set {
		document[member] = value
	}
	for _, member := range testCase.Remove {
		delete(document, member)
	}
	merged, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("case %q does not re-encode: %v", testCase.Name, err)
	}
	var contract archproto.Import
	if err := json.Unmarshal(merged, &contract); err != nil {
		t.Fatalf("case %q does not decode as an import: %v", testCase.Name, err)
	}
	if testCase.Consistency != "" {
		preset, defined := corpus.ConsistencyPresets[testCase.Consistency]
		if !defined {
			t.Fatalf("case %q names consistency preset %q, which the corpus does not define", testCase.Name, testCase.Consistency)
		}
		contract.Consistency = preset
	}
	return contract
}

func behaviorUpdates(t *testing.T, declared []behaviorUpdate) []Update[string] {
	t.Helper()
	updates := make([]Update[string], 0, len(declared))
	for _, update := range declared {
		updates = append(updates, Update[string]{
			ID: update.ID, Value: update.Value, SourceVersion: update.SourceVersion,
			IdempotencyKey: update.IdempotencyKey, ObservedAt: behaviorOptionalTime(t, update.ObservedAt),
		})
	}
	return updates
}

func behaviorTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("the corpus seeds an unparseable clock %q: %v", value, err)
	}
	return parsed
}

func behaviorOptionalTime(t *testing.T, value string) time.Time {
	t.Helper()
	if value == "" {
		return time.Time{}
	}
	return behaviorTime(t, value)
}

func loadBehaviorCorpus(t *testing.T, name string) behaviorCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(conformanceDir, name))
	if err != nil {
		t.Fatalf("read the conformance corpus: %v", err)
	}
	var corpus behaviorCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("the conformance corpus does not parse: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatalf("%s is empty; a corpus that asserts nothing is worse than none", name)
	}
	return corpus
}
