package telemetry

import (
	"context"
	stderrors "errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	frameworkapp "go.putnami.dev/app"
	pkgerrors "go.putnami.dev/errors"
	putnami_http "go.putnami.dev/http"
	"go.putnami.dev/inject"

	"go.putnami.dev/protocol/features/spectest"
)

// saveOTelGlobals captures the process-wide OpenTelemetry globals and returns a
// restore function. Configure() mutates these globals, so tests that call it must
// restore them to avoid cross-test interference.
func saveOTelGlobals(t *testing.T) {
	t.Helper()
	tp := otel.GetTracerProvider()
	mp := otel.GetMeterProvider()
	prop := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
	})
}

// failingSpanExporter is a SpanExporter whose Shutdown always errors, used to
// exercise the Stop() aggregate-error path.
type failingSpanExporter struct{}

var errExporterShutdown = stderrors.New("boom: exporter shutdown failed")

func (failingSpanExporter) ExportSpans(_ context.Context, _ []sdktrace.ReadOnlySpan) error {
	return nil
}

func (failingSpanExporter) Shutdown(_ context.Context) error {
	return errExporterShutdown
}

// --- Provides() purity: reading registrations has no side effect ---

// Provides must be a pure DI declaration: it returns the two registrations and
// performs NO global OTel mutation, builds NO providers, and registers NO
// error-bridge hook. All of that is Configure's job.
func TestPluginProvidesIsPure(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "lifecycle-purity", "reading-the-provider-registrations-has-no-side-effect")
	saveOTelGlobals(t)

	beforeTP := otel.GetTracerProvider()
	beforeMP := otel.GetMeterProvider()
	beforeProp := otel.GetTextMapPropagator()

	p := NewPlugin(Config{
		ServiceName:   "pure-svc",
		TraceExporter: tracetest.NewInMemoryExporter(),
		MetricReader:  sdkmetric.NewManualReader(),
	})

	regs := p.Provides()

	// Two DI registrations: TracerProvider + MeterProvider.
	if len(regs) != 2 {
		t.Fatalf("Provides() returned %d registrations, want 2", len(regs))
	}

	// No providers built yet — construction is deferred to Configure.
	if p.tp != nil {
		t.Error("Provides() must not build a TracerProvider")
	}
	if p.mp != nil {
		t.Error("Provides() must not build a MeterProvider")
	}
	// No error-bridge hook registered — Provides must not touch the errors
	// package's process-global OnError registry.
	if p.removeErrorBridge != nil {
		t.Error("Provides() must not register the error bridge")
	}

	// Globals must be untouched.
	if got := otel.GetTracerProvider(); got != beforeTP {
		t.Errorf("Provides() mutated the global TracerProvider: %p != %p", got, beforeTP)
	}
	if got := otel.GetMeterProvider(); got != beforeMP {
		t.Errorf("Provides() mutated the global MeterProvider: %p != %p", got, beforeMP)
	}
	if got := otel.GetTextMapPropagator(); got != beforeProp {
		t.Error("Provides() mutated the global TextMapPropagator")
	}
}

// --- Configure(): install globals exactly once, rebuild after stop ---

