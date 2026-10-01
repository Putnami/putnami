package consumer

// The observability family of the Go→Go raw octet cell. A real inbound request
// carrying W3C trace context reaches a consumer endpoint, which echoes a raw
// octet payload through the generated client. The assertions read what the
// telemetry plugin actually exports, from its in-memory exporter and manual
// reader: the call is traced and measured like any other, and none of its
// octets reaches an export.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
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

	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/telemetry"
)

// clientSeriesKeys is the label vocabulary of every generated-client series:
// the call vocabulary, plus the attempt ordinal the attempt series carry, which
// the declared attempt cap bounds.
var clientSeriesKeys = append(slices.Clone(clientMetricKeys), "rpc.client.attempt")

// octetMarker is a payload no telemetry field could carry by accident:
// readable text after a run of octets no text or JSON pipeline passes through
// unchanged.
var octetMarker = append([]byte{0x00, 0xff, 0xfe, 0x80}, "octet-marker-3460"...)

func TestARawOctetCallIsTracedAndMeasuredWithoutItsOctets(t *testing.T) {
	spectest.Proves(t, matrixFeature, "a-call-is-observable-and-ends-with-its-application", "a-raw-octet-call-is-traced-and-measured-without-its-octets")
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
	consumerServer.Handle(http.MethodGet, "/echo", func(ctx *phttp.Context) *phttp.Response {
		echoed, err := generated.CreateBlobsEcho(ctx.Request.Context(), itemsclient.CreateBlobsEchoInput{
			Body: bytes.NewReader(octetMarker),
		})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return phttp.JSON(map[string]any{"same": bytes.Equal(echoed.Body, octetMarker)})
	})
	consumer := httptest.NewServer(consumerServer.Handler())
	t.Cleanup(consumer.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, consumer.URL+"/echo", nil)
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
			return span.SpanKind != trace.SpanKindClient && strings.Contains(span.Name, "/echo")
		})
		call := findSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == "postBlobs_Echo" })
		attempt := findSpan(t, exported, func(span tracetest.SpanStub) bool { return span.Name == "postBlobs_Echo attempt" })
		if call.SpanContext.TraceID().String() != incomingTraceID || call.Parent.SpanID() != server.SpanContext.SpanID() {
			t.Fatal("the call span is not a child of the inbound request span")
		}
		if attempt.Parent.SpanID() != call.SpanContext.SpanID() {
			t.Fatal("the attempt span is not a child of its call span")
		}
		want := fmt.Sprintf("00-%s-%s-01", incomingTraceID, attempt.SpanContext.SpanID())
		if got := receivedTraceparent(wire.snapshot(), "/blobs/echo"); got != want {
			t.Fatalf("the provider received traceparent %q, want the attempt span %q", got, want)
		}
	})

	t.Run("the call and its single attempt are counted once, under the protocol that carried it", func(t *testing.T) {
		points := int64SumPoints(collected, "rpc.client.calls")
		if len(points) != 1 || points[0].Value != 1 {
			t.Fatalf("rpc.client.calls = %v, want exactly one call", points)
		}
		want := attribute.NewSet(
			attribute.String("rpc.system", "putnami"),
			attribute.String("rpc.service", "items"),
			attribute.String("rpc.method", "postBlobs_Echo"),
			attribute.String("network.protocol.name", "rest-json"),
			attribute.Int("http.response.status_code", http.StatusOK),
		)
		if !points[0].Attributes.Equals(&want) {
			t.Fatalf("rpc.client.calls labels = %v, want exactly %v", points[0].Attributes.ToSlice(), want.ToSlice())
		}
		// One attempt: a raw octet body is read once from its source, so it is
		// sent once and measured once.
		attempts := int64SumPoints(collected, "rpc.client.attempts")
		if len(attempts) != 1 || attempts[0].Value != 1 {
			t.Fatalf("rpc.client.attempts = %v, want exactly one attempt", attempts)
		}
	})

	t.Run("no octet, rendering of the octets, or credential reaches an export", func(t *testing.T) {
		forbidden := []string{
			service.CatalogAPIKey, string(octetMarker), "octet-marker-3460",
			hex.EncodeToString(octetMarker), base64.StdEncoding.EncodeToString(octetMarker),
		}
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
			check("span "+span.Name, span.Attributes)
			if span.SpanKind == trace.SpanKindClient && len(span.Events) != 0 {
				t.Errorf("client span %s carries events %v", span.Name, span.Events)
			}
		}
		for _, attrs := range clientSeriesAttributes(collected) {
			check("client series", attrs)
			for _, attr := range attrs {
				if !slices.Contains(clientSeriesKeys, string(attr.Key)) {
					t.Errorf("a client series carries the unbounded label %s", attr.Key)
				}
			}
		}
	})
}

// clientSeriesAttributes returns the attribute set of every data point of every
// generated-client instrument, whatever its kind.
func clientSeriesAttributes(metrics metricdata.ResourceMetrics) [][]attribute.KeyValue {
	var sets [][]attribute.KeyValue
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if !strings.HasPrefix(instrument.Name, "rpc.client.") {
				continue
			}
			switch data := instrument.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					sets = append(sets, point.Attributes.ToSlice())
				}
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					sets = append(sets, point.Attributes.ToSlice())
				}
			case metricdata.Histogram[int64]:
				for _, point := range data.DataPoints {
					sets = append(sets, point.Attributes.ToSlice())
				}
			}
		}
	}
	return sets
}
