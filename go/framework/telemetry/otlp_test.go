package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/logger"
	diag "go.putnami.dev/protocol/diagnostic"
	otlp "go.putnami.dev/protocol/telemetry"

	"go.putnami.dev/protocol/features/spectest"
)

// --- test collector ---------------------------------------------------------

type capturedReq struct {
	path  string
	body  []byte
	auth  string
	ctype string
	hdr   string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type testCollector struct {
	mu     sync.Mutex
	reqs   []capturedReq
	status int // response code; 0 → 200
}

func (c *testCollector) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.reqs = append(c.reqs, capturedReq{
			path:  r.URL.Path,
			body:  body,
			auth:  r.Header.Get("Authorization"),
			ctype: r.Header.Get("Content-Type"),
			hdr:   r.Header.Get("X-Tenant"),
		})
		status := c.status
		c.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (c *testCollector) only(t *testing.T) capturedReq {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) != 1 {
		t.Fatalf("want exactly 1 collector request, got %d", len(c.reqs))
	}
	return c.reqs[0]
}

func (c *testCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

func testResource() *resource.Resource {
	return resource.NewSchemaless(
		attribute.String("service.name", "checkout"),
		attribute.String("service.version", "1.4.2"),
	)
}

func TestOTLPClient_UsesInjectedHTTPClient(t *testing.T) {
	col := &testCollector{}
	srv := col.server(t)

	var calls int
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		req.Header.Set("Authorization", fmt.Sprintf("Bearer refreshed-%d", calls))
		return http.DefaultTransport.RoundTrip(req)
	})}
	client := newOTLPClient(OTLPConfig{Endpoint: srv.URL, HTTPClient: httpClient})
	if client.http != httpClient {
		t.Fatal("newOTLPClient did not preserve the injected HTTP client")
	}

	for range 2 {
		if err := client.post(context.Background(), otlp.PathMetrics, []byte(`{}`)); err != nil {
			t.Fatalf("post: %v", err)
		}
	}

	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.reqs) != 2 {
		t.Fatalf("want 2 collector requests, got %d", len(col.reqs))
	}
	for i, req := range col.reqs {
		want := fmt.Sprintf("Bearer refreshed-%d", i+1)
		if req.auth != want {
			t.Errorf("request %d auth = %q, want %q", i+1, req.auth, want)
		}
	}
}

func TestOTLPClient_DefaultsHTTPTimeout(t *testing.T) {
	client := newOTLPClient(OTLPConfig{Endpoint: "https://collector.example"})
	if want := time.Duration(otlp.DefaultContract().Export.DefaultTimeoutMS) * time.Millisecond; client.http.Timeout != want {
		t.Errorf("default timeout = %v, want %v", client.http.Timeout, want)
	}

	client = newOTLPClient(OTLPConfig{Endpoint: "https://collector.example", Timeout: 2 * time.Second})
	if client.http.Timeout != 2*time.Second {
		t.Errorf("configured timeout = %v, want 2s", client.http.Timeout)
	}
}

// --- metrics ----------------------------------------------------------------

