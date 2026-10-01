package parse

import (
	"testing"
)

func TestParseTscOutput_Empty(t *testing.T) {
	diags := ParseTscOutput("")
	if diags != nil {
		t.Errorf("expected nil diagnostics, got %v", diags)
	}
}

func TestParseTscOutput_NoMatches(t *testing.T) {
	input := "Starting compilation in watch mode...\nFound 0 errors.\n"
	diags := ParseTscOutput(input)
	if len(diags) != 0 {
		t.Errorf("expected 0 diagnostics, got %d", len(diags))
	}
}

func TestParseTscOutput_SingleError(t *testing.T) {
	input := "src/foo.ts(10,5): error TS2345: Argument of type 'string' is not assignable to parameter of type 'number'."
	diags := ParseTscOutput(input)

	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	d := diags[0]
	if d.File != "src/foo.ts" {
		t.Errorf("expected file 'src/foo.ts', got %q", d.File)
	}
	if d.Line != 10 {
		t.Errorf("expected line 10, got %d", d.Line)
	}
	if d.Column != 5 {
		t.Errorf("expected column 5, got %d", d.Column)
	}
	if d.Category != "error" {
		t.Errorf("expected category 'error', got %q", d.Category)
	}
	if d.Code != "TS2345" {
		t.Errorf("expected code 'TS2345', got %q", d.Code)
	}
	if d.Message != "Argument of type 'string' is not assignable to parameter of type 'number'." {
		t.Errorf("unexpected message: %q", d.Message)
	}
}

func TestParseTscOutput_MultipleLines(t *testing.T) {
	input := `src/a.ts(1,1): error TS2304: Cannot find name 'x'.
src/b.ts(20,15): warning TS4053: Return type of public method from exported class has or is using private name 'Foo'.
src/c.ts(5,3): error TS2345: Another error.`

	diags := ParseTscOutput(input)
	if len(diags) != 3 {
		t.Fatalf("expected 3 diagnostics, got %d", len(diags))
	}

	if diags[0].File != "src/a.ts" || diags[0].Line != 1 || diags[0].Column != 1 {
		t.Errorf("unexpected first diagnostic: %+v", diags[0])
	}
	if diags[1].File != "src/b.ts" || diags[1].Category != "warning" {
		t.Errorf("unexpected second diagnostic: %+v", diags[1])
	}
	if diags[2].File != "src/c.ts" || diags[2].Code != "TS2345" {
		t.Errorf("unexpected third diagnostic: %+v", diags[2])
	}
}

func TestParseTscOutput_WithNonDiagnosticLines(t *testing.T) {
	input := `
Starting incremental compilation...
src/main.ts(42,8): error TS7006: Parameter 'event' implicitly has an 'any' type.

Found 1 error.
`
	diags := ParseTscOutput(input)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	if diags[0].File != "src/main.ts" {
		t.Errorf("expected file 'src/main.ts', got %q", diags[0].File)
	}
	if diags[0].Line != 42 {
		t.Errorf("expected line 42, got %d", diags[0].Line)
	}
}

func TestParseTscOutput_LocationlessError(t *testing.T) {
	input := "error TS6053: File 'nonexistent.ts' not found.\n"
	diags := ParseTscOutput(input)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	d := diags[0]
	if d.File != "" {
		t.Errorf("expected empty file, got %q", d.File)
	}
	if d.Category != "error" {
		t.Errorf("expected category 'error', got %q", d.Category)
	}
	if d.Code != "TS6053" {
		t.Errorf("expected code 'TS6053', got %q", d.Code)
	}
	if d.Message != "File 'nonexistent.ts' not found." {
		t.Errorf("unexpected message: %q", d.Message)
	}
}

func TestParseTscOutput_MixedLocationAndLocationless(t *testing.T) {
	input := `error TS5083: Cannot read file 'tsconfig.json'.
src/foo.ts(10,5): error TS2345: Argument of type 'string' is not assignable to parameter of type 'number'.
Found 2 errors.`

	diags := ParseTscOutput(input)
	if len(diags) != 2 {
		t.Fatalf("expected 2 diagnostics, got %d", len(diags))
	}
	if diags[0].File != "" || diags[0].Code != "TS5083" {
		t.Errorf("unexpected first diagnostic: %+v", diags[0])
	}
	if diags[1].File != "src/foo.ts" || diags[1].Code != "TS2345" {
		t.Errorf("unexpected second diagnostic: %+v", diags[1])
	}
}

func TestParseTscOutput_WindowsPath(t *testing.T) {
	// tsc on Windows may output paths like C:\path\to\file.ts
	// but the regex matches any characters before the parenthesis
	input := `src/util/helper.ts(100,20): error TS2339: Property 'foo' does not exist on type 'Bar'.`
	diags := ParseTscOutput(input)
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %d", len(diags))
	}
	if diags[0].Message != "Property 'foo' does not exist on type 'Bar'." {
		t.Errorf("unexpected message: %q", diags[0].Message)
	}
}
