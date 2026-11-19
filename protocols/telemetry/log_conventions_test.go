package telemetry

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestConventions_LogRecordMetricsMatchTheirFixture keeps the Go log record
// conventions equal to the committed fixture the TypeScript runtime reads.
func TestConventions_LogRecordMetricsMatchTheirFixture(t *testing.T) {
	data, err := os.ReadFile("fixtures/conventions/log-records.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Metrics  []MetricConvention `json:"metrics"`
		Levels   []string           `json:"levels"`
		Outcomes []string           `json:"outcomes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if got := LogRecordMetrics(); !reflect.DeepEqual(got, fixture.Metrics) {
		t.Fatalf("LogRecordMetrics() = %+v, fixture = %+v", got, fixture.Metrics)
	}
	if !reflect.DeepEqual(LogLevels(), fixture.Levels) {
		t.Fatalf("LogLevels() = %v, fixture = %v", LogLevels(), fixture.Levels)
	}
	if !reflect.DeepEqual(LogOutcomes(), fixture.Outcomes) {
		t.Fatalf("LogOutcomes() = %v, fixture = %v", LogOutcomes(), fixture.Outcomes)
	}
}
