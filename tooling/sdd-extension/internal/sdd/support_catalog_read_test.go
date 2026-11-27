package sdd

import (
	"path/filepath"
	"testing"

	supportproto "go.putnami.dev/protocol/support"
)

// TestSpecValidationVerdictFollowsTheSupportCatalog shows that the catalog
// specs-validate keys is one its verdict depends on: the same workspace,
// assessed against a catalog that lacks a publishable project's entry and then
// against one that holds it, gives two reports. A key that left the catalog
// out would replay the first report for the second tree.
func TestSpecValidationVerdictFollowsTheSupportCatalog(t *testing.T) {
	assess := func(catalog string) SpecValidationReport {
		t.Helper()
		ws := specWorkspace(t)
		writeFixtureFile(t, filepath.Join(ws.Root, supportproto.CatalogFilename), catalog)
		report, err := BuildSpecValidationResult(ws, Selection{})
		if err != nil {
			t.Fatalf("specs validate: %v", err)
		}
		if !report.Completeness.SupportAssessed {
			t.Fatalf("support was not assessed against a readable catalog: %+v", report.Completeness)
		}
		return report
	}

	without := assess(`{
  "protocolVersion": 1,
  "entries": [{"id": "billing-app", "kind": "package", "status": "preview"}]
}`)
	if !containsString(without.Completeness.ProjectsWithoutSupport, "docs-site") {
		t.Fatalf("projectsWithoutSupport = %v, want docs-site while the catalog lacks its entry", without.Completeness.ProjectsWithoutSupport)
	}

	with := assess(`{
  "protocolVersion": 1,
  "entries": [
    {"id": "billing-app", "kind": "package", "status": "preview"},
    {"id": "docs-site", "kind": "package", "status": "preview"}
  ]
}`)
	if containsString(with.Completeness.ProjectsWithoutSupport, "docs-site") {
		t.Fatalf("projectsWithoutSupport = %v, want no docs-site once the catalog holds its entry", with.Completeness.ProjectsWithoutSupport)
	}
}
