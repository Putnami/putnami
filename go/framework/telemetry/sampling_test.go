package telemetry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"net/http/httptest"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/config"
	putnami_http "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
	otlp "go.putnami.dev/protocol/telemetry"
)

const samplingFixturePath = "../../../protocols/telemetry/fixtures/conventions/trace-sampling.json"

// samplingProbe is a plugin configured against an in-memory exporter and a
// manual metric reader, with no config file and no CONFIG_DATA unless the
// test sets one.
type samplingProbe struct {
	plugin   *Plugin
	exporter *tracetest.InMemoryExporter
	reader   *sdkmetric.ManualReader
}

func newSamplingProbe(t *testing.T, cfg Config) *samplingProbe {
	t.Helper()
	saveOTelGlobals(t)
	if _, set := os.LookupEnv("CONFIG_DATA"); !set {
		t.Setenv("CONFIG_DATA", "")
	}
	probe := &samplingProbe{exporter: tracetest.NewInMemoryExporter(), reader: sdkmetric.NewManualReader()}
	cfg.TraceExporter = probe.exporter
	cfg.MetricReader = probe.reader
	probe.plugin = NewPlugin(cfg)
	if err := probe.plugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	t.Cleanup(func() { _ = probe.plugin.Stop(context.Background(), nil) })
	return probe
}

// roots starts and ends n root spans and returns how many were exported.
func (p *samplingProbe) roots(t *testing.T, n int) int {
	t.Helper()
	before := len(p.exported(t))
	tracer := otel.Tracer("sampling-probe")
	for range n {
		_, span := tracer.Start(context.Background(), "root")
		span.End()
	}
	return len(p.exported(t)) - before
}

func (p *samplingProbe) exported(t *testing.T) tracetest.SpanStubs {
	t.Helper()
	if err := p.plugin.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}
	return p.exporter.GetSpans()
}

// decisions reads trace.decisions by outcome.
func (p *samplingProbe) decisions(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := p.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != otlp.MetricTraceDecisions {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || !sum.IsMonotonic {
				t.Fatalf("%s is %T, want a monotonic int64 sum", m.Name, m.Data)
			}
			for _, point := range sum.DataPoints {
				if point.Attributes.Len() != 1 {
					t.Fatalf("%s carries %d attributes, want only %s", m.Name, point.Attributes.Len(), otlp.AttrTraceDecisionOutcome)
				}
				outcome, _ := point.Attributes.Value(otlp.AttrTraceDecisionOutcome)
				counts[outcome.AsString()] += point.Value
			}
		}
	}
	return counts
}

// parentCase is a case of the fixture's parentCases: a request's two trace
// headers under fixed settings, and the server span it gives.
type parentCase struct {
	Name               string        `json:"name"`
	Settings           TraceSampling `json:"settings"`
	Traceparent        string        `json:"traceparent"`
	PutnamiTraceparent string        `json:"putnamiTraceparent"`
	Sampled            bool          `json:"sampled"`
	Decided            bool          `json:"decided"`
	ParentSpanID       string        `json:"parentSpanId"`
}

