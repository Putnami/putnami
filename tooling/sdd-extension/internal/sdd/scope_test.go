package sdd

import (
	"path/filepath"
	"strings"
	"testing"

	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// manyProjectWorkspace is the fixture the "unrelated design graphs are not
// parsed" acceptance is argued against. It is built so that a RENDERER-ONLY
// filter cannot pass:
//
//   - `unrelated` carries a design graph that exists and cannot be parsed. A
//     catalog reports such an artifact in Unreadable, which is keyed by PATH and
//     not by feature — so no post-hoc filter over the feature rows could remove
//     it. Its absence therefore proves the file was never opened.
//   - `unrelated` also carries a structurally broken evidence document, so a run
//     that still evaluated it would fail rather than merely print more.
//   - `shipping` produces the evidence for `billing`'s feature, so a scope that
//     stopped at the project boundary would report a missing-evidence warning
//     the unscoped run does not.
//
// The returned view lists all three members. That is deliberate: what is under
// test is the ENGINE's scoping — which projects a narrowed run reads — and it
// can only be tested when the engine is handed more than the selection. How
// much membership the wire actually delivers to a narrowed run is a separate,
// documented limit (see internal/wsview's package doc).
func manyProjectWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"scoped","includes":["billing","shipping","unrelated"]}`)
	for _, project := range []string{"billing", "shipping", "unrelated"} {
		writeFixtureFile(t, filepath.Join(root, project, "putnami.json"),
			`{"name":"@acme/`+project+`","type":"application","tags":["`+project+`"]}`)
	}
	writeFixtureFile(t, filepath.Join(root, "billing", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "billing",
  "features": [{
    "id": "billing/invoice",
    "type": "feature",
    "name": "Invoice export",
    "outcome": "Customers export issued invoices",
    "owner": "billing",
    "target": "modeled"
  }]
}`)
	writeFixtureFile(t, filepath.Join(root, "unrelated", featureproto.ManifestFilename), `{
  "protocolVersion": 1,
  "namespace": "warehouse",
  "features": [{
    "id": "warehouse/stock",
    "type": "feature",
    "name": "Stock levels",
    "outcome": "Operators see stock levels",
    "owner": "warehouse",
    "target": "modeled"
  }]
}`)
	writeScopedDesignGraph(t, root, "billing", "@acme/billing", "billing/invoice", "Invoice export")
	// Exists, and cannot be projected. Only a run that OPENS it can report it.
	writeFixtureFile(t,
		filepath.Join(root, "unrelated", ".gen", filepath.FromSlash(featureproto.DesignGraphArtifact)),
		`{"compatibility": not-json`)
	// Structurally broken and wholly outside a billing selection.
	writeFixtureFile(t,
		filepath.Join(root, "unrelated", "schema", "feature-evidence", "broken.json"),
		`{"protocolVersion":1,"evidence":[{"id":""}]}`)
	projects := make([]*workspace.Project, 0, 3)
	for _, name := range []string{"billing", "shipping", "unrelated"} {
		projects = append(projects, &workspace.Project{
			ID: "/" + name, Name: "@acme/" + name, SourceName: "@acme/" + name,
			Type: "application", Path: name, Tags: []string{name},
			Config: &wsproto.ProjectConfig{Name: "@acme/" + name, Type: "application", Tags: []string{name}},
		})
	}
	return workspace.NewWorkspace(root, &wsproto.Config{Name: "scoped"}, projects)
}

// billingSelection is what the orchestrator writes on the wire for
// `--projects @acme/billing` over the fixture above: mode, the narrowing flag,
// and the resolved ids. Core resolved the selector; the engine only reads the
// answer.
func billingSelection() Selection {
	return Selection{Mode: "projects", Scoped: true, ProjectIDs: []string{"/billing"}}
}

// writeScopedDesignGraph writes one project's design graph at its exact root,
// keeping the graph's declared project name independent of the directory.
func writeScopedDesignGraph(t *testing.T, root, directory, project, featureID, name string) {
	t.Helper()
	graph := &featureproto.DesignGraph{
		Compatibility: featureproto.DesignGraphCompatibility,
		Project:       project,
		Nodes: []featureproto.DesignNode{{
			ID: "feature:" + featureID, Kind: featureproto.DesignNodeFeature, Name: name,
			Properties: map[string]string{"outcome": "Customers export issued invoices", "owner": "billing"},
		}},
		Edges: []featureproto.DesignEdge{},
	}
	encoded, err := featureproto.MarshalDesignGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t,
		filepath.Join(root, directory, ".gen", filepath.FromSlash(featureproto.DesignGraphArtifact)), string(encoded))
}