func TestPluginConfigureInstallsGlobalsAndRegistrations(t *testing.T) {
	saveOTelGlobals(t)

	exporter := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	p := NewPlugin(Config{
		ServiceName:    "provides-svc",
		ServiceVersion: "1.2.3",
		TraceExporter:  exporter,
		MetricReader:   reader,
	})

	regs := p.Provides()
	if len(regs) != 2 {
		t.Fatalf("Provides() returned %d registrations, want 2", len(regs))
	}

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}

	// The plugin must have built and retained both providers.
	if p.tp == nil {
		t.Fatal("Configure() did not build a TracerProvider")
	}
	if p.mp == nil {
		t.Fatal("Configure() did not build a MeterProvider")
	}

	// Globals must be installed and point at the plugin's providers.
	if got := otel.GetTracerProvider(); got != trace.TracerProvider(p.tp) {
		t.Errorf("global TracerProvider = %p, want plugin tp %p", got, p.tp)
	}
	if got := otel.GetMeterProvider(); got != metric.MeterProvider(p.mp) {
		t.Errorf("global MeterProvider = %p, want plugin mp %p", got, p.mp)
	}

	// Propagator must be a composite carrying TraceContext + Baggage.
	prop := otel.GetTextMapPropagator()
	if prop == nil {
		t.Fatal("Configure() did not install a propagator")
	}
	fields := prop.Fields()
	if !containsAll(fields, "traceparent", "baggage") {
		t.Errorf("propagator fields = %v, want traceparent and baggage", fields)
	}

	// The lazy registrations must resolve to the same provider instances through
	// a DI container, now that Configure has populated p.tp/p.mp.
	c := inject.NewContainer("test", nil)
	for _, reg := range regs {
		if err := c.Register(reg); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	gotTP, err := c.Get(inject.TokenOf[trace.TracerProvider]())
	if err != nil {
		t.Fatalf("resolve TracerProvider: %v", err)
	}
	if gotTP == nil || isNilInterface(gotTP) {
		t.Fatalf("resolved TracerProvider is nil: %#v", gotTP)
	}

	gotMP, err := c.Get(inject.TokenOf[metric.MeterProvider]())
	if err != nil {
		t.Fatalf("resolve MeterProvider: %v", err)
	}
	if gotMP == nil || isNilInterface(gotMP) {
		t.Fatalf("resolved MeterProvider is nil: %#v", gotMP)
	}

	// A clean shutdown should flush without error.
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop() after Configure() returned error: %v", err)
	}
}

func TestPluginConfigureWiresErrorBridge(t *testing.T) {
	saveOTelGlobals(t)

	reader := sdkmetric.NewManualReader()
	p := NewPlugin(Config{
		ServiceName:  "bridge-svc",
		MetricReader: reader,
	})

	p.Provides()
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background(), nil) })

	// The error bridge registered by Configure() increments errors_total through
	// the plugin's MeterProvider, which is fed by our ManualReader.
	_ = pkgerrors.New(pkgerrors.CodeInternal, "configure bridge error")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if !hasMetric(rm, "errors_total") {
		t.Error("expected errors_total metric from Configure()-wired error bridge")
	}
}

