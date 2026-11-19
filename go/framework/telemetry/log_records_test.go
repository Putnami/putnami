package telemetry

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"sort"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
	otlp "go.putnami.dev/protocol/telemetry"
)

// TestPluginRecordsTheLogRecordsConvention reads back log.records from a
// configured plugin and compares it with the protocol fixture: name, kind,
// description, label keys and label values.
func TestPluginRecordsTheLogRecordsConvention(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "log-records-are-shared", "the-go-logger-records-the-log-records-convention")
	saveOTelGlobals(t)

	data, err := os.ReadFile("../../../protocols/telemetry/fixtures/conventions/log-records.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Metrics  []otlp.MetricConvention `json:"metrics"`
		Levels   []string                `json:"levels"`
		Outcomes []string                `json:"outcomes"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	want := fixture.Metrics[0]

	reader := sdkmetric.NewManualReader()
	p := NewPlugin(Config{ServiceName: "log-records-svc", TraceExporter: tracetest.NewInMemoryExporter(), MetricReader: reader})
	if err := p.Configure(context.Background(), nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background(), nil) })

	log := logger.New("log-records", logger.LevelWarn, logger.NewMemorySink())
	log.Info("dropped")
	log.Error("emitted", nil)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var got *metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == want.Name {
				got = &sm.Metrics[i]
			}
		}
	}
	if got == nil {
		t.Fatalf("%s was not recorded", want.Name)
	}
	if got.Description != want.Description || got.Unit != want.Unit {
		t.Errorf("%s description %q unit %q, want %q and %q", want.Name, got.Description, got.Unit, want.Description, want.Unit)
	}
	sum, ok := got.Data.(metricdata.Sum[int64])
	if !ok || want.Kind != otlp.MetricKindCounter || !sum.IsMonotonic {
		t.Fatalf("%s data = %T, want a monotonic counter", want.Name, got.Data)
	}

	series := map[string]int64{}
	for _, point := range sum.DataPoints {
		keys := make([]string, 0, point.Attributes.Len())
		for _, kv := range point.Attributes.ToSlice() {
			keys = append(keys, string(kv.Key))
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, want.Attributes) {
			t.Fatalf("label keys = %v, want %v", keys, want.Attributes)
		}
		level, _ := point.Attributes.Value(otlp.AttrLogLevel)
		outcome, _ := point.Attributes.Value(otlp.AttrLogOutcome)
		series[level.AsString()+"/"+outcome.AsString()] = point.Value
	}
	if len(series) != len(fixture.Levels)*len(fixture.Outcomes) {
		t.Fatalf("series = %v, want one per level and outcome", series)
	}
	for _, level := range fixture.Levels {
		for _, outcome := range fixture.Outcomes {
			if _, ok := series[level+"/"+outcome]; !ok {
				t.Errorf("no %s/%s series in %v", level, outcome, series)
			}
		}
	}
	if series["info/dropped"] < 1 || series["error/emitted"] < 1 {
		t.Errorf("series = %v, want the dropped info and the emitted error counted", series)
	}
}

func TestClampInt64HoldsACountPastMaxInt64(t *testing.T) {
	if got := clampInt64(math.MaxUint64); got != math.MaxInt64 {
		t.Fatalf("clampInt64(MaxUint64) = %d, want MaxInt64", got)
	}
	if got := clampInt64(42); got != 42 {
		t.Fatalf("clampInt64(42) = %d", got)
	}
}
