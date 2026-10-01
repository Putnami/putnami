package agentcontext

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ValidDocuments parses + validates every JSON file under
// fixtures/valid. Each must strict-parse and validate clean.
func TestConformance_ValidDocuments(t *testing.T) {
	forEachFixture(t, "fixtures/valid/*.json", func(t *testing.T, path string, data []byte) {
		d, diags := ParseAndValidateDocument(data)
		if diag.HasErrors(diags) {
			t.Errorf("valid document fixture %s produced errors: %v", path, diags)
		}
		if d == nil {
			t.Errorf("valid document fixture %s returned nil document", path)
		}
	})
}

// TestConformance_InvalidDocuments asserts every JSON file under
// fixtures/invalid produces at least one coded error diagnostic drawn from the
// agent-context taxonomy.
func TestConformance_InvalidDocuments(t *testing.T) {
	forEachFixture(t, "fixtures/invalid/*.json", func(t *testing.T, path string, data []byte) {
		_, diags := ParseAndValidateDocument(data)
		assertCodedError(t, path, diags)
	})
}

// TestConformance_ValidOverrides parses + validates every JSON file under
// fixtures/overrides/valid. Each must strict-parse and validate clean.
func TestConformance_ValidOverrides(t *testing.T) {
	forEachFixture(t, "fixtures/overrides/valid/*.json", func(t *testing.T, path string, data []byte) {
		o, diags := ParseAndValidateOverrides(data)
		if diag.HasErrors(diags) {
			t.Errorf("valid overrides fixture %s produced errors: %v", path, diags)
		}
		if o == nil {
			t.Errorf("valid overrides fixture %s returned nil overrides file", path)
		}
	})
}

// TestConformance_InvalidOverrides asserts every JSON file under
// fixtures/overrides/invalid produces at least one coded error diagnostic.
func TestConformance_InvalidOverrides(t *testing.T) {
	forEachFixture(t, "fixtures/overrides/invalid/*.json", func(t *testing.T, path string, data []byte) {
		_, diags := ParseAndValidateOverrides(data)
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
