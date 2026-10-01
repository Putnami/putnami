package contracts

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ValidFixtures parses + validates every JSON file under
// fixtures/valid and fixtures/equivalence. Each must strict-parse and validate
// clean. This corpus is the cross-language contract: the Go and TypeScript
// emitters (Slice 3) must both produce IR documents that pass here.
func TestConformance_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}
	equivalence, err := filepath.Glob("fixtures/equivalence/*.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(equivalence) == 0 {
		t.Fatal("no equivalence fixtures found")
	}
	files = append(files, equivalence...)

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			m, diags := ParseAndValidateManifest(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if m == nil {
				t.Errorf("valid fixture %s returned nil manifest", path)
			}
		})
	}
}

// TestConformance_InvalidFixtures asserts every JSON file under
// fixtures/invalid produces at least one coded error diagnostic drawn from the
// canonical taxonomy.
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
			_, diags := ParseAndValidateManifest(data)
			if !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
			for _, d := range diags {
				if d.Severity == diag.Error && !ValidErrorCodes[d.Code] {
					t.Errorf("invalid fixture %s produced uncoded error %q", path, d.Code)
				}
			}
		})
	}
}
