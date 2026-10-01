package consumer

// The observability family of the Go consumer cells. A real inbound request
// carrying W3C trace context reaches a consumer endpoint, which calls the real
// provider through the generated client — once over Connect, once over REST.
// The assertions read what the telemetry plugin actually exports, from its own
// in-memory exporter and manual reader, the way an OTLP pipeline receives it.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/telemetry"
)

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

// keptSpans keeps what the provider exported: the in-memory exporter forgets
// its spans on Shutdown, and stopping the plugin is what flushes them.
type keptSpans struct{ *tracetest.InMemoryExporter }

func (keptSpans) Shutdown(context.Context) error { return nil }

func TestGeneratedCallsContinueTheIncomingTraceAndAreMeasuredOnce(t *testing.T) {
	spectest.Proves(t, matrixFeature, "a-call-is-observable-and-ends-with-its-application", "a-generated-call-continues-the-inbound-trace-and-is-measured-once")
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

	providerURL, wire := realConnectProvider(t)
	generated := boundClient(t, sampleBinding(providerURL))

	// The consumer endpoint: a real HTTP route, traced by the framework
	// middleware, that calls the provider through the generated client with the
	// request's own context. It writes no header and no span of its own.
	consumerServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	consumerServer.Use(telemetry.HTTPMiddleware("catalog-consumer"))
	consumerServer.Handle(http.MethodGet, "/quote-and-items", func(ctx *phttp.Context) *phttp.Response {
		requestCtx := ctx.Request.Context()
		quote, err := generated.GetQuotes(requestCtx, itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		list, err := generated.ListItems(requestCtx, itemsclient.ListItemsInput{
			Query: itemsclient.ListItemsQuery{Search: "et", Limit: 10},
		})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		// Two streams on the same inbound request: the declared SSE feed and the
		// declared WebSocket one. A stream is one call whatever its duration, so
		// it is measured like the unary ones and traced under the same request.
		watch, err := WatchItem(requestCtx, generated, "1", false)
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		watched := 0
		for range watch.Messages() {
			watched++
		}
		if err := watch.Err(); err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		history, err := ReadItemHistory(requestCtx, generated, "1")
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		revisions := 0
		for range history.Messages() {
			revisions++
		}
		if err := history.Err(); err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return phttp.JSON(map[string]any{
			"quote": quote.Id, "items": len(*list.Items), "watched": watched, "revisions": revisions,
		})
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
		server := findSpan(t, exported, func(span tracetest.SpanStub) bool {
			return span.SpanKind != trace.SpanKindClient && strings.Contains(span.Name, "/quote-and-items")
		})
		if server.SpanContext.TraceID().String() != incomingTraceID || server.Parent.SpanID().String() != incomingSpanID {
			t.Fatalf("server span trace=%s parent=%s, want the incoming %s/%s",
				server.SpanContext.TraceID(), server.Parent.SpanID(), incomingTraceID, incomingSpanID)
		}
		for operation, path := range map[string]string{
			"getQuotes_Id": "/items.v1.ApiService/GetQuotes",
			"getItems":     "/items",
		} {
			call := findSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == operation })
			attempt := findSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == operation+" attempt" })
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
		want := map[string]string{
			"getQuotes_Id":        "connect",
			"getItems":            "rest-json",
			"getItems_Id_Watch":   "sse",
			"getItems_Id_History": "websocket",
		}
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
		forbidden := []string{service.CatalogAPIKey, service.WorkloadToken, "search=", "Widget", "Gadget"}
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

func findSpan(t *testing.T, spans tracetest.SpanStubs, match func(tracetest.SpanStub) bool) tracetest.SpanStub {
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

func receivedTraceparent(requests []connectWireRequest, path string) string {
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
