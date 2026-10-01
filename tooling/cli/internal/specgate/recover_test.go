package specgate

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// The fixture: a spec-owning project (billing) whose one executable
// requirement is attested by a test that lives in the LIBRARY it depends on
// (ledger). billing's own test job is planned; ledger's is not, which is
// exactly what a narrowed selection produces.
const attestedProjection = `{
  "protocolVersion": 1,
  "groups": [
    {
      "feature": "billing/invoicing",
      "spec": "billing/specs/invoicing.json",
      "specRequirements": ["issue"],
      "requirements": [
        {
          "id": "issue",
          "stage": "coded",
          "evidenceKinds": ["attestation"],
          "verification": {"kind": "acceptance", "checks": ["issue-check"]}
        }
      ]
    }
  ]
}`

func attestingReport(status, provenance string) string {
	return `{
  "protocolVersion": 1,
  "observations": [
    {
      "feature": "billing/invoicing",
      "requirement": "issue",
      "check": "issue-check",
      "status": "` + status + `",
      "provenance": {"path": "` + provenance + `", "symbol": "TestLedgerIssues"}
    }
  ]
}`
}

// attestedFixture lays out the two-project workspace and the billing session
// that planned validate+test for billing only.
type attestedFixture struct {
	ws      *workspace.Workspace
	planned []*jobs.ScheduledJob
	results map[string]*jobs.JobResult
	root    string
}

func newAttestedFixture(t *testing.T) *attestedFixture {
	t.Helper()
	root := t.TempDir()
	ledger := &workspace.Project{ID: "/ledger", Name: "@acme/ledger", Path: "ledger",
		Config: &wsproto.ProjectConfig{}}
	billing := &workspace.Project{ID: "/billing", Name: "@acme/billing", Path: "billing",
		Dependencies: []string{"@acme/ledger"}, Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "gate"},
		[]*workspace.Project{ledger, billing})

	// The declaration the restored observation names has to be a real regular
	// file of the LEDGER project, or provenance containment drops it.
	writeFile(t, filepath.Join(root, "ledger", "ledger_test.go"), "package ledger\n")
	writeFile(t, filepath.Join(root, ".putnami", "out", "billing", "validate", "spec-criteria.json"),
		attestedProjection)

	validate := &jobs.ScheduledJob{Project: billing,
		Extension: &extension.ExtensionDescription{Name: "@putnami/sdd"},
		JobDef:    &extension.JobDefinition{Name: "validate~specs", CommandName: "validate"}}
	billingTest := testStep(billing, "test", true)

	fixture := &attestedFixture{ws: ws, root: root,
		planned: []*jobs.ScheduledJob{validate, billingTest},
		results: map[string]*jobs.JobResult{
			validate.Key(): resultWithArtifact("success",
				features.SpecCriteriaProjectionArtifactID, "spec-criteria.json"),
			// billing's own test ran and reported nothing: the report artifact is
			// simply absent, which is what a project with no bound test leaves.
			billingTest.Key(): {Status: "success"},
		}}
	billing.Config.Options = map[string]map[string]any{
		"sdd": {"verification": map[string]any{"specs": "enforce"}},
	}
	return fixture
}

// testStep builds one planned `test` pipeline step the way the planner expands
// it: the job points at its step, and the step's task carries the declaration.
// reports says whether that task declares the verification report, which is
// what makes the job its project's report producer.
func testStep(project *workspace.Project, step string, reports bool) *jobs.ScheduledJob {
	declaration := &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{}}
	if reports {
		declaration.Outputs["featureVerification"] = extension.DeclaredOutput{
			Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput,
			Path: features.VerificationReportFilename, OptionalEmpty: true}
	}
	task := step + "-exec"
	return &jobs.ScheduledJob{Project: project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/go",
			Tasks: map[string]extension.TaskDefinition{task: {Kind: "command", Declares: declaration}}},
		Step:   &extension.PipelineStep{ID: step, Task: task},
		JobDef: &extension.JobDefinition{Name: "test~" + step, CommandName: "test", StepID: step}}
}

// rebuildGraph re-derives the dependency graph after a test rewires the edges.
// NewWorkspace is the only constructor that builds it, so the fixture rebuilds
// through it rather than reaching into the model.
func (f *attestedFixture) rebuildGraph() {
	f.ws = workspace.NewWorkspace(f.ws.Root, f.ws.Config, f.ws.Projects)
}