// TestFeatureCatalogNeverParsesAnUnselectedDesignGraph is the acceptance proof.
// The unscoped control shows the unrelated graph IS reached; the scoped run
// shows it is not reached at all, rather than reached and then filtered.
func TestFeatureCatalogNeverParsesAnUnselectedDesignGraph(t *testing.T) {
	ws := manyProjectWorkspace(t)
	unreadablePath := "unrelated/.gen/" + featureproto.DesignGraphArtifact

	control, err := BuildFeatureCatalogResult(ws, "", Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnreadable(control.Unreadable, unreadablePath) {
		t.Fatalf("control run did not reach %s: %+v — the fixture no longer proves anything", unreadablePath, control.Unreadable)
	}
	if len(control.Features) != 2 {
		t.Fatalf("control features = %+v, want both projects' features", control.Features)
	}
	if control.Selection.Scoped {
		t.Fatalf("control selection = %+v, want the whole-workspace default", control.Selection)
	}

	scoped, err := BuildFeatureCatalogResult(ws, "", billingSelection())
	if err != nil {
		t.Fatal(err)
	}
	if hasUnreadable(scoped.Unreadable, unreadablePath) {
		t.Fatalf("scoped run parsed an unselected project's design graph: %+v", scoped.Unreadable)
	}
	if len(scoped.Unreadable) != 0 {
		t.Fatalf("scoped run reported artifacts outside the selection: %+v", scoped.Unreadable)
	}
	if scoped.Graphs != 1 {
		t.Fatalf("scoped run read %d design graph(s), want only the selected project's", scoped.Graphs)
	}
	if len(scoped.Features) != 1 || scoped.Features[0].ID != "billing/invoice" {
		t.Fatalf("scoped features = %+v, want only the selected project's", scoped.Features)
	}
	if !scoped.Selection.Scoped || strings.Join(scoped.Selection.Projects, ",") != "/billing" {
		t.Fatalf("scoped selection = %+v", scoped.Selection)
	}
	if scoped.Selection.Features != 1 {
		t.Fatalf("scoped selection features = %d, want the count the evaluation produced", scoped.Selection.Features)
	}
}

// TestFeatureCatalogAppliesQueryAfterProjectSelection pins the composition
// order the MCP contract promises: selection decides what is read, the query
// only filters what survived.
func TestFeatureCatalogAppliesQueryAfterProjectSelection(t *testing.T) {
	ws := manyProjectWorkspace(t)
	report, err := BuildFeatureCatalogResult(ws, "stock levels", billingSelection())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Features) != 0 {
		t.Fatalf("features = %+v, want the query to find nothing inside the selection", report.Features)
	}
	// The scope summary still reports what the SELECTION covered, not what the
	// query left, so a caller can tell "no match here" from "nothing selected".
	if report.Selection.Features != 1 {
		t.Fatalf("selection features = %d, want the selected projection's count", report.Selection.Features)
	}
	if report.Query != "stock levels" {
		t.Fatalf("query = %q", report.Query)
	}
}

// TestScopedFeatureValidationIgnoresUnrelatedBrokenEvidence pins that scoping
// reaches evaluation: the unscoped run fails on the unrelated project's broken
// document and the scoped run passes.
func TestScopedFeatureValidationIgnoresUnrelatedBrokenEvidence(t *testing.T) {
	ws := manyProjectWorkspace(t)
	if _, err := BuildFeatureValidationResult(ws, Selection{}); err == nil {
		t.Fatal("the control run passed; the fixture no longer contains a workspace-wide failure")
	}

	report, err := BuildFeatureValidationResult(ws, billingSelection())
	if err != nil {
		t.Fatalf("scoped validation failed on work outside the selection: %v", err)
	}
	if !report.Valid || report.Counts.Errors != 0 {
		t.Fatalf("scoped validation report = %+v", report)
	}
	if !report.Selection.Scoped || strings.Join(report.Selection.Projects, ",") != "/billing" {
		t.Fatalf("selection metadata = %+v", report.Selection)
	}
	if report.Summary.Features != 1 {
		t.Fatalf("scoped summary = %+v, want only the selected feature assessed", report.Summary)
	}
}