func TestOTLPMetricExporter_Export(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "push-transport", "the-metric-exporter-posts-otlp-json-to-the-collectors-metrics-path")
	col := &testCollector{}
	srv := col.server(t)
	ctx := context.Background()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(testResource()))
	meter := mp.Meter("test")
	ctr, _ := meter.Int64Counter("http.requests")
	ctr.Add(ctx, 5, metric.WithAttributes(attribute.String("method", "GET")))
	g, _ := meter.Float64Gauge("queue.size")
	g.Record(ctx, 3)
	h, _ := meter.Float64Histogram("http.duration")
	h.Record(ctx, 12)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}

	exp := newOTLPMetricExporter(OTLPConfig{Endpoint: srv.URL, BearerToken: "secret", Headers: map[string]string{"X-Tenant": "acme"}})
	if err := exp.Export(ctx, &rm); err != nil {
		t.Fatalf("Export: %v", err)
	}

	req := col.only(t)
	if req.path != "/v1/metrics" {
		t.Errorf("path = %q, want /v1/metrics", req.path)
	}
	if req.auth != "Bearer secret" {
		t.Errorf("auth = %q, want Bearer secret", req.auth)
	}
	if req.ctype != "application/json" {
		t.Errorf("content-type = %q", req.ctype)
	}
	if req.hdr != "acme" {
		t.Errorf("custom header = %q, want acme", req.hdr)
	}

	parsed, diags := otlp.ParseAndValidateMetrics(req.body)
	if diag.HasErrors(diags) {
		t.Fatalf("collector body is not valid OTLP metrics: %v\n%s", diags, req.body)
	}
	names := metricNames(parsed)
	for _, want := range []string{"http.requests", "queue.size", "http.duration"} {
		if !names[want] {
			t.Errorf("metric %q missing from export; got %v", want, names)
		}
	}
	// Resource must carry the framework marker and the service identity.
	res := parsed.ResourceMetrics[0].Resource
	if !hasAttr(res.Attributes, otlp.AttrPutnamiFramework, "go") {
		t.Errorf("resource missing putnami.framework=go: %+v", res)
	}
	if !hasAttr(res.Attributes, "service.name", "checkout") {
		t.Errorf("resource missing service.name=checkout: %+v", res)
	}
}

func TestOTLPMetricReader_FinalFlushOnShutdown(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "final-flush", "the-metric-reader-flushes-once-more-during-provider-shutdown")
	col := &testCollector{}
	srv := col.server(t)
	ctx := context.Background()

	reader := NewOTLPMetricReader(OTLPConfig{Endpoint: srv.URL, FlushInterval: time.Hour})
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(testResource()))
	ctr, _ := mp.Meter("test").Int64Counter("jobs.done")
	ctr.Add(ctx, 1)

	// A long flush interval means nothing is pushed until shutdown drives the
	// final flush.
	if got := col.count(); got != 0 {
		t.Fatalf("expected no pushes before shutdown, got %d", got)
	}
	if err := mp.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := col.count(); got == 0 {
		t.Fatal("expected final flush on shutdown, got no collector request")
	}
	if p := col.reqs[0].path; p != "/v1/metrics" {
		t.Errorf("final flush path = %q, want /v1/metrics", p)
	}
}

func TestOTLPMetricExporter_DropsOnCollectorError(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "never-fails-the-workload", "a-collector-error-is-dropped-rather-than-propagated")
	col := &testCollector{status: http.StatusInternalServerError}
	srv := col.server(t)
	ctx := context.Background()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	ctr, _ := mp.Meter("t").Int64Counter("c")
	ctr.Add(ctx, 1)
	var rm metricdata.ResourceMetrics
	_ = reader.Collect(ctx, &rm)

	exp := newOTLPMetricExporter(OTLPConfig{Endpoint: srv.URL})
	// A 5xx must be swallowed: Export returns nil so the SDK does not spam.
	if err := exp.Export(ctx, &rm); err != nil {
		t.Fatalf("Export should swallow collector errors, got %v", err)
	}
}

func TestOTLPTemporalityMapping(t *testing.T) {
	// SDK Cumulative=1 must map to OTLP Cumulative=2, not a raw cast.
	if got := otlpTemporality(metricdata.CumulativeTemporality); got != otlp.TemporalityCumulative {
		t.Errorf("cumulative → %d, want %d", got, otlp.TemporalityCumulative)
	}
	if got := otlpTemporality(metricdata.DeltaTemporality); got != otlp.TemporalityDelta {
		t.Errorf("delta → %d, want %d", got, otlp.TemporalityDelta)
	}
}

// --- traces -----------------------------------------------------------------

