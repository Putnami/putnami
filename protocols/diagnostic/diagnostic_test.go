package diagnostic

import (
	"testing"
)

func TestErrorf(t *testing.T) {
	d := Errorf("test-code", "field.name", "something went %s", "wrong")
	if d.Severity != Error {
		t.Errorf("Severity = %q, want error", d.Severity)
	}
	if d.Code != "test-code" {
		t.Errorf("Code = %q, want test-code", d.Code)
	}
	if d.Field != "field.name" {
		t.Errorf("Field = %q, want field.name", d.Field)
	}
	if d.Message != "something went wrong" {
		t.Errorf("Message = %q, want 'something went wrong'", d.Message)
	}
}

func TestWarningf(t *testing.T) {
	d := Warningf("warn-code", "", "be careful")
	if d.Severity != Warning {
		t.Errorf("Severity = %q, want warning", d.Severity)
	}
	if d.Field != "" {
		t.Errorf("Field = %q, want empty", d.Field)
	}
}

func TestDiagnostic_String(t *testing.T) {
	d := Errorf("code", "field", "msg")
	got := d.String()
	want := "[error] field: msg (code)"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	d2 := Errorf("code", "", "msg")
	got2 := d2.String()
	want2 := "[error] msg (code)"
	if got2 != want2 {
		t.Errorf("String() = %q, want %q", got2, want2)
	}
}

func TestHasErrors(t *testing.T) {
	if HasErrors(nil) {
		t.Error("nil should not have errors")
	}
	if HasErrors([]Diagnostic{{Severity: Warning}}) {
		t.Error("warnings only should not have errors")
	}
	if !HasErrors([]Diagnostic{{Severity: Warning}, {Severity: Error}}) {
		t.Error("should have errors")
	}
}

func TestSeverityRank(t *testing.T) {
	if SeverityRank(Error) >= SeverityRank(Warning) || SeverityRank(Warning) >= SeverityRank(Info) {
		t.Fatalf("severity order is not error, warning, info")
	}
	if SeverityRank(Severity("unknown")) <= SeverityRank(Info) {
		t.Fatalf("unknown severity must sort after the closed vocabulary")
	}
}

func TestErrors(t *testing.T) {
	diags := []Diagnostic{
		{Severity: Warning, Code: "w1"},
		{Severity: Error, Code: "e1"},
		{Severity: Info, Code: "i1"},
		{Severity: Error, Code: "e2"},
	}
	errs := Errors(diags)
	if len(errs) != 2 {
		t.Fatalf("len = %d, want 2", len(errs))
	}
	if errs[0].Code != "e1" || errs[1].Code != "e2" {
		t.Errorf("Errors = %v", errs)
	}
}

func TestErrorText(t *testing.T) {
	diags := []Diagnostic{
		Errorf("a.code", "items[0].name", "is required"),
		Warningf("w.code", "", "ignored"),
		Errorf("b.code", "", "unreadable document"),
	}
	got := ErrorText(diags)
	want := "  - [error] items[0].name: is required (a.code)\n  - [error] unreadable document (b.code)"
	if got != want {
		t.Errorf("ErrorText() = %q, want %q", got, want)
	}
	if got := ErrorText([]Diagnostic{Warningf("w.code", "", "only a warning")}); got != "" {
		t.Errorf("ErrorText(warnings only) = %q, want empty", got)
	}
}