func (f *attestedFixture) collect(t *testing.T, recovery ObservationRecovery) *features.SpecVerificationRecord {
	t.Helper()
	record, err := Collect(f.ws, f.planned, f.results,
		time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC), recovery)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if record == nil {
		t.Fatal("collect returned no record for a session that projected criteria")
	}
	return record
}

// stubRecovery stands in for the engine's cache lookup. It records what it was
// asked, so the tests can pin the candidate set and the lazy trigger without
// building a cache store.
type stubRecovery struct {
	answer Recovery
	asked  [][]string
}

func (s *stubRecovery) RecoverReports(candidates []*workspace.Project) Recovery {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	s.asked = append(s.asked, ids)
	return s.answer
}

// writeCachedReport writes a report where a cache entry's files/ directory
// would hold it and returns the RecoveredReport the engine would produce.
func (f *attestedFixture) writeCachedReport(t *testing.T, body string) RecoveredReport {
	t.Helper()
	path := filepath.Join(f.root, ".putnami", "cache", "blobs", "ab", "entry", "files", "featureVerification")
	writeFile(t, path, body)
	return RecoveredReport{
		Project: "/ledger",
		Task:    "/ledger:test~test",
		Path:    path,
		Field:   ".putnami/cache/blobs/ab/entry/files/featureVerification",
	}
}

// TestRecoveredObservationResolvesAnUnplannedAttester is the issue's primary
// case: the requirement's only attesting test lives in a dependency the
// selection did not plan, and the dependency's cached report resolves it.
func TestRecoveredObservationResolvesAnUnplannedAttester(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"an-unplanned-attester-resolves-from-its-cache-entry")
	fixture := newAttestedFixture(t)

	// Without recovery the check is exactly the bug: not observed, and blocking.
	blind := fixture.collect(t, nil)
	group := soleGroup(t, blind)
	if got := requirementState(t, group, "issue"); got != features.RequirementMissing {
		t.Fatalf("baseline issue = %s, want missing (the bug this fixture reproduces)", got)
	}
	if !group.Blocked {
		t.Fatal("baseline did not block; the fixture no longer reproduces the issue")
	}

	report := fixture.writeCachedReport(t, attestingReport("passed", "ledger_test.go"))
	stub := &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}}
	record := fixture.collect(t, stub)
	group = soleGroup(t, record)

	if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
		t.Fatalf("issue = %s, want verified from the dependency's cached observation", got)
	}
	if group.Blocked {
		t.Error("a requirement verified from cache still blocked")
	}
	if len(group.Reports) != 1 {
		t.Fatalf("report refs = %+v, want the one restored reference", group.Reports)
	}
	reference := group.Reports[0]
	if !reference.Restored {
		t.Error("the restored reference does not say it was restored")
	}
	if reference.Project != "/ledger" || reference.Task != "/ledger:test~test" {
		t.Errorf("reference = %+v, want the real reporting project and task", reference)
	}
	if !strings.HasPrefix(reference.Digest, "sha256:") {
		t.Errorf("reference digest = %q, want the exact bytes' content address", reference.Digest)
	}
	// The candidate set is the closure minus the projects whose test was
	// planned: billing planned its own test, so only ledger may be asked.
	if len(stub.asked) != 1 || len(stub.asked[0]) != 1 || stub.asked[0][0] != "/ledger" {
		t.Errorf("candidates asked = %v, want exactly [/ledger]", stub.asked)
	}
}

// TestPlannedPrerequisitesDoNotHideADependencyAttester pins that a narrowed run
// plans a dependency's `test~generate` and `test~config-merge` as prerequisites
// (`^generate`, `^config-merge`). Both carry command `test`, and neither runs
// the dependency's tests, so neither may drop it from the candidate set.
func TestPlannedPrerequisitesDoNotHideADependencyAttester(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"an-unplanned-attester-resolves-from-its-cache-entry")
	fixture := newAttestedFixture(t)
	ledger := fixture.ws.ProjectByID("/ledger")
	for _, step := range []string{"config-merge", "generate"} {
		prerequisite := testStep(ledger, step, false)
		fixture.planned = append(fixture.planned, prerequisite)
		fixture.results[prerequisite.Key()] = &jobs.JobResult{Status: "success"}
	}

	report := fixture.writeCachedReport(t, attestingReport("passed", "ledger_test.go"))
	stub := &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}}
	group := soleGroup(t, fixture.collect(t, stub))

	if len(stub.asked) != 1 || len(stub.asked[0]) != 1 || stub.asked[0][0] != "/ledger" {
		t.Fatalf("candidates asked = %v, want [/ledger]: a planned prerequisite is not a planned test", stub.asked)
	}
	if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
		t.Fatalf("issue = %s, want verified from the dependency's cached observation", got)
	}
	if group.Blocked {
		t.Error("a requirement its dependency attests still blocked the narrowed run")
	}
}

