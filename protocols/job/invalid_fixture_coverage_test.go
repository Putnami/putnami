package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestInvalidFixtureCoverage holds the "corpus is the spec" contract for the job
// context: every fixtures/invalid case must be genuinely rejected by
// ParseAndValidate, and each required-field / selectedProjects[] reject branch
// in Validate must be triggered by a fixture. The corpus is what another
// language's SDK validates its parser against, so a branch no fixture reaches is
// a rule that only this implementation enforces. Relaxing one of those
// required(...) checks turns a red fixture green and fails this test.
func TestInvalidFixtureCoverage(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	triggered := map[string]bool{}
	fieldTriggered := map[string]bool{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, diags := ParseAndValidate(data)
		if !diag.HasErrors(diags) {
			t.Errorf("invalid fixture %s should produce errors but none found", path)
		}
		for _, d := range diags {
			triggered[d.Code] = true
			fieldTriggered[d.Field] = true
		}
	}

	if !triggered[ErrorCodeMissingField] {
		t.Errorf("no invalid fixture triggers %q — required-field reject branches are unguarded", ErrorCodeMissingField)
	}

	// Each required-field branch must be exercised by a fixture. Field paths are
	// asserted, not just the code, so relaxing any single check is caught rather
	// than being masked by another fixture that happens to raise the same code.
	requiredFields := []string{
		"outputPath",
		"cacheRoot",
		"extension.name",
	}
	for _, field := range requiredFields {
		if !fieldTriggered[field] {
			t.Errorf("no invalid fixture triggers the %q required-field reject branch", field)
		}
	}

	// The selectedProjects[] loop reject path must be covered by a fixture whose
	// diagnostic field carries the "selectedProjects[" prefix.
	selectedCovered := false
	for field := range fieldTriggered {
		if strings.HasPrefix(field, "selectedProjects[") {
			selectedCovered = true
			break
		}
	}
	if !selectedCovered {
		t.Errorf("no invalid fixture triggers the selectedProjects[] loop reject branch")
	}
}
