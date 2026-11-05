package sdd

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const (
	specsSourceRequirement = "specs-surface-names-the-source"
	openSpecWithGaps       = `# invoice-export Specification

## Purpose
Customers receive invoices.

## Status
Draft

## Requirements
### Requirement: Issue an invoice
The system SHALL issue an invoice, as [the design](./design.md) shows.

#### Scenario: Issued
- **WHEN** an order closes
- **THEN** an invoice is issued
`
)

// openSpecWorkspaceWithGaps is openSpecWorkspace with a derived section and a
// link that is not a decision record.
func openSpecWorkspaceWithGaps(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws := openSpecWorkspace(t)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", "openspec", "specs", "invoice-export", "spec.md"), openSpecWithGaps)
	return ws
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSpecsListNamesTheSource(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", specsSourceRequirement, "specs-list-names-the-source")
	report, err := BuildSpecCatalogResult(openSpecWorkspace(t), Selection{})
	if err != nil || len(report.Specs) != 1 {
		t.Fatalf("specs list: %v, %+v", err, report.Specs)
	}
	if row := mustJSON(t, report.Specs[0]); !strings.Contains(row, `"path":"app/openspec/specs/invoice-export","source":"openspec"`) {
		t.Fatalf("row = %s, want source openspec after the path", row)
	}

	ws := specWorkspace(t)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", "specs", "invoice.json"), specDocument("billing/invoice-export"))
	native, err := BuildSpecCatalogResult(ws, Selection{})
	if err != nil || len(native.Specs) != 1 || native.Specs[0].Source != SpecSourceNative {
		t.Fatalf("specs list: %v, %+v, want source native", err, native.Specs)
	}
}

func TestSpecsInspectListsTheNativeGaps(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", specsSourceRequirement, "specs-inspect-lists-the-native-gaps")
	ws := openSpecWorkspaceWithGaps(t)
	report, err := BuildSpecContextResult(ws, "billing/invoicing")
	if err != nil {
		t.Fatalf("specs inspect: %v, %+v", err, report.Diagnostics)
	}
	if source := mustJSON(t, report.Source); source != `{"path":"app/openspec/specs/invoice-export","project":"billing-app","format":"openspec"}` {
		t.Fatalf("source = %s", source)
	}
	const file = "app/openspec/specs/invoice-export/spec.md"
	want := `[{"kind":"derived_section","field":"` + file + `#L6",` +
		`"message":"section \"Status\" restates a fact Putnami derives: remove it",` +
		`"step":"delete the section: Putnami derives this fact, and a native spec has no place for it; or draft a native spec with ` + "`putnami specs init billing/invoicing --from openspec:app/openspec/specs/invoice-export`" + `"},` +
		`{"kind":"context_link","field":"` + file + `#L10",` +
		`"message":"link \"./design.md\" is context, not a decision: a native spec links only doc/adr/*.md records",` +
		`"step":"record the decision in a doc/adr/*.md file and link that file; any other link stays context, and a native spec drops it from its decisions; or draft a native spec with ` + "`putnami specs init billing/invoicing --from openspec:app/openspec/specs/invoice-export`" + `"}]`
	if gaps := mustJSON(t, report.NativeGaps); gaps != want {
		t.Fatalf("nativeGaps =\n%s\nwant\n%s", gaps, want)
	}
	// The derived-fact warning names the file, so inspect reports it too.
	if finding, found := findDiagnostic(report.Diagnostics, "features.spec_derived_fact"); !found || finding.Field != file+"#L6" {
		t.Fatalf("diagnostics = %+v, want the warning on the spec file", report.Diagnostics)
	}
}

func TestNativeGapsNeverChangeTheExitCode(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", specsSourceRequirement, "native-gaps-never-change-the-exit-code")
	// The project enforces specs, and the gaps still block nothing.
	ws := openSpecWorkspaceWithGaps(t)
	inspected, err := BuildSpecContextResult(ws, "billing/invoicing")
	if err != nil || len(inspected.NativeGaps) == 0 {
		t.Fatalf("specs inspect: %v, %d gaps", err, len(inspected.NativeGaps))
	}
	validated, err := BuildSpecValidationResult(ws, Selection{})
	if err != nil || !validated.Valid {
		t.Fatalf("specs validate: %v, %+v", err, validated.Diagnostics)
	}
	// Two gaps, one finding: the derived-fact warning is the reader's own, and
	// the context link adds nothing to the diagnostics.
	if len(inspected.NativeGaps) != 2 || len(inspected.Diagnostics) != 1 {
		t.Fatalf("gaps = %+v, diagnostics = %+v, want two gaps and only the derived-fact warning",
			inspected.NativeGaps, inspected.Diagnostics)
	}
}

func TestANativeWorkspaceKeepsItsOutput(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", specsSourceRequirement, "a-native-workspace-keeps-its-output")
	ws := specWorkspace(t)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", "specs", "invoice.json"), specDocument("billing/invoice-export"))
	report, err := BuildSpecContextResult(ws, "billing/invoice-export")
	if err != nil {
		t.Fatalf("specs inspect: %v", err)
	}
	data := mustJSON(t, report)
	if strings.Contains(data, `"format"`) || strings.Contains(data, `"nativeGaps"`) {
		t.Fatalf("a native spec's inspect output gained a member: %s", data)
	}
	if source := mustJSON(t, report.Source); source != `{"path":"app/specs/invoice.json","project":"billing-app"}` {
		t.Fatalf("source = %s", source)
	}
}

func TestSpecsInspectKeepsTheReasonAnExternalSpecWasNotRead(t *testing.T) {
	ws := openSpecWorkspace(t)
	ws.Projects[0].Config.Options["sdd"]["specFormats"] = []any{"speckit"}
	report, err := BuildSpecContextResult(ws, "billing/invoicing")
	if err == nil {
		t.Fatal("specs inspect passed over an external spec it did not read")
	}
	finding, found := findDiagnostic(report.Diagnostics, "features.spec_format_not_enabled")
	if !found || finding.Field != "app/putnami.features.json#features[0].spec.format" {
		t.Fatalf("diagnostics = %+v, want the opt-in error on the manifest entry", report.Diagnostics)
	}
	for _, finding := range report.Diagnostics {
		if strings.Contains(finding.Message, "specs init") {
			t.Fatalf("inspect suggests specs init for a feature that names an external spec: %+v", finding)
		}
	}
}

func TestSpecsInspectKeepsTheReaderErrorOfAnUnreadSpec(t *testing.T) {
	ws := openSpecWorkspace(t)
	writeFixtureFile(t, filepath.Join(ws.Root, "app", "openspec", "specs", "invoice-export", "spec.md"), "## Requirements\n")
	report, err := BuildSpecContextResult(ws, "billing/invoicing")
	if err == nil || len(report.Diagnostics) == 0 {
		t.Fatalf("specs inspect: %v, %+v, want the reader error", err, report.Diagnostics)
	}
	for _, finding := range report.Diagnostics {
		if !strings.HasPrefix(finding.Field, "app/openspec/specs/invoice-export") {
			t.Fatalf("finding %+v does not name the spec", finding)
		}
	}
}
