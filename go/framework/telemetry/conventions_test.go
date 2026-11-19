package telemetry

import (
	"context"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	putnami_http "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
	otlp "go.putnami.dev/protocol/telemetry"
)

// TestHTTPMiddlewareRecordsTheProtocolConventions reads back what the middleware
// records for one request and compares each metric with the protocol's HTTP
// server conventions: name, kind, unit, description, label keys and bounds.
func TestHTTPMiddlewareRecordsTheProtocolConventions(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "http-server-metrics-are-shared", "the-go-middleware-records-the-conventions")
	saveOTelGlobals(t)

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)

	mw := HTTPMiddleware("conventions-svc")
	ctx := putnami_http.NewContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/widgets/7", nil))
	ctx.Route = "/widgets/{id}"
	mw(ctx, putnami_http.NoContent)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	recorded := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			recorded[m.Name] = m
		}
	}

	for _, want := range otlp.HTTPServerMetrics() {
		got, ok := recorded[want.Name]
		if !ok {
			t.Errorf("%s was not recorded", want.Name)
			continue
		}
		if got.Unit != want.Unit || got.Description != want.Description {
			t.Errorf("%s unit %q description %q, want %q and %q", want.Name, got.Unit, got.Description, want.Unit, want.Description)
		}
		var attrs attribute.Set
		switch data := got.Data.(type) {
		case metricdata.Sum[int64]:
			if want.Kind != otlp.MetricKindCounter || len(data.DataPoints) != 1 {
				t.Fatalf("%s is a counter with %d points, want a %s with one", want.Name, len(data.DataPoints), want.Kind)
			}
			attrs = data.DataPoints[0].Attributes
		case metricdata.Histogram[float64]:
			if want.Kind != otlp.MetricKindHistogram || len(data.DataPoints) != 1 {
				t.Fatalf("%s is a histogram with %d points, want a %s with one", want.Name, len(data.DataPoints), want.Kind)
			}
			point := data.DataPoints[0]
			if !reflect.DeepEqual(point.Bounds, want.Bounds) {
				t.Errorf("%s bounds = %v, want %v", want.Name, point.Bounds, want.Bounds)
			}
			attrs = point.Attributes
		default:
			t.Fatalf("%s has data %T", want.Name, got.Data)
		}
		keys := make([]string, 0, attrs.Len())
		for _, kv := range attrs.ToSlice() {
			keys = append(keys, string(kv.Key))
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, want.Attributes) {
			t.Errorf("%s label keys = %v, want %v", want.Name, keys, want.Attributes)
		}
		if route, _ := attrs.Value(otlp.AttrHTTPRoute); route.AsString() != "/widgets/{id}" {
			t.Errorf("%s route = %q, want the route pattern", want.Name, route.AsString())
		}
	}
}
