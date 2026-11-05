package sdd

import (
	"path/filepath"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const (
	openSpecMarkdown = `# invoice-export Specification

## Purpose
Customers receive invoices.

## Requirements
{anchor}### Requirement: Issue an invoice
The system SHALL issue an invoice.

#### Scenario: Issued
- **WHEN** an order closes
- **THEN** an invoice is issued
`
	openSpecBaseline = `{
  "$schema": "https://putnami.dev/schemas/putnami-specs-baseline.json",
  "protocolVersion": 2,
  "executableRequirements": [
    "billing/invoicing#issue-an-invoice"
  ]
}
`
)

// openSpecWorkspace is one enforcing project whose feature names an OpenSpec
// capability, with no native spec file.
func openSpecWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"openspec-test","includes":["app"]}`)
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
    "spec": {"format": "openspec", "path": "app/openspec/specs/invoice-export"},
    "requirements": [
      {"id": "issue-an-invoice", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["issued"]}}
    ]
  }]
}`)
	writeOpenSpec(t, root, "", "### Requirement: Issue an invoice")
	options := enforceOptions()
	options["sdd"]["specFormats"] = []any{"openspec"}
	project := appProject("billing-app", "app")
	project.Config = &wsproto.ProjectConfig{Name: "billing-app", Type: "application", Options: options}
	return fixtureWorkspace("openspec-test", root, project)
}

func writeOpenSpec(t *testing.T, root, anchor, heading string) {
	t.Helper()
	text := strings.Replace(openSpecMarkdown, "{anchor}", anchor, 1)
	text = strings.Replace(text, "### Requirement: Issue an invoice", heading, 1)
	writeFixtureFile(t, filepath.Join(root, "app", "openspec", "specs", "invoice-export", "spec.md"), text)
}

func TestOpenSpecProjectReachesSpecsValidate(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "openspec-specs-reach-the-gate", "an-openspec-project-reaches-specs-validate")
	report, err := BuildSpecValidationResult(openSpecWorkspace(t), Selection{})
	if err != nil || diag.HasErrors(report.Diagnostics) {
		t.Fatalf("specs validate: %v, %+v", err, report.Diagnostics)
	}
	for _, finding := range report.Diagnostics {
		if finding.Code == featureproto.WarningCodeSpecScenarioMismatch {
			t.Fatalf("the scenario and the declared check differ: %+v", finding)
		}
	}
}

func TestRenamedOpenSpecRequirementFailsTheRatchet(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "openspec-specs-reach-the-gate", "a-renamed-openspec-requirement-fails-the-ratchet")
	ws := openSpecWorkspace(t)
	committedBaseline(t, ws.Root, openSpecBaseline)
	if report, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the floor holds before the rename: %v, %+v", err, report.Diagnostics)
	}

	writeOpenSpec(t, ws.Root, "", "### Requirement: Send an invoice")
	report, err := BuildSpecsRatchetResult(ws)
	if err == nil {
		t.Fatal("a renamed requirement left the floor and the ratchet passed")
	}
	if !hasDiagnostic(report.Diagnostics, featureproto.ErrorCodeRatchetRegression, "billing/invoicing#issue-an-invoice") {
		t.Fatalf("diagnostics = %+v, want the regression that names the identity", report.Diagnostics)
	}

	// An anchor is how a rename keeps its identity.
	writeOpenSpec(t, ws.Root, "<!-- putnami:id issue-an-invoice -->\n", "### Requirement: Send an invoice")
	if report, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the anchor kept the ID: %v, %+v", err, report.Diagnostics)
	}
}

func TestOpenSpecTextOverTheBoundBlocksOnlyInEnforce(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "enforce-needs-anchors", "enforce-fails-a-text-over-the-bound")
	ws := openSpecWorkspace(t)
	text := strings.Replace(openSpecMarkdown, "{anchor}", "", 1)
	text = strings.Replace(text, "The system SHALL issue an invoice.", "The system SHALL issue an invoice."+strings.Repeat(" Again.", 80), 1)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", "openspec", "specs", "invoice-export", "spec.md"), text)

	report, err := BuildSpecValidationResult(ws, Selection{})
	if err == nil || !hasDiagnostic(report.Diagnostics, featureproto.FindingCodeSpecTextTooLong, "split it") {
		t.Fatalf("enforce passed over a text past the bound: %v, %+v", err, report.Diagnostics)
	}
	for _, finding := range report.Diagnostics {
		if finding.Code == featureproto.FindingCodeSpecTextTooLong && finding.Field != "app/openspec/specs/invoice-export/spec.md#L7" {
			t.Fatalf("the finding names %s, not the requirement's line", finding.Field)
		}
	}

	ws.Projects[0].Config.Options["sdd"]["verification"] = map[string]any{"specs": "report"}
	report, err = BuildSpecValidationResult(ws, Selection{})
	if err != nil || diag.HasErrors(report.Diagnostics) || !hasDiagnostic(report.Diagnostics, featureproto.FindingCodeSpecTextTooLong, "split it") {
		t.Fatalf("report must read the spec and warn: %v, %+v", err, report.Diagnostics)
	}
}
