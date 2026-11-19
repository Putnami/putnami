package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// TestConventions_HTTPServerMetricsMatchTheirFixture keeps the Go conventions
// equal to the committed fixture the TypeScript runtime reads, so neither
// runtime can rename a metric, a unit, a label key or a bound alone.
func TestConventions_HTTPServerMetricsMatchTheirFixture(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "http-server-metrics-are-shared", "the-conventions-match-their-fixture")
	data, err := os.ReadFile("fixtures/conventions/http-server-metrics.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Metrics []MetricConvention `json:"metrics"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if got := HTTPServerMetrics(); !reflect.DeepEqual(got, fixture.Metrics) {
		t.Fatalf("HTTPServerMetrics() = %+v, fixture = %+v", got, fixture.Metrics)
	}
	want := map[string]MetricKind{
		"http.server.duration":      MetricKindHistogram,
		"http.server.request_count": MetricKindCounter,
	}
	for _, m := range fixture.Metrics {
		if want[m.Name] != m.Kind {
			t.Errorf("%s kind = %q, want %q", m.Name, m.Kind, want[m.Name])
		}
		if !reflect.DeepEqual(m.Attributes, []string{"http.method", "http.route", "http.status_code"}) {
			t.Errorf("%s attributes = %v", m.Name, m.Attributes)
		}
	}
	bounds := HTTPServerDurationBounds()
	for i := 1; i < len(bounds); i++ {
		if bounds[i] <= bounds[i-1] {
			t.Fatalf("bounds must ascend strictly: %v", bounds)
		}
	}
}