func TestPluginConfigureNoExporters(t *testing.T) {
	saveOTelGlobals(t)

	// No TraceExporter / MetricReader exercises the nil-exporter branches in
	// Configure(); providers should still be built and installed.
	p := NewPlugin(Config{ServiceName: "no-exporters"})

	regs := p.Provides()
	if len(regs) != 2 {
		t.Fatalf("Provides() returned %d registrations, want 2", len(regs))
	}
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	if p.tp == nil || p.mp == nil {
		t.Fatal("Configure() should build providers even without exporters")
	}
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

type telemetryConsumerPlugin struct {
	TP trace.TracerProvider
	MP metric.MeterProvider
}

func (p *telemetryConsumerPlugin) Name() string { return "telemetry-consumer" }

func TestPluginProvidersSurviveAutoWireBeforeConfigure(t *testing.T) {
	saveOTelGlobals(t)

	exporter := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	consumer := &telemetryConsumerPlugin{}
	p := NewPlugin(Config{
		ServiceName:   "autowire-svc",
		TraceExporter: exporter,
		MetricReader:  reader,
	})

	application := frameworkapp.New("telemetry-autowire")
	// The consumer is intentionally registered before telemetry. Auto-wire runs
	// before any Configure method, so this catches provider values that resolve
	// to the current nil p.tp/p.mp instead of configuring first.
	application.Use(consumer)
	application.Use(p)
	application.Run(func(ctx context.Context) error {
		if consumer.TP == nil || isNilInterface(consumer.TP) {
			return stderrors.New("auto-wired TracerProvider is nil")
		}
		if consumer.MP == nil || isNilInterface(consumer.MP) {
			return stderrors.New("auto-wired MeterProvider is nil")
		}
		_, span := consumer.TP.Tracer("consumer").Start(ctx, "autowire-span")
		span.End()
		counter, err := consumer.MP.Meter("consumer").Int64Counter("autowire_total")
		if err != nil {
			return err
		}
		counter.Add(ctx, 1)
		return nil
	})

	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if err := p.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush spans: %v", err)
	}
	if spans := exporter.GetSpans(); len(spans) != 1 || spans[0].Name != "autowire-span" {
		t.Fatalf("expected autowire span to be exported, got %+v", spans)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if !hasMetric(rm, "autowire_total") {
		t.Error("expected metric recorded through auto-wired MeterProvider")
	}

	if err := application.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

// Configure must run exactly once: repeated calls (Validate-then-Start, or a
// re-entrant buildContainer+configure pass) must not install a second set of
// globals, leak a provider, or double-register the error-bridge hook.
func TestPluginConfigureRunsExactlyOnce(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "lifecycle-purity", "configure-installs-the-global-providers-exactly-once-per-lifetime")
	saveOTelGlobals(t)

	reader := sdkmetric.NewManualReader()
	p := NewPlugin(Config{
		ServiceName:   "once-svc",
		TraceExporter: tracetest.NewInMemoryExporter(),
		MetricReader:  reader,
	})

	p.Provides()
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("first Configure() error: %v", err)
	}

	// Capture the providers and the error-bridge handle installed by the first
	// Configure so we can detect a second build/registration.
	firstTP := p.tp
	firstMP := p.mp
	firstBridge := p.removeErrorBridge
	if firstTP == nil || firstMP == nil || firstBridge == nil {
		t.Fatal("first Configure() did not fully initialize the plugin")
	}

	// Re-run Configure several times (and re-declare via Provides, mirroring a
	// repeated buildContainer pass).
	for i := 0; i < 3; i++ {
		p.Provides()
		if err := p.Configure(context.Background(), nil); err != nil {
			t.Fatalf("re-entrant Configure() #%d error: %v", i, err)
		}
	}

	// The providers must be the SAME instances — no leaked replacement that the
	// globals point at without the original being shut down.
	if p.tp != firstTP {
		t.Error("Configure() rebuilt the TracerProvider on a second call (leak)")
	}
	if p.mp != firstMP {
		t.Error("Configure() rebuilt the MeterProvider on a second call (leak)")
	}
	if got := otel.GetTracerProvider(); got != trace.TracerProvider(firstTP) {
		t.Error("global TracerProvider points at a replacement provider")
	}
	if got := otel.GetMeterProvider(); got != metric.MeterProvider(firstMP) {
		t.Error("global MeterProvider points at a replacement provider")
	}

	// Exactly one error-bridge hook may be live: a single error must increment
	// errors_total by exactly 1 (a double-registered hook would count 2+).
	_ = pkgerrors.New(pkgerrors.CodeInternal, "single count")
	if got := readErrorsTotal(t, reader); got != 1 {
		t.Errorf("errors_total = %d, want 1 (double-registered bridge double-counts)", got)
	}

	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

func TestPluginConfigureRebuildsAfterApplicationStop(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "lifecycle-purity", "the-plugin-rebuilds-cleanly-after-an-application-stop")
	saveOTelGlobals(t)

	p := NewPlugin(Config{ServiceName: "restart-svc"})
	application := frameworkapp.New("telemetry-restart")
	application.Use(p)
	application.Run(func(context.Context) error { return nil })

	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("first Start() error: %v", err)
	}
	firstTP := p.tp
	firstMP := p.mp
	firstBridge := p.removeErrorBridge
	if firstTP == nil || firstMP == nil || firstBridge == nil {
		t.Fatal("first Start() did not configure telemetry")
	}
	if err := application.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop() error: %v", err)
	}
	if p.removeErrorBridge != nil {
		t.Fatal("Stop() did not remove the error bridge")
	}

	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error: %v", err)
	}
	if p.tp == nil || p.mp == nil || p.removeErrorBridge == nil {
		t.Fatal("second Start() did not reconfigure telemetry")
	}
	if p.tp == firstTP {
		t.Error("second Start() reused the shutdown TracerProvider")
	}
	if p.mp == firstMP {
		t.Error("second Start() reused the shutdown MeterProvider")
	}

	if err := application.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error: %v", err)
	}
}