func readParentCases(t *testing.T) []parentCase {
	t.Helper()
	data, err := os.ReadFile(samplingFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ParentCases []parentCase `json:"parentCases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.ParentCases) == 0 {
		t.Fatal("the fixture holds no parent case")
	}
	return fixture.ParentCases
}

// serveRequest sends one request with the two trace headers through the HTTP
// middleware and returns the span context of its server span.
func serveRequest(t *testing.T, traceparent, putnamiTraceparent string) trace.SpanContext {
	t.Helper()
	request := httptest.NewRequest("GET", "/", nil)
	if traceparent != "" {
		request.Header.Set(otlp.HeaderTraceparent, traceparent)
	}
	if putnamiTraceparent != "" {
		request.Header.Set(otlp.HeaderPutnamiTraceparent, putnamiTraceparent)
	}
	ctx := putnami_http.NewContext(httptest.NewRecorder(), request)
	ctx.Route = "/"
	var span trace.SpanContext
	HTTPMiddleware("test-svc")(ctx, func() *putnami_http.Response {
		span = trace.SpanContextFromContext(ctx.Request.Context())
		return nil
	})
	return span
}

func TestSampling_UnsetSamplingUsesTheProtocolDefaults(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "trace-budget", "unset-sampling-uses-the-protocol-defaults")
	probe := newSamplingProbe(t, Config{ServiceName: "test"})
	if got := probe.plugin.Sampler().Settings(); got != otlp.DefaultTraceSampling() {
		t.Fatalf("Settings() = %+v, want the protocol defaults %+v", got, otlp.DefaultTraceSampling())
	}
	// The first 100 roots are kept; the ratio of 0.01 keeps few of the next 50.
	if got := probe.roots(t, 150); got < 100 || got >= 150 {
		t.Fatalf("150 roots exported %d spans, want the budget of 100 plus what the ratio picks", got)
	}
}

func TestSampling_TheConfigSectionOverridesThePluginSettings(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "trace-budget", "the-config-section-overrides-the-plugin-settings")
	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    budget: 3\n")
	// The plugin's ratio and floor of 0 differ from the defaults, so they show
	// that only the key the section sets is overridden.
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{Budget: 10}})
	want := TraceSampling{Budget: 3}
	if got := probe.plugin.Sampler().Settings(); got != want {
		t.Fatalf("Settings() = %+v, want the budget from config and the other fields from the plugin, %+v", got, want)
	}
	if got := probe.roots(t, 10); got != 3 {
		t.Fatalf("10 roots exported %d spans, want the configured budget of 3", got)
	}
}

func TestSampling_AChildFollowsItsParentsSampledFlag(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "trace-budget", "a-child-follows-its-parents-sampled-flag")
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{}})
	tracer := otel.Tracer("sampling-probe")
	remote := func(flags trace.TraceFlags) context.Context {
		sc := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
			TraceFlags: flags,
			Remote:     true,
		})
		return trace.ContextWithRemoteSpanContext(context.Background(), sc)
	}

	// The budget samples no root, yet a sampled caller's trace is kept, and
	// so is a local child of that span.
	ctx, server := tracer.Start(remote(trace.FlagsSampled), "server")
	_, child := tracer.Start(ctx, "child")
	child.End()
	server.End()
	if got := len(probe.exported(t)); got != 2 {
		t.Fatalf("a sampled caller exported %d spans, want the server span and its child", got)
	}

	// A raised budget keeps nothing the caller dropped.
	if err := probe.plugin.Sampler().Update(TraceSampling{Budget: 10}); err != nil {
		t.Fatal(err)
	}
	ctx, server = tracer.Start(remote(0), "server")
	_, child = tracer.Start(ctx, "child")
	child.End()
	server.End()
	if got := len(probe.exported(t)); got != 2 {
		t.Fatalf("an unsampled caller exported %d more spans, want none", got-2)
	}
	if got := probe.decisions(t); len(got) != 0 {
		t.Fatalf("spans with a parent made root decisions: %v", got)
	}
}

func TestSampling_EachRootDecisionIsCounted(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "trace-budget", "each-root-decision-is-counted")
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{Budget: 2}})
	if got := probe.roots(t, 5); got != 2 {
		t.Fatalf("5 roots exported %d spans, want 2", got)
	}
	got := probe.decisions(t)
	if got[otlp.TraceDecisionKept] != 2 || got[otlp.TraceDecisionDropped] != 3 || len(got) != 2 {
		t.Fatalf("trace.decisions = %v, want kept 2 and dropped 3", got)
	}
}

func TestSampling_ZeroSettingsSampleNoRoot(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "trace-budget", "zero-settings-sample-no-root")
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{}})
	if got := probe.roots(t, 40); got != 0 {
		t.Fatalf("zero settings exported %d of 40 roots, want none", got)
	}
}

func TestSampling_InvalidPluginSettingsFailConfigure(t *testing.T) {
	saveOTelGlobals(t)
	p := NewPlugin(Config{ServiceName: "test", TraceSampling: &TraceSampling{Ratio: 2}})
	if err := p.Configure(context.Background(), nil); err == nil {
		_ = p.Stop(context.Background(), nil)
		t.Fatal("Configure accepted a ratio of 2")
	}
}

func TestSampling_TheGoSamplerDecidesTheFixtureScenarios(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "trace-sampling-is-shared", "the-go-sampler-decides-the-fixture-scenarios")
	data, err := os.ReadFile(samplingFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Scenarios []struct {
			Name     string        `json:"name"`
			Settings TraceSampling `json:"settings"`
			Roots    []struct {
				AtSecond int64  `json:"atSecond"`
				TraceID  string `json:"traceId"`
				Kept     bool   `json:"kept"`
			} `json:"roots"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Scenarios) == 0 {
		t.Fatal("the fixture holds no scenario")
	}
	for _, scenario := range fixture.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			sampler := newSampler(scenario.Settings)
			if sampler.Settings() != scenario.Settings {
				t.Fatalf("settings %+v are out of range", scenario.Settings)
			}
			root := sdktrace.ParentBased(sampler)
			for i, step := range scenario.Roots {
				var id trace.TraceID
				if _, err := hex.Decode(id[:], []byte(step.TraceID)); err != nil {
					t.Fatal(err)
				}
				sampler.now = func() time.Time { return time.Unix(step.AtSecond, 0) }
				result := root.ShouldSample(sdktrace.SamplingParameters{ParentContext: context.Background(), TraceID: id, Name: "root"})
				if kept := result.Decision == sdktrace.RecordAndSample; kept != step.Kept {
					t.Errorf("root %d at %ds: kept = %v, want %v", i, step.AtSecond, kept, step.Kept)
				}
			}
		})
	}
}

