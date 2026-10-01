package itemsclient_test

// Go→TS observability cell of the cross-language interop matrix: a real inbound request carrying
// W3C trace context reaches a Go consumer endpoint, which calls the real
// TypeScript provider through the generated client — once over Connect, once
// over REST JSON. The provider is the subprocess foreign_provider_test.go
// starts; only the consumer is instrumented, so every span and series read here
// belongs to the Go side of the boundary.
//
// The assertions read what the telemetry plugin actually exports, from its own
// in-memory exporter and manual reader, the way an OTLP pipeline receives it.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	itemsclient "go.putnami.dev/examples/ts-items-client"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/telemetry"
)

// The trace the consumer's inbound request carries, and the span that sent it.
const (
	incomingTraceID = "11111111111111111111111111111111"
	incomingSpanID  = "2222222222222222"
)

// clientMetricKeys is the whole label vocabulary a generated call may export.
// Every value in it is bounded by the contract: no identifier, query or payload.
var clientMetricKeys = []string{
	"rpc.system", "rpc.service", "rpc.method", "network.protocol.name",
	"http.response.status_code", "error.type",
}

// keptSpans keeps what the consumer exported: the in-memory exporter forgets
// its spans on Shutdown, and stopping the plugin is what flushes them.
type keptSpans struct{ *tracetest.InMemoryExporter }

func (keptSpans) Shutdown(context.Context) error { return nil }

func TestGeneratedCallsToTheTypeScriptProviderContinueTheTraceAndAreMeasuredOnce(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	metrics := sdkmetric.NewManualReader()
	plugin := telemetry.NewPlugin(telemetry.Config{
		ServiceName: "catalog-consumer", TraceExporter: keptSpans{spans}, MetricReader: metrics,
	})
	if err := plugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("configure telemetry: %v", err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		if err := plugin.Stop(context.Background(), nil); err != nil {
			t.Errorf("stop telemetry: %v", err)
		}
	}
	t.Cleanup(stop)

	// A recording proxy in front of the TypeScript provider shows the trace
	// context each generated call really put on the wire, so what the provider
	// received is read from the request rather than assumed from local spans.
	wireURL, wire := recordTracedWire(t, startForeignProvider(t))
	generated := boundClientWithKey(t, wireURL, catalogAPIKey)

	// The consumer endpoint: a real HTTP route, traced by the framework
	// middleware, that calls the foreign provider through the generated client
	// with the request's own context. It writes no header and no span of its own.
	consumerServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	consumerServer.Use(telemetry.HTTPMiddleware("catalog-consumer"))
	consumerServer.Handle(http.MethodGet, "/quote-and-items", func(ctx *phttp.Context) *phttp.Response {
		requestCtx := ctx.Request.Context()
		quote, err := generated.GetQuotes(requestCtx, itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		list, err := generated.ListItems(requestCtx, itemsclient.ListItemsInput{
			Query:  itemsclient.ListItemsQuery{Search: "et", Limit: 10},
			Header: itemsclient.ListItemsHeader{XCatalogTenant: sampleTenant},
		})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return phttp.JSON(map[string]any{"quote": quote.Id, "items": len(list.Items)})
	})
	consumer := httptest.NewServer(consumerServer.Handler())
	t.Cleanup(consumer.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, consumer.URL+"/quote-and-items", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-01", incomingTraceID, incomingSpanID))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("inbound request: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("consumer answered %d", response.StatusCode)
	}

	// Metrics first: stopping the plugin flushes the spans and ends the reader.
	var collected metricdata.ResourceMetrics
	if err := metrics.Collect(t.Context(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	stop()
	exported := spans.GetSpans()

	t.Run("the call and attempt spans continue the incoming trace, and the provider receives the attempt", func(t *testing.T) {
		server := findExportedSpan(t, exported, func(span tracetest.SpanStub) bool {
			return span.SpanKind != trace.SpanKindClient && strings.Contains(span.Name, "/quote-and-items")
		})
		if server.SpanContext.TraceID().String() != incomingTraceID || server.Parent.SpanID().String() != incomingSpanID {
			t.Fatalf("server span trace=%s parent=%s, want the incoming %s/%s",
				server.SpanContext.TraceID(), server.Parent.SpanID(), incomingTraceID, incomingSpanID)
		}
		for operation, path := range map[string]string{
			"getQuotes_id": "/catalog.items.v1.QuotesService/GetQuotesById",
			"getItems":     "/items",
		} {
			call := findExportedSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == operation })
			attempt := findExportedSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == operation+" attempt" })
			if call.Parent.SpanID() != server.SpanContext.SpanID() || call.SpanContext.TraceID().String() != incomingTraceID {
				t.Errorf("%s call span is not a child of the inbound request span", operation)
			}
			if attempt.Parent.SpanID() != call.SpanContext.SpanID() {
				t.Errorf("%s attempt span is not a child of its call span", operation)
			}
			want := fmt.Sprintf("00-%s-%s-01", incomingTraceID, attempt.SpanContext.SpanID())
			if got := receivedTraceparent(wire.snapshot(), path); got != want {
				t.Errorf("%s: the provider received traceparent %q, want the attempt span %q", operation, got, want)
			}
		}
	})

	t.Run("each call is counted once, under the protocol that carried it", func(t *testing.T) {
		points := int64SumPoints(collected, "rpc.client.calls")
		want := map[string]string{"getQuotes_id": "connect", "getItems": "rest-json"}
		if len(points) != len(want) {
			t.Fatalf("rpc.client.calls has %d series, want one per operation: %v", len(points), points)
		}
		for _, point := range points {
			method, _ := point.Attributes.Value("rpc.method")
			protocol, _ := point.Attributes.Value("network.protocol.name")
			if point.Value != 1 || want[method.AsString()] != protocol.AsString() {
				t.Errorf("series %s: value %d protocol %q, want 1 over %q",
					method.AsString(), point.Value, protocol.AsString(), want[method.AsString()])
			}
		}
	})

	t.Run("no secret, payload or query string reaches an export, and every label is bounded", func(t *testing.T) {
		// The api key the binding injected, the tenant the route carried, the
		// query string the client built, and the item names the provider
		// answered with: none of them is telemetry's to publish.
		forbidden := []string{catalogAPIKey, sampleTenant, "search=", "Widget", "Gadget"}
		check := func(where string, attrs []attribute.KeyValue) {
			for _, attr := range attrs {
				for _, value := range forbidden {
					if strings.Contains(attr.Value.Emit(), value) {
						t.Errorf("%s attribute %s carries %q", where, attr.Key, value)
					}
				}
			}
		}
		for _, span := range exported {
			if span.SpanKind != trace.SpanKindClient {
				continue
			}
			check("span "+span.Name, span.Attributes)
			if len(span.Events) != 0 {
				t.Errorf("client span %s carries events %v", span.Name, span.Events)
			}
		}
		for _, point := range int64SumPoints(collected, "rpc.client.calls") {
			check("rpc.client.calls", point.Attributes.ToSlice())
			for _, attr := range point.Attributes.ToSlice() {
				if !slices.Contains(clientMetricKeys, string(attr.Key)) {
					t.Errorf("rpc.client.calls carries the unbounded label %s", attr.Key)
				}
			}
		}
	})
}

