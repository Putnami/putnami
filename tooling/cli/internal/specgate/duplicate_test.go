package specgate

import (
	"path/filepath"
	"strings"
	"testing"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

// newDuplicatedFixture is the session with a duplicated proof: billing owns the spec, and
// the tests of billing and of its dependency ledger both ran and both reported
// the one check of requirement "issue".
func newDuplicatedFixture(t *testing.T, mode string) *attestedFixture {
	t.Helper()
	fixture := newAttestedFixture(t)
	// The mode is the workspace's: billing drops the override the base fixture
	// gives it.
	fixture.ws.ProjectByID("/billing").Config.Options = nil
	(&gateFixture{ws: fixture.ws}).setPolicy("workspace", mode)
	writeFile(t, filepath.Join(fixture.root, "billing", "invoice_test.go"), "package billing\n")
	for _, reporter := range []struct{ id, path, provenance string }{
		{"/billing", "billing", "invoice_test.go"},
		{"/ledger", "ledger", "ledger_test.go"},
	} {
		writeFile(t, filepath.Join(fixture.root, ".putnami", "out", reporter.path, "~test",
			features.VerificationReportFilename), attestingReport("passed", reporter.provenance))
		job := testStep(fixture.ws.ProjectByID(reporter.id), "test", true)
		if reporter.id == "/ledger" {
			fixture.planned = append(fixture.planned, job)
		}
		fixture.results[job.Key()] = resultWithArtifact("success",
			features.VerificationReportArtifactID, features.VerificationReportFilename)
	}
	return fixture
}

func TestADuplicatedCheckNamesItsRequirementAndReportingProjects(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-names-a-duplicated-proof",
		"a-duplicated-check-names-its-requirement-and-reporting-projects")
	group := soleGroup(t, newDuplicatedFixture(t, "enforce").collect(t, nil))

	// The wire keeps what ADR 0002 of protocols/features settles: a duplicated
	// check grants nothing, and the requirement still blocks under enforce.
	if got := requirementState(t, group, "issue"); got != features.RequirementMissing {
		t.Fatalf("issue = %s, want missing", got)
	}
	if !group.Blocked {
		t.Fatal("a duplicated proof did not block under enforce")
	}
	if reason := group.Requirements[0].Checks[0].Reason; reason != features.ReasonDuplicateObservation {
		t.Fatalf("check reason = %q, want %q", reason, features.ReasonDuplicateObservation)
	}
	finding := findDiagnostic(t, group.Diagnostics, features.ErrorCodeDuplicateObservation)
	if finding.Severity != "warning" {
		t.Errorf("finding severity = %q, want warning: the requirement's state already blocks", finding.Severity)
	}
	for _, want := range []string{`"billing/invoicing"`, `requirement "issue"`, `check "issue-check"`,
		"/billing, /ledger", "2 times"} {
		if !strings.Contains(finding.Message, want) {
			t.Errorf("finding %q does not name %s", finding.Message, want)
		}
	}
}

func TestARestoredDuplicateNamesTheProjectItsReportCameFrom(t *testing.T) {
	t.Parallel()
	fixture := newAttestedFixture(t)
	writeFile(t, filepath.Join(fixture.root, "billing", "invoice_test.go"), "package billing\n")
	// billing reports the check and a second requirement stays unresolved, so
	// the gate consults ledger's cache entry, which carries the same check.
	writeFile(t, filepath.Join(fixture.root, ".putnami", "out", "billing", "~validate", "spec-criteria.json"),
		strings.Replace(strings.Replace(attestedProjection, `["issue"]`, `["issue", "record"]`, 1),
			`"requirements": [`, `"requirements": [
        {"id": "record", "stage": "coded", "evidenceKinds": ["attestation"],
         "verification": {"kind": "acceptance", "checks": ["record-check"]}},`, 1))
	writeFile(t, filepath.Join(fixture.root, ".putnami", "out", "billing", "~test",
		features.VerificationReportFilename), attestingReport("passed", "invoice_test.go"))
	fixture.results[testStep(fixture.ws.ProjectByID("/billing"), "test", true).Key()] = resultWithArtifact(
		"success", features.VerificationReportArtifactID, features.VerificationReportFilename)

	report := fixture.writeCachedReport(t, attestingReport("passed", "ledger_test.go"))
	record := fixture.collect(t, &stubRecovery{answer: Recovery{Reports: []RecoveredReport{report}}})
	finding := findDiagnostic(t, soleGroup(t, record).Diagnostics, features.ErrorCodeDuplicateObservation)
	if !strings.Contains(finding.Message, "/billing, /ledger") {
		t.Errorf("finding %q does not name the session reporter and the restored one", finding.Message)
	}
}