func TestOTLPSpanExporter_Export(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "push-transport", "the-span-exporter-posts-otlp-json-to-the-collectors-traces-path")
	col := &testCollector{}
	srv := col.server(t)
	ctx := context.Background()

	exp := newOTLPSpanExporter(OTLPConfig{Endpoint: srv.URL})
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithResource(testResource()))
	_, span := tp.Tracer("test").Start(ctx, "GET /checkout", trace.WithSpanKind(trace.SpanKindServer))
	span.SetAttributes(attribute.String("http.method", "GET"), attribute.Int("http.status_code", 200))
	span.SetStatus(codes.Error, "boom")
	span.End()

	req := col.only(t)
	if req.path != "/v1/traces" {
		t.Errorf("path = %q, want /v1/traces", req.path)
	}
	parsed, diags := otlp.ParseAndValidateTraces(req.body)
	if diag.HasErrors(diags) {
		t.Fatalf("collector body is not valid OTLP traces: %v\n%s", diags, req.body)
	}
	sp := parsed.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if sp.Name != "GET /checkout" {
		t.Errorf("span name = %q", sp.Name)
	}
	if sp.Kind != otlp.SpanKindServer {
		t.Errorf("span kind = %d, want %d (server)", sp.Kind, otlp.SpanKindServer)
	}
	// codes.Error (OTel=1) must map to OTLP Error (2), not a raw cast.
	if sp.Status == nil || sp.Status.Code != otlp.StatusError {
		t.Errorf("span status = %+v, want code %d (error)", sp.Status, otlp.StatusError)
	}
	if len(sp.TraceID) != 32 || len(sp.SpanID) != 16 {
		t.Errorf("ids: trace=%q span=%q want 32/16 hex", sp.TraceID, sp.SpanID)
	}
}

func TestOTLPStatusCodeMapping(t *testing.T) {
	if got := otlpStatusCode(codes.Ok); got != otlp.StatusOK {
		t.Errorf("codes.Ok → %d, want %d", got, otlp.StatusOK)
	}
	if got := otlpStatusCode(codes.Error); got != otlp.StatusError {
		t.Errorf("codes.Error → %d, want %d", got, otlp.StatusError)
	}
	if got := otlpStatusCode(codes.Unset); got != otlp.StatusUnset {
		t.Errorf("codes.Unset → %d, want %d", got, otlp.StatusUnset)
	}
}

// --- logs -------------------------------------------------------------------

func TestOTLPLogSink_Export(t *testing.T) {
	col := &testCollector{}
	srv := col.server(t)

	sink := NewOTLPLogSink(OTLPConfig{Endpoint: srv.URL, ServiceName: "checkout", ServiceVersion: "1.4.2", FlushInterval: time.Hour})
	sink.Write(logger.LogEntry{
		Level:     logger.LevelInfo,
		Message:   "request handled",
		Timestamp: time.Unix(1700000000, 0),
		Logger:    "checkout.http",
		Context:   map[string]any{"route": "/checkout"},
		TraceID:   "5b8efff798038103d269b633813fc60c",
	})
	sink.Write(logger.LogEntry{
		Level:     logger.LevelError,
		Message:   "payment failed",
		Timestamp: time.Unix(1700000001, 0),
		Error:     &logger.ErrorInfo{Name: "PaymentError", Message: "declined", Code: "card_declined"},
	})
	if err := sink.Close(); err != nil { // final flush
		t.Fatalf("Close: %v", err)
	}

	req := col.only(t)
	if req.path != "/v1/logs" {
		t.Errorf("path = %q, want /v1/logs", req.path)
	}
	parsed, diags := otlp.ParseAndValidateLogs(req.body)
	if diag.HasErrors(diags) {
		t.Fatalf("collector body is not valid OTLP logs: %v\n%s", diags, req.body)
	}
	recs := parsed.ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(recs) != 2 {
		t.Fatalf("want 2 log records, got %d", len(recs))
	}
	if recs[0].SeverityNumber != otlp.SeverityInfo || recs[0].SeverityText != "info" {
		t.Errorf("record 0 severity = %d/%q", recs[0].SeverityNumber, recs[0].SeverityText)
	}
	if recs[0].TraceID != "5b8efff798038103d269b633813fc60c" {
		t.Errorf("record 0 traceId = %q", recs[0].TraceID)
	}
	if recs[1].SeverityNumber != otlp.SeverityError {
		t.Errorf("record 1 severity = %d, want error", recs[1].SeverityNumber)
	}
	if !hasAttr(recs[1].Attributes, "error.code", "card_declined") {
		t.Errorf("record 1 missing error.code attr: %+v", recs[1].Attributes)
	}
	res := parsed.ResourceLogs[0].Resource
	if !hasAttr(res.Attributes, otlp.AttrPutnamiFramework, "go") {
		t.Errorf("log resource missing putnami.framework=go")
	}
}

