package sdd

import (
	"path/filepath"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const (
	gherkinFeaturePath = "app/features/invoicing.feature"
	gherkinIssue       = "  @putnami-id:issue-an-invoice\n  Rule: An order that closes gets an invoice\n    @putnami-check:issued\n    Scenario: A closed order\n      Given a closed order\n      Then an invoice exists\n"
	gherkinNumber      = "  @putnami-id:number-invoices\n  Rule: Invoices are numbered in order\n    @putnami-check:numbered\n    Scenario: Two invoices\n      Then their numbers follow\n"
	gherkinHeader      = "Feature: Invoicing\n  Customers receive invoices.\n\n"
	gherkinBaseline    = `{
  "$schema": "https://putnami.dev/schemas/putnami-specs-baseline.json",
  "protocolVersion": 2,
  "executableRequirements": ["billing/invoicing#issue-an-invoice", "billing/invoicing#number-invoices"]
}`
)

// gherkinWorkspace is one project in mode whose feature names a .feature file.
func gherkinWorkspace(t *testing.T, mode, text string) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"gherkin-test","includes":["app"]}`)
	writeFixtureFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"billing-app","type":"application"}`)
	writeFixtureFile(t, filepath.Join(root, "app", featureproto.ManifestFilename), `{
  "protocolVersion": 3,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoicing",
    "type": "feature",
    "name": "Invoicing",
    "outcome": "Customers receive invoices",
    "owner": "billing",
    "target": "coded",
    "spec": {"format": "gherkin", "path": "app/features/invoicing.feature"},
    "requirements": [
      {"id": "issue-an-invoice", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["issued"]}},
      {"id": "number-invoices", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["numbered"]}}
    ]
  }]
}`)
	writeFixtureFile(t, filepath.Join(root, filepath.FromSlash(gherkinFeaturePath)), text)
	options := enforceOptions()
	options["sdd"]["verification"] = map[string]any{"specs": mode}
	options["sdd"]["specFormats"] = []any{"gherkin"}
	project := appProject("billing-app", "app")
	project.Config = &wsproto.ProjectConfig{Name: "billing-app", Type: "application", Options: options}
	return fixtureWorkspace("gherkin-test", root, project)
}

func TestGherkinProjectReachesSpecsValidate(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "gherkin-specs-reach-the-gate", "a-gherkin-project-reaches-specs-validate")
	report, err := BuildSpecValidationResult(gherkinWorkspace(t, "enforce", gherkinHeader+gherkinIssue+gherkinNumber), Selection{})
	if err != nil || diag.HasErrors(report.Diagnostics) || hasDiagnosticCode(report.Diagnostics, featureproto.WarningCodeSpecScenarioMismatch) {
		t.Fatalf("specs validate: %v, %+v", err, report.Diagnostics)
	}
}

func TestGherkinMismatchReachesSpecsValidateAsAWarning(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "gherkin-specs-reach-the-gate", "a-gherkin-mismatch-reaches-specs-validate-as-a-warning")
	text := gherkinHeader + gherkinIssue + "    Scenario: An extra case\n" + "  @putnami-id:number-invoices\n  Rule: Invoices are numbered in order\n"
	report, err := BuildSpecValidationResult(gherkinWorkspace(t, "enforce", text), Selection{})
	if err != nil || diag.HasErrors(report.Diagnostics) {
		t.Fatalf("a mismatch failed the gate: %v, %+v", err, report.Diagnostics)
	}
	fields := map[string]bool{}
	for _, finding := range report.Diagnostics {
		if finding.Code == featureproto.WarningCodeSpecScenarioMismatch {
			fields[finding.Field] = true
		}
	}
	if len(fields) != 2 || !fields[gherkinFeaturePath+"#L10"] || !fields[gherkinFeaturePath+"#L12"] {
		t.Fatalf("diagnostics = %+v, want the extra scenario at line 10 and the unproved check at line 12", report.Diagnostics)
	}
}

func TestReorderedGherkinSpecKeepsTheRatchet(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "gherkin-specs-reach-the-gate", "a-reordered-gherkin-spec-keeps-the-ratchet")
	ws := gherkinWorkspace(t, "enforce", gherkinHeader+gherkinIssue+gherkinNumber)
	committedBaseline(t, ws.Root, gherkinBaseline)
	if report, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the floor holds before the move: %v, %+v", err, report.Diagnostics)
	}
	writeFixtureFile(t, filepath.Join(ws.Root, filepath.FromSlash(gherkinFeaturePath)), gherkinHeader+gherkinNumber+gherkinIssue)
	if report, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the tags kept the IDs: %v, %+v", err, report.Diagnostics)
	}
	writeFixtureFile(t, filepath.Join(ws.Root, filepath.FromSlash(gherkinFeaturePath)), gherkinHeader+gherkinIssue)
	report, err := BuildSpecsRatchetResult(ws)
	if err == nil {
		t.Fatal("a removed rule left the floor and the ratchet passed")
	}
	if !hasDiagnostic(report.Diagnostics, featureproto.ErrorCodeRatchetRegression, "billing/invoicing#number-invoices") {
		t.Fatalf("diagnostics = %+v, want the regression that names the identity", report.Diagnostics)
	}
}
