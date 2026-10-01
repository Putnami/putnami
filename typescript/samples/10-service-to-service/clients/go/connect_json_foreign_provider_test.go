package itemsclient_test

// TS→Go Connect unary JSON cell of the cross-language interop matrix. The TypeScript provider
// serves Connect protobuf before JSON; GET /quotes/[id]/snapshot declares its
// own encoding order, JSON first, and nothing else about it differs from
// GET /quotes/[id]. The committed Go client therefore dispatches this operation
// over Connect JSON with no consumer branch, which is what these tests read off
// the wire of a real provider subprocess (the D0.5 harness of
// foreign_provider_test.go).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/client"
	itemsclient "go.putnami.dev/examples/ts-items-client"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/telemetry"
)

// snapshotMethod is the protobuf method identity the provider publishes for
// GET /quotes/[id]/snapshot; a Connect URL is the method.
const snapshotMethod = "/catalog.items.v1.QuotesService/ListQuotesByIdSnapshot"

func TestTypeScriptProviderAnswersTheGeneratedGoClientOverConnectJSON(t *testing.T) {
	providerURL := startForeignProvider(t)
	wireURL, wire := recordConnectWire(t, providerURL)
	generated := boundClientWithKey(t, wireURL, catalogAPIKey)

	t.Run("the declared JSON-first order is the dispatched wire, and every value survives it", func(t *testing.T) {
		wire.reset()
		quote, err := generated.GetQuotesSnapshot(t.Context(), itemsclient.GetQuotesSnapshotInput{
			Path: itemsclient.GetQuotesSnapshotPath{Id: "1"},
		})
		if err != nil {
			t.Fatalf("GetQuotesSnapshot: %v", err)
		}
		// Connect JSON carries a 64-bit integer as a decimal string; 2^53 - 1 is
		// the widest a TypeScript provider holds exactly. The negative 32-bit
		// value is a JSON number, and the list keeps its order.
		if quote.Id != "1" || quote.Units != 9007199254740991 || quote.Offset != -7 || !slices.Equal(quote.Tags, []string{"a", "b"}) {
			t.Fatalf("quote = %+v", *quote)
		}
		wire.assertOneCall(t, snapshotMethod, "application/json", true)
	})

	t.Run("the declared error arrives typed with its declared details over JSON", func(t *testing.T) {
		wire.reset()
		_, err := generated.GetQuotesSnapshot(t.Context(), itemsclient.GetQuotesSnapshotInput{
			Path: itemsclient.GetQuotesSnapshotPath{Id: "absent"},
		})
		var typed *itemsclient.GetQuotesSnapshotNotFoundError
		if !errors.As(err, &typed) {
			t.Fatalf("error = %T %v, want the generated not_found type", err, err)
		}
		if typed.Remote.Code() != "not_found" || typed.Remote.StatusCode != http.StatusNotFound {
			t.Fatalf("error = %s/%d, want not_found/404", typed.Remote.Code(), typed.Remote.StatusCode)
		}
		want := itemsclient.GetQuotesSnapshotNotFoundErrorPayload{Id: "absent", Resource: "quote"}
		if typed.Payload == nil || *typed.Payload != want {
			t.Fatalf("details = %+v, want %+v", typed.Payload, want)
		}
		wire.assertOneCall(t, snapshotMethod, "application/json", true)
	})
}

// A Connect JSON call the provider refuses reaches this consumer as the refusal
// it is, with its status, and resolves no value.
func TestTypeScriptProviderConnectJSONRefusalReachesTheGoConsumer(t *testing.T) {
	refused := identityClient(t, client.ServicesOptions{
		ClientID: "cross-language-consumer",
		Services: map[string]client.ServiceBinding{serviceID: {
			URL:         startForeignProvider(t),
			Credentials: map[string]client.CredentialBinding{"catalog-key": {Source: client.CredentialSourceStatic, Value: "not-the-catalog-key"}},
		}},
	})
	quote, err := refused.GetQuotesSnapshot(t.Context(), itemsclient.GetQuotesSnapshotInput{
		Path: itemsclient.GetQuotesSnapshotPath{Id: "1"},
	})
	var remote *client.RemoteError
	if quote != nil || !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refusal = %v, %T %v; want no quote and a 401 remote error", quote, err, err)
	}
}

