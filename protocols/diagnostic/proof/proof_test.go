package proof

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "diagnostic/finding-record"

func TestTheRecordMarshalsToItsFourJSONMembers(t *testing.T) {
	spectest.Proves(t, feature, "the-record-has-four-json-members", "the-json-members-are-severity-code-message-and-field")
	data, err := json.Marshal(diagnostic.Errorf("job.invalid_key", "jobs[0].key", "is %s", "empty"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"severity":"error","code":"job.invalid_key","message":"is empty","field":"jobs[0].key"}`
	if string(data) != want {
		t.Errorf("json.Marshal = %s, want %s", data, want)
	}
}

func TestAnEmptyFieldIsLeftOut(t *testing.T) {
	spectest.Proves(t, feature, "the-record-has-four-json-members", "an-empty-field-is-left-out")
	data, err := json.Marshal(diagnostic.Warningf("doctor.unknown_field", "", "ignored"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"severity":"warning","code":"doctor.unknown_field","message":"ignored"}`
	if string(data) != want {
		t.Errorf("json.Marshal = %s, want %s", data, want)
	}
}

func TestTheSeveritiesAreErrorWarningAndInfo(t *testing.T) {
	spectest.Proves(t, feature, "the-record-has-four-json-members", "the-severities-are-error-warning-and-info")
	got := []diagnostic.Severity{diagnostic.Error, diagnostic.Warning, diagnostic.Info}
	want := []diagnostic.Severity{"error", "warning", "info"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("severities = %q, want %q", got, want)
	}
}

func TestErrorf(t *testing.T) {
	spectest.Proves(t, feature, "the-constructors-fill-the-record", "errorf-builds-an-error-with-its-code-field-and-message")
	d := diagnostic.Errorf("test-code", "field.name", "something went %s", "wrong")
	if d.Severity != diagnostic.Error {
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
	spectest.Proves(t, feature, "the-constructors-fill-the-record", "warningf-builds-a-warning")
	d := diagnostic.Warningf("warn-code", "", "be careful")
	if d.Severity != diagnostic.Warning {
		t.Errorf("Severity = %q, want warning", d.Severity)
	}
	if d.Field != "" {
		t.Errorf("Field = %q, want empty", d.Field)
	}
}

func TestDiagnostic_String(t *testing.T) {
	spectest.Proves(t, feature, "string-shows-the-field-only-when-set", "string-renders-with-and-without-a-field")
	d := diagnostic.Errorf("code", "field", "msg")
	got := d.String()
	want := "[error] field: msg (code)"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	d2 := diagnostic.Errorf("code", "", "msg")
	got2 := d2.String()
	want2 := "[error] msg (code)"
	if got2 != want2 {
		t.Errorf("String() = %q, want %q", got2, want2)
	}
}

func TestHasErrors(t *testing.T) {
	spectest.Proves(t, feature, "only-an-error-blocks", "has-errors-is-true-only-with-an-error")
	if diagnostic.HasErrors(nil) {
		t.Error("nil should not have errors")
	}
	if diagnostic.HasErrors([]diagnostic.Diagnostic{{Severity: diagnostic.Warning}}) {
		t.Error("warnings only should not have errors")
	}
	if !diagnostic.HasErrors([]diagnostic.Diagnostic{{Severity: diagnostic.Warning}, {Severity: diagnostic.Error}}) {
		t.Error("should have errors")
	}
}

func TestErrors(t *testing.T) {
	spectest.Proves(t, feature, "only-an-error-blocks", "errors-keeps-the-errors-in-order")
	diags := []diagnostic.Diagnostic{
		{Severity: diagnostic.Warning, Code: "w1"},
		{Severity: diagnostic.Error, Code: "e1"},
		{Severity: diagnostic.Info, Code: "i1"},
		{Severity: diagnostic.Error, Code: "e2"},
	}
	errs := diagnostic.Errors(diags)
	if len(errs) != 2 {
		t.Fatalf("len = %d, want 2", len(errs))
	}
	if errs[0].Code != "e1" || errs[1].Code != "e2" {
		t.Errorf("Errors = %v", errs)
	}
}

func TestErrorText(t *testing.T) {
	spectest.Proves(t, feature, "only-an-error-blocks", "error-text-lists-the-errors-one-per-line")
	diags := []diagnostic.Diagnostic{
		diagnostic.Errorf("a.code", "items[0].name", "is required"),
		diagnostic.Warningf("w.code", "", "ignored"),
		diagnostic.Errorf("b.code", "", "unreadable document"),
	}
	got := diagnostic.ErrorText(diags)
	want := "  - [error] items[0].name: is required (a.code)\n  - [error] unreadable document (b.code)"
	if got != want {
		t.Errorf("ErrorText() = %q, want %q", got, want)
	}
	if got := diagnostic.ErrorText([]diagnostic.Diagnostic{diagnostic.Warningf("w.code", "", "only a warning")}); got != "" {
		t.Errorf("ErrorText(warnings only) = %q, want empty", got)
	}
}

func TestSeverityRank(t *testing.T) {
	spectest.Proves(t, feature, "severities-rank-error-warning-info", "severity-rank-orders-the-vocabulary-then-the-rest")
	if diagnostic.SeverityRank(diagnostic.Error) >= diagnostic.SeverityRank(diagnostic.Warning) ||
		diagnostic.SeverityRank(diagnostic.Warning) >= diagnostic.SeverityRank(diagnostic.Info) {
		t.Fatalf("severity order is not error, warning, info")
	}
	if diagnostic.SeverityRank(diagnostic.Severity("unknown")) <= diagnostic.SeverityRank(diagnostic.Info) {
		t.Fatalf("unknown severity must sort after the closed vocabulary")
	}
}