func TestTheFailureCountsADuplicateApartFromMissing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-names-a-duplicated-proof",
		"the-failure-counts-a-duplicate-apart-from-missing")
	record := newDuplicatedFixture(t, "enforce").collect(t, nil)
	sanction, failed := Sanctions(record)["/billing"]
	if !failed {
		t.Fatalf("sanctions = %v, want billing to fail", Sanctions(record))
	}
	if sanction.Code != features.ErrorCodeUnresolvedRequirement {
		t.Errorf("sanction code = %q, want %q", sanction.Code, features.ErrorCodeUnresolvedRequirement)
	}
	for _, want := range []string{"1 of 1", "missing 0, duplicated 1", `check "issue-check"`,
		`requirement "issue"`, "/billing, /ledger"} {
		if !strings.Contains(sanction.Message, want) {
			t.Errorf("sanction %q does not carry %q", sanction.Message, want)
		}
	}
	if warnings := WarningMessages(record); len(warnings) != 0 {
		t.Errorf("warnings = %v, want none: the failure already repeats the finding", warnings)
	}
}

func TestTheFailureKeepsARequirementWithAnUnprovenCheckMissing(t *testing.T) {
	t.Parallel()
	duplicate := features.SpecCheckVerdict{Check: "a", State: features.CheckUnclassified,
		Reason: features.ReasonDuplicateObservation}
	unproven := features.SpecCheckVerdict{Check: "b", State: features.CheckMissing,
		Reason: features.ReasonCheckNotObserved}
	passed := features.SpecCheckVerdict{Check: "c", State: features.CheckSatisfied, Reason: features.ReasonCheckPassed}
	requirements := []features.SpecRequirementVerdict{
		{Requirement: "both", State: features.RequirementMissing, Checks: []features.SpecCheckVerdict{duplicate, unproven}},
		{Requirement: "duplicated", State: features.RequirementMissing, Checks: []features.SpecCheckVerdict{duplicate, passed}},
		{Requirement: "failed", State: features.RequirementContradicted, Checks: []features.SpecCheckVerdict{duplicate}},
	}
	record := &features.SpecVerificationRecord{Groups: []features.SpecVerificationGroup{{
		Project: "/a", Feature: "go/x", Mode: features.VerificationModeEnforce, Blocked: true,
		Requirements: requirements, Counts: features.CountSpecVerdicts(requirements),
	}}}
	message := Sanctions(record)["/a"].Message
	if !strings.Contains(message, "missing 1, duplicated 1, stale 0, contradicted 1") {
		t.Errorf("sanction %q, want one missing, one duplicated and one contradicted", message)
	}
}

func TestTheFailureBoundsTheFindingsItSpellsOut(t *testing.T) {
	t.Parallel()
	findings := []string{"g", "f", "e", "d", "c", "b", "a"}
	clause := findingsClause(findings)
	if clause != ": a; b; c; d; e; and 2 more finding(s), in the group diagnostics of `putnami specs verify --output=json`" {
		t.Errorf("clause = %q", clause)
	}
	if got := reportingProjects([]string{"/b", "/a", "/b"}); got != "/a, /b (2 reports)" {
		t.Errorf("reporters = %q, want each project once and the repeated one counted", got)
	}
	if got := reportingProjects(nil); !strings.Contains(got, "could not attribute") {
		t.Errorf("reporters = %q, want the unattributed wording", got)
	}
}

func TestADuplicateUnderReportIsPrintedAsAWarning(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-names-a-duplicated-proof",
		"a-duplicate-under-report-is-printed-as-a-warning")
	record := newDuplicatedFixture(t, "report").collect(t, nil)
	if soleGroup(t, record).Blocked {
		t.Fatal("report mode blocked on a duplicated proof")
	}
	if sanctions := Sanctions(record); len(sanctions) != 0 {
		t.Fatalf("sanctions = %v, want none under report", sanctions)
	}
	warnings := WarningMessages(record)["/billing"]
	if len(warnings) != 1 || !strings.Contains(warnings[0], "/billing, /ledger") ||
		!strings.Contains(warnings[0], `check "issue-check"`) {
		t.Errorf("warnings = %v, want the duplicated check with its reporting projects", warnings)
	}
}
