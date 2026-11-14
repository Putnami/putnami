package specgate

import (
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// plantSessionReport makes the ledger's own test run in the session and report
// the billing check from ledger/ledger_test.go, so the observation reaches the
// gate in-session instead of through recovery.
func (f *attestedFixture) plantSessionReport(t *testing.T) {
	t.Helper()
	ledger := f.ws.ProjectByID("/ledger")
	writeFile(t, filepath.Join(f.root, ".putnami", "out", "ledger", "~test", features.VerificationReportFilename),
		attestingReport("passed", "ledger_test.go"))
	job := testStep(ledger, "test", true)
	f.planned = append(f.planned, job)
	f.results[job.Key()] = resultWithArtifact("success",
		features.VerificationReportArtifactID, features.VerificationReportFilename)
}

func outOfScopeFindings(group features.SpecVerificationGroup) []diag.Diagnostic {
	var findings []diag.Diagnostic
	for _, finding := range group.Diagnostics {
		if finding.Code == features.ErrorCodeInvalidObservation && strings.Contains(finding.Message, "evidence scope") {
			findings = append(findings, finding)
		}
	}
	return findings
}

// TestAnOutOfScopeObservationGrantsNoSupport is the issue's case: the ledger's
// test proves a billing check, the two projects share no edge, and the ledger
// is no feature authority of billing. The proof resolved only on a run that
// selected both projects; it now grants no support on any run, and the finding
// names what to change.
func TestAnOutOfScopeObservationGrantsNoSupport(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-rejects-an-out-of-scope-proof",
		"an-out-of-scope-observation-grants-no-support-and-names-both-projects")
	fixture := newAttestedFixture(t)
	fixture.ws.Projects[1].Dependencies = nil
	fixture.rebuildGraph()
	fixture.plantSessionReport(t)

	group := soleGroup(t, fixture.collect(t, nil))

	if got := requirementState(t, group, "issue"); got == features.RequirementVerified {
		t.Fatal("an observation from outside the owner's evidence scope verified the requirement")
	}
	if !group.Blocked {
		t.Error("an out-of-scope proof did not block under enforce")
	}
	findings := outOfScopeFindings(group)
	if len(findings) != 1 {
		t.Fatalf("out-of-scope findings = %+v, want exactly one", group.Diagnostics)
	}
	finding := findings[0]
	if finding.Severity != diag.Error {
		t.Errorf("severity = %s, want error", finding.Severity)
	}
	if finding.Field != "billing/specs/invoicing.json" {
		t.Errorf("field = %q, want the spec of the group", finding.Field)
	}
	for _, want := range []string{
		`"issue-check"`, `"issue"`, `"billing/invoicing"`,
		"/ledger", "ledger/ledger_test.go", "/billing",
		"spectest.Proves", "dependency edge", "featureAuthority.owner",
	} {
		if !strings.Contains(finding.Message, want) {
			t.Errorf("finding does not name %s: %s", want, finding.Message)
		}
	}
}

// TestAnOutOfScopeObservationBlocksWhenACandidateIsUnconsulted pins that the
// rejection is what blocks: an unconsulted candidate turns the missing
// requirement into unobserved, which alone would only warn.
func TestAnOutOfScopeObservationBlocksWhenACandidateIsUnconsulted(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-rejects-an-out-of-scope-proof",
		"an-out-of-scope-observation-blocks-even-when-a-candidate-is-unconsulted")
	fixture := newAttestedFixture(t)
	tax := &workspace.Project{ID: "/tax", Name: "@acme/tax", Path: "tax", Config: &wsproto.ProjectConfig{}}
	fixture.ws.Projects = append(fixture.ws.Projects, tax)
	fixture.ws.Projects[1].Dependencies = []string{"@acme/tax"}
	fixture.rebuildGraph()
	fixture.plantSessionReport(t)

	stub := &stubRecovery{answer: Recovery{Unconsulted: []UnconsultedProject{
		{Project: "/tax", Reason: "no cache entry", Code: features.UnconsultedNoEntry},
	}}}
	group := soleGroup(t, fixture.collect(t, stub))

	if got := requirementState(t, group, "issue"); got != features.RequirementUnobserved {
		t.Fatalf("issue = %s, want unobserved: /tax could not be consulted", got)
	}
	if len(outOfScopeFindings(group)) != 1 {
		t.Fatalf("out-of-scope findings = %+v, want exactly one", group.Diagnostics)
	}
	if !group.Blocked {
		t.Error("an out-of-scope proof was excused because another candidate was unconsulted")
	}
}

