package template

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

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