// TestRecoveredObservationResolvesADirectDependentAttester covers the case
// where the attesting project is a direct DEPENDENT of the spec owner, not a
// dependency. Every cross-project attestation in this repository has that shape
// — protocols/architecture's contract is proven by tooling/sdd-extension, and
// go/framework/api's by openapi/proto/grpc — so a candidate set built from the
// dependency closure alone resolves nothing at all.
func TestRecoveredObservationResolvesADirectDependentAttester(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"a-direct-dependent-attester-resolves-from-its-cache-entry")
	fixture := newAttestedFixture(t)
	// Flip the edge: the contract project (billing) is now depended UPON by the
	// implementation (ledger), which is where the attesting test lives.
	fixture.ws.Projects[1].Dependencies = nil
	fixture.ws.Projects[0].Dependencies = []string{"@acme/billing"}
	fixture.rebuildGraph()

	report := fixture.writeCachedReport(t, attestingReport("passed", "ledger_test.go"))
	stub := &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}}
	record := fixture.collect(t, stub)
	group := soleGroup(t, record)

	if len(stub.asked) != 1 || len(stub.asked[0]) != 1 || stub.asked[0][0] != "/ledger" {
		t.Fatalf("candidates asked = %v, want the direct dependent [/ledger]", stub.asked)
	}
	if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
		t.Fatalf("issue = %s, want verified from the direct dependent's cached observation", got)
	}
	if group.Blocked {
		t.Error("a requirement verified from a dependent's cache still blocked")
	}
}

// TestTransitiveDependentsStayOutOfScope pins the one radius the rule refuses.
// A dependent of a dependent exercises the owner's contract only through the
// layer between them, which is already a candidate; admitting the transitive
// set would key very nearly the whole workspace on a gate-recovery path.
func TestTransitiveDependentsStayOutOfScope(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"transitive-dependents-stay-out-of-the-candidate-set")
	fixture := newAttestedFixture(t)
	fixture.ws.Projects[1].Dependencies = nil
	fixture.ws.Projects[0].Dependencies = []string{"@acme/billing"}
	distant := &workspace.Project{ID: "/distant", Name: "@acme/distant", Path: "distant",
		Dependencies: []string{"@acme/ledger"}, Config: &wsproto.ProjectConfig{}}
	fixture.ws.Projects = append(fixture.ws.Projects, distant)
	fixture.rebuildGraph()

	stub := &stubRecovery{}
	fixture.collect(t, stub)
	if len(stub.asked) != 1 {
		t.Fatalf("recovery was consulted %d time(s), want once", len(stub.asked))
	}
	for _, id := range stub.asked[0] {
		if id == "/distant" {
			t.Fatalf("a transitive dependent entered the candidate set: %v", stub.asked[0])
		}
	}
}

// TestExhaustedScopeNamesItselfAndStillBlocks is review point 5: when every
// project that could attest a check WAS in the run and none reported it, the
// requirement is a genuine gap. It keeps blocking, and the reader is told the
// gate looked everywhere instead of being left with a bare check-not-observed.
func TestExhaustedScopeNamesItselfAndStillBlocks(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"an-exhausted-evidence-scope-blocks-and-says-so")
	fixture := newAttestedFixture(t)
	// The one candidate answers with an entry that reports nothing.
	report := fixture.writeCachedReport(t, `{"protocolVersion": 1, "observations": []}`)
	stub := &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}}
	record := fixture.collect(t, stub)
	group := soleGroup(t, record)

	if got := requirementState(t, group, "issue"); got != features.RequirementMissing {
		t.Fatalf("issue = %s, want missing: the evidence scope was exhausted", got)
	}
	if !group.Blocked {
		t.Fatal("an exhausted scope stopped blocking under enforce")
	}
	finding := findDiagnostic(t, group.Diagnostics, features.WarningCodeUnobservedRequirement)
	if finding.Severity != diag.Warning {
		t.Errorf("finding severity = %s, want a warning beside the blocking verdict", finding.Severity)
	}
	if !strings.Contains(finding.Message, "evidence scope is exhausted") ||
		!strings.Contains(finding.Message, "spectest.Proves") {
		t.Errorf("the message does not say the gate looked everywhere or what to do: %q", finding.Message)
	}
	// It must NOT be the unresolvable-evidence message: that one excuses the
	// requirement, and this one does not.
	if strings.Contains(finding.Message, "never sanctioned") {
		t.Error("the exhausted-scope finding claimed the requirement was excused")
	}
}

