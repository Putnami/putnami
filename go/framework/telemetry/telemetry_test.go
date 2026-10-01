package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"go.putnami.dev/errors"
	putnami_http "go.putnami.dev/http"

	"go.putnami.dev/protocol/features/spectest"
)

// --- Tracer helpers ---

func TestTracerReturnsGlobalTracer(t *testing.T) {
	tracer := Tracer("test-service")
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
}

func TestStartSpanCreatesSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	ctx, span := StartSpan(context.Background(), "test-service", "test-op")
	span.End()

	if !span.SpanContext().IsValid() {
		t.Fatal("expected valid span context")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != "test-op" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "test-op")
	}

	_ = ctx
}

func TestSpanFromContextNoOp(t *testing.T) {
	span := SpanFromContext(context.Background())
	if span == nil {
		t.Fatal("SpanFromContext should return a no-op span, not nil")
	}
}

func TestSetSpanError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "error-op")
	SetSpanError(span, context.DeadlineExceeded)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", spans[0].Status.Code)
	}
}

func TestSetSpanOK(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "ok-op")
	SetSpanOK(span)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Ok {
		t.Errorf("span status = %v, want Ok", spans[0].Status.Code)
	}
}

func TestSetSpanErrorNil(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "nil-err-op")
	SetSpanError(span, nil) // should be a no-op
	span.End()

	spans := exporter.GetSpans()
	if spans[0].Status.Code == codes.Error {
		t.Error("SetSpanError(nil) should not set error status")
	}
}

func TestTraceIDFromContext(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	ctx, span := StartSpan(context.Background(), "test", "trace-id-op")
	defer span.End()

	traceID := TraceIDFromContext(ctx)
	if traceID == "" {
		t.Fatal("expected non-empty trace ID")
	}
	if len(traceID) != 32 {
		t.Errorf("trace ID length = %d, want 32 hex chars", len(traceID))
	}
}

func TestTraceIDFromContextEmpty(t *testing.T) {
	traceID := TraceIDFromContext(context.Background())
	if traceID != "" {
		t.Errorf("expected empty trace ID from background context, got %q", traceID)
	}
}

// --- Meter helpers ---

func TestMeterReturnsGlobalMeter(t *testing.T) {
	meter := Meter("test-service")
	if meter == nil {
		t.Fatal("Meter() returned nil")
	}
}

func TestCounterCreation(t *testing.T) {
	meter := Meter("test-counters")
	counter, err := Counter(meter, "test.requests_total")
	if err != nil {
		t.Fatalf("Counter() error: %v", err)
	}
	if counter == nil {
		t.Fatal("Counter() returned nil")
	}
	// Increment should not panic
	counter.Add(context.Background(), 1)
}

func TestHistogramCreation(t *testing.T) {
	meter := Meter("test-histograms")
	hist, err := Histogram(meter, "test.request_duration")
	if err != nil {
		t.Fatalf("Histogram() error: %v", err)
	}
	if hist == nil {
		t.Fatal("Histogram() returned nil")
	}
	hist.Record(context.Background(), 42.5)
}

func TestGaugeCreation(t *testing.T) {
	meter := Meter("test-gauges")
	gauge, err := Gauge(meter, "test.temperature")
	if err != nil {
		t.Fatalf("Gauge() error: %v", err)
	}
	if gauge == nil {
		t.Fatal("Gauge() returned nil")
	}
	gauge.Record(context.Background(), 72.5)
}

// --- Plugin lifecycle ---

func TestPluginConfigDefaults(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "explicit-sampling", "an-unset-sample-rate-samples-every-trace")
	cfg := Config{ServiceName: "test"}
	cfg = cfg.withDefaults()

	if cfg.TraceSampleRate == nil || *cfg.TraceSampleRate != 1.0 {
		t.Errorf("TraceSampleRate = %v, want 1.0 when unset", cfg.TraceSampleRate)
	}
	if cfg.ShutdownTimeout == 0 {
		t.Error("ShutdownTimeout should not be zero")
	}

	// The default is only real if the sampler the provider is built with honors
	// it: assert the exported spans, not just the resolved config field.
	if got := sampledSpans(t, Config{ServiceName: "test"}, sampleProbeSpans); got != sampleProbeSpans {
		t.Errorf("an unset sample rate exported %d/%d spans, want every one", got, sampleProbeSpans)
	}
}

