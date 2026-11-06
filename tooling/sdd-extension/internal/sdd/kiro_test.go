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
	kiroRequirementsPath = ".kiro/specs/invoice-export/requirements.md"
	kiroHeader           = "# Requirements Document\n\n## Introduction\n\nCustomers receive invoices.\n\n### Requirement 1\n\n" +
		"**User Story:** As a customer, I want an invoice, so that I can pay.\n\n#### Acceptance Criteria\n\n"
	kiroIssue   = "1. WHEN an order closes THE system SHALL issue an invoice\n"
	kiroNumber  = "2. THE system SHALL number invoices\n"
	kiroAnchors = kiroHeader + "<!-- putnami:id issue-an-invoice -->\n" + kiroIssue + "<!-- putnami:id number-invoices -->\n" + kiroNumber
	// kiroReordered holds the same anchored criteria in the other order.
	kiroReordered = kiroHeader + "<!-- putnami:id number-invoices -->\n" + kiroIssue + "<!-- putnami:id issue-an-invoice -->\n" + kiroNumber
	kiroBaseline  = `{
  "$schema": "https://putnami.dev/schemas/putnami-specs-baseline.json",
  "protocolVersion": 2,
  "executableRequirements": [
    "billing/invoicing#issue-an-invoice",
    "billing/invoicing#number-invoices"
  ]
}
`
)

// kiroWorkspace is one project in mode whose feature names a .kiro directory at
// the workspace root, where Kiro writes it.
func kiroWorkspace(t *testing.T, mode, requirements string) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"kiro-test","includes":["app"]}`)
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
    "spec": {"format": "kiro", "path": ".kiro/specs/invoice-export"},
    "requirements": [
      {"id": "issue-an-invoice", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["issued"]}},
      {"id": "number-invoices", "stage": "coded", "evidenceKinds": ["attestation"],
       "verification": {"kind": "acceptance", "checks": ["numbered"]}}
    ]
  }]
}`)
	writeFixtureFile(t, filepath.Join(root, filepath.FromSlash(kiroRequirementsPath)), requirements)
	writeFixtureFile(t, filepath.Join(root, ".kiro", "specs", "invoice-export", "design.md"), "# Design\n")
	writeFixtureFile(t, filepath.Join(root, ".kiro", "steering", "product.md"), "# Product\n")
	options := enforceOptions()
	options["sdd"]["verification"] = map[string]any{"specs": mode}
	options["sdd"]["specFormats"] = []any{"kiro"}
	project := appProject("billing-app", "app")
	project.Config = &wsproto.ProjectConfig{Name: "billing-app", Type: "application", Options: options}
	return fixtureWorkspace("kiro-test", root, project)
}

func TestKiroProjectReachesSpecsValidate(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "kiro-specs-reach-the-gate", "a-kiro-project-reaches-specs-validate")
	report, err := BuildSpecValidationResult(kiroWorkspace(t, "enforce", kiroAnchors), Selection{})
	if err != nil || diag.HasErrors(report.Diagnostics) {
		t.Fatalf("specs validate: %v, %+v", err, report.Diagnostics)
	}
	if hasDiagnosticCode(report.Diagnostics, featureproto.WarningCodeSpecCriterionMalformed) {
		t.Fatalf("a well-formed spec has a criterion finding: %+v", report.Diagnostics)
	}
}

func TestEnforceFailsEachUnanchoredKiroCriterion(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "kiro-specs-reach-the-gate", "enforce-fails-each-unanchored-kiro-criterion")
	text := kiroHeader + "<!-- putnami:id issue-an-invoice -->\n" + kiroIssue + kiroNumber
	report, err := BuildSpecValidationResult(kiroWorkspace(t, "report", text), Selection{})
	if err != nil || diag.HasErrors(report.Diagnostics) {
		t.Fatalf("report mode: %v, %+v", err, report.Diagnostics)
	}

	report, err = BuildSpecValidationResult(kiroWorkspace(t, "enforce", text), Selection{})
	if err == nil && !diag.HasErrors(report.Diagnostics) {
		t.Fatal("enforce passed over an unanchored criterion")
	}
	found := 0
	for _, finding := range report.Diagnostics {
		if finding.Code == featureproto.ErrorCodeSpecRequirementUnanchored {
			found++
			if finding.Field != kiroRequirementsPath+"#L15" {
				t.Errorf("field = %s", finding.Field)
			}
		}
	}
	if found != 1 {
		t.Fatalf("diagnostics = %+v, want one unanchored finding", report.Diagnostics)
	}
}

func TestReorderedAnchoredKiroSpecKeepsTheRatchet(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "kiro-specs-reach-the-gate", "a-reordered-anchored-kiro-spec-keeps-the-ratchet")
	ws := kiroWorkspace(t, "enforce", kiroAnchors)
	committedBaseline(t, ws.Root, kiroBaseline)
	if report, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the floor holds before the move: %v, %+v", err, report.Diagnostics)
	}
	writeFixtureFile(t, filepath.Join(ws.Root, filepath.FromSlash(kiroRequirementsPath)), kiroReordered)
	if report, err := BuildSpecsRatchetResult(ws); err != nil {
		t.Fatalf("the anchors kept the IDs: %v, %+v", err, report.Diagnostics)
	}

	writeFixtureFile(t, filepath.Join(ws.Root, filepath.FromSlash(kiroRequirementsPath)), kiroHeader+"<!-- putnami:id issue-an-invoice -->\n"+kiroIssue)
	report, err := BuildSpecsRatchetResult(ws)
	if err == nil {
		t.Fatal("a removed criterion left the floor and the ratchet passed")
	}
	if !hasDiagnostic(report.Diagnostics, featureproto.ErrorCodeRatchetRegression, "billing/invoicing#number-invoices") {
		t.Fatalf("diagnostics = %+v, want the regression that names the identity", report.Diagnostics)
	}
}
