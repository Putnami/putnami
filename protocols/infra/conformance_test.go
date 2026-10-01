package infra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ValidFixtures parses + validates every JSON file under
// fixtures/valid. Per-project fixtures use the per-project schema id;
// aggregated fixtures use the aggregated schema id. The fixture's
// $schema field selects which parser to run.
func TestConformance_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if isAggregatedFixture(t, data) {
				m, diags := ParseAndValidateAggregatedManifest(data)
				if diag.HasErrors(diags) {
					t.Errorf("valid aggregated fixture %s produced errors: %v", path, diags)
				}
				if m == nil {
					t.Errorf("valid aggregated fixture %s returned nil manifest", path)
				}
				return
			}
			m, diags := ParseAndValidatePerProjectManifest(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid per-project fixture %s produced errors: %v", path, diags)
			}
			if m == nil {
				t.Errorf("valid per-project fixture %s returned nil manifest", path)
			}
		})
	}
}

// TestConformance_InvalidFixtures asserts every JSON file under
// fixtures/invalid produces at least one error diagnostic. Most invalid
// fixtures are per-project manifests; runtime-scaling-min.json and
// runtime-billing-policy.json are aggregated, because a runtime block is
// only legal on a workload. An aggregated invalid fixture must carry the
// `workload` key that isAggregatedFixture routes on.
func TestConformance_InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var diags []diag.Diagnostic
			if isAggregatedFixture(t, data) {
				_, diags = ParseAndValidateAggregatedManifest(data)
			} else {
				_, diags = ParseAndValidatePerProjectManifest(data)
			}
			if !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
		})
	}
}

// isAggregatedFixture returns true if the fixture is an aggregated
// manifest. The per-project and aggregated shapes share the same
// `$schema` URL, so routing keys off the presence of the
// `workload` field, which only aggregated manifests carry. The probe
// uses non-strict JSON parsing so fixtures with unknown fields still
// route correctly.
func isAggregatedFixture(t *testing.T, data []byte) bool {
	t.Helper()
	var probe struct {
		Workload string `json:"workload"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		// Malformed JSON routes to the per-project parser; the test will
		// observe a parse-error diagnostic either way.
		return false
	}
	return probe.Workload != ""
}
