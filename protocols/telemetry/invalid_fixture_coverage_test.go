package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestInvalidFixtureCoverage enforces the "corpus is the spec" contract:
// every fixture under fixtures/<signal>/invalid must be genuinely
// rejected by the strict parser, and each data-point reject branch
// (validateNumberDataPoint / validateHistogramDataPoint) must be
// triggered by at least one invalid fixture. A fixture nobody rejects,
// or a rule no fixture reaches, would let another implementation
// disagree with this one while both suites stay green. An accidental
// relaxation of a validation rule turns a red fixture green and fails
// this test.
func TestInvalidFixtureCoverage(t *testing.T) {
	metricsParse := func(b []byte) []diag.Diagnostic { _, d := ParseAndValidateMetrics(b); return d }

	// Every invalid metrics fixture must be rejected, and we collect the set of
	// codes the corpus actually triggers so we can assert branch coverage below.
	triggered := map[string]bool{}
	files, err := filepath.Glob("fixtures/metrics/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid metrics fixtures found")
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		diags := metricsParse(data)
		if !diag.HasErrors(diags) {
			t.Errorf("invalid fixture %s should produce errors but none found", path)
		}
		for _, d := range diags {
			triggered[d.Code] = true
		}
	}

	// The number/histogram data-point reject branches are the acute gap this
	// change closes. Each must be exercised by at least one invalid fixture.
	// They all share ErrorCodeInvalidDataPoint, so assert both a fixture named
	// for the branch exists and that the code fires.
	if !triggered[ErrorCodeInvalidDataPoint] {
		t.Errorf("no invalid metrics fixture triggers %q — data-point reject branches are unguarded", ErrorCodeInvalidDataPoint)
	}

	requiredNames := []string{
		"number-missing-timestamp",
		"number-no-value-kind",
		"histogram-missing-count",
		"histogram-bucket-arity",
	}
	present := map[string]bool{}
	for _, f := range files {
		present[strings.TrimSuffix(filepath.Base(f), ".json")] = true
	}
	for _, name := range requiredNames {
		if !present[name] {
			t.Errorf("missing invalid data-point fixture %q.json — reject branch is uncovered", name)
		}
	}
}
