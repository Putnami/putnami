package parse

import (
	"path/filepath"
	"testing"
)

func TestParseESLintOutput_EachMessageIsADiagnostic(t *testing.T) {
	root := filepath.FromSlash("/ws")
	output := `[
		{"filePath":"` + filepath.ToSlash(filepath.Join(root, "packages", "app", "src", "a.js")) + `","messages":[
			{"ruleId":"prefer-const","severity":2,"message":"'x' is never reassigned. Use 'const' instead.","line":1,"column":5,"endLine":1,"endColumn":6},
			{"ruleId":"no-console","severity":1,"message":"Unexpected console statement.","line":2,"column":1}
		],"errorCount":1,"warningCount":1},
		{"filePath":"` + filepath.ToSlash(filepath.Join(root, "packages", "app", "src", "b.js")) + `","messages":[
			{"ruleId":null,"fatal":true,"severity":2,"message":"Parsing error: Unexpected token ;","line":1,"column":11}
		]},
		{"filePath":"/elsewhere/c.js","messages":[]}
	]`

	report, err := ParseESLintOutput(output, root)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Errors != 2 || report.Summary.Warnings != 1 {
		t.Errorf("summary = %+v, want 2 errors and 1 warning", report.Summary)
	}
	want := []BiomeDiagnostic{
		{Category: "prefer-const", Severity: "error", Description: "'x' is never reassigned. Use 'const' instead.", File: "packages/app/src/a.js", Line: 1, Column: 5, EndLine: 1, EndColumn: 6},
		{Category: "no-console", Severity: "warning", Description: "Unexpected console statement.", File: "packages/app/src/a.js", Line: 2, Column: 1},
		{Category: ESLintCategory, Severity: "error", Description: "Parsing error: Unexpected token ;", File: "packages/app/src/b.js", Line: 1, Column: 11},
	}
	if len(report.Diagnostics) != len(want) {
		t.Fatalf("diagnostics = %+v, want %+v", report.Diagnostics, want)
	}
	for i := range want {
		got := report.Diagnostics[i]
		if got.Category != want[i].Category || got.Severity != want[i].Severity || got.Description != want[i].Description ||
			got.File != want[i].File || got.Line != want[i].Line || got.Column != want[i].Column ||
			got.EndLine != want[i].EndLine || got.EndColumn != want[i].EndColumn {
			t.Errorf("diagnostic %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestParseESLintOutput_NoJSONIsAnError(t *testing.T) {
	for _, output := range []string{"", "Oops! Something went wrong!", `{"not":"an array"}`} {
		if _, err := ParseESLintOutput(output, ""); err == nil {
			t.Errorf("ParseESLintOutput(%q) error = nil, want an error", output)
		}
	}
	report, err := ParseESLintOutput("[]", "")
	if err != nil || len(report.Diagnostics) != 0 {
		t.Errorf("ParseESLintOutput([]) = %+v, %v, want an empty report", report, err)
	}
}
