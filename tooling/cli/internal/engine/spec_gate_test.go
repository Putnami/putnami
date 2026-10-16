package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/specgate"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// specGateFixture builds the on-disk shape a completed session leaves behind:
// one spec-owning project whose criteria command emitted a criteria
// projection and whose verification command emitted an observation report.
type specGateFixture struct {
	ws      *workspace.Workspace
	planned []*jobs.ScheduledJob
	results map[string]*jobs.JobResult
}

func newSpecGateFixture(t *testing.T, observationStatus string) *specGateFixture {
	t.Helper()
	root := t.TempDir()
	project := &workspace.Project{ID: "/billing", Name: "@acme/billing", Path: "billing",
		Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "gate"}, []*workspace.Project{project})

	writeSpecGateFile(t, filepath.Join(root, "billing", "invoice_test.go"), "package billing\n")
	writeSpecGateFile(t, filepath.Join(root, ".putnami", "out", "billing", "validate", "spec-criteria.json"), `{
  "protocolVersion": 1,
  "groups": [{
    "feature": "billing/invoicing",
    "spec": "billing/specs/invoicing.json",
    "specRequirements": ["issue"],
    "requirements": [{
      "id": "issue", "stage": "coded", "evidenceKinds": ["attestation"],
      "verification": {"kind": "acceptance", "checks": ["issue-check"]}
    }]
  }]
}`)
	writeSpecGateFile(t, filepath.Join(root, ".putnami", "out", "billing", "test", "putnami-feature-verification.json"), `{
  "protocolVersion": 1,
  "observations": [{
    "feature": "billing/invoicing", "requirement": "issue", "check": "issue-check",
    "status": "`+observationStatus+`",
    "provenance": {"path": "invoice_test.go", "symbol": "TestInvoiceIssued"}
  }]
}`)

	validate := &jobs.ScheduledJob{Project: project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/sdd"},
		JobDef: &extension.JobDefinition{Name: "validate~specs", CommandName: "validate",
			Traits: extension.CommandTraits{Kind: extensionproto.CommandKindCriteria}}}
	test := &jobs.ScheduledJob{Project: project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef: &extension.JobDefinition{Name: "test", CommandName: "test",
			Traits: extension.CommandTraits{Kind: extensionproto.CommandKindVerification}}}
	return &specGateFixture{
		ws:      ws,
		planned: []*jobs.ScheduledJob{validate, test},
		results: map[string]*jobs.JobResult{
			validate.Key(): specGateResult("success", features.SpecCriteriaProjectionArtifactID, "spec-criteria.json"),
			test.Key():     specGateResult("success", features.VerificationReportArtifactID, "putnami-feature-verification.json"),
		},
	}
}

func (f *specGateFixture) setProjectMode(mode string) {
	f.ws.Projects[0].Config.Options = map[string]map[string]any{
		"sdd": {"verification": map[string]any{"specs": mode}},
	}
}

func specGateResult(status, id, path string) *jobs.JobResult {
	return &jobs.JobResult{Status: status, Events: []jobs.RawJobEvent{{
		Type: jobs.EventTypeArtifact,
		Data: map[string]any{"id": id, "name": id, "kind": "report", "path": path},
	}}}
}

func writeSpecGateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSpecGateFinalizer_AttachmentConditions pins when the gate exists at
// all: only sessions naming a verification command (here `test`) carry it,
// off-everywhere attaches no collector, and a run without a workspace has
// nothing to attach to.
func TestSpecGateFinalizer_AttachmentConditions(t *testing.T) {
	t.Parallel()
	fixture := newSpecGateFixture(t, "passed")

	if specGateFinalizer(&Request{Commands: []string{"build", "lint"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, &specGateOutcome{}) != nil {
		t.Error("a session without test carried the spec gate")
	}
	if specGateFinalizer(&Request{Commands: []string{"test"}}, nil, nil, []string{"test"}, nil, nil, &specGateOutcome{}) != nil {
		t.Error("a run with no loaded workspace carried the spec gate")
	}
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, &specGateOutcome{}) == nil {
		t.Error("a test session under the default report mode carried no spec gate")
	}
	// One project going off does NOT disarm the workspace: the built-in default
	// stays report for everyone else, so the collector still attaches.
	fixture.setProjectMode("off")
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, &specGateOutcome{}) == nil {
		t.Error("a single project's off disarmed the whole workspace's gate")
	}
	// Off EVERYWHERE — the committed workspace policy plus no overriding
	// project — attaches no collector at all.
	fixture.ws.Config.Options = map[string]map[string]any{"sdd": {"verification": map[string]any{"specs": "off"}}}
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, &specGateOutcome{}) != nil {
		t.Error("off everywhere still attached the collector")
	}
	// A project override back on re-attaches even under a workspace off.
	fixture.setProjectMode("enforce")
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, &specGateOutcome{}) == nil {
		t.Error("a committed enforce override under a workspace off attached nothing")
	}
	// A watch iteration replans through Engine.Run and rebuilds the finalizer
	// list, so an iteration request attaches the gate exactly as a terminal
	// run does — watch parity costs no special case.
	iteration := &Request{Commands: []string{"test"}, watchIteration: true}
	if specGateFinalizer(iteration, fixture.ws, fixture.planned, []string{"test"}, nil, nil, &specGateOutcome{}) == nil {
		t.Error("a watch iteration carried no spec gate")
	}
}

// TestSpecGateFinalizer_EnforceSanctionsThroughTheReducer is the gate's
// midpoint contract: enforce adds exactly one failed synthetic result for the
// spec-owning project, keyed so the reduction attributes it there, and only
// for an unresolved requirement.
func TestSpecGateFinalizer_EnforceSanctionsThroughTheReducer(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "recorded-run-context", "declared-check-failures-carry-a-code")
	t.Parallel()
	for name, tc := range map[string]struct {
		status  string
		blocked bool
	}{
		"verified passes":      {"passed", false},
		"failed check blocks":  {"failed", true},
		"skipped check blocks": {"skipped", true},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSpecGateFixture(t, tc.status)
			fixture.setProjectMode("enforce")
			outcome := &specGateOutcome{}
			finalize := specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, outcome)
			if finalize == nil {
				t.Fatal("enforce session carried no finalizer")
			}
			finalize(fixture.results)

			synthetic, sanctioned := fixture.results["/billing:specs~verify"]
			if sanctioned != tc.blocked {
				t.Fatalf("sanctioned = %v, want %v (results %v)", sanctioned, tc.blocked, keysOf(fixture.results))
			}
			if tc.blocked {
				if synthetic.Status != "failed" || synthetic.Error == nil ||
					!strings.Contains(synthetic.Error.Message, "putnami specs verify") {
					t.Errorf("synthetic result = %+v; it must fail and point at the audit surface", synthetic)
				}
				if synthetic.Canonical == nil || len(synthetic.Canonical.Diagnostics) != 1 ||
					synthetic.Canonical.Diagnostics[0].Code != features.ErrorCodeUnresolvedRequirement {
					t.Errorf("synthetic result carries no %s diagnostic: %+v",
						features.ErrorCodeUnresolvedRequirement, synthetic.Canonical)
				}
			}
			if outcome.record == nil || len(outcome.record.Groups) != 1 {
				t.Fatalf("the decided record was not stashed: %+v", outcome.record)
			}
		})
	}
}

// TestSpecGateFinalizer_ReportNeverChangesACleanExit pins the default mode's
// half of the tri-state: everything is evaluated and retained, and nothing is
// added to the result map whatever was found.
func TestSpecGateFinalizer_ReportNeverChangesACleanExit(t *testing.T) {
	t.Parallel()
	fixture := newSpecGateFixture(t, "failed")
	outcome := &specGateOutcome{}
	finalize := specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, outcome)
	if finalize == nil {
		t.Fatal("default report session carried no finalizer")
	}
	before := len(fixture.results)
	finalize(fixture.results)
	if len(fixture.results) != before {
		t.Fatalf("report mode changed the result map: %v", keysOf(fixture.results))
	}
	if outcome.record == nil || outcome.record.Groups[0].Requirements[0].State != features.RequirementContradicted {
		t.Fatalf("report mode did not retain the contradicted verdict: %+v", outcome.record)
	}
}

