package engine

import (
	"testing"

	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// runPlanRecoveryFixture is the recovery fixture with a second project,
// /notes, that no extension serves: a run that selects it plans no task for
// it, so nothing can write its verification report.
func runPlanRecoveryFixture(t *testing.T) (*recoveryPlanFixture, *workspace.Project) {
	t.Helper()
	fixture := newRecoveryPlanFixture(t, false)
	notes := &workspace.Project{ID: "/notes", Name: "@acme/notes", Path: "notes", Config: &wsproto.ProjectConfig{}}
	fixture.ws = workspace.NewWorkspace(fixture.ws.Root, fixture.ws.Config, append(fixture.ws.Projects, notes))
	fixture.ws.Graph = workspace.BuildGraph(fixture.ws.Projects)
	fixture.source.ws = fixture.ws
	return fixture, notes
}

// runRecovery is the fixture's recovery source as a run's spec gate builds
// it, over the run's plan planned and its selection selected.
func (f *recoveryPlanFixture) runRecovery(t *testing.T, planned []*scheduler.ScheduledJob,
	selected []*workspace.Project) *cachedObservationRecovery {
	t.Helper()
	return runObservationRecovery(t.Context(), f.source.req, f.ws, f.source.extensions, f.cache, planned, selected)
}

// TestRecoverReportsAnswersFromTheRunPlan pins the reuse: a candidate the run
// selected under every verification command, and for which the run planned no
// task that can write the report, is answered from the run's plan, consulted
// and silent, with no second plan. A candidate the run did not select is
// still planned and keyed, and returns its cached report.
func TestRecoverReportsAnswersFromTheRunPlan(t *testing.T) {
	t.Parallel()
	fixture, notes := runPlanRecoveryFixture(t)
	fixture.publish(t, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)
	billing := fixture.ws.ProjectByID(recoveryProjectID)

	// The run selected /notes only: its plan holds nothing for /billing.
	source := fixture.runRecovery(t, nil, []*workspace.Project{notes})
	recovery := source.RecoverReports([]*workspace.Project{notes})
	if len(recovery.Reports) != 0 || len(recovery.Unconsulted) != 0 {
		t.Errorf("recovery = %+v, want the selected candidate consulted and silent", recovery)
	}
	if plans := source.plans.Load(); plans != 0 {
		t.Fatalf("a candidate the run's plan answers was planned again (%d plans)", plans)
	}

	recovery = source.RecoverReports([]*workspace.Project{billing, notes})
	if plans := source.plans.Load(); plans != 1 {
		t.Fatalf("plans = %d, want one plan for the candidate the run did not select", plans)
	}
	if len(recovery.Reports) != 1 || recovery.Reports[0].Project != recoveryProjectID || len(recovery.Unconsulted) != 0 {
		t.Errorf("recovery = %+v, want the unselected candidate's cached report and nothing unconsulted", recovery)
	}
}

// TestRecoverReportsPlansTheCandidatesTheRunPlanCannotAnswer pins the cases where the
// run's plan could differ from a recovery plan, and every candidate is
// planned as before: the run did not plan every verification command, it
// planned from a measured diff, or it is a lifecycle run. A selected candidate
// whose producer the run planned is planned too, since only a plan keys it.
func TestRecoverReportsPlansTheCandidatesTheRunPlanCannotAnswer(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Request){
		"another command": func(req *Request) { req.Commands = []string{"build"} },
		"a measured diff": func(req *Request) { req.selectionEvidence.changedPaths = []string{} },
		"a lifecycle run": func(req *Request) { req.WorkspaceLifecycle = true },
		"task scopes": func(req *Request) {
			req.selectionEvidence.taskScopes = map[string][]string{"/notes": {"/notes"}}
		},
		"an executing dry run": func(req *Request) {
			req.ExecutesUnderDryRun, req.CommandParams = true, extension.ParamMap{"dry-run": true}
		},
		"a planned report": func(*Request) {},
	}
	for name, adjust := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture, notes := runPlanRecoveryFixture(t)
			adjust(fixture.source.req)
			candidates := []*workspace.Project{notes}
			planned := []*scheduler.ScheduledJob(nil)
			if name == "a planned report" {
				candidates, planned = []*workspace.Project{fixture.ws.ProjectByID(recoveryProjectID)}, fixture.planned
			}
			source := fixture.runRecovery(t, planned, fixture.ws.Projects)
			source.RecoverReports(candidates)
			if plans := source.plans.Load(); plans != 1 {
				t.Errorf("plans = %d, want the candidates planned again", plans)
			}
		})
	}
}

// TestRecoverReportsPlansACandidateOncePerGate pins the memo: spec owners that
// name the same unselected candidate share its one plan, and each call still
// answers for it.
func TestRecoverReportsPlansACandidateOncePerGate(t *testing.T) {
	t.Parallel()
	fixture, notes := runPlanRecoveryFixture(t)
	fixture.publish(t, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)
	billing := fixture.ws.ProjectByID(recoveryProjectID)
	source := fixture.runRecovery(t, nil, []*workspace.Project{notes})

	for owner := 0; owner < 3; owner++ {
		recovery := source.RecoverReports([]*workspace.Project{billing, notes})
		if len(recovery.Reports) != 1 || recovery.Reports[0].Project != recoveryProjectID || len(recovery.Unconsulted) != 0 {
			t.Fatalf("call %d: recovery = %+v, want the candidate's cached report", owner, recovery)
		}
	}
	if plans := source.plans.Load(); plans != 1 {
		t.Errorf("plans = %d, want one plan for a candidate three owners name", plans)
	}
}

// TestRecoverReportsReportsEveryCandidateWhenThePlanFails pins the failure
// path: a plan that cannot be derived leaves every candidate of the call
// unconsulted, the ones the run's plan answers included.
func TestRecoverReportsReportsEveryCandidateWhenThePlanFails(t *testing.T) {
	t.Parallel()
	fixture, notes := runPlanRecoveryFixture(t)
	billing := fixture.ws.ProjectByID(recoveryProjectID)
	source := fixture.runRecovery(t, nil, []*workspace.Project{notes})
	// Two tasks of the candidate's pipeline claiming one output break the
	// plan contract, so the recovery plan cannot be derived.
	generate := source.extensions[0].Tasks["lang-generate"]
	generate.Declares = &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"gen": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "gen"},
	}}
	source.extensions[0].Tasks["lang-generate"] = generate
	test := source.extensions[0].Tasks["lang-test"]
	test.Declares.Outputs["gen"] = extension.DeclaredOutput{Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "gen"}
	source.extensions[0].Tasks["lang-test"] = test

	recovery := source.RecoverReports([]*workspace.Project{billing, notes})
	if len(recovery.Unconsulted) != 2 {
		t.Fatalf("unconsulted = %+v, want both candidates", recovery.Unconsulted)
	}
	for _, entry := range recovery.Unconsulted {
		if entry.Code != features.UnconsultedPlanFailed {
			t.Errorf("%s: code = %s, want %s", entry.Project, entry.Code, features.UnconsultedPlanFailed)
		}
	}
}
