package consumer

// The Go→Go Connect unary protobuf cell. The bridge serves Connect JSON before
// protobuf; GET /quotes/{id}/snapshot declares its own encoding order,
// protobuf first, and nothing else about it differs from GET /quotes/{id}. The
// committed Go client therefore dispatches it over Connect protobuf with no
// consumer branch, which is what these tests read off the provider's socket.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/client"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/telemetry"
)

// snapshotMethod is the protobuf method identity the provider publishes for
// GET /quotes/{id}/snapshot; a Connect URL is the method.
const snapshotMethod = "/items.v1.ApiService/GetQuotesSnapshot"

func TestReadQuoteSnapshotTravelsOverTheDeclaredProtobufFirstOrder(t *testing.T) {
	baseURL, wire := realConnectProvider(t)
	generated := boundClient(t, sampleBinding(baseURL))

	t.Run("every value survives protobuf, the encoding the operation declares first", func(t *testing.T) {
		quote, err := generated.GetQuotesSnapshot(t.Context(), itemsclient.GetQuotesSnapshotInput{
			Path: itemsclient.GetQuotesSnapshotPath{Id: "1"},
		})
		if err != nil {
			t.Fatalf("GetQuotesSnapshot: %v", err)
		}
		if !reflect.DeepEqual(*quote, wantQuote) {
			t.Fatalf("quote = %+v, want %+v", *quote, wantQuote)
		}
		assertConnectCall(t, wire.snapshot(), snapshotMethod, "application/proto", true)
	})

	t.Run("the declared error arrives typed over protobuf", func(t *testing.T) {
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
	})
}

// A credential the provider refuses is refused over Connect protobuf too, and
// the call resolves no value.
func TestQuoteSnapshotRefusedCredentialReachesTheConsumerOverProtobuf(t *testing.T) {
	baseURL, wire := realConnectProvider(t)
	options := sampleBinding(baseURL)
	binding := options.Services["items"]
	binding.Credentials = map[string]client.CredentialBinding{
		"catalog-key": {Source: client.CredentialSourceStatic, Value: "not-the-catalog-key"},
	}
	options.Services["items"] = binding
	generated := boundClient(t, options)

	quote, err := generated.GetQuotesSnapshot(t.Context(), itemsclient.GetQuotesSnapshotInput{
		Path: itemsclient.GetQuotesSnapshotPath{Id: "1"},
	})
	var remote *client.RemoteError
	if quote != nil || !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refusal = %v, %T %v; want no quote and a 401 remote error", quote, err, err)
	}
	requests := wire.snapshot()
	if len(requests) != 1 || requests[0].Path != snapshotMethod || requests[0].ContentType != "application/proto" {
		t.Fatalf("the refused call reached the provider as %+v, want one protobuf request", requests)
	}
}

// A Connect protobuf call made inside a traced inbound request continues that
// trace, hands the provider its attempt span, and is measured once under the
// protocol that carried it, with no credential or payload in any export.
func TestAConnectProtobufCallContinuesTheTraceAndIsMeasuredOnce(t *testing.T) {
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

	server := findSpan(t, exported, func(span tracetest.SpanStub) bool {
		return span.SpanKind != trace.SpanKindClient && strings.Contains(span.Name, "/snapshot")
	})
	call := findSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == "getQuotes_Id_Snapshot" })
	attempt := findSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == "getQuotes_Id_Snapshot attempt" })
	if call.SpanContext.TraceID().String() != incomingTraceID || call.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Fatal("the call span is not a child of the inbound request span")
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
		attribute.String("rpc.service", "items"),
		attribute.String("rpc.method", "getQuotes_Id_Snapshot"),
		attribute.String("network.protocol.name", "connect"),
		attribute.Int("http.response.status_code", http.StatusOK),
	)
	if !points[0].Attributes.Equals(&wantLabels) {
		t.Fatalf("rpc.client.calls labels = %v, want exactly %v", points[0].Attributes.ToSlice(), wantLabels.ToSlice())
	}
	for _, span := range []tracetest.SpanStub{call, attempt} {
		for _, attr := range span.Attributes {
			for _, forbidden := range []string{service.CatalogAPIKey, "18446744073709551615"} {
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