// --- Parent policy ---

func TestSampling_TheGoRuntimeDecidesTheFixtureParents(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "trace-parent-policy", "the-go-runtime-decides-the-fixture-parents")
	spectest.Proves(t, "go/service-telemetry", "parent-policy", "an-outside-parent-takes-a-root-decision-under-decide")
	spectest.Proves(t, "go/service-telemetry", "parent-policy", "a-putnami-parent-is-followed-under-decide")
	for _, c := range readParentCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			settings := c.Settings
			probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &settings})
			span := serveRequest(t, c.Traceparent, c.PutnamiTraceparent)
			if span.IsSampled() != c.Sampled {
				t.Errorf("sampled = %v, want %v", span.IsSampled(), c.Sampled)
			}
			parent, ok := otlp.ParseTraceparent(c.Traceparent)
			if !ok || span.TraceID().String() != parent.TraceID {
				t.Errorf("the server span is on trace %s, want the parent's", span.TraceID())
			}
			decisions := probe.decisions(t)
			if decided := decisions[otlp.TraceDecisionKept]+decisions[otlp.TraceDecisionDropped] == 1; decided != c.Decided {
				t.Errorf("trace.decisions = %v, want decided %v", decisions, c.Decided)
			}
			exported := probe.exported(t)
			if c.Sampled != (len(exported) == 1) {
				t.Fatalf("exported %d spans, want sampled %v", len(exported), c.Sampled)
			}
			if c.Sampled && exported[0].Parent.SpanID().String() != c.ParentSpanID {
				t.Errorf("parent span = %s, want %s", exported[0].Parent.SpanID(), c.ParentSpanID)
			}
		})
	}
}

func TestSampling_TheConfigSectionSetsTheParentPolicy(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "parent-policy", "the-config-section-sets-the-parent-policy")
	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    parent: decide\n")
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{Ratio: 1}})
	if got := probe.plugin.Sampler().Settings(); got != (TraceSampling{Ratio: 1, Parent: ParentDecide}) {
		t.Fatalf("Settings() = %+v, want the parent policy from config", got)
	}
	if span := serveRequest(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-a1a1a1a1a1a1a1a1-00", ""); !span.IsSampled() {
		t.Fatal("under decide an outside parent's unsampled flag still decided the span")
	}
}

func TestSampling_AnInvalidParentPolicyFailsConfigure(t *testing.T) {
	saveOTelGlobals(t)
	t.Setenv("CONFIG_DATA", "")
	plugin := NewPlugin(Config{ServiceName: "test", TraceSampling: &TraceSampling{Parent: "always"}})
	if err := plugin.Configure(context.Background(), nil); err == nil {
		_ = plugin.Stop(context.Background(), nil)
		t.Fatal("Configure accepted the parent policy always")
	}
}

// --- Live settings ---

func TestSampling_AChangedKeyAppliesWithoutARestart(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "live-sampling-settings", "a-changed-key-applies-without-a-restart")
	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    budget: 1\n    ratio: 0\n    floorPerMinute: 0\n")
	probe := newSamplingProbe(t, Config{ServiceName: "test", samplingRefresh: 10 * time.Millisecond})
	if got := probe.roots(t, 3); got != 1 {
		t.Fatalf("3 roots exported %d spans, want the budget of 1", got)
	}

	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    budget: 4\n    ratio: 0\n    floorPerMinute: 0\n")
	// The plugin's other settings are the protocol defaults, parent policy included.
	waitForSettings(t, probe.plugin.Sampler(), TraceSampling{Budget: 4, Parent: ParentFollow})
	// One root is already kept, so a budget of 4 keeps 3 more.
	if got := probe.roots(t, 5); got != 3 {
		t.Fatalf("after the change 5 roots exported %d spans, want 3", got)
	}
}