// An explicit SampleRate(0) must be honored as "sample nothing", not
// silently overridden to full sampling.
func TestPluginConfigHonorsExplicitZeroSampleRate(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "explicit-sampling", "an-explicit-rate-of-zero-samples-none")
	cfg := Config{ServiceName: "test", TraceSampleRate: SampleRate(0)}.withDefaults()
	if cfg.TraceSampleRate == nil || *cfg.TraceSampleRate != 0 {
		t.Errorf("explicit SampleRate(0) = %v, want 0 (must not be overridden to 1.0)", cfg.TraceSampleRate)
	}

	// A non-zero explicit rate is preserved too.
	cfg = Config{ServiceName: "test", TraceSampleRate: SampleRate(0.25)}.withDefaults()
	if cfg.TraceSampleRate == nil || *cfg.TraceSampleRate != 0.25 {
		t.Errorf("explicit SampleRate(0.25) = %v, want 0.25", cfg.TraceSampleRate)
	}

	// "Samples none" is a claim about traces, not about a struct field: a
	// provider built with a sampler that ignores the rate would keep both
	// assertions above green while exporting every span.
	if got := sampledSpans(t, Config{ServiceName: "test", TraceSampleRate: SampleRate(0)}, sampleProbeSpans); got != 0 {
		t.Errorf("an explicit rate of zero exported %d/%d spans, want none", got, sampleProbeSpans)
	}
}

// sampleProbeSpans is large enough that a sampler which honors neither extreme
// would have to be improbably lucky to produce an all-or-nothing count.
const sampleProbeSpans = 40

// sampledSpans configures a plugin against an in-memory exporter, starts n
// root spans through the global tracer, and reports how many were exported —
// i.e. how many the configured sampler actually sampled.
func sampledSpans(t *testing.T, cfg Config, n int) int {
	t.Helper()
	saveOTelGlobals(t)

	exporter := tracetest.NewInMemoryExporter()
	cfg.TraceExporter = exporter
	p := NewPlugin(cfg)
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background(), nil) })

	tracer := otel.Tracer("sampling-probe")
	for i := range n {
		_, span := tracer.Start(context.Background(), fmt.Sprintf("op-%d", i))
		span.End()
	}
	if err := p.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}
	return len(exporter.GetSpans())
}

func TestPluginName(t *testing.T) {
	p := NewPlugin(Config{ServiceName: "test"})
	if p.Name() != "telemetry" {
		t.Errorf("Name() = %q, want %q", p.Name(), "telemetry")
	}
}

func TestPluginStopWithoutStart(t *testing.T) {
	p := NewPlugin(Config{ServiceName: "test"})
	// Stop without Configure should not panic
	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

// --- SpanAttrs ---

func TestSpanAttrsOddCount(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "attrs-op")
	SpanAttrs(span, "key1") // odd count — should be ignored
	span.End()

	spans := exporter.GetSpans()
	if len(spans[0].Attributes) != 0 {
		t.Error("SpanAttrs with odd keyValues should not set any attributes")
	}
}

func TestSpanAttrsEvenCount(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "attrs-op")
	SpanAttrs(span, "user.id", "123", "method", "GET")
	span.End()

	spans := exporter.GetSpans()
	if len(spans[0].Attributes) != 2 {
		t.Errorf("expected 2 attributes, got %d", len(spans[0].Attributes))
	}
}

// --- No-op provider integration ---

func TestTracerWithNoopProvider(t *testing.T) {
	otel.SetTracerProvider(noop.NewTracerProvider())

	ctx, span := StartSpan(context.Background(), "noop", "op")
	span.End()

	traceID := TraceIDFromContext(ctx)
	if traceID != "" {
		t.Error("noop provider should produce empty trace ID")
	}
}

// --- HTTPMiddleware ---

func TestHTTPMiddlewareCreatesSpan(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "span-always-ends", "the-span-ends-on-a-normal-response")
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	mw := HTTPMiddleware("test-svc")

	req := httptest.NewRequest("GET", "/users/123", nil)
	w := httptest.NewRecorder()
	ctx := putnami_http.NewContext(w, req)
	ctx.Route = "/users/{id}"

	resp := mw(ctx, func() *putnami_http.Response {
		return putnami_http.JSON(map[string]string{"ok": "true"})
	})

	if resp == nil {
		t.Fatal("expected response")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Name != "GET /users/{id}" {
		t.Errorf("span name = %q, want %q", spans[0].Name, "GET /users/{id}")
	}
	if spans[0].Status.Code != codes.Ok {
		t.Errorf("span status = %v, want Ok", spans[0].Status.Code)
	}
}