// TestSpecGateFinalizer_IncompleteSessionsGainNoSecondaryFailures pins the
// rule that a failed or canceled test run keeps its own verdict: available
// observations are still collected and persisted, but no missing-report
// sanction is stacked on top of the original failure.
func TestSpecGateFinalizer_IncompleteSessionsGainNoSecondaryFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			fixture := newSpecGateFixture(t, "skipped")
			fixture.setProjectMode("enforce")
			fixture.results[fixture.planned[1].Key()].Status = status
			outcome := &specGateOutcome{}
			finalize := specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, []string{"test"}, nil, nil, outcome)
			finalize(fixture.results)

			if _, sanctioned := fixture.results["/billing:specs~verify"]; sanctioned {
				t.Error("an incomplete session acquired a secondary spec-verification failure")
			}
			if outcome.record == nil {
				t.Error("the incomplete session's observations were not retained for the audit surface")
			}
		})
	}
}

// TestSpecGateFinalizer_TruncatedSessionsWarnAboutNothing pins the second half
// of the incomplete-session rule: a run whose work failed or was canceled gains
// no secondary failure AND says nothing about its evidence scope.
//
// The distinction matters because the two look identical in the record. When a
// canceled test never reports, every requirement it attests reads as
// never-observed, and the exhausted-scope message would tell a reader "no test
// proves this yet — add a spectest.Proves call" about requirements whose tests
// exist and simply did not run. The failing job is already on screen and is the
// fact that explains the absence.
//
// It cannot be parallel: captureStderr swaps the process-wide os.Stderr.
func TestSpecGateFinalizer_TruncatedSessionsWarnAboutNothing(t *testing.T) {
	// unobserved builds a session whose test job reported no observation at
	// all, which is what makes the group's only requirement check-not-observed.
	unobserved := func(t *testing.T, status string) *specGateFixture {
		fixture := newSpecGateFixture(t, "passed")
		fixture.setProjectMode("enforce")
		fixture.results[fixture.planned[1].Key()] = &jobs.JobResult{Status: status}
		return fixture
	}
	// A non-nil recovery is what arms the scope diagnostics at all; production
	// always has one. exhaustedRecovery is the answer a real lookup gives when
	// every candidate was already in the session: nothing recovered, nobody
	// unconsulted.
	run := func(t *testing.T, fixture *specGateFixture) string {
		t.Helper()
		return captureStderr(t, func() {
			finalize := specGateFinalizer(&Request{Commands: []string{"test"}},
				fixture.ws, fixture.planned, []string{"test"}, exhaustedRecovery{}, nil, &specGateOutcome{})
			finalize(fixture.results)
		})
	}

	// The control: the same absent observation on a session that SUCCEEDED is
	// a real finding and must be reported, or this test would pass on a gate
	// that never warns at all.
	t.Run("success", func(t *testing.T) {
		if stderr := run(t, unobserved(t, "success")); !strings.Contains(stderr, "spec verification") {
			t.Errorf("a completed session said nothing about its unobserved requirement; stderr = %q", stderr)
		}
	})

	for _, status := range []string{"failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			fixture := unobserved(t, status)
			stderr := run(t, fixture)
			if strings.Contains(stderr, "spec verification") {
				t.Errorf("a %s session advised the reader about an evidence scope it never reached; stderr = %q",
					status, stderr)
			}
			if _, sanctioned := fixture.results["/billing:specs~verify"]; sanctioned {
				t.Errorf("a %s session acquired a secondary spec-verification failure", status)
			}
		})
	}
}