func findExportedSpan(t *testing.T, spans tracetest.SpanStubs, match func(tracetest.SpanStub) bool) tracetest.SpanStub {
	t.Helper()
	for _, span := range spans {
		if match(span) {
			return span
		}
	}
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	t.Fatalf("no exported span matches; exported: %v", names)
	return tracetest.SpanStub{}
}

func receivedTraceparent(requests []tracedWireRequest, path string) string {
	for _, request := range requests {
		if request.Path == path {
			return request.Traceparent
		}
	}
	return ""
}

func int64SumPoints(metrics metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != name {
				continue
			}
			if sum, ok := instrument.Data.(metricdata.Sum[int64]); ok {
				return sum.DataPoints
			}
		}
	}
	return nil
}

// tracedWireRequest is the trace context one request carried to the provider.
// A credential value is never recorded.
type tracedWireRequest struct {
	Path        string
	Traceparent string
}

type tracedWire struct {
	mu       sync.Mutex
	requests []tracedWireRequest
}

func (wire *tracedWire) snapshot() []tracedWireRequest {
	wire.mu.Lock()
	defer wire.mu.Unlock()
	return slices.Clone(wire.requests)
}

// recordTracedWire puts a recording reverse proxy in front of the provider. It
// forwards every byte unchanged and logs the trace context each request carried.
func recordTracedWire(t *testing.T, providerURL string) (string, *tracedWire) {
	t.Helper()
	target, err := url.Parse(providerURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	wire := &tracedWire{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		wire.mu.Lock()
		wire.requests = append(wire.requests, tracedWireRequest{
			Path:        request.URL.Path,
			Traceparent: request.Header.Get("traceparent"),
		})
		wire.mu.Unlock()
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server.URL, wire
}