func TestSampling_AnInvalidSectionKeepsTheSettingsInForce(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "live-sampling-settings", "an-invalid-section-keeps-the-settings-in-force")
	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    budget: 2\n")
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{}})
	refresh := &samplingRefresh{sampler: probe.plugin.Sampler(), base: TraceSampling{}, log: probe.plugin.log}
	refresh.apply(context.Background())
	want := TraceSampling{Budget: 2}

	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    ratio: 3\n")
	refresh.apply(context.Background())
	if got := probe.plugin.Sampler().Settings(); got != want {
		t.Fatalf("a ratio of 3 changed the settings to %+v, want %+v", got, want)
	}

	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    budget: many\n")
	refresh.apply(context.Background())
	if got := probe.plugin.Sampler().Settings(); got != want {
		t.Fatalf("a section that fails to load changed the settings to %+v, want %+v", got, want)
	}
}

// onceSource keeps its first load for the life of the process, as CONFIG_DATA
// and the Cloud remote sources do.
type onceSource struct {
	loads  int
	values []map[string]any
	first  map[string]any
}

func (s *onceSource) Name() string  { return "once" }
func (s *onceSource) Priority() int { return 50 }
func (s *onceSource) Load() (map[string]any, error) {
	if s.first == nil {
		s.first = s.values[s.loads]
	}
	s.loads++
	return s.first, nil
}

func TestSampling_ACachedSourceShowsNoChange(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "live-sampling-settings", "a-cached-source-shows-no-change")
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{}})
	source := &onceSource{values: []map[string]any{
		{"telemetry": map[string]any{"traces": map[string]any{"budget": 2}}},
		{"telemetry": map[string]any{"traces": map[string]any{"budget": 9}}},
	}}
	refresh := &samplingRefresh{sampler: probe.plugin.Sampler(), base: TraceSampling{}, log: probe.plugin.log, sources: []config.Source{source}}
	refresh.apply(context.Background())
	refresh.apply(context.Background())
	if source.loads != 2 {
		t.Fatalf("two refreshes loaded the source %d times, want 2", source.loads)
	}
	if got := probe.plugin.Sampler().Settings(); got != (TraceSampling{Budget: 2}) {
		t.Fatalf("a source that keeps its first load changed the settings to %+v", got)
	}
}

func TestSampling_AnUpdateHoldsUntilTheSectionChanges(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "live-sampling-settings", "an-update-holds-until-the-section-changes")
	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    budget: 5\n")
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{}})
	refresh := &samplingRefresh{sampler: probe.plugin.Sampler(), base: TraceSampling{}, log: probe.plugin.log}
	refresh.apply(context.Background())

	tightened := TraceSampling{Budget: 1}
	if err := probe.plugin.Sampler().Update(tightened); err != nil {
		t.Fatal(err)
	}
	refresh.apply(context.Background())
	if got := probe.plugin.Sampler().Settings(); got != tightened {
		t.Fatalf("re-reading an unchanged section replaced the update with %+v", got)
	}

	t.Setenv("CONFIG_DATA", "telemetry:\n  traces:\n    budget: 7\n")
	refresh.apply(context.Background())
	if got := probe.plugin.Sampler().Settings(); got != (TraceSampling{Budget: 7}) {
		t.Fatalf("a changed section left the settings at %+v, want a budget of 7", got)
	}
}

func TestSampling_StopEndsTheRefresh(t *testing.T) {
	saveOTelGlobals(t)
	t.Setenv("CONFIG_DATA", "")
	p := NewPlugin(Config{ServiceName: "test", samplingRefresh: time.Millisecond})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	done := p.refreshDone
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Stop returned while the refresh still runs")
	}
}

func waitForSettings(t *testing.T, sampler *Sampler, want TraceSampling) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for sampler.Settings() != want {
		if time.Now().After(deadline) {
			t.Fatalf("Settings() = %+v after 5s, want %+v", sampler.Settings(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