// exhaustedRecovery answers every candidate set with silence, which is the
// shape of a run whose evidence scope holds no project it did not already plan.
type exhaustedRecovery struct{}

func (exhaustedRecovery) RecoverReports([]*workspace.Project) specgate.Recovery {
	return specgate.Recovery{}
}

func keysOf(results map[string]*jobs.JobResult) []string {
	keys := make([]string, 0, len(results))
	for key := range results {
		keys = append(keys, key)
	}
	return keys
}

// projectionProducerExtension is an SDD-shaped extension: a pipeline step of
// the criteria command `validate` whose task declares the reserved criteria
// projection, active in a project that holds specs/*.json.
func projectionProducerExtension() *extension.ExtensionDescription {
	enabled := true
	return &extension.ExtensionDescription{
		Name:    "@acme/sdd",
		Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{
			"validate": {
				ExtensionName:   "@acme/sdd",
				Name:            "validate",
				Kind:            "command",
				Command:         "sdd",
				ActivationFiles: []string{"specs/*.json"},
				PipelineSteps:   []extension.PipelineStep{{ID: "specs", Task: "specs-validate"}},
				Traits:          extension.CommandTraits{Kind: extensionproto.CommandKindCriteria},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"specs-validate": {
				Kind: "command", Command: "sdd",
				Cache: &extension.TaskCachePolicy{Enabled: &enabled},
				Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"criteria": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput,
						Path: features.SpecCriteriaProjectionFilename, OptionalEmpty: true},
				}},
			},
		},
	}
}

// TestProjectionProducerPlanAnswersFromTheWholeExtensionSet pins the engine half
// of the empty-evaluation rule: a producer applies by the planner's activation
// rule alone, the session's disable lists never hide one, and a project that
// disables validate itself leaves the answer unknown.
func TestProjectionProducerPlanAnswersFromTheWholeExtensionSet(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSpecGateFile(t, filepath.Join(root, "billing", "specs", "invoicing.json"), "{}")
	writeSpecGateFile(t, filepath.Join(root, "ledger", "ledger.go"), "package ledger\n")
	writeSpecGateFile(t, filepath.Join(root, "audit", "specs", "audit.json"), "{}")
	billing := &workspace.Project{ID: "/billing", Name: "billing", Path: "billing",
		Extensions: []string{"@acme/sdd"}, Config: &wsproto.ProjectConfig{}}
	ledger := &workspace.Project{ID: "/ledger", Name: "ledger", Path: "ledger",
		Extensions: []string{"@acme/sdd"}, Config: &wsproto.ProjectConfig{}}
	audit := &workspace.Project{ID: "/audit", Name: "audit", Path: "audit",
		Extensions: []string{"@acme/sdd"}, Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "gate"}, []*workspace.Project{billing, ledger, audit})
	req := &Request{Commands: []string{"test"}, Config: &wsproto.Config{Disable: &wsproto.DisableConfig{
		Jobs: []string{"validate"}, Extensions: []string{"@acme/sdd"},
	}}}
	plan := &projectionProducerPlan{req: req, ws: ws, projects: []*workspace.Project{billing, ledger},
		extensions: []*extension.ExtensionDescription{projectionProducerExtension()}}

	applicable, err := plan.ApplicableProjects()
	if err != nil {
		t.Fatalf("ApplicableProjects: %v", err)
	}
	if !applicable["/billing"] || applicable["/ledger"] || applicable["/audit"] || len(applicable) != 1 {
		t.Fatalf("applicable = %v, want only the selected project that holds a spec", applicable)
	}

	plan.skipped = []extension.SkippedExtension{{Ref: "@acme/other", Name: "@acme/other"}}
	if _, err := plan.ApplicableProjects(); err == nil {
		t.Fatal("a discovery that skipped an extension answered applicability")
	}
	plan.skipped = nil

	req.Config.Extensions.List = map[string]string{"@acme/sdd": "", "@acme/other": ""}
	req.Config.Disable.Extensions = []string{"@acme/sdd", "@acme/other"}
	if _, err := plan.ApplicableProjects(); err == nil {
		t.Fatal("a configured registry extension that is not loaded answered applicability")
	}
	req.Config.Extensions.List = map[string]string{"@acme/sdd": ""}
	if _, err := plan.ApplicableProjects(); err != nil {
		t.Fatalf("ApplicableProjects with every configured extension loaded: %v", err)
	}

	billing.Config.Disable = &wsproto.ProjectDisableConfig{Jobs: []string{"@acme/sdd:validate"}}
	if _, err := plan.ApplicableProjects(); err == nil {
		t.Fatal("a selected project that disables validate answered applicability")
	}
}

