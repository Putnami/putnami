package parse

import (
	"testing"
)

func TestEmptyBiomeReport(t *testing.T) {
	report := EmptyBiomeReport()
	if report.Summary.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", report.Summary.Errors)
	}
	if report.Diagnostics == nil {
		t.Error("expected non-nil diagnostics slice")
	}
	if len(report.Diagnostics) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(report.Diagnostics))
	}
}

func TestParseBiomeOutput_Empty(t *testing.T) {
	report := ParseBiomeOutput("")
	if len(report.Diagnostics) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(report.Diagnostics))
	}
}

func TestParseBiomeOutput_InvalidJSON(t *testing.T) {
	report := ParseBiomeOutput("not json")
	if len(report.Diagnostics) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(report.Diagnostics))
	}
}

func TestParseBiomeOutput_NullDiagnostics(t *testing.T) {
	input := `{"summary":{"errors":0,"warnings":0,"infos":0,"changed":0,"unchanged":1,"skipped":0},"command":"check"}`
	report := ParseBiomeOutput(input)
	if len(report.Diagnostics) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(report.Diagnostics))
	}
}

func TestParseBiomeOutput_WithDiagnostics(t *testing.T) {
	input := `{
		"summary": {"errors": 1, "warnings": 2, "infos": 0, "changed": 0, "unchanged": 5, "skipped": 0},
		"command": "lint",
		"diagnostics": [
			{
				"category": "lint/suspicious/noDoubleEquals",
				"severity": "error",
				"message": "Use === instead of ==",
				"location": {
					"path": "src/foo.ts",
					"start": {"line": 10, "column": 5},
					"end": {"line": 10, "column": 7}
				},
				"tags": []
			},
			{
				"category": "lint/style/noVar",
				"severity": "warning",
				"message": "Use let or const",
				"location": {
					"path": "src/bar.ts",
					"start": {"line": 3, "column": 1},
					"end": {"line": 3, "column": 10}
				},
				"tags": ["fixable"]
			}
		]
	}`

	report := ParseBiomeOutput(input)

	if report.Command != "lint" {
		t.Errorf("expected command 'lint', got %q", report.Command)
	}
	if report.Summary.Errors != 1 {
		t.Errorf("expected 1 error, got %d", report.Summary.Errors)
	}
	if report.Summary.Warnings != 2 {
		t.Errorf("expected 2 warnings, got %d", report.Summary.Warnings)
	}
	if report.Summary.Unchanged != 5 {
		t.Errorf("expected 5 unchanged, got %d", report.Summary.Unchanged)
	}
	if len(report.Diagnostics) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d", len(report.Diagnostics))
	}

	d0 := report.Diagnostics[0]
	if d0.Category != "lint/suspicious/noDoubleEquals" {
		t.Errorf("expected category 'lint/suspicious/noDoubleEquals', got %q", d0.Category)
	}
	if d0.Severity != "error" {
		t.Errorf("expected severity 'error', got %q", d0.Severity)
	}
	if d0.Description != "Use === instead of ==" {
		t.Errorf("expected description 'Use === instead of ==', got %q", d0.Description)
	}
	if d0.File != "src/foo.ts" {
		t.Errorf("expected file 'src/foo.ts', got %q", d0.File)
	}
	if d0.Line != 10 {
		t.Errorf("expected line 10, got %d", d0.Line)
	}
	if d0.Column != 5 {
		t.Errorf("expected column 5, got %d", d0.Column)
	}
	if d0.EndLine != 10 {
		t.Errorf("expected end line 10, got %d", d0.EndLine)
	}
	if d0.EndColumn != 7 {
		t.Errorf("expected end column 7, got %d", d0.EndColumn)
	}

	d1 := report.Diagnostics[1]
	if d1.Severity != "warning" {
		t.Errorf("expected severity 'warning', got %q", d1.Severity)
	}
	if len(d1.Tags) != 1 || d1.Tags[0] != "fixable" {
		t.Errorf("expected tags ['fixable'], got %v", d1.Tags)
	}
}

func TestParseBiomeOutput_NoLocation(t *testing.T) {
	input := `{
		"summary": {"errors": 1, "warnings": 0, "infos": 0, "changed": 0, "unchanged": 0, "skipped": 0},
		"command": "check",
		"diagnostics": [
			{
				"category": "lint/a11y/test",
				"severity": "error",
				"message": "Some error without location",
				"tags": []
			}
		]
	}`

	report := ParseBiomeOutput(input)
	if len(report.Diagnostics) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(report.Diagnostics))
	}

	d := report.Diagnostics[0]
	if d.File != "" {
		t.Errorf("expected empty file, got %q", d.File)
	}
	if d.Line != 0 {
		t.Errorf("expected line 0, got %d", d.Line)
	}
}

func TestParseBiomeOutput_SeverityMapping(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"error", "error"},
		{"warning", "warning"},
		{"info", "info"},
		{"hint", "info"}, // unknown maps to info
		{"", "info"},     // empty maps to info
	}

	for _, tt := range tests {
		input := `{"summary":{},"diagnostics":[{"category":"test","severity":"` + tt.input + `","message":"msg","tags":[]}]}`
		report := ParseBiomeOutput(input)
		if len(report.Diagnostics) != 1 {
			t.Fatalf("expected 1 diagnostic for severity %q", tt.input)
		}
		if report.Diagnostics[0].Severity != tt.expected {
			t.Errorf("severity %q: expected %q, got %q", tt.input, tt.expected, report.Diagnostics[0].Severity)
		}
	}
}

func TestParseBiomeOutput_LocationPartial(t *testing.T) {
	// Location with start but no end
	input := `{
		"summary": {},
		"diagnostics": [
			{
				"category": "test",
				"severity": "error",
				"message": "msg",
				"location": {
					"path": "src/x.ts",
					"start": {"line": 5, "column": 3}
				},
				"tags": []
			}
		]
	}`

	report := ParseBiomeOutput(input)
	if len(report.Diagnostics) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(report.Diagnostics))
	}
	d := report.Diagnostics[0]
	if d.File != "src/x.ts" {
		t.Errorf("expected file 'src/x.ts', got %q", d.File)
	}
	if d.Line != 5 {
		t.Errorf("expected line 5, got %d", d.Line)
	}
	if d.EndLine != 0 {
		t.Errorf("expected end line 0, got %d", d.EndLine)
	}
}