// TestScopedSpecSurfacesNarrowRowsCountsAndCompleteness pins that the spec
// catalog and validation both honor selection, and that the per-project
// completeness counts narrow with it.
func TestScopedSpecSurfacesNarrowRowsCountsAndCompleteness(t *testing.T) {
	ws := manyProjectWorkspace(t)
	writeFixtureFile(t, filepath.Join(ws.Root, "billing", "specs", "invoice.json"),
		`{"protocolVersion":1,"feature":"billing/invoice","outcomes":["Customers export invoices"],"requirements":[]}`)
	writeFixtureFile(t, filepath.Join(ws.Root, "unrelated", "specs", "stock.json"),
		`{"protocolVersion":1,"feature":"warehouse/stock","outcomes":["Operators see stock"],"requirements":[]}`)

	control, err := BuildSpecCatalogResult(ws, Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if control.Counts.Specs != 2 || control.Counts.AuthoredFeatures != 2 {
		t.Fatalf("control catalog counts = %+v", control.Counts)
	}

	scoped, err := BuildSpecCatalogResult(ws, billingSelection())
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Counts.Specs != 1 || scoped.Counts.AuthoredFeatures != 1 || scoped.Counts.FeaturesWithoutSpec != 0 {
		t.Fatalf("scoped catalog counts = %+v", scoped.Counts)
	}
	if len(scoped.Specs) != 1 || scoped.Specs[0].Feature != "billing/invoice" {
		t.Fatalf("scoped rows = %+v", scoped.Specs)
	}
	if !scoped.Selection.Scoped || scoped.Selection.Specs != 1 {
		t.Fatalf("scoped selection = %+v", scoped.Selection)
	}

	validation, err := BuildSpecValidationResult(ws, billingSelection())
	if err != nil {
		t.Fatalf("scoped validation: %v", err)
	}
	if validation.Summary.Specs != 1 || validation.Summary.AuthoredFeatures != 1 {
		t.Fatalf("scoped validation summary = %+v", validation.Summary)
	}
	// Completeness is per PROJECT, so a selection must narrow it too — otherwise
	// a scoped run still reports the whole repository's authoring backlog.
	for _, name := range validation.Completeness.ProjectsWithoutFeature {
		if name != "@acme/billing" {
			t.Fatalf("scoped completeness names an unselected project %q", name)
		}
	}
}

// TestScopedSpecValidationKeepsWorkspaceDuplicateDetection pins that narrowing
// never weakens the "one spec per feature" guarantee.
func TestScopedSpecValidationKeepsWorkspaceDuplicateDetection(t *testing.T) {
	ws := manyProjectWorkspace(t)
	writeFixtureFile(t, filepath.Join(ws.Root, "billing", "specs", "invoice.json"),
		`{"protocolVersion":1,"feature":"billing/invoice","outcomes":["Customers export invoices"],"requirements":[]}`)
	// A second spec for the SELECTED feature, hosted by a project nobody selected.
	writeFixtureFile(t, filepath.Join(ws.Root, "shipping", "specs", "invoice-copy.json"),
		`{"protocolVersion":1,"feature":"billing/invoice","outcomes":["A second claim on one feature"],"requirements":[]}`)

	report, err := BuildSpecValidationResult(ws, billingSelection())
	if err == nil {
		t.Fatalf("scoped validation passed with a duplicate spec: %+v", report)
	}
	if !hasDiagnosticCode(report.Diagnostics, featureproto.ErrorCodeDuplicateSpec) {
		t.Fatalf("diagnostics = %+v, want the duplicate reported", report.Diagnostics)
	}
	finding, _ := findDiagnostic(report.Diagnostics, featureproto.ErrorCodeDuplicateSpec)
	if !strings.Contains(finding.Field, "shipping/specs/invoice-copy.json") {
		t.Fatalf("duplicate finding lost the external document's provenance: %+v", finding)
	}
}

// TestEmptyImpactedRunSucceedsExplicitly pins the no-op precedent on every
// scoped surface: nothing changed is an answer, not a failure. Core resolves
// `--impacted` and publishes the outcome, so the wire selection below is the
// document a genuinely empty impact produces.
func TestEmptyImpactedRunSucceedsExplicitly(t *testing.T) {
	ws := manyProjectWorkspace(t)
	selection := Selection{
		Mode: "impacted", Scoped: true, Baseline: "HEAD", BaselineSource: "explicit",
		ProjectIDs: []string{}, EmptyImpact: true,
	}

	catalog, err := BuildFeatureCatalogResult(ws, "", selection)
	if err != nil {
		t.Fatalf("features list --impacted: %v", err)
	}
	if !catalog.Selection.EmptyImpact || len(catalog.Features) != 0 {
		t.Fatalf("catalog = %+v, want the explicit no-op", catalog.Selection)
	}
	if catalog.Selection.Baseline == "" {
		t.Fatal("the no-op does not name the ref it measured against")
	}

	validation, err := BuildFeatureValidationResult(ws, selection)
	if err != nil {
		t.Fatalf("features validate --impacted: %v", err)
	}
	if !validation.Valid || validation.Summary.Features != 0 {
		t.Fatalf("features validate --impacted report = %+v", validation)
	}
	specs, err := BuildSpecValidationResult(ws, selection)
	if err != nil {
		t.Fatalf("specs validate --impacted: %v", err)
	}
	if !specs.Selection.EmptyImpact || specs.Summary.Specs != 0 {
		t.Fatalf("specs validate --impacted report = %+v", specs)
	}
}

func hasUnreadable(issues []FeatureDesignGraphIssue, path string) bool {
	for _, issue := range issues {
		if issue.Path == path {
			return true
		}
	}
	return false
}