// reportCommandExtension is a third-party extension whose `check` command
// declares the verification kind and whose pipeline step declares the reserved
// verification report: a report producer under a command that is not `test`.
func reportCommandExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name:    "@acme/check",
		Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{
			"check": {
				ExtensionName: "@acme/check",
				Name:          "check",
				Kind:          "command",
				Command:       "check",
				PipelineSteps: []extension.PipelineStep{{ID: "run", Task: "check-run"}},
				Traits:        extension.CommandTraits{Kind: extensionproto.CommandKindVerification},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"check-run": {
				Kind: "command", Command: "check",
				Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"report": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput,
						Path: features.VerificationReportFilename, OptionalEmpty: true},
				}},
			},
		},
	}
}

// TestSpecGateFinalizer_AVerificationKindCommandCarriesTheGate pins the kind
// rule at the gate: a session naming a command whose resolved traits declare
// the verification kind carries the gate whatever the command is called, reads
// its declaring task's observations, and sanctions an unresolved requirement
// under enforce. A session naming a `test` command whose manifest declared
// traits without that kind carries none.
func TestSpecGateFinalizer_AVerificationKindCommandCarriesTheGate(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-verification-producers",
		"a-verification-kind-command-carries-the-gate")
	t.Parallel()
	for name, tc := range map[string]struct {
		status  string
		blocked bool
		state   features.RequirementVerificationState
	}{
		"verified passes":     {"passed", false, features.RequirementVerified},
		"failed check blocks": {"failed", true, features.RequirementContradicted},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newSpecGateFixture(t, tc.status)
			fixture.setProjectMode("enforce")
			ext := reportCommandExtension()
			kindless := &extension.ExtensionDescription{Name: "@acme/legacy", Jobs: map[string]*extension.JobDefinition{
				"test": {Name: "test", Kind: "command", TraitsDeclared: true, Traits: extension.CommandTraits{Heavy: true}},
			}}
			verification := jobs.CommandsOfKind([]*extension.ExtensionDescription{kindless, ext},
				extension.CommandKindVerification)
			if len(verification) != 1 || verification[0] != "check" {
				t.Fatalf("verification commands = %v, want [check]", verification)
			}
			// The report now sits in the check command's output tree, written by
			// a step whose task declares it.
			outRoot := filepath.Join(fixture.ws.Root, ".putnami", "out", "billing")
			if err := os.Rename(filepath.Join(outRoot, "test"), filepath.Join(outRoot, "check")); err != nil {
				t.Fatalf("move report: %v", err)
			}
			check := &jobs.ScheduledJob{Project: fixture.ws.Projects[0], Extension: ext,
				JobDef: &extension.JobDefinition{Name: "check~run", CommandName: "check",
					Traits: ext.Jobs["check"].Traits},
				Step: &ext.Jobs["check"].PipelineSteps[0]}
			delete(fixture.results, fixture.planned[1].Key())
			fixture.planned[1] = check
			fixture.results[check.Key()] = specGateResult("success", features.VerificationReportArtifactID,
				features.VerificationReportFilename)

			if specGateFinalizer(&Request{Commands: []string{"validate", "build"}}, fixture.ws, fixture.planned,
				verification, nil, nil, &specGateOutcome{}) != nil {
				t.Error("a session naming no verification command carried the gate")
			}
			if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, verification,
				nil, nil, &specGateOutcome{}) != nil {
				t.Error("a session naming a test command without the verification kind carried the gate")
			}
			outcome := &specGateOutcome{}
			finalize := specGateFinalizer(&Request{Commands: []string{"validate", "check"}}, fixture.ws, fixture.planned,
				verification, nil, nil, outcome)
			if finalize == nil {
				t.Fatal("a session naming the verification command carried no gate")
			}
			finalize(fixture.results)

			if _, sanctioned := fixture.results["/billing:specs~verify"]; sanctioned != tc.blocked {
				t.Fatalf("sanctioned = %v, want %v (results %v)", sanctioned, tc.blocked, keysOf(fixture.results))
			}
			if outcome.record == nil || len(outcome.record.Groups) != 1 ||
				outcome.record.Groups[0].Requirements[0].State != tc.state {
				t.Fatalf("record = %+v, want one group whose requirement is %s from the check report",
					outcome.record, tc.state)
			}
		})
	}
}