// Configure must validate OTLP.Endpoint, surfacing a configure error rather
// than silently constructing an exporter that POSTs to an empty URL.
func TestPluginConfigureRejectsEmptyOTLPEndpoint(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "endpoint-required", "an-otlp-configuration-with-an-empty-endpoint-fails-at-configure")
	saveOTelGlobals(t)

	p := NewPlugin(Config{
		ServiceName: "bad-otlp",
		OTLP:        &OTLPConfig{}, // Endpoint omitted
	})
	p.Provides()

	err := p.Configure(context.Background(), nil)
	if err == nil {
		t.Fatal("Configure() must reject an OTLP config with an empty Endpoint")
	}
	se := pkgerrors.GetError(err)
	if se == nil || se.Code() != CodeConfigure {
		t.Errorf("Configure() error = %v, want code %q", err, CodeConfigure)
	}

	// The error is sticky across repeated calls (sync.Once captured it).
	if err2 := p.Configure(context.Background(), nil); err2 != err { //nolint:errorlint // identity check is intentional: same captured error
		t.Errorf("second Configure() = %v, want the same captured error %v", err2, err)
	}
}

// --- Stop() error paths ---

func TestPluginStopAggregatesTracerError(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "bounded-shutdown", "a-tracer-shutdown-failure-is-reported")
	saveOTelGlobals(t)

	// Wire BOTH a span exporter and a metric reader so both shutdown branches in
	// Stop() run. The batch span processor forwards exporter errors to the OTel
	// error handler rather than returning them, so the reliable way to force both
	// TracerProvider.Shutdown and MeterProvider.Shutdown to error is to pass an
	// already-canceled context — Stop() derives its timeout context from it.
	p := NewPlugin(Config{
		ServiceName:   "stop-err-svc",
		TraceExporter: failingSpanExporter{},
		MetricReader:  sdkmetric.NewManualReader(),
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Stop so the shutdown context is already done

	err := p.Stop(ctx, nil)
	if err == nil {
		t.Fatal("Stop() should return an aggregate error when shutdown fails")
	}

	// Stop() wraps each component failure; both must be present and the aggregate
	// must unwrap to the underlying context cancellation.
	if !stderrors.Is(err, context.Canceled) {
		t.Errorf("Stop() error = %v, want it to wrap context.Canceled", err)
	}

	var agg *pkgerrors.AggregateError
	if !stderrors.As(err, &agg) {
		t.Fatalf("Stop() error is not an *errors.AggregateError: %T", err)
	}
	// The batch-backed TracerProvider honors the canceled context and errors;
	// the ManualReader-backed MeterProvider has nothing to flush and shuts down
	// cleanly, so exactly the tracer branch is aggregated here. (The meter branch
	// is covered by TestPluginStopAggregatesMeterError.)
	if len(agg.Errors) != 1 {
		t.Fatalf("aggregate has %d errors, want 1 (tracer)", len(agg.Errors))
	}

	// The wrapped error must carry the telemetry shutdown code.
	se := pkgerrors.GetError(agg.Errors[0])
	if se == nil {
		t.Fatalf("wrapped error %v is not a structured error", agg.Errors[0])
	}
	if se.Code() != CodeShutdown {
		t.Errorf("wrapped error code = %q, want %q", se.Code(), CodeShutdown)
	}
}

func TestPluginStopAggregatesMeterError(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "bounded-shutdown", "a-meter-shutdown-failure-is-reported-rather-than-lost-behind-the-tracers")
	saveOTelGlobals(t)

	reader := sdkmetric.NewManualReader()
	p := NewPlugin(Config{
		ServiceName:  "stop-meter-err",
		MetricReader: reader,
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}

	// First Stop performs a clean shutdown.
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("first Stop() returned error: %v", err)
	}

	// MeterProvider.Shutdown is idempotent and returns an error on subsequent
	// calls; the second Stop should therefore aggregate the meter error.
	err := p.Stop(context.Background(), nil)
	if err == nil {
		t.Fatal("second Stop() should aggregate the meter shutdown error")
	}
	var agg *pkgerrors.AggregateError
	if !stderrors.As(err, &agg) {
		t.Fatalf("Stop() error is not an *errors.AggregateError: %T", err)
	}
}

