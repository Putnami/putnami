package workspace

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// TestWarningReportHoldsEachWarningOnce holds Report to skipping a warning the
// report already holds: two loads of one workspace report and write each
// warning once, in order, and the envelope lists each once.
func TestWarningReportHoldsEachWarningOnce(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	report := NewWarningReport(&out)
	first := Warning{Code: WarningCodeJobsIgnored, Message: "first"}
	second := Warning{Code: WarningCodeNameDivergence, Message: "second"}
	ws := &Workspace{Warnings: []Warning{first}}

	report.Report(ws)
	report.Report(ws)
	report.Report(&Workspace{Warnings: []Warning{first, second, second}})

	if got := report.Warnings(); !reflect.DeepEqual(got, []Warning{first, second}) {
		t.Fatalf("Warnings = %+v, want each warning once, in order", got)
	}
	if want := "putnami: warning: first\nputnami: warning: second\n"; out.String() != want {
		t.Fatalf("written = %q, want %q", out.String(), want)
	}
	want := []protocolcli.WorkspaceWarning{
		{Code: string(first.Code), Message: "first"},
		{Code: string(second.Code), Message: "second"},
	}
	if got := report.Envelope(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Envelope = %+v, want %+v", got, want)
	}
}

// TestWarningReportQuietWritesNothingAndKeepsEveryWarning holds a report with
// no writer, the quiet mode, to keeping every warning for the envelope and
// writing none, and a nil report to doing nothing.
func TestWarningReportQuietWritesNothingAndKeepsEveryWarning(t *testing.T) {
	t.Parallel()
	first := Warning{Code: WarningCodeJobsIgnored, Message: "first"}
	quiet := NewWarningReport(nil)
	quiet.Report(&Workspace{Warnings: []Warning{first}})
	if got := quiet.Warnings(); !reflect.DeepEqual(got, []Warning{first}) {
		t.Fatalf("Warnings = %+v, want the quiet report to keep the warning", got)
	}

	var none *WarningReport
	none.Report(&Workspace{Warnings: []Warning{first}})
	if none.Warnings() != nil || none.Envelope() != nil {
		t.Fatal("a nil report kept a warning")
	}
}

func TestAWarningAddedAfterTheLoadIsWrittenAndKeptOnce(t *testing.T) {
	var out strings.Builder
	report := NewWarningReport(&out)
	warning := Warning{Code: "cache.input_record_discarded", Message: "discarded"}
	report.Add(warning)
	report.Add(warning)
	if got := report.Warnings(); len(got) != 1 || got[0] != warning {
		t.Fatalf("warnings = %v, want the one added", got)
	}
	if strings.Count(out.String(), "putnami: warning: discarded") != 1 {
		t.Fatalf("output = %q, want the warning once", out.String())
	}
	var none *WarningReport
	none.Add(warning)
}

func TestAKeptWarningIsCarriedAtOnceAndWrittenOnFlush(t *testing.T) {
	var out strings.Builder
	report := NewWarningReport(&out)
	warning := Warning{Code: "cache.input_record_discarded", Message: "discarded"}
	report.Keep(warning)
	report.Keep(warning)
	if got := report.Warnings(); len(got) != 1 || got[0] != warning || out.Len() != 0 {
		t.Fatalf("warnings = %v, output %q: want the warning kept and nothing written", got, out.String())
	}
	report.Flush()
	report.Flush()
	if strings.Count(out.String(), "putnami: warning: discarded") != 1 {
		t.Fatalf("output = %q, want the warning once", out.String())
	}
	var none *WarningReport
	none.Keep(warning)
	none.Flush()
}
