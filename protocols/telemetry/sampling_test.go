package telemetry

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

// samplingFixture is fixtures/conventions/trace-sampling.json, which the
// TypeScript runtime reads too.
type samplingFixture struct {
	ConfigPath string `json:"configPath"`
	Keys       struct {
		Budget         string `json:"budget"`
		Ratio          string `json:"ratio"`
		FloorPerMinute string `json:"floorPerMinute"`
	} `json:"keys"`
	Defaults TraceSampling `json:"defaults"`
	Counter  struct {
		Name      string `json:"name"`
		Attribute string `json:"attribute"`
		Kept      string `json:"kept"`
		Dropped   string `json:"dropped"`
	} `json:"counter"`
	RatioCases []struct {
		TraceID string  `json:"traceId"`
		Ratio   float64 `json:"ratio"`
		Picked  bool    `json:"picked"`
	} `json:"ratioCases"`
	Scenarios []SamplingScenario `json:"scenarios"`
}

// SamplingScenario is one sequence of root decisions under fixed settings.
type SamplingScenario struct {
	Name     string        `json:"name"`
	Settings TraceSampling `json:"settings"`
	Roots    []struct {
		AtSecond int64  `json:"atSecond"`
		TraceID  string `json:"traceId"`
		Kept     bool   `json:"kept"`
	} `json:"roots"`
}

func readSamplingFixture(t *testing.T) samplingFixture {
	t.Helper()
	data, err := os.ReadFile("fixtures/conventions/trace-sampling.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture samplingFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return fixture
}

func traceIDOf(t *testing.T, value string) [16]byte {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 16 {
		t.Fatalf("trace ID %q is not 16 hex bytes", value)
	}
	var id [16]byte
	copy(id[:], raw)
	return id
}

// TestSampling_TheConventionMatchesItsFixture keeps the keys, the defaults,
// the counter and the ratio test equal to the fixture the TypeScript runtime
// reads.
func TestSampling_TheConventionMatchesItsFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "trace-sampling-is-shared", "the-sampling-convention-matches-its-fixture")
	fixture := readSamplingFixture(t)
	if fixture.ConfigPath != ConfigTraceSamplingPath {
		t.Errorf("configPath = %q, want %q", fixture.ConfigPath, ConfigTraceSamplingPath)
	}
	if fixture.Keys.Budget != KeyTraceBudget || fixture.Keys.Ratio != KeyTraceRatio || fixture.Keys.FloorPerMinute != KeyTraceFloorPerMinute {
		t.Errorf("keys = %+v", fixture.Keys)
	}
	if len(fixture.RatioCases) == 0 || len(fixture.Scenarios) == 0 {
		t.Fatal("the fixture holds no ratio case or no scenario")
	}
	for _, c := range fixture.RatioCases {
		if got := TraceRatioPicks(traceIDOf(t, c.TraceID), c.Ratio); got != c.Picked {
			t.Errorf("TraceRatioPicks(%s, %v) = %v, want %v", c.TraceID, c.Ratio, got, c.Picked)
		}
	}
}

func TestSampling_TheDefaultsAndTheCounterMatchTheFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "trace-sampling-defaults", "the-defaults-and-the-counter-match-the-fixture")
	fixture := readSamplingFixture(t)
	if fixture.Defaults != DefaultTraceSampling() {
		t.Errorf("defaults = %+v, want %+v", fixture.Defaults, DefaultTraceSampling())
	}
	if fixture.Counter.Name != MetricTraceDecisions || fixture.Counter.Attribute != AttrTraceDecisionOutcome ||
		fixture.Counter.Kept != TraceDecisionKept || fixture.Counter.Dropped != TraceDecisionDropped {
		t.Errorf("counter = %+v", fixture.Counter)
	}
	if err := DefaultTraceSampling().Validate(); err != nil {
		t.Errorf("the defaults are out of range: %v", err)
	}
}

// TestSampling_TheBudgetDecidesTheFixtureScenarios runs every scenario
// through TraceBudget, the rule the Go runtime samples with.
func TestSampling_TheBudgetDecidesTheFixtureScenarios(t *testing.T) {
	for _, scenario := range readSamplingFixture(t).Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			budget, err := NewTraceBudget(scenario.Settings)
			if err != nil {
				t.Fatal(err)
			}
			for i, root := range scenario.Roots {
				got := budget.Decide(traceIDOf(t, root.TraceID), time.Unix(root.AtSecond, 0))
				if got != root.Kept {
					t.Errorf("root %d at %ds: kept = %v, want %v", i, root.AtSecond, got, root.Kept)
				}
			}
		})
	}
}

func TestSampling_ValidateRefusesSettingsOutOfRange(t *testing.T) {
	for _, settings := range []TraceSampling{
		{Budget: -1},
		{FloorPerMinute: -1},
		{Ratio: -0.1},
		{Ratio: 1.5},
		{Ratio: math.NaN()},
	} {
		if settings.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want an error", settings)
		}
		if _, err := NewTraceBudget(settings); err == nil {
			t.Errorf("NewTraceBudget(%+v) accepted settings out of range", settings)
		}
	}
	if err := (TraceSampling{}).Validate(); err != nil {
		t.Errorf("zero settings are valid: %v", err)
	}
}

func TestSampling_UpdateKeepsTheKeptCountAndRefusesInvalidSettings(t *testing.T) {
	var none [16]byte
	for i := range none {
		none[i] = 0xff
	}
	budget, err := NewTraceBudget(TraceSampling{Budget: 1})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(0, 0)
	if !budget.Decide(none, at) || budget.Decide(none, at) {
		t.Fatal("a budget of 1 keeps exactly one root")
	}
	if err := budget.Update(TraceSampling{Budget: 2}); err != nil {
		t.Fatal(err)
	}
	if !budget.Decide(none, at) || budget.Decide(none, at) {
		t.Fatal("raising the budget to 2 keeps one more root, not two")
	}
	if err := budget.Update(TraceSampling{Ratio: 2}); err == nil {
		t.Fatal("Update accepted a ratio of 2")
	}
	if got := budget.Settings(); got != (TraceSampling{Budget: 2}) {
		t.Fatalf("an invalid update changed the settings to %+v", got)
	}
}

func TestSampling_TheMinuteIsFlooredBeforeTheEpoch(t *testing.T) {
	var none [16]byte
	for i := range none {
		none[i] = 0xff
	}
	budget, err := NewTraceBudget(TraceSampling{FloorPerMinute: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !budget.Decide(none, time.Unix(-30, 0)) || budget.Decide(none, time.Unix(-1, 0)) || !budget.Decide(none, time.Unix(0, 0)) {
		t.Fatal("seconds -30 and -1 share a minute, and second 0 starts the next")
	}
}

func TestSampling_TheBudgetIsSafeForConcurrentUse(t *testing.T) {
	var none [16]byte
	for i := range none {
		none[i] = 0xff
	}
	budget, err := NewTraceBudget(TraceSampling{Budget: 50})
	if err != nil {
		t.Fatal(err)
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		kept int
	)
	at := time.Unix(0, 0)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if budget.Decide(none, at) {
					mu.Lock()
					kept++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if kept != 50 {
		t.Fatalf("kept %d roots from 800, want the budget of 50", kept)
	}
}