// A Connect JSON call made inside a traced inbound request continues that
// trace, hands the provider its attempt span, and is measured once under the
// protocol that carried it — with no credential or payload in any export.
func TestAConnectJSONCallToTheTypeScriptProviderContinuesTheTraceAndIsMeasuredOnce(t *testing.T) {
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

	wireURL, wire := recordTracedWire(t, startForeignProvider(t))
	generated := boundClientWithKey(t, wireURL, catalogAPIKey)

	consumerServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	consumerServer.Use(telemetry.HTTPMiddleware("catalog-consumer"))
	consumerServer.Handle(http.MethodGet, "/snapshot", func(ctx *phttp.Context) *phttp.Response {
		quote, err := generated.GetQuotesSnapshot(ctx.Request.Context(), itemsclient.GetQuotesSnapshotInput{
			Path: itemsclient.GetQuotesSnapshotPath{Id: "1"},
		})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return phttp.JSON(map[string]any{"quote": quote.Id})
	})
	consumer := httptest.NewServer(consumerServer.Handler())
	t.Cleanup(consumer.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, consumer.URL+"/snapshot", nil)
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

	server := findExportedSpan(t, exported, func(span tracetest.SpanStub) bool {
		return span.SpanKind != trace.SpanKindClient && strings.Contains(span.Name, "/snapshot")
	})
	call := findExportedSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == "getQuotes_idSnapshot" })
	attempt := findExportedSpan(t, exported, func(span tracetest.SpanStub) bool {
		return span.Name == "getQuotes_idSnapshot attempt"
	})
	if server.SpanContext.TraceID().String() != incomingTraceID || call.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Fatalf("call span trace=%s parent=%s, want a child of the inbound request span in trace %s",
			call.SpanContext.TraceID(), call.Parent.SpanID(), incomingTraceID)
	}
	if attempt.Parent.SpanID() != call.SpanContext.SpanID() {
		t.Fatal("the attempt span is not a child of its call span")
	}
	want := fmt.Sprintf("00-%s-%s-01", incomingTraceID, attempt.SpanContext.SpanID())
	if got := receivedTraceparent(wire.snapshot(), snapshotMethod); got != want {
		t.Fatalf("the provider received traceparent %q, want the attempt span %q", got, want)
	}

	points := int64SumPoints(collected, "rpc.client.calls")
	if len(points) != 1 || points[0].Value != 1 {
		t.Fatalf("rpc.client.calls = %v, want exactly one call", points)
	}
	wantLabels := attribute.NewSet(
		attribute.String("rpc.system", "putnami"),
		attribute.String("rpc.service", "catalog.items"),
		attribute.String("rpc.method", "getQuotes_idSnapshot"),
		attribute.String("network.protocol.name", "connect"),
		attribute.Int("http.response.status_code", http.StatusOK),
	)
	if !points[0].Attributes.Equals(&wantLabels) {
		t.Fatalf("rpc.client.calls labels = %v, want exactly %v", points[0].Attributes.ToSlice(), wantLabels.ToSlice())
	}
	for _, span := range []tracetest.SpanStub{call, attempt} {
		for _, attr := range span.Attributes {
			for _, forbidden := range []string{catalogAPIKey, "9007199254740991"} {
				if strings.Contains(attr.Value.Emit(), forbidden) {
					t.Errorf("span %s attribute %s carries %q", span.Name, attr.Key, forbidden)
				}
			}
		}
		if len(span.Events) != 0 {
			t.Errorf("client span %s carries events %v", span.Name, span.Events)
		}
	}
}