// TestProjectionProducerPlanFollowsTheDeclaringCommand pins applicability under
// a projection producer that is not `validate`: the plan covers the criteria
// command whose task declares the projection, a project's disable list hides a
// producer only when it names that command, and a workspace with no producer
// command answers that no producer applies. A `validate` command without the
// criteria kind is no producer command, even when its task declares the
// projection.
func TestProjectionProducerPlanFollowsTheDeclaringCommand(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-verification-producers",
		"projection-applicability-plans-the-declaring-commands")
	t.Parallel()
	root := t.TempDir()
	writeSpecGateFile(t, filepath.Join(root, "billing", "specs", "invoicing.json"), "{}")
	writeSpecGateFile(t, filepath.Join(root, "ledger", "ledger.go"), "package ledger\n")
	billing := &workspace.Project{ID: "/billing", Name: "billing", Path: "billing",
		Extensions: []string{"@acme/sdd"}, Config: &wsproto.ProjectConfig{}}
	ledger := &workspace.Project{ID: "/ledger", Name: "ledger", Path: "ledger",
		Extensions: []string{"@acme/sdd"}, Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "gate"}, []*workspace.Project{billing, ledger})

	sdd := projectionProducerExtension()
	criteria := sdd.Jobs["validate"]
	criteria.Name = "criteria"
	sdd.Jobs = map[string]*extension.JobDefinition{"criteria": criteria}
	plan := &projectionProducerPlan{req: &Request{Commands: []string{"criteria"}}, ws: ws,
		projects:   []*workspace.Project{billing, ledger},
		extensions: []*extension.ExtensionDescription{sdd}}

	billing.Config.Disable = &wsproto.ProjectDisableConfig{Jobs: []string{"validate"}}
	applicable, err := plan.ApplicableProjects()
	if err != nil {
		t.Fatalf("ApplicableProjects with validate disabled and the producer under criteria: %v", err)
	}
	if !applicable["/billing"] || len(applicable) != 1 {
		t.Fatalf("applicable = %v, want only the selected project that holds a spec", applicable)
	}

	billing.Config.Disable = &wsproto.ProjectDisableConfig{Jobs: []string{"@acme/sdd:criteria"}}
	if _, err := plan.ApplicableProjects(); err == nil || !strings.Contains(err.Error(), "disables criteria") {
		t.Fatalf("a selected project that disables the producer command answered applicability (err %v)", err)
	}

	kindless := projectionProducerExtension()
	kindless.Jobs["validate"].Traits = extension.CommandTraits{}
	for _, extensions := range [][]*extension.ExtensionDescription{{reportCommandExtension()}, {kindless}} {
		plan.extensions = extensions
		billing.Config.Disable = &wsproto.ProjectDisableConfig{Jobs: []string{"validate"}}
		applicable, err = plan.ApplicableProjects()
		if err != nil || len(applicable) != 0 {
			t.Fatalf("with no projection producer: applicable = %v, err = %v; want none and no error", applicable, err)
		}
	}
}