// TestEveryInScopeReporterIsAdmitted pins the other side: the owner itself, a
// dependency, a direct dependent and a feature-authority project each verify
// the requirement in-session with no finding.
func TestEveryInScopeReporterIsAdmitted(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-rejects-an-out-of-scope-proof",
		"every-in-scope-reporter-is-admitted")
	cases := map[string]func(f *attestedFixture){
		"dependency": func(*attestedFixture) {},
		"direct dependent": func(f *attestedFixture) {
			f.ws.Projects[1].Dependencies = nil
			f.ws.Projects[0].Dependencies = []string{"@acme/billing"}
		},
		"feature authority": func(f *attestedFixture) {
			f.ws.Projects[1].Dependencies = nil
			f.ws.Projects[0].Config.FeatureAuthority = &wsproto.ProjectFeatureAuthority{Owner: "billing/invoicing"}
		},
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newAttestedFixture(t)
			wire(fixture)
			fixture.rebuildGraph()
			fixture.plantSessionReport(t)
			assertAdmitted(t, fixture.collect(t, nil))
		})
	}
	t.Run("owner", func(t *testing.T) {
		t.Parallel()
		fixture := newGateFixture(t, attestedProjection, `{
  "protocolVersion": 1,
  "observations": [{"feature": "billing/invoicing", "requirement": "issue", "check": "issue-check",
    "status": "passed", "provenance": {"path": "invoice_test.go", "symbol": "TestIssue"}}]
}`)
		fixture.setPolicy("project", "enforce")
		assertAdmitted(t, collect(t, fixture))
	})
}

func assertAdmitted(t *testing.T, record *features.SpecVerificationRecord) {
	t.Helper()
	group := soleGroup(t, record)
	if got := requirementState(t, group, "issue"); got != features.RequirementVerified {
		t.Fatalf("issue = %s, want verified; diagnostics: %+v", got, group.Diagnostics)
	}
	if findings := outOfScopeFindings(group); len(findings) > 0 {
		t.Fatalf("an in-scope reporter was rejected: %+v", findings)
	}
	if group.Blocked {
		t.Error("a requirement verified by an in-scope reporter blocked")
	}
}

// TestTheFailureRepeatsAnOutOfScopeFinding pins that the run says what blocked:
// under enforce the project's failure carries the finding, and under report the
// run prints it as a warning. It also pins that the group references no report
// of a rejected project.
func TestTheFailureRepeatsAnOutOfScopeFinding(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-rejects-an-out-of-scope-proof",
		"the-failure-repeats-the-finding-and-report-prints-it")
	for _, mode := range []string{"enforce", "report"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fixture := newAttestedFixture(t)
			fixture.ws.Projects[1].Dependencies = nil
			fixture.rebuildGraph()
			fixture.ws.ProjectByID("/billing").Config.Options = nil
			(&gateFixture{ws: fixture.ws}).setPolicy("workspace", mode)
			fixture.plantSessionReport(t)
			record := fixture.collect(t, nil)

			group := soleGroup(t, record)
			for _, report := range group.Reports {
				if report.Project == "/ledger" {
					t.Errorf("the group references the rejected report %+v", report)
				}
			}
			sanction, sanctioned := Sanctions(record)["/billing"]
			warnings := WarningMessages(record)["/billing"]
			if mode == "enforce" {
				if !sanctioned || !strings.Contains(sanction.Message, "outside the evidence scope of /billing") {
					t.Fatalf("sanction = %+v, want it to repeat the out-of-scope finding", sanction)
				}
				return
			}
			if sanctioned {
				t.Fatalf("sanction = %+v, want none under report", sanction)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "outside the evidence scope of /billing") {
				t.Fatalf("warnings = %v, want the out-of-scope finding", warnings)
			}
		})
	}
}
