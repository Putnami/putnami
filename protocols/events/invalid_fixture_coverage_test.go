package events

import (
	"os"
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_ConformanceManifestFixtures holds the "corpus is the spec"
// rule for the conformance-manifest wire shape: a reject branch that no fixture
// triggers is only aspirationally covered, because another language's runner
// validates itself against these files and never sees a branch the corpus does
// not exercise. ValidateConformanceManifest's duplicate-id and
// unknown-transport branches are therefore each pinned by an invalid fixture.
func TestConformance_ConformanceManifestFixtures(t *testing.T) {
	runManifestDir(t, "valid", false)
	runManifestDir(t, "invalid", true)
}

func runManifestDir(t *testing.T, kind string, wantErrors bool) {
	t.Helper()
	glob := filepath.Join("fixtures", "conformance-manifest", kind, "*.json")
	files, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures matched %s", glob)
	}
	for _, path := range files {
		t.Run(kind+"/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseConformanceManifestStrict(data)
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

// TestConformance_ConformanceManifestRejectCodes pins that the acute reject
// branches are each exercised: a duplicate case id and an unknown transport.
func TestConformance_ConformanceManifestRejectCodes(t *testing.T) {
	cases := map[string]string{
		"duplicate-case-id.json": "duplicate-case",
		"bad-transport.json":     "invalid-enum",
	}
	for name, wantCode := range cases {
		data, err := os.ReadFile(filepath.Join("fixtures", "conformance-manifest", "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		_, diags := ParseConformanceManifestStrict(data)
		if !hasEventsCode(diags, wantCode) {
			t.Errorf("fixture %s should trigger %q, got %v", name, wantCode, diags)
		}
	}
}

// TestConformance_HandlerOptionsFixtures graduates the handler-options shape
// into the fixture corpus. ValidateHandlerOptions' 3 enum and 5 negative-number
// reject branches (validate.go:284) each get a representative invalid fixture.
func TestConformance_HandlerOptionsFixtures(t *testing.T) {
	runHandlerOptionsDir(t, "valid", false)
	runHandlerOptionsDir(t, "invalid", true)
}

func runHandlerOptionsDir(t *testing.T, kind string, wantErrors bool) {
	t.Helper()
	glob := filepath.Join("fixtures", "handler-options", kind, "*.json")
	files, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures matched %s", glob)
	}
	for _, path := range files {
		t.Run(kind+"/"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, diags := ParseAndValidateHandlerOptions(data)
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

// TestConformance_HandlerOptionsRejectBranches pins every enum and numeric
// reject branch to a specific fixture field, so relaxing any single check turns
// a red fixture green and fails here.
func TestConformance_HandlerOptionsRejectBranches(t *testing.T) {
	want := map[string]string{
		"bad-distribution.json":     "distribution",
		"bad-overflow.json":         "overflow",
		"bad-ack.json":              "ack",
		"negative-max-retries.json": "maxRetries",
		"negative-max-backoff.json": "maxBackoffMs",
		"negative-timeout.json":     "timeoutMs",
		"negative-concurrency.json": "concurrency",
		"negative-queue-limit.json": "queueLimit",
	}
	for name, field := range want {
		data, err := os.ReadFile(filepath.Join("fixtures", "handler-options", "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		_, diags := ParseAndValidateHandlerOptions(data)
		if !hasEventsField(diags, field) {
			t.Errorf("fixture %s should trigger a reject on field %q, got %v", name, field, diags)
		}
	}
}

func hasEventsCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func hasEventsField(diags []diag.Diagnostic, field string) bool {
	for _, d := range diags {
		if d.Field == field {
			return true
		}
	}
	return false
}
