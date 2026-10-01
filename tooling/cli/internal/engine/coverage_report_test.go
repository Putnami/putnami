package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

func TestExecuteWritesCoverageReportOnlyAfterSuccessfulValidation(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	f.req.Commands = []string{"test"}
	f.req.CommandParams = extension.ParamMap{"enforce-coverage": true}
	f.planned[0].JobDef.Name = "test"
	injectCoverage := func(results map[string]*jobs.JobResult) {
		results[f.planned[0].Key()].Data = extension.ParamMap{
			"coverageSummary": extension.ParamMap{
				"percentage":  81.5,
				"granularity": runtimeproto.CoverageStatements,
				"covered":     163,
				"total":       200,
			},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project},
		&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}},
		f.planned, nil, nil, injectCoverage)
	if result.ExitCode != ExitSuccess {
		t.Fatalf("exit code = %d, want success", result.ExitCode)
	}

	store := workspace_state.NewSessionStore(f.wsRoot)
	ids, err := store.List()
	if err != nil || len(ids) != 1 {
		t.Fatalf("sessions = %v, err=%v", ids, err)
	}
	data, err := os.ReadFile(filepath.Join(store.Root(), ids[0], "report.json"))
	if err != nil {
		t.Fatalf("validation report was not written: %v", err)
	}
	var report workspace_state.SessionReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if len(report.Coverage) != 1 || report.Coverage[0].Summary.Percentage != 81.5 {
		t.Fatalf("report coverage = %+v", report.Coverage)
	}
}