// TestGreenGroupNeverConsultsTheCache pins the lazy trigger: the whole recovery
// path — planning, keying, hashing — must not run for a session that already
// resolved everything.
func TestGreenGroupNeverConsultsTheCache(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"a-resolved-session-consults-no-cache")
	fixture := newAttestedFixture(t)
	// billing's own test reports the check this time, so the in-session join
	// closes the group by itself.
	writeFile(t, filepath.Join(fixture.root, "billing", "invoice_test.go"), "package billing\n")
	writeFile(t, filepath.Join(fixture.root, ".putnami", "out", "billing", "test",
		"putnami-feature-verification.json"), attestingReport("passed", "invoice_test.go"))
	fixture.results["/billing:test~test"] = resultWithArtifact("success",
		features.VerificationReportArtifactID, "putnami-feature-verification.json")

	stub := &stubRecovery{}
	record := fixture.collect(t, stub)
	if got := requirementState(t, soleGroup(t, record), "issue"); got != features.RequirementVerified {
		t.Fatalf("issue = %s, want verified in session", got)
	}
	if len(stub.asked) != 0 {
		t.Errorf("a fully resolved group consulted the cache %d time(s): %v", len(stub.asked), stub.asked)
	}
}

// TestUnresolvableEvidenceWarnsAndDoesNotBlock is Part B: no source at all was
// consultable, so the requirement is unobserved — reported, named, warned, and
// NOT sanctioned.
func TestUnresolvableEvidenceWarnsAndDoesNotBlock(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"unresolvable-evidence-warns-with-the-attesting-projects-named")
	fixture := newAttestedFixture(t)
	stub := &stubRecovery{answer: Recovery{Unconsulted: []UnconsultedProject{
		{Project: "/ledger", Reason: "--no-cache: no cached observation may be served"},
	}}}
	record := fixture.collect(t, stub)
	group := soleGroup(t, record)

	if got := requirementState(t, group, "issue"); got != features.RequirementUnobserved {
		t.Fatalf("issue = %s, want unobserved", got)
	}
	if group.Blocked {
		t.Error("an unresolvable requirement sanctioned the run under enforce")
	}
	if group.Counts.Unobserved != 1 || group.Counts.Missing != 0 || group.Counts.Executable != 1 {
		t.Errorf("counts = %+v, want the requirement counted unobserved and executable", group.Counts)
	}
	warning := findDiagnostic(t, group.Diagnostics, features.WarningCodeUnobservedRequirement)
	if warning.Severity != diag.Warning {
		t.Errorf("finding severity = %s, want a warning", warning.Severity)
	}
	// The DX deliverable: the message names the project that would have
	// attested the check and why it was not consulted.
	if !strings.Contains(warning.Message, "/ledger") || !strings.Contains(warning.Message, "--no-cache") {
		t.Errorf("warning does not name the attesting project and the reason: %q", warning.Message)
	}
	if messages := WarningMessages(record); len(messages["/billing"]) != 1 {
		t.Errorf("WarningMessages = %+v, want one line for /billing", messages)
	}
	// SanctionMessages must not exist for a project nothing blocked.
	if _, sanctioned := SanctionMessages(record)["/billing"]; sanctioned {
		t.Error("an unobserved-only project produced a sanction message")
	}
}

