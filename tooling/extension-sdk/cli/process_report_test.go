package cli

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	runtime "go.putnami.dev/protocol/runtime"
)

func TestAResultCarriesTheJobProcessReport(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cpu", "an-sdk-job-reports-its-own-cpu-apart-from-its-children")
	data := map[string]any{"testSummary": map[string]any{"total": 1}}
	out := withProcessReport(data)
	if _, changed := data[runtime.ProcessReportResultDataKey]; changed {
		t.Fatal("the job's own data map was changed")
	}
	if out["testSummary"] == nil {
		t.Fatal("the job's data was dropped")
	}
	report, ok := out[runtime.ProcessReportResultDataKey].(*runtime.ProcessReport)
	if !ok || report == nil {
		t.Fatalf("processReport = %#v", out[runtime.ProcessReportResultDataKey])
	}
	if err := runtime.ValidateProcessReport(report); err != nil {
		t.Fatal(err)
	}
	if withProcessReport(nil)[runtime.ProcessReportResultDataKey] == nil {
		t.Fatal("a result without data carries no report")
	}
}