// TestOTLPLogSink_FlattensNestedContext proves a nested plain-map context value
// arrives as dotted attributes (http.method, event.publishCount) rather than a
// Go-syntax map[…] string, and that arrays are JSON-stringified.
func TestOTLPLogSink_FlattensNestedContext(t *testing.T) {
	rec := convLogRecord(logger.LogEntry{
		Level:   logger.LevelInfo,
		Message: "request handled",
		Context: map[string]any{
			"http":  map[string]any{"method": "GET", "statusCode": 200},
			"event": map[string]any{"publishes": []any{"orders", "carts"}, "publishCount": 2},
		},
	})

	if !hasAttr(rec.Attributes, "http.method", "GET") {
		t.Errorf("missing flattened http.method=GET: %+v", rec.Attributes)
	}
	if v, ok := intStrAttr(rec.Attributes, "http.statusCode"); !ok || v != "200" {
		t.Errorf("http.statusCode = %q ok=%v, want 200", v, ok)
	}
	if v, ok := intStrAttr(rec.Attributes, "event.publishCount"); !ok || v != "2" {
		t.Errorf("event.publishCount = %q ok=%v, want 2", v, ok)
	}
	// Arrays are JSON-stringified, not flattened into per-index keys.
	if !hasAttr(rec.Attributes, "event.publishes", `["orders","carts"]`) {
		t.Errorf("event.publishes not JSON-stringified: %+v", rec.Attributes)
	}
	// No Go-syntax map[…] leaked as a value.
	for _, kv := range rec.Attributes {
		if kv.Value.StringValue != nil && strings.HasPrefix(*kv.Value.StringValue, "map[") {
			t.Errorf("attr %q leaked a Go-syntax map string: %q", kv.Key, *kv.Value.StringValue)
		}
	}
}

// TestOTLPLogSink_FlattensStructuredAttrs verifies the contract-shaped slog
// attrs used by HTTP, events, database, and migrations remain queryable in
// OTLP instead of becoming opaque slog.Value strings.
func TestOTLPLogSink_FlattensStructuredAttrs(t *testing.T) {
	rec := convLogRecord(logger.LogEntry{
		Level:   logger.LevelWarn,
		Message: "query failed",
		Attrs: []slog.Attr{
			slog.Any("database", map[string]any{"operation": "query", "outcome": "failure"}),
			slog.Group("event", slog.String("topic", "orders"), slog.Int("attempt", 2)),
			logger.ErrorAttr(fmt.Errorf("database unavailable")),
		},
	})

	if !hasAttr(rec.Attributes, "database.operation", "query") {
		t.Errorf("missing flattened database.operation=query: %+v", rec.Attributes)
	}
	if !hasAttr(rec.Attributes, "database.outcome", "failure") {
		t.Errorf("missing flattened database.outcome=failure: %+v", rec.Attributes)
	}
	if !hasAttr(rec.Attributes, "event.topic", "orders") {
		t.Errorf("missing flattened event.topic=orders: %+v", rec.Attributes)
	}
	if v, ok := intStrAttr(rec.Attributes, "event.attempt"); !ok || v != "2" {
		t.Errorf("event.attempt = %q ok=%v, want 2", v, ok)
	}
	if !hasAttr(rec.Attributes, "error.message", "database unavailable") {
		t.Errorf("missing semantic error.message: %+v", rec.Attributes)
	}
}

