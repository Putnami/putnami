package doctor

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ValidReports parses + validates every JSON file under
// fixtures/valid. Each must strict-parse and validate clean.
func TestConformance_ValidReports(t *testing.T) {
	forEachFixture(t, "fixtures/valid/*.json", func(t *testing.T, path string, data []byte) {
		r, diags := ParseAndValidateReport(data)
		if diag.HasErrors(diags) {
			t.Errorf("valid report fixture %s produced errors: %v", path, diags)
		}
		if r == nil {
			t.Errorf("valid report fixture %s returned nil report", path)
		}
	})
}

// TestConformance_ValidWaiverFiles parses + validates every JSON file under
// fixtures/waivers/valid. Each must strict-parse and validate clean.
func TestConformance_ValidWaiverFiles(t *testing.T) {
	forEachFixture(t, "fixtures/waivers/valid/*.json", func(t *testing.T, path string, data []byte) {
		w, diags := ParseAndValidateWaiverFile(data)
		if diag.HasErrors(diags) {
			t.Errorf("valid waiver fixture %s produced errors: %v", path, diags)
		}
		if w == nil {
			t.Errorf("valid waiver fixture %s returned nil waiver file", path)
		}
	})
}

// TestConformance_InvalidReports asserts every JSON file under fixtures/invalid
// produces at least one error diagnostic drawn from the doctor taxonomy.
func TestConformance_InvalidReports(t *testing.T) {
	forEachFixture(t, "fixtures/invalid/*.json", func(t *testing.T, path string, data []byte) {
		_, diags := ParseAndValidateReport(data)
		assertCodedError(t, path, diags)
	})
}

// TestConformance_InvalidWaiverFiles asserts every JSON file under
// fixtures/waivers/invalid produces at least one coded error diagnostic.
func TestConformance_InvalidWaiverFiles(t *testing.T) {
	forEachFixture(t, "fixtures/waivers/invalid/*.json", func(t *testing.T, path string, data []byte) {
		_, diags := ParseAndValidateWaiverFile(data)
		assertCodedError(t, path, diags)
	})
}

// forEachFixture globs a fixture pattern, fails if it is empty, and runs fn for
// each file in a named subtest.
func forEachFixture(t *testing.T, pattern string, fn func(*testing.T, string, []byte)) {
	t.Helper()
	files, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures matched %s", pattern)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fn(t, path, data)
		})
	}
}

// assertCodedError requires at least one error diagnostic and that every error
// diagnostic uses a code from the canonical taxonomy.
func assertCodedError(t *testing.T, path string, diags []diag.Diagnostic) {
	t.Helper()
	if !diag.HasErrors(diags) {
		t.Errorf("invalid fixture %s should produce errors but none found", path)
		return
	}
	for _, d := range diag.Errors(diags) {
		if !ValidDiagnosticCodes[d.Code] {
			t.Errorf("invalid fixture %s produced error code %q not in ValidDiagnosticCodes", path, d.Code)
		}
	}
}
