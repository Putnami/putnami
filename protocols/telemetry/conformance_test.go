package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("ProtocolVersion = %d, want 1 — bumping requires a migration story", ProtocolVersion)
	}
}

// TestConformance_Signals pins the canonical signal set and their collector
// paths. Adding or removing a signal is a protocol change.
func TestConformance_Signals(t *testing.T) {
	want := []struct {
		signal Signal
		path   string
	}{
		{SignalMetrics, "/v1/metrics"},
		{SignalTraces, "/v1/traces"},
		{SignalLogs, "/v1/logs"},
	}
	if len(CanonicalSignals) != len(want) {
		t.Fatalf("CanonicalSignals has %d entries, want %d", len(CanonicalSignals), len(want))
	}
	for i, w := range want {
		if CanonicalSignals[i] != w.signal {
			t.Errorf("CanonicalSignals[%d] = %q, want %q", i, CanonicalSignals[i], w.signal)
		}
		got, ok := PathFor(w.signal)
		if !ok {
			t.Errorf("PathFor(%q) reports unknown signal", w.signal)
		}
		if got != w.path {
			t.Errorf("PathFor(%q) = %q, want %q", w.signal, got, w.path)
		}
	}
	if _, ok := PathFor("bogus"); ok {
		t.Error("PathFor must reject unknown signals")
	}
}

// TestConformance_ResourceAttributes pins the canonical resource attribute keys
// every runtime populates.
func TestConformance_ResourceAttributes(t *testing.T) {
	want := []string{"service.name", "service.version", "putnami.framework"}
	if len(CanonicalResourceAttrs) != len(want) {
		t.Fatalf("CanonicalResourceAttrs has %d entries, want %d", len(CanonicalResourceAttrs), len(want))
	}
	for i, w := range want {
		if CanonicalResourceAttrs[i] != w {
			t.Errorf("CanonicalResourceAttrs[%d] = %q, want %q", i, CanonicalResourceAttrs[i], w)
		}
	}
}

// TestConformance_ExportContract pins the exporter behavior contract. These
// values are part of the protocol: every runtime's exporter must honor them.
func TestConformance_ExportContract(t *testing.T) {
	e := DefaultContract().Export
	if e.DefaultFlushIntervalMS != 10000 {
		t.Errorf("DefaultFlushIntervalMS = %d, want 10000", e.DefaultFlushIntervalMS)
	}
	if e.DefaultTimeoutMS != 5000 {
		t.Errorf("DefaultTimeoutMS = %d, want 5000", e.DefaultTimeoutMS)
	}
	if !e.FinalFlushOnShutdown {
		t.Error("FinalFlushOnShutdown must be true — short-lived containers must flush on exit")
	}
	if !e.DropOnCollectorError {
		t.Error("DropOnCollectorError must be true — telemetry must never affect the workload")
	}
	if e.MaxQueueRecords != 10000 {
		t.Errorf("MaxQueueRecords = %d, want 10000", e.MaxQueueRecords)
	}
}

// TestConformance_ContentType pins the OTLP/JSON content type.
func TestConformance_ContentType(t *testing.T) {
	if ContentType != "application/json" {
		t.Errorf("ContentType = %q, want application/json", ContentType)
	}
}

// TestConformance_ErrorCodes validates that every protocol error code uses the
// telemetry.* prefix and that the canonical set is complete.
func TestConformance_ErrorCodes(t *testing.T) {
	canonical := []string{
		"telemetry.invalid_metrics",
		"telemetry.invalid_traces",
		"telemetry.invalid_logs",
		"telemetry.invalid_metric",
		"telemetry.invalid_data_point",
		"telemetry.invalid_attribute",
		"telemetry.invalid_resource",
		"telemetry.invalid_span",
		"telemetry.invalid_trace_id",
		"telemetry.invalid_span_id",
		"telemetry.invalid_temporality",
		"telemetry.invalid_log_record",
		"telemetry.invalid_severity",
	}
	for _, code := range canonical {
		if !ValidErrorCodes[code] {
			t.Errorf("canonical error code %q missing from ValidErrorCodes", code)
		}
		if !strings.HasPrefix(code, "telemetry.") {
			t.Errorf("error code %q must use telemetry.* prefix", code)
		}
	}
	if len(ValidErrorCodes) != len(canonical) {
		t.Errorf("ValidErrorCodes has %d entries, want %d canonical codes", len(ValidErrorCodes), len(canonical))
	}
}

// parseFunc is the strict parse-and-validate entry point for one signal.
type parseFunc func([]byte) []diag.Diagnostic

// TestConformance_Fixtures runs every fixture under fixtures/<signal>: those in
// valid/ must produce no errors, those in invalid/ must produce at least one.
// Non-Go runtimes validate their envelopes against the same corpus.
func TestConformance_Fixtures(t *testing.T) {
	signals := map[string]parseFunc{
		"metrics": func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateMetrics(b); return d },
		"traces":  func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateTraces(b); return d },
		"logs":    func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateLogs(b); return d },
	}
	for signal, parse := range signals {
		runFixtureDir(t, signal, "valid", parse, false)
		runFixtureDir(t, signal, "invalid", parse, true)
	}
}

func runFixtureDir(t *testing.T, signal, kind string, parse parseFunc, wantErrors bool) {
	t.Helper()
	glob := filepath.Join("fixtures", signal, kind, "*.json")
	paths, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures matched %s", glob)
	}
	for _, path := range paths {
		t.Run(signal+"/"+kind+"/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			diags := parse(data)
			hasErr := diag.HasErrors(diags)
			if wantErrors && !hasErr {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
			if !wantErrors && hasErr {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}
}
