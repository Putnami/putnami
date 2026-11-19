package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/config"
	"go.putnami.dev/logger"
	otlp "go.putnami.dev/protocol/telemetry"
)

// TraceSampling holds the three settings of the trace budget: the first
// Budget root traces are kept, then a root is kept when Ratio picks its trace
// ID, and at least FloorPerMinute roots are kept each minute. The protocol
// package go.putnami.dev/protocol/telemetry defines the rule, which the
// TypeScript runtime shares.
type TraceSampling = otlp.TraceSampling

// DefaultTraceSampling returns the settings that apply when neither the plugin
// nor the config sets them: a budget of 100 roots, a ratio of 0.01 and a floor
// of 1 root per minute.
func DefaultTraceSampling() TraceSampling { return otlp.DefaultTraceSampling() }

// DefaultSamplingRefresh is how often the plugin reloads the config sources
// and re-reads the telemetry.traces section while it runs. A source that keeps
// its first load for the process, such as CONFIG_DATA, or a Cloud remote
// source without CONFIG_SERVER_REFRESH, shows no change; Sampler.Update
// changes a running process.
const DefaultSamplingRefresh = 30 * time.Second

// samplingMeterName names the meter trace.decisions is recorded on.
const samplingMeterName = "go.putnami.dev/telemetry"

// traceSamplingSection is the telemetry.traces config section. Each key it
// sets overrides the matching field of the plugin's TraceSampling; a key it
// leaves out keeps that field.
type traceSamplingSection struct {
	Budget         *int64   `json:"budget"`
	Ratio          *float64 `json:"ratio"`
	FloorPerMinute *int64   `json:"floorPerMinute"`
}

var traceSamplingConfig = config.Config[traceSamplingSection](otlp.ConfigTraceSamplingPath)

// over returns base with each key the section sets.
func (s traceSamplingSection) over(base TraceSampling) TraceSampling {
	if s.Budget != nil {
		base.Budget = *s.Budget
	}
	if s.Ratio != nil {
		base.Ratio = *s.Ratio
	}
	if s.FloorPerMinute != nil {
		base.FloorPerMinute = *s.FloorPerMinute
	}
	return base
}

// key identifies the values of the section, so a re-read that finds the same
// values changes nothing.
func (s traceSamplingSection) key() string {
	return optionalKey(s.Budget) + "|" + optionalKey(s.Ratio) + "|" + optionalKey(s.FloorPerMinute)
}

func optionalKey[T int64 | float64](value *T) string {
	if value == nil {
		return "unset"
	}
	return fmt.Sprint(*value)
}

// Sampler decides the root spans of the tracer provider the plugin builds by
// the trace budget, and counts each decision in trace.decisions. The provider
// wraps it in a parent-based sampler, so a span with a parent, remote or
// local, follows its parent's sampled flag and never reaches it.
//
// Settings returns the settings in force, and Update replaces them for the
// next root decision. The plugin calls Update when the telemetry.traces config
// section changes; a caller that tightens the budget at runtime calls it too,
// and its settings hold until that section changes again.
type Sampler struct {
	budget  *otlp.TraceBudget
	now     func() time.Time
	counter atomic.Pointer[metric.Int64Counter]
}

var (
	keptOutcome    = metric.WithAttributeSet(attribute.NewSet(attribute.String(otlp.AttrTraceDecisionOutcome, otlp.TraceDecisionKept)))
	droppedOutcome = metric.WithAttributeSet(attribute.NewSet(attribute.String(otlp.AttrTraceDecisionOutcome, otlp.TraceDecisionDropped)))
)

// newSampler returns a sampler that applies settings, or the defaults when
// settings are out of range; Configure reports such settings.
func newSampler(settings TraceSampling) *Sampler {
	budget, err := otlp.NewTraceBudget(settings)
	if err != nil {
		// Configure reports the settings; until then the defaults apply.
		if budget, err = otlp.NewTraceBudget(DefaultTraceSampling()); err != nil {
			panic("telemetry: the default trace sampling is out of range: " + err.Error())
		}
	}
	return &Sampler{budget: budget, now: time.Now}
}

// ShouldSample implements sdktrace.Sampler for a root span.
func (s *Sampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	kept := s.budget.Decide(p.TraceID, s.now())
	if counter := s.counter.Load(); counter != nil {
		outcome := droppedOutcome
		if kept {
			outcome = keptOutcome
		}
		(*counter).Add(p.ParentContext, 1, outcome)
	}
	decision := sdktrace.Drop
	if kept {
		decision = sdktrace.RecordAndSample
	}
	return sdktrace.SamplingResult{
		Decision:   decision,
		Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState(),
	}
}

// Description implements sdktrace.Sampler.
func (s *Sampler) Description() string { return "PutnamiTraceBudget" }

// Settings returns the settings in force.
func (s *Sampler) Settings() TraceSampling { return s.budget.Settings() }

// Update applies settings to the next root decision. The roots already kept
// still count against the new budget and floor. Settings out of range are
// reported and leave the settings in force.
func (s *Sampler) Update(settings TraceSampling) error { return s.budget.Update(settings) }

// bindMeter records the decisions that follow on meter.
func (s *Sampler) bindMeter(meter metric.Meter) error {
	counter, err := meter.Int64Counter(otlp.MetricTraceDecisions,
		metric.WithDescription("Root trace sampling decisions, by outcome"))
	if err != nil {
		return err
	}
	s.counter.Store(&counter)
	return nil
}

// samplingRefresh re-reads the telemetry.traces section and applies a change
// to the plugin's sampler.
type samplingRefresh struct {
	sampler *Sampler
	base    TraceSampling
	log     *logger.Logger
	// sources replaces the discovered config sources when set.
	sources []config.Source

	mu      sync.Mutex
	section string
	loadErr string
}

// apply reads the section once. A section whose values did not change since
// the last read changes nothing, so an Update a caller made holds. A section
// that fails to load, or holds a value out of range, keeps the settings in
// force and warns once until it changes.
func (r *samplingRefresh) apply(ctx context.Context) {
	section, err := config.LoadContext(ctx, traceSamplingConfig, r.sources...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil && err.Error() != r.loadErr {
			r.loadErr = err.Error()
			r.log.Warn("trace sampling keeps its settings: the "+otlp.ConfigTraceSamplingPath+" config section failed to load",
				slog.String("error", err.Error()))
		}
		return
	}
	r.loadErr = ""
	key := section.key()
	if key == r.section {
		return
	}
	r.section = key
	if err := r.sampler.Update(section.over(r.base)); err != nil {
		r.log.Warn("trace sampling keeps its settings: "+err.Error(),
			slog.String("section", otlp.ConfigTraceSamplingPath))
	}
}

// run applies the section every interval until ctx ends, then closes done.
func (r *samplingRefresh) run(ctx context.Context, interval time.Duration, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.apply(ctx)
		}
	}
}