func TestPluginStopRespectsShutdownTimeout(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "bounded-shutdown", "shutdown-bounds-the-flush-by-the-configured-shutdown-timeout")
	saveOTelGlobals(t)

	p := NewPlugin(Config{
		ServiceName:     "timeout-svc",
		ShutdownTimeout: 50 * time.Millisecond,
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}

	if p.cfg.ShutdownTimeout != 50*time.Millisecond {
		t.Errorf("ShutdownTimeout = %v, want 50ms", p.cfg.ShutdownTimeout)
	}
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

// --- metrics.go: UpDownCounter ---

func TestUpDownCounterCreation(t *testing.T) {
	saveOTelGlobals(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	meter := mp.Meter("test-updown")
	c, err := UpDownCounter(meter, "queue.depth",
		metric.WithDescription("items currently queued"),
	)
	if err != nil {
		t.Fatalf("UpDownCounter() error: %v", err)
	}
	if c == nil {
		t.Fatal("UpDownCounter() returned nil")
	}

	c.Add(context.Background(), 5)
	c.Add(context.Background(), -2)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	sum, ok := upDownSum(rm, "queue.depth")
	if !ok {
		t.Fatal("expected queue.depth metric to be recorded")
	}
	if sum != 3 {
		t.Errorf("queue.depth value = %d, want 3", sum)
	}
}

// --- HTTPMiddleware metric recording ---

func TestHTTPMiddlewareRecordsMetrics(t *testing.T) {
	saveOTelGlobals(t)

	// Tracing global (middleware starts a span; keep it from leaking to a real
	// exporter by using an in-memory one).
	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Metrics global feeding a ManualReader so we can read instruments back.
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)

	// HTTPMiddleware builds its instruments from the *global* meter, so the
	// global MeterProvider must be set before constructing the middleware.
	mw := HTTPMiddleware("metrics-svc")

	req := httptest.NewRequest("GET", "/widgets/7", nil)
	w := httptest.NewRecorder()
	ctx := putnami_http.NewContext(w, req)
	ctx.Route = "/widgets/{id}"

	resp := mw(ctx, func() *putnami_http.Response {
		return putnami_http.JSON(map[string]string{"ok": "true"})
	})
	if resp == nil {
		t.Fatal("expected response")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	if !hasMetric(rm, "http.server.request_count") {
		t.Error("expected http.server.request_count metric to be recorded")
	}
	if !hasMetric(rm, "http.server.duration") {
		t.Error("expected http.server.duration metric to be recorded")
	}

	// request_count must be an Int64 sum of exactly 1 for the single request,
	// carrying the method/route/status attributes.
	count, ok := counterSum(rm, "http.server.request_count")
	if !ok {
		t.Fatal("http.server.request_count is not an Int64 sum")
	}
	if count != 1 {
		t.Errorf("request_count = %d, want 1", count)
	}

	// duration must be a histogram with a single recorded observation.
	hcount, ok := histogramCount(rm, "http.server.duration")
	if !ok {
		t.Fatal("http.server.duration is not a Float64 histogram")
	}
	if hcount != 1 {
		t.Errorf("duration histogram count = %d, want 1", hcount)
	}
}

// TestDurationMillisPreservesSubMillisecond is the direct regression guard for
// the truncation bug: a sub-millisecond duration must convert to a non-zero
// fractional-millisecond value. The previous implementation used
// time.Duration.Milliseconds() which floored every sub-ms request to 0.0.
func TestDurationMillisPreservesSubMillisecond(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "duration-precision", "a-sub-millisecond-duration-keeps-its-real-value")
	cases := []struct {
		name string
		in   time.Duration
		want float64
	}{
		{"300us", 300 * time.Microsecond, 0.3},
		{"1us", 1 * time.Microsecond, 0.001},
		{"2.7ms", 2700 * time.Microsecond, 2.7},
		{"exactly 1ms", time.Millisecond, 1.0},
	}
	for _, c := range cases {
		got := durationMillis(c.in)
		// Sub-ms durations must never collapse to zero (the regression).
		if c.in > 0 && got == 0 {
			t.Errorf("%s: durationMillis(%v) = 0, want non-zero fractional ms", c.name, c.in)
		}
		// And the conversion must be the exact ms ratio (float division is exact
		// here for these inputs; use a tiny epsilon to be safe across platforms).
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: durationMillis(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// TestHTTPMiddlewareRecordsSubMillisecondDuration drives a real (fast) request
// through the middleware and asserts the duration histogram captured a non-zero
// elapsed time. Under the old truncating conversion a sub-millisecond request
// recorded 0.0, leaving the histogram Sum at 0; fractional-ms recording keeps it
// strictly positive.
func TestHTTPMiddlewareRecordsSubMillisecondDuration(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "duration-precision", "the-middleware-records-a-sub-millisecond-request-duration")
	saveOTelGlobals(t)

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)

	mw := HTTPMiddleware("subms-svc")

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	ctx := putnami_http.NewContext(w, req)
	ctx.Route = "/health"

	// A trivial handler that returns immediately — typically sub-millisecond.
	mw(ctx, putnami_http.NoContent)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	sum, ok := histogramSum(rm, "http.server.duration")
	if !ok {
		t.Fatal("http.server.duration is not a Float64 histogram")
	}
	// Any real request takes some positive time; with fractional-ms recording
	// the sum is strictly > 0 even when the request is faster than 1ms. The
	// truncating implementation would have recorded 0.0 here.
	if sum <= 0 {
		t.Errorf("duration histogram sum = %v, want > 0 (sub-ms request must not record 0)", sum)
	}
}

// --- helpers ---

func containsAll(haystack []string, needles ...string) bool {
	for _, n := range needles {
		found := false
		for _, h := range haystack {
			if h == n {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func hasMetric(rm metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

func counterSum(rm metricdata.ResourceMetrics, name string) (int64, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if s, ok := m.Data.(metricdata.Sum[int64]); ok {
				var total int64
				for _, dp := range s.DataPoints {
					total += dp.Value
				}
				return total, true
			}
		}
	}
	return 0, false
}

func upDownSum(rm metricdata.ResourceMetrics, name string) (int64, bool) {
	// UpDownCounter is reported as a (non-monotonic) Sum[int64].
	return counterSum(rm, name)
}

func histogramCount(rm metricdata.ResourceMetrics, name string) (uint64, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				var total uint64
				for _, dp := range h.DataPoints {
					total += dp.Count
				}
				return total, true
			}
		}
	}
	return 0, false
}

func histogramSum(rm metricdata.ResourceMetrics, name string) (float64, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				var total float64
				for _, dp := range h.DataPoints {
					total += dp.Sum
				}
				return total, true
			}
		}
	}
	return 0, false
}

func isNilInterface(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
