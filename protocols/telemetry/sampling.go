package telemetry

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"time"
)

// Trace sampling conventions. Every runtime decides each root span by the same
// rule, from the same three config keys, and counts each decision under the
// same counter, so a Go and a TypeScript service export a comparable, bounded
// set of traces.
//
// The values are also committed in fixtures/conventions/trace-sampling.json,
// which the TypeScript runtime reads in its tests; sampling_test.go keeps these
// constants and the rule equal to that file.
const (
	// ConfigTraceSamplingPath is the config section that holds the three keys.
	ConfigTraceSamplingPath = "telemetry.traces"
	// KeyTraceBudget is the number of root traces an instance keeps first.
	KeyTraceBudget = "budget"
	// KeyTraceRatio is the share of root traces kept once the budget is spent.
	KeyTraceRatio = "ratio"
	// KeyTraceFloorPerMinute is the number of root traces kept each minute
	// whatever the ratio picks.
	KeyTraceFloorPerMinute = "floorPerMinute"

	// DefaultTraceBudget is the budget when nothing sets one.
	DefaultTraceBudget = 100
	// DefaultTraceRatio is the ratio when nothing sets one.
	DefaultTraceRatio = 0.01
	// DefaultTraceFloorPerMinute is the floor when nothing sets one.
	DefaultTraceFloorPerMinute = 1

	// MetricTraceDecisions counts each root decision.
	MetricTraceDecisions = "trace.decisions"
	// AttrTraceDecisionOutcome labels a decision kept or dropped.
	AttrTraceDecisionOutcome = "outcome"
	// TraceDecisionKept is the outcome of a kept root.
	TraceDecisionKept = "kept"
	// TraceDecisionDropped is the outcome of a dropped root.
	TraceDecisionDropped = "dropped"
)

// TraceSampling holds the three settings of the sampling rule.
type TraceSampling struct {
	// Budget is the number of root traces the instance keeps first.
	Budget int64 `json:"budget"`
	// Ratio is the share of root traces kept once the budget is spent,
	// between 0 and 1.
	Ratio float64 `json:"ratio"`
	// FloorPerMinute is the number of root traces kept each UTC minute
	// whatever the ratio picks.
	FloorPerMinute int64 `json:"floorPerMinute"`
}

// DefaultTraceSampling returns the settings that apply when nothing sets them.
func DefaultTraceSampling() TraceSampling {
	return TraceSampling{
		Budget:         DefaultTraceBudget,
		Ratio:          DefaultTraceRatio,
		FloorPerMinute: DefaultTraceFloorPerMinute,
	}
}

// Validate reports a setting out of range: a negative budget or floor, or a
// ratio outside [0, 1].
func (s TraceSampling) Validate() error {
	if s.Budget < 0 {
		return fmt.Errorf("%s.%s must be 0 or more, got %d", ConfigTraceSamplingPath, KeyTraceBudget, s.Budget)
	}
	if math.IsNaN(s.Ratio) || s.Ratio < 0 || s.Ratio > 1 {
		return fmt.Errorf("%s.%s must lie between 0 and 1, got %v", ConfigTraceSamplingPath, KeyTraceRatio, s.Ratio)
	}
	if s.FloorPerMinute < 0 {
		return fmt.Errorf("%s.%s must be 0 or more, got %d", ConfigTraceSamplingPath, KeyTraceFloorPerMinute, s.FloorPerMinute)
	}
	return nil
}

// TraceRatioPicks reports whether ratio picks traceID: its last 8 bytes, read
// as a big-endian unsigned integer and shifted right by one bit, are below
// ratio times 2^63. It is the test of the OpenTelemetry TraceIDRatioBased
// sampler, so every runtime picks the same trace IDs.
func TraceRatioPicks(traceID [16]byte, ratio float64) bool {
	if ratio >= 1 {
		return true
	}
	if !(ratio > 0) {
		return false
	}
	bound := uint64(ratio * (1 << 63))
	return binary.BigEndian.Uint64(traceID[8:16])>>1 < bound
}

// TraceBudget applies the sampling rule to the root spans of one instance. It
// is safe for concurrent use.
//
// A root is kept while the instance has kept fewer than Budget roots, or while
// the current UTC minute holds fewer than FloorPerMinute kept roots, or when
// the ratio picks its trace ID. Otherwise it is dropped.
type TraceBudget struct {
	mu           sync.Mutex
	settings     TraceSampling
	kept         int64
	minute       int64
	keptInMinute int64
	// directive limits settings until directiveEnds.
	directive     ExportDirective
	directiveEnds time.Time
}

// NewTraceBudget returns a budget that applies settings. It reports settings
// out of range.
func NewTraceBudget(settings TraceSampling) (*TraceBudget, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	return &TraceBudget{settings: settings, minute: math.MinInt64}, nil
}

// Decide reports whether the root span of traceID, started at now, is kept.
func (b *TraceBudget) Decide(traceID [16]byte, now time.Time) bool {
	seconds := now.Unix()
	minute := seconds / 60
	if seconds%60 < 0 {
		minute-- // floor, not truncation, before the epoch
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if minute != b.minute {
		b.minute = minute
		b.keptInMinute = 0
	}
	settings := b.inForceLocked(now)
	keep := b.kept < settings.Budget ||
		b.keptInMinute < settings.FloorPerMinute ||
		TraceRatioPicks(traceID, settings.Ratio)
	if keep {
		b.kept++
		b.keptInMinute++
	}
	return keep
}

// Restrict applies directive, which arrived at now, to the decisions until its
// TTL ends: each one takes the stricter of the directive and the settings. It
// replaces a directive in force. The settings themselves stay, and Update
// still changes them.
func (b *TraceBudget) Restrict(directive ExportDirective, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.directive = directive
	b.directiveEnds = now.Add(directive.TTL)
}

// InForce returns the settings a decision at now applies: the settings,
// limited by a directive that still holds.
func (b *TraceBudget) InForce(now time.Time) TraceSampling {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inForceLocked(now)
}

func (b *TraceBudget) inForceLocked(now time.Time) TraceSampling {
	if now.Before(b.directiveEnds) {
		return b.directive.LimitTraceSampling(b.settings)
	}
	return b.settings
}

// Settings returns the settings Update or NewTraceBudget set, without a
// directive's limits.
func (b *TraceBudget) Settings() TraceSampling {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settings
}

// Update applies settings to the next decision. The roots already kept still
// count against the new budget and floor. Settings out of range are reported
// and leave the settings in force unchanged.
func (b *TraceBudget) Update(settings TraceSampling) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	b.settings = settings
	b.mu.Unlock()
	return nil
}
