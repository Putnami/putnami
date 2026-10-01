package capabilities

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ValidFixtures parses + validates every JSON file under
// fixtures/valid. Each must strict-parse and validate clean. This corpus is
// the cross-language contract: the Go and TypeScript emitters must both
// produce manifests that pass here.
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
// fixtures/invalid produces at least one error diagnostic.
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
		})
	}
}

func TestConformance_V2Fixtures(t *testing.T) {
	valid, err := filepath.Glob("fixtures/v2/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	equivalence, err := filepath.Glob("fixtures/v2/equivalence/*.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	valid = append(valid, equivalence...)
	if len(valid) == 0 {
		t.Fatal("no valid v2 fixtures found")
	}
	for _, fixture := range valid {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			document, diagnostics := ParseAndValidateManifestDocument(data)
			if document == nil || document.V2 == nil || diag.HasErrors(diagnostics) {
				t.Fatalf("valid v2 fixture produced errors: %v", diagnostics)
			}
			if strings.HasSuffix(fixture, ".golden.json") {
				canonical, err := MarshalManifestV2(document.V2)
				if err != nil {
					t.Fatal(err)
				}
				if string(canonical) != string(data) {
					t.Fatalf("v2 fixture is not canonical\n--- got ---\n%s\n--- want ---\n%s", canonical, data)
				}
			}
		})
	}

	invalid, err := filepath.Glob("fixtures/v2/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) == 0 {
		t.Fatal("no invalid v2 fixtures found")
	}
	for _, fixture := range invalid {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if _, diagnostics := ParseAndValidateManifestDocument(data); !diag.HasErrors(diagnostics) {
				t.Fatalf("invalid v2 fixture should fail")
			}
		})
	}
}