func TestHTTPMiddleware500SetsErrorStatus(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "span-always-ends", "the-span-ends-marked-errored-on-a-server-error-response")
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	mw := HTTPMiddleware("test-svc")

	req := httptest.NewRequest("POST", "/fail", nil)
	w := httptest.NewRecorder()
	ctx := putnami_http.NewContext(w, req)
	ctx.Route = "/fail"

	mw(ctx, func() *putnami_http.Response {
		return putnami_http.JSONStatus(500, map[string]string{"error": "boom"})
	})

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error for 500", spans[0].Status.Code)
	}
}

func TestHTTPMiddlewareW3CTracePropagation(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "trace-propagation", "an-inbound-w3c-traceparent-continues-the-callers-trace")
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	mw := HTTPMiddleware("test-svc")

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	w := httptest.NewRecorder()
	ctx := putnami_http.NewContext(w, req)
	ctx.Route = "/"

	mw(ctx, putnami_http.NoContent)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	parentTraceID := spans[0].SpanContext.TraceID().String()
	if parentTraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace ID = %q, want propagated trace ID", parentTraceID)
	}
}

func TestHTTPMiddlewareNilResponse(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "span-always-ends", "the-span-ends-on-a-nil-response")
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	mw := HTTPMiddleware("test-svc")

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	ctx := putnami_http.NewContext(w, req)
	ctx.Route = "/"

	resp := mw(ctx, func() *putnami_http.Response {
		return nil
	})

	if resp != nil {
		t.Error("expected nil response passthrough")
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	// nil response should default to status 200
	for _, attr := range spans[0].Attributes {
		if string(attr.Key) == "http.status_code" && attr.Value.AsInt64() != 200 {
			t.Errorf("expected status 200 for nil response, got %d", attr.Value.AsInt64())
		}
	}
}

// A downstream panic must still end the span, mark it errored, and
// re-propagate the panic — otherwise the span is orphaned in the batch
// processor and the request loses error attribution.
func TestHTTPMiddlewarePanicEndsSpanAndRepanics(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "span-always-ends", "a-downstream-panic-is-recorded-on-the-span-and-re-raised")
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	mw := HTTPMiddleware("test-svc")

	req := httptest.NewRequest("GET", "/boom", nil)
	w := httptest.NewRecorder()
	ctx := putnami_http.NewContext(w, req)
	ctx.Route = "/boom"

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		mw(ctx, func() *putnami_http.Response {
			panic("handler exploded")
		})
	}()

	// The panic must propagate past the middleware unchanged.
	if recovered == nil {
		t.Fatal("expected panic to propagate past HTTPMiddleware")
	}
	if recovered != "handler exploded" {
		t.Errorf("recovered panic = %v, want %q", recovered, "handler exploded")
	}

	// The span must have been ended (exported) despite the panic.
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 ended span after panic, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error after panic", spans[0].Status.Code)
	}

	// The span must carry the panic attribute and a 500 status code.
	attrs := map[string]string{}
	for _, attr := range spans[0].Attributes {
		attrs[string(attr.Key)] = attr.Value.Emit()
	}
	if attrs["http.panic"] != "true" {
		t.Errorf("http.panic attribute = %q, want %q", attrs["http.panic"], "true")
	}
	if attrs["http.status_code"] != "500" {
		t.Errorf("http.status_code attribute = %q, want %q", attrs["http.status_code"], "500")
	}
}

// --- Error Bridge ---

func TestSetSpanErrorStructuredWithStructuredError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "structured-err")
	err := errors.New(errors.CodeNotFound, "user not found").WithCategory(errors.CategoryUser)
	SetSpanErrorStructured(span, err)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", spans[0].Status.Code)
	}

	attrMap := make(map[string]string)
	for _, attr := range spans[0].Attributes {
		attrMap[string(attr.Key)] = attr.Value.Emit()
	}
	if attrMap["error.code"] != "not_found" {
		t.Errorf("error.code = %q, want %q", attrMap["error.code"], "not_found")
	}
	if attrMap["error.category"] != "user" {
		t.Errorf("error.category = %q, want %q", attrMap["error.category"], "user")
	}
}

