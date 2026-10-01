package database

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_Requirements parses + validates every fixture under
// fixtures/requirement: those in valid/ must produce no errors, those in
// invalid/ must produce at least one.
func TestConformance_Requirements(t *testing.T) {
	runConformance(t, "fixtures/requirement", func(data []byte) []diag.Diagnostic {
		_, diags := ParseAndValidateRequirementManifest(data)
		return diags
	})
}

// TestConformance_Bindings does the same for fixtures/binding.
func TestConformance_Bindings(t *testing.T) {
	runConformance(t, "fixtures/binding", func(data []byte) []diag.Diagnostic {
		_, diags := ParseAndValidateBinding(data)
		return diags
	})
}

// TestConformance_TestBindings does the same for fixtures/test-binding.
func TestConformance_TestBindings(t *testing.T) {
	runConformance(t, "fixtures/test-binding", func(data []byte) []diag.Diagnostic {
		_, diags := ParseAndValidateTestBinding(data)
		return diags
	})
}

func runConformance(t *testing.T, dir string, parse func([]byte) []diag.Diagnostic) {
	t.Helper()

	valid, err := filepath.Glob(filepath.Join(dir, "valid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(valid) == 0 {
		t.Fatalf("no valid fixtures in %s/valid", dir)
	}
	for _, path := range valid {
		t.Run("valid/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diags := parse(data); diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
		})
	}

	invalid, err := filepath.Glob(filepath.Join(dir, "invalid", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) == 0 {
		t.Fatalf("no invalid fixtures in %s/invalid", dir)
	}
	for _, path := range invalid {
		t.Run("invalid/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diags := parse(data); !diag.HasErrors(diags) {
				t.Errorf("invalid fixture %s should produce errors but none found", path)
			}
		})
	}
}
