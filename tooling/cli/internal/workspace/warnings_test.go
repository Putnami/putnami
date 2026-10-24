package workspace

import (
	"bytes"
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

// TestWarningReportKeepWritesNothingAndKeepsInOrder holds Keep to the warnings
// of a producer that already wrote them: the report keeps them, in order, and
// writes none again.
func TestWarningReportKeepWritesNothingAndKeepsInOrder(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	report := NewWarningReport(&out)
	first := Warning{Code: WarningCodeJobsIgnored, Message: "first"}
	second := Warning{Code: WarningCodeNameDivergence, Message: "second"}

	report.Keep([]Warning{first})
	report.Keep(nil)
	report.Keep([]Warning{second})

	if out.Len() != 0 {
		t.Fatalf("Keep wrote %q, want nothing", out.String())
	}
	if got := report.Warnings(); !reflect.DeepEqual(got, []Warning{first, second}) {
		t.Fatalf("Warnings = %+v, want both in order", got)
	}
	want := []protocolcli.WorkspaceWarning{
		{Code: string(first.Code), Message: "first"},
		{Code: string(second.Code), Message: "second"},
	}
	if got := report.Envelope(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Envelope = %+v, want %+v", got, want)
	}

	var none *WarningReport
	none.Keep([]Warning{first})
	if none.Warnings() != nil {
		t.Fatal("a nil report kept a warning")
	}
}
