package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"go.putnami.dev/client"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func TestPluginAutomaticallyObservesAndPropagatesGeneratedClientCalls(t *testing.T) {
	saveOTelGlobals(t)

	spanExporter := tracetest.NewInMemoryExporter()
	metricReader := sdkmetric.NewManualReader()
	plugin := NewPlugin(Config{
		ServiceName:   "consumer",
		TraceExporter: spanExporter,
		MetricReader:  metricReader,
	})
	if err := plugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure telemetry: %v", err)
	}
	t.Cleanup(func() { _ = plugin.Stop(context.Background(), nil) })

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("traceparent") == "" {
			t.Error("generated call omitted W3C trace context")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()

	descriptor := client.ServiceDescriptor{Contract: clientcontract.DocumentV1{
		ProtocolVersion: clientcontract.ProtocolVersion,
		Service:         clientcontract.Service{ID: "inventory", Audience: "urn:inventory"},
		Credentials:     map[string]clientcontract.CredentialProfile{},
	}}
	bound, err := client.NewServiceClientBinding(client.ServiceBinding{
		URL:      server.URL,
		ClientID: "consumer.workload",
	}, descriptor)
	if err != nil {
		t.Fatalf("bind generated client: %v", err)
	}
	additionalProperties := false
	operation := client.Operation{
		ID: "getItem",
		Contract: clientcontract.OperationV1{
			Stream: clientcontract.StreamUnary,
			Transports: []clientcontract.Transport{{
				Protocol: clientcontract.TransportRESTJSON,
				Path:     "/items",
				Encoding: clientcontract.EncodingJSON,
			}},
			Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
			Errors:      []clientcontract.DeclaredError{},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
		},
		Successes: []client.OperationSuccess{{
			Status: http.StatusOK,
			Content: []client.OperationContent{{
				MediaType: "application/json",
				Schema: &clientcontract.Schema{
					Type: "object",
					Properties: map[string]clientcontract.Schema{
						"value": {Type: "string"},
					},
					Required: []string{"value"},
					AdditionalProperties: &clientcontract.AdditionalProperties{
						Allowed: &additionalProperties,
					},
				},
			}},
		}},
	}
	for range 2 {
		output, err := client.Call[struct {
			Value string `json:"value"`
		}](t.Context(), bound, &client.Request{Method: http.MethodGet, Path: "/items"}, operation)
		if err != nil || output.Value != "ok" {
			t.Fatalf("generated call output = %#v, %v", output, err)
		}
	}

	if err := plugin.tp.ForceFlush(t.Context()); err != nil {
		t.Fatalf("flush generated client spans: %v", err)
	}
	spans := spanExporter.GetSpans()
	if len(spans) != 4 {
		t.Fatalf("generated client spans = %d, want call and attempt for each call", len(spans))
	}
	var metrics metricdata.ResourceMetrics
	if err := metricReader.Collect(t.Context(), &metrics); err != nil {
		t.Fatalf("collect generated client metrics: %v", err)
	}
	for _, name := range []string{"rpc.client.calls", "rpc.client.attempts", "rpc.client.duration", "rpc.client.attempt.duration", "rpc.client.auth.duration"} {
		if !hasMetric(metrics, name) {
			t.Errorf("generated client metric %q missing", name)
		}
	}
	if series, count := floatHistogramSeries(metrics, "rpc.client.auth.duration"); series != 1 || count != 2 {
		t.Errorf("auth duration histogram series=%d count=%d, want one bounded series containing both calls", series, count)
	}
}

func floatHistogramSeries(metrics metricdata.ResourceMetrics, name string) (int, uint64) {
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != name {
				continue
			}
			histogram, ok := instrument.Data.(metricdata.Histogram[float64])
			if !ok {
				return 0, 0
			}
			var count uint64
			for _, point := range histogram.DataPoints {
				count += point.Count
			}
			return len(histogram.DataPoints), count
		}
	}
	return 0, 0
}

func TestTheBridgeCountsAStaleCachedAnswerOnTheCallIdentity(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "generated-client-telemetry", "a-stale-cached-answer-is-counted-on-the-call-identity")
	reader := sdkmetric.NewManualReader()
	bridge, err := newGeneratedClientTelemetry(noop.NewTracerProvider(), sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), propagation.TraceContext{})
	if err != nil {
		t.Fatal(err)
	}
	var observer client.ServiceCallTelemetry = bridge
	cache, ok := observer.(client.ServiceCacheTelemetry)
	if !ok {
		t.Fatal("the generated-client bridge does not observe the response cache")
	}
	info := client.ServiceCallInfo{ServiceID: "identity", OperationID: "effectiveAccess", Protocol: "rest-json"}
	cache.ServiceCacheStaleServed(t.Context(), info, 42*time.Second)
	cache.ServiceCacheStaleServed(t.Context(), info, time.Second)

	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &metrics); err != nil {
		t.Fatal(err)
	}
	for _, scope := range metrics.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if instrument.Name != "rpc.client.cache.stale_served" {
				continue
			}
			sum, ok := instrument.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 2 {
				t.Fatalf("stale_served = %#v, want one series counting 2", instrument.Data)
			}
			if method, _ := sum.DataPoints[0].Attributes.Value("rpc.method"); method.AsString() != "effectiveAccess" {
				t.Fatalf("stale_served attributes = %v", sum.DataPoints[0].Attributes)
			}
			return
		}
	}
	t.Fatal("rpc.client.cache.stale_served was not recorded")
}