func intStrAttr(attrs []otlp.KeyValue, key string) (string, bool) {
	for _, kv := range attrs {
		if kv.Key == key && kv.Value.IntValue != nil {
			return *kv.Value.IntValue, true
		}
	}
	return "", false
}

func TestOTLPLogSink_InjectedHTTPClientWithoutTimeout(t *testing.T) {
	col := &testCollector{}
	srv := col.server(t)

	sink := NewOTLPLogSink(OTLPConfig{
		Endpoint:      srv.URL,
		HTTPClient:    &http.Client{},
		FlushInterval: time.Hour,
	})
	sink.Write(logger.LogEntry{Level: logger.LevelInfo, Message: "authenticated request"})
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := col.count(); got != 1 {
		t.Fatalf("collector requests = %d, want 1", got)
	}
}

// TestOTLPLogSink_ConcurrentWriteAndFlush hammers Write from many goroutines
// while the background flushLoop ticker and an explicit Flush loop drain the
// same buffer, then Close performs the final flush. It is meant to be run under
// `go test -race`: the race detector verifies the mutex/WaitGroup/done
// synchronization is sound, and the union assertion verifies every written
// record is flushed exactly once (no loss, no duplication, no corruption).
//
// The total record count is kept well under the bounded queue's MaxQueueRecords
// (10000) so the oldest-record drop path never triggers, which makes the
// exact-set assertion deterministic.
func TestOTLPLogSink_ConcurrentWriteAndFlush(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "never-fails-the-workload", "the-log-sink-batches-records-onto-a-background-flush")
	col := &testCollector{}
	srv := col.server(t)

	const (
		writers      = 24
		perWriter    = 40 // 24*40 = 960 records, far below MaxQueueRecords (10000)
		totalRecords = writers * perWriter
	)

	// A short flush interval makes the background ticker fire repeatedly while the
	// writers are still appending, maximizing Write/Flush interleaving.
	sink := NewOTLPLogSink(OTLPConfig{
		Endpoint:       srv.URL,
		ServiceName:    "checkout",
		ServiceVersion: "1.4.2",
		FlushInterval:  time.Millisecond,
	})

	// id encodes (writer, index) so every record is unique and identifiable in the
	// captured collector batches.
	id := func(w, i int) string { return fmt.Sprintf("w%d-i%d", w, i) }

	// A concurrent explicit-Flush loop runs alongside the writers and the
	// background ticker, drained on stopFlush after the writers finish.
	stopFlush := make(chan struct{})
	var flushWG sync.WaitGroup
	flushWG.Add(1)
	go func() {
		defer flushWG.Done()
		for {
			select {
			case <-stopFlush:
				return
			default:
				_ = sink.Flush()
			}
		}
	}()

	var writeWG sync.WaitGroup
	writeWG.Add(writers)
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer writeWG.Done()
			for i := 0; i < perWriter; i++ {
				sink.Write(logger.LogEntry{
					Level:     logger.LevelInfo,
					Message:   "concurrent",
					Timestamp: time.Unix(1700000000, int64(i)),
					Context:   map[string]any{"rid": id(w, i)},
				})
			}
		}(w)
	}

	writeWG.Wait()
	close(stopFlush)
	flushWG.Wait()

	// Final flush + background-loop shutdown; must be race-free with everything
	// above already joined, but Close itself also drains any tail records.
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Collect every "rid" value across every captured batch. The union must equal
	// the full set of written ids: nothing dropped, nothing duplicated.
	seen := make(map[string]int, totalRecords)
	col.mu.Lock()
	batches := col.reqs
	col.mu.Unlock()
	if len(batches) == 0 {
		t.Fatal("collector received no batches; expected at least the final flush")
	}
	for _, b := range batches {
		if b.path != "/v1/logs" {
			t.Errorf("batch path = %q, want /v1/logs", b.path)
		}
		parsed, diags := otlp.ParseAndValidateLogs(b.body)
		if diag.HasErrors(diags) {
			t.Fatalf("collector body is not valid OTLP logs: %v\n%s", diags, b.body)
		}
		for _, rl := range parsed.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, rec := range sl.LogRecords {
					rid, ok := stringAttr(rec.Attributes, "rid")
					if !ok {
						t.Fatalf("log record missing rid attr: %+v", rec.Attributes)
					}
					seen[rid]++
				}
			}
		}
	}

	if len(seen) != totalRecords {
		t.Errorf("flushed %d distinct records, want %d (records were lost or duplicated)", len(seen), totalRecords)
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			rid := id(w, i)
			switch seen[rid] {
			case 1:
				// exactly once — correct
			case 0:
				t.Errorf("record %s was never flushed (lost)", rid)
			default:
				t.Errorf("record %s flushed %d times (duplicated)", rid, seen[rid])
			}
		}
	}
}

