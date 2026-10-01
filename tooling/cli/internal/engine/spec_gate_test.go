package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/extension"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/specgate"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// specGateFixture builds the on-disk shape a completed session leaves behind:
// one spec-owning project whose validate step declared a criteria projection
// and whose test step declared an observation report.
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
		JobDef:    &extension.JobDefinition{Name: "validate~specs", CommandName: "validate"}}
	test := &jobs.ScheduledJob{Project: project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef:    &extension.JobDefinition{Name: "test", CommandName: "test"}}
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
// all: only sessions containing test carry it, off-everywhere attaches no
// collector, and a run without a workspace has nothing to attach to.
func TestSpecGateFinalizer_AttachmentConditions(t *testing.T) {
	t.Parallel()
	fixture := newSpecGateFixture(t, "passed")

	if specGateFinalizer(&Request{Commands: []string{"build", "lint"}}, fixture.ws, fixture.planned, nil, &specGateOutcome{}) != nil {
		t.Error("a session without test carried the spec gate")
	}
	if specGateFinalizer(&Request{Commands: []string{"test"}}, nil, nil, nil, &specGateOutcome{}) != nil {
		t.Error("a run with no loaded workspace carried the spec gate")
	}
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, nil, &specGateOutcome{}) == nil {
		t.Error("a test session under the default report mode carried no spec gate")
	}
	// One project going off does NOT disarm the workspace: the built-in default
	// stays report for everyone else, so the collector still attaches.
	fixture.setProjectMode("off")
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, nil, &specGateOutcome{}) == nil {
		t.Error("a single project's off disarmed the whole workspace's gate")
	}
	// Off EVERYWHERE — the committed workspace policy plus no overriding
	// project — attaches no collector at all.
	fixture.ws.Config.Options = map[string]map[string]any{"sdd": {"verification": map[string]any{"specs": "off"}}}
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, nil, &specGateOutcome{}) != nil {
		t.Error("off everywhere still attached the collector")
	}
	// A project override back on re-attaches even under a workspace off.
	fixture.setProjectMode("enforce")
	if specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, nil, &specGateOutcome{}) == nil {
		t.Error("a committed enforce override under a workspace off attached nothing")
	}
	// A watch iteration replans through Engine.Run and rebuilds the finalizer
	// list, so an iteration request attaches the gate exactly as a terminal
	// run does — watch parity costs no special case.
	iteration := &Request{Commands: []string{"test"}, watchIteration: true}
	if specGateFinalizer(iteration, fixture.ws, fixture.planned, nil, &specGateOutcome{}) == nil {
		t.Error("a watch iteration carried no spec gate")
	}
}

// TestSpecGateFinalizer_EnforceSanctionsThroughTheReducer is the gate's
// midpoint contract: enforce adds exactly one failed synthetic result for the
// spec-owning project, keyed so the reduction attributes it there, and only
// for an unresolved requirement.
func TestSpecGateFinalizer_EnforceSanctionsThroughTheReducer(t *testing.T) {
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
			finalize := specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, nil, outcome)
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
	finalize := specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, nil, outcome)
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
			finalize := specGateFinalizer(&Request{Commands: []string{"test"}}, fixture.ws, fixture.planned, nil, outcome)
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
				fixture.ws, fixture.planned, exhaustedRecovery{}, &specGateOutcome{})
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