// TestConsultedSourcesKeepTheGateBlocking is the other half of Part B: once
// every candidate answered, a still-missing check is a real gap again.
func TestConsultedSourcesKeepTheGateBlocking(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"a-consulted-source-that-did-not-support-the-check-still-blocks")
	fixture := newAttestedFixture(t)
	// The dependency's entry was found and read; it simply reports nothing for
	// this check. Nothing is unconsulted, so nothing is excused.
	report := fixture.writeCachedReport(t, `{"protocolVersion": 1, "observations": []}`)
	stub := &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}}
	record := fixture.collect(t, stub)
	group := soleGroup(t, record)

	if got := requirementState(t, group, "issue"); got != features.RequirementMissing {
		t.Fatalf("issue = %s, want missing: every candidate source was consulted", got)
	}
	if !group.Blocked {
		t.Error("a consulted-but-silent source stopped blocking under enforce")
	}
}

// TestRestoredFailureAndSkipStillBlock is the acceptance guard that the warning
// exemption can never launder a real defect: a restored observation that fails
// or skips rests on evidence that DID arrive, so it sanctions even while a
// candidate stayed unconsulted.
func TestRestoredFailureAndSkipStillBlock(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]string{
		"a failing attesting check contradicts": "failed",
		"a skipped attesting check misses":      "skipped",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
				"a-consulted-source-that-did-not-support-the-check-still-blocks")
			fixture := newAttestedFixture(t)
			report := fixture.writeCachedReport(t, attestingReport(status, "ledger_test.go"))
			stub := &stubRecovery{answer: Recovery{
				Reports: []RecoveredReport{report},
				// Deliberately ALSO unconsulted: the exemption is armed and must
				// still not apply, because the check itself was observed.
				Unconsulted: []UnconsultedProject{{Project: "/ghost", Reason: "not selected"}},
			}}
			record := fixture.collect(t, stub)
			group := soleGroup(t, record)
			if got := requirementState(t, group, "issue"); got == features.RequirementUnobserved {
				t.Fatalf("issue = %s; an observed %s check was excused as unobserved", got, status)
			}
			if !group.Blocked {
				t.Errorf("an observed %s check did not block under enforce", status)
			}
		})
	}
}

// TestRestoredObservationKeepsProvenanceContainment pins that a cached report
// gets no relaxation: it may only name declarations inside the project that
// published it.
func TestRestoredObservationKeepsProvenanceContainment(t *testing.T) {
	t.Parallel()
	fixture := newAttestedFixture(t)
	writeFile(t, filepath.Join(fixture.root, "billing", "invoice_test.go"), "package billing\n")
	// The ledger entry names a declaration that exists only under BILLING. The
	// path is lexically fine — the report's own strict reader accepts it — so
	// only the filesystem half of containment, resolved against the REPORTING
	// project's root, can refuse it.
	report := fixture.writeCachedReport(t, attestingReport("passed", "invoice_test.go"))
	stub := &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}}
	record := fixture.collect(t, stub)
	group := soleGroup(t, record)

	if got := requirementState(t, group, "issue"); got == features.RequirementVerified {
		t.Fatal("an observation naming another project's source granted support")
	}
	if len(group.Reports) != 0 {
		t.Errorf("a report whose every observation was dropped still contributed a reference: %+v", group.Reports)
	}
	findDiagnostic(t, group.Diagnostics, features.ErrorCodeInvalidPath)
}

// TestRestoredReportFailingTheStrictReaderIsAnError pins that recovered bytes
// go through the same strict reader session bytes do, and that a refusal fails
// closed rather than reading as an absence.
func TestRestoredReportFailingTheStrictReaderIsAnError(t *testing.T) {
	t.Parallel()
	fixture := newAttestedFixture(t)
	report := fixture.writeCachedReport(t, `{"protocolVersion": 99, "observations": []}`)
	stub := &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}}
	record := fixture.collect(t, stub)
	group := soleGroup(t, record)

	finding := findDiagnostic(t, group.Diagnostics, features.ErrorCodeInvalidObservation)
	if finding.Severity != diag.Error {
		t.Errorf("finding severity = %s, want an error that fails closed", finding.Severity)
	}
	if !group.Blocked {
		t.Error("an unreadable restored report did not block under enforce")
	}
}

func findDiagnostic(t *testing.T, diagnostics []diag.Diagnostic, code string) diag.Diagnostic {
	t.Helper()
	for _, finding := range diagnostics {
		if finding.Code == code {
			return finding
		}
	}
	t.Fatalf("no %s diagnostic in %+v", code, diagnostics)
	return diag.Diagnostic{}
}