// stringAttr returns the string value of a named attribute, or false if absent
// or not a string.
func stringAttr(attrs []otlp.KeyValue, key string) (string, bool) {
	for _, kv := range attrs {
		if kv.Key == key && kv.Value.StringValue != nil {
			return *kv.Value.StringValue, true
		}
	}
	return "", false
}

func TestOTLPSeverityMapping(t *testing.T) {
	cases := []struct {
		level logger.Level
		num   otlp.SeverityNumber
		text  string
	}{
		{logger.LevelDebug, otlp.SeverityDebug, "debug"},
		{logger.LevelInfo, otlp.SeverityInfo, "info"},
		{logger.LevelWarn, otlp.SeverityWarn, "warn"},
		{logger.LevelError, otlp.SeverityError, "error"},
	}
	for _, c := range cases {
		n, txt := otlpSeverity(c.level)
		if n != c.num || txt != c.text {
			t.Errorf("level %v → %d/%q, want %d/%q", c.level, n, txt, c.num, c.text)
		}
	}
}

// --- plugin wiring ----------------------------------------------------------

func TestPlugin_OTLPConfigWiresExporters(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "push-transport", "an-otlp-configuration-wires-the-metric-reader-and-the-span-exporter")
	saveOTelGlobals(t)
	p := NewPlugin(Config{
		ServiceName: "checkout",
		OTLP:        &OTLPConfig{Endpoint: "https://collector.example:4318"},
	})
	// OTLP exporters are auto-wired during the configure phase, not in Provides.
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	if p.cfg.MetricReader == nil {
		t.Error("OTLP config did not wire a MetricReader")
	}
	if p.cfg.TraceExporter == nil {
		t.Error("OTLP config did not wire a TraceExporter")
	}
	_ = p.Stop(context.Background(), nil)
}

func TestPlugin_OTLPDoesNotOverrideExplicit(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "push-transport", "an-explicitly-supplied-reader-or-exporter-is-not-replaced-by-auto-wiring")
	saveOTelGlobals(t)
	explicit := sdkmetric.NewManualReader()
	p := NewPlugin(Config{
		ServiceName:  "checkout",
		MetricReader: explicit,
		OTLP:         &OTLPConfig{Endpoint: "https://collector.example:4318"},
	})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("Configure() error: %v", err)
	}
	if p.cfg.MetricReader != explicit {
		t.Error("OTLP config overrode an explicitly supplied MetricReader")
	}
	_ = p.Stop(context.Background(), nil)
}

// --- helpers ----------------------------------------------------------------

func metricNames(req *otlp.MetricsRequest) map[string]bool {
	names := map[string]bool{}
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				names[m.Name] = true
			}
		}
	}
	return names
}

func hasAttr(attrs []otlp.KeyValue, key, val string) bool {
	for _, kv := range attrs {
		if kv.Key == key && kv.Value.StringValue != nil && *kv.Value.StringValue == val {
			return true
		}
	}
	return false
}