func TestSetSpanErrorStructuredWithPlainError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "plain-err")
	SetSpanErrorStructured(span, context.DeadlineExceeded)
	span.End()

	spans := exporter.GetSpans()
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error", spans[0].Status.Code)
	}
	// Plain error should not have structured attributes
	for _, attr := range spans[0].Attributes {
		if string(attr.Key) == "error.code" {
			t.Error("plain error should not have error.code attribute")
		}
	}
}

func TestSetSpanErrorStructuredNil(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer tp.Shutdown(context.Background())
	otel.SetTracerProvider(tp)

	_, span := StartSpan(context.Background(), "test", "nil-structured")
	SetSpanErrorStructured(span, nil) // should be no-op
	span.End()

	spans := exporter.GetSpans()
	if spans[0].Status.Code == codes.Error {
		t.Error("nil error should not set error status")
	}
}

func TestInitErrorBridgeCountsErrors(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := mp.Meter("test")

	remove := initErrorBridge(meter)
	t.Cleanup(remove) // don't leak the hook into other tests

	// Create a structured error — triggers the hook
	_ = errors.New(errors.CodeInternal, "test error")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "errors_total" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected errors_total metric to be recorded")
	}
}

// --- ServiceName/ServiceVersion populate the OTel resource ---

func TestProvidesSetsServiceResource(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "service-identity", "a-configured-service-name-and-version-merge-onto-the-sdk-default-resource")
	saveOTelGlobals(t)

	exporter := tracetest.NewInMemoryExporter()
	p := NewPlugin(Config{
		ServiceName:    "checkout",
		ServiceVersion: "1.2.3",
		TraceExporter:  exporter,
	})
	if regs := p.Provides(); len(regs) == 0 {
		t.Fatal("Provides returned no registrations")
	}
	// The service resource is built when Configure constructs the providers.
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background(), nil) })

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	span.End()

	if err := p.tp.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := map[string]string{}
	for _, kv := range spans[0].Resource.Attributes() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got["service.name"] != "checkout" {
		t.Errorf("service.name = %q, want %q", got["service.name"], "checkout")
	}
	if got["service.version"] != "1.2.3" {
		t.Errorf("service.version = %q, want %q", got["service.version"], "1.2.3")
	}
}

func TestConfigResourceNilWhenUnset(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "service-identity", "leaving-name-and-version-unset-leaves-the-default-resource-untouched")
	// With neither field set, the provider's default resource must be left
	// untouched (no empty service.name clobbering unknown_service).
	if res := (Config{}).resource(); res != nil {
		t.Errorf("resource() = %v, want nil when ServiceName/ServiceVersion are empty", res)
	}
}

// --- the error bridge hook is removed on Stop ---

func TestInitErrorBridgeRemove(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	remove := initErrorBridge(mp.Meter("errors"))

	_ = errors.New(errors.CodeInternal, "one")
	if got := readErrorsTotal(t, reader); got != 1 {
		t.Fatalf("errors_total before remove = %d, want 1", got)
	}

	// After removal the bridge hook must stop firing into this counter.
	remove()
	_ = errors.New(errors.CodeInternal, "two")
	if got := readErrorsTotal(t, reader); got != 1 {
		t.Errorf("errors_total after remove = %d, want 1 (hook should be gone)", got)
	}
}

func TestPluginStopRemovesBridgeHook(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "lifecycle-purity", "the-error-count-bridge-hook-is-removed-on-stop")
	saveOTelGlobals(t)

	p := NewPlugin(Config{MetricReader: sdkmetric.NewManualReader()})

	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	if p.removeErrorBridge == nil {
		t.Fatal("Configure should have registered the error bridge and stored its remove handle")
	}

	if err := p.Stop(context.Background(), nil); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if p.removeErrorBridge != nil {
		t.Error("Stop should have deregistered the error bridge and cleared the remove handle")
	}
}

func readErrorsTotal(t *testing.T, reader sdkmetric.Reader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "errors_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("errors_total has unexpected data type %T", m.Data)
			}
			var total int64
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
			return total
		}
	}
	return 0
}

// Ensure unused imports don't cause compile errors.
var _ = http.StatusOK
