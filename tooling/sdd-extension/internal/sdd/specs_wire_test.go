package sdd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// This file is the verdict-parity harness for the PROJECT-SCOPED half of
// `validate`: features and specs, evaluated from the job context a per-project
// task receives.
//
// It exists because the answer was measured, twice, in both directions.
//
// A project-scoped job receives no `selectedProjects` — the orchestrator
// attaches that only to jobs that run once for the workspace — so a view built
// from that member alone had ZERO projects, discovery had exactly one root (the
// workspace root), and a project whose manifest and spec were both broken
// validated as:
//
//	valid:true  specs:0  authoredFeatures:0  errors:0
//
// while the same repository through a loader-built view reported six errors. A
// per-project validation job wired into CI on those terms would have been green
// on every project in the workspace, forever.
//
// Showing it its OWN project and nothing else fixed that and broke the opposite
// way, which the first `--impacted` gate run caught: go/templates/go-library
// declares a parent relation to a feature authored in tooling/scaffold, and a
// one-project identity tier reported that real relation as dangling. Both are
// pinned below.

// specsFixtureProject writes one project with a durable feature manifest and a
// spec that details it, and returns the workspace root.
func specsFixtureProject(t *testing.T, spec string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("billing/putnami.features.json", `{
	  "$schema": "https://putnami.dev/schemas/putnami-features.json",
	  "protocolVersion": 1,
	  "namespace": "billing",
	  "features": [{
	    "id": "billing/invoicing",
	    "type": "feature",
	    "name": "Invoicing",
	    "outcome": "A customer receives an invoice they can pay",
	    "owner": "billing",
	    "target": "coded",
	    "requirements": [{"id": "issue", "stage": "coded", "evidenceKinds": ["artifact"]}]
	  }]
	}`)
	write("billing/specs/invoicing.json", spec)
	return root
}

const validBillingSpec = `{
  "$schema": "https://putnami.dev/schemas/putnami-spec.json",
  "protocolVersion": 1,
  "feature": "billing/invoicing",
  "outcomes": ["A customer receives an invoice they can pay"],
  "requirements": [{"id": "issue", "text": "An invoice is issued for every completed order."}]
}`

// invalidBillingSpec details a feature no manifest in the workspace mints. It
// is the seeded contract violation both halves of the parity test look for.
const invalidBillingSpec = `{
  "$schema": "https://putnami.dev/schemas/putnami-spec.json",
  "protocolVersion": 1,
  "feature": "billing/refunds",
  "outcomes": ["A customer is refunded"],
  "requirements": []
}`

// projectScopedContext is the job context a per-project task actually receives:
// its own project, the run's selection, the complete workspace membership, and
// no `selectedProjects` (the orchestrator attaches that only to jobs that run
// once for the workspace).
//
// The membership is on it deliberately. A per-project SDD verdict OWNS one
// project's rows and RESOLVES against every authored manifest in the workspace,
// so a context without it makes the identity tier blind — see
// TestFeatureRelationsResolveAcrossProjects below.
func projectScopedContext(root string, members ...pctx.ProjectRef) *pctx.Context {
	own := pctx.Project{
		Name:     "@acme/billing",
		Path:     "billing",
		FullPath: filepath.Join(root, "billing"),
		Type:     "library",
		Options:  map[string]json.RawMessage{"publish": json.RawMessage(`{"npm":true}`)},
	}
	membership := make([]pctx.ProjectRef, 0, 1+len(members))
	membership = append(membership,
		pctx.ProjectRef{ID: "/billing", Name: "@acme/billing", Path: "billing", FullPath: filepath.Join(root, "billing")})
	membership = append(membership, members...)
	return &pctx.Context{
		WorkspaceRoot:     root,
		Project:           own,
		WorkspaceProjects: membership,
		Selection:         &pctx.Selection{Mode: pctx.SelectionModeAll, ProjectIDs: []string{"/billing"}},
	}
}

// TestSpecVerdictMatchesTheLoaderOnAProjectScopedJob is the parity assertion for
// the healthy repository: the wire-built view must SEE the project's documents,
// not merely fail to object to them.
func TestSpecVerdictMatchesTheLoaderOnAProjectScopedJob(t *testing.T) {
	root := specsFixtureProject(t, validBillingSpec)

	loaded := workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/billing", Name: "@acme/billing", Path: "billing"},
	})
	loadedReport, loadedErr := BuildSpecValidationResult(loaded, Selection{Mode: pctx.SelectionModeAll})
	if loadedErr != nil {
		t.Fatalf("the loader-built evaluation failed on a healthy repository: %v", loadedErr)
	}

	wired, _ := workspace.FromContext(projectScopedContext(root))
	wiredReport, wiredErr := BuildSpecValidationResult(wired, Selection{Mode: pctx.SelectionModeAll})
	if wiredErr != nil {
		t.Fatalf("the wire-built evaluation failed on a healthy repository: %v", wiredErr)
	}

	if wiredReport.Summary.Specs != loadedReport.Summary.Specs {
		t.Fatalf("specs seen = %d across the wire, %d through the loader",
			wiredReport.Summary.Specs, loadedReport.Summary.Specs)
	}
	if wiredReport.Summary.AuthoredFeatures != loadedReport.Summary.AuthoredFeatures {
		t.Fatalf("authored features = %d across the wire, %d through the loader",
			wiredReport.Summary.AuthoredFeatures, loadedReport.Summary.AuthoredFeatures)
	}
	// A validation that saw nothing also reports zero, which is why the counts
	// are asserted to be non-zero rather than merely equal.
	if wiredReport.Summary.Specs != 1 || wiredReport.Summary.AuthoredFeatures != 1 {
		t.Fatalf("the wire-built evaluation saw %d spec(s) and %d feature(s); it read nothing",
			wiredReport.Summary.Specs, wiredReport.Summary.AuthoredFeatures)
	}
	// `options.publish.npm` travels on the job's own project, so a TypeScript
	// package is assessed for completeness instead of reading as unpublished.
	if wiredReport.Summary.PublishableProjects != 1 {
		t.Fatalf("publishable projects = %d across the wire; options.publish did not travel",
			wiredReport.Summary.PublishableProjects)
	}
}

// TestSpecVerdictFailsOnASeededViolationAcrossTheWire is the assertion that
// makes the one above non-vacuous: the same view must FAIL on a broken
// document, with the same verdict the loader reaches.
func TestSpecVerdictFailsOnASeededViolationAcrossTheWire(t *testing.T) {
	root := specsFixtureProject(t, invalidBillingSpec)

	loaded := workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/billing", Name: "@acme/billing", Path: "billing"},
	})
	loadedReport, loadedErr := BuildSpecValidationResult(loaded, Selection{Mode: pctx.SelectionModeAll})
	if loadedErr == nil || loadedReport.Valid {
		t.Fatalf("the loader-built evaluation accepted a spec no manifest mints: %+v", loadedReport)
	}

	wired, _ := workspace.FromContext(projectScopedContext(root))
	wiredReport, wiredErr := BuildSpecValidationResult(wired, Selection{Mode: pctx.SelectionModeAll})
	if wiredErr == nil || wiredReport.Valid {
		t.Fatalf("the wire-built evaluation accepted a spec no manifest mints: %+v", wiredReport)
	}
	if wiredReport.Counts.Errors != loadedReport.Counts.Errors {
		t.Fatalf("errors = %d across the wire, %d through the loader",
			wiredReport.Counts.Errors, loadedReport.Counts.Errors)
	}
	codes := map[string]bool{}
	for _, finding := range wiredReport.Diagnostics {
		codes[finding.Code] = true
	}
	if !codes["features.unknown_feature"] {
		t.Fatalf("diagnostics = %+v, want the unauthored-feature violation", wiredReport.Diagnostics)
	}
}

// TestFeatureVerdictMatchesTheLoaderOnAProjectScopedJob pins the first step of
// the `validate` pipeline the same way. features-validate runs before
// specs-validate and reads the same membership, so a view that could not see
// the project would have made both steps vacuous together.
func TestFeatureVerdictMatchesTheLoaderOnAProjectScopedJob(t *testing.T) {
	root := specsFixtureProject(t, validBillingSpec)

	loaded := workspace.NewWorkspace(root, nil, []*workspace.Project{
		{ID: "/billing", Name: "@acme/billing", Path: "billing"},
	})
	loadedReport, loadedErr := BuildFeatureValidationResult(loaded, Selection{Mode: pctx.SelectionModeAll})
	if loadedErr != nil {
		t.Fatalf("the loader-built evaluation failed: %v", loadedErr)
	}

	wired, _ := workspace.FromContext(projectScopedContext(root))
	wiredReport, wiredErr := BuildFeatureValidationResult(wired, Selection{Mode: pctx.SelectionModeAll})
	if wiredErr != nil {
		t.Fatalf("the wire-built evaluation failed: %v", wiredErr)
	}
	if wiredReport.Summary.Features != loadedReport.Summary.Features {
		t.Fatalf("features = %d across the wire, %d through the loader",
			wiredReport.Summary.Features, loadedReport.Summary.Features)
	}
	if wiredReport.Summary.Features != 1 {
		t.Fatalf("the wire-built evaluation saw %d feature(s); it read nothing", wiredReport.Summary.Features)
	}
	if wiredReport.Summary.Requirements != loadedReport.Summary.Requirements {
		t.Fatalf("requirements = %d across the wire, %d through the loader",
			wiredReport.Summary.Requirements, loadedReport.Summary.Requirements)
	}
}

// TestFeatureRelationsResolveAcrossProjects pins the identity tier's breadth.
//
// A feature relation names a feature, not a file, and the target may be
// authored anywhere in the workspace — `parent` relations to a shared
// scaffolding feature are the normal case, not an exotic one. So the tier that
// mints identities has to read every manifest, which is exactly why the wire
// carries the whole membership to a per-project task and why the task's cache
// key declares `**/putnami.features.json` from the workspace.
//
// Both halves are asserted. Without the sibling in the membership the relation
// is reported as dangling — the false failure this test exists to prevent — and
// with it the run is clean.
func TestFeatureRelationsResolveAcrossProjects(t *testing.T) {
	root := specsFixtureProject(t, validBillingSpec)
	write := func(rel, body string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The sibling that MINTS the parent identity. It lives in another project,
	// which is the whole point.
	write("platform/putnami.features.json", `{
	  "$schema": "https://putnami.dev/schemas/putnami-features.json",
	  "protocolVersion": 1,
	  "namespace": "platform",
	  "features": [{
	    "id": "platform/commerce",
	    "type": "feature",
	    "name": "Commerce",
	    "outcome": "A business sells something",
	    "owner": "platform",
	    "target": "coded",
	    "requirements": [{"id": "sell", "stage": "coded", "evidenceKinds": ["artifact"]}]
	  }]
	}`)
	// The job's own project now points at it.
	write("billing/putnami.features.json", `{
	  "$schema": "https://putnami.dev/schemas/putnami-features.json",
	  "protocolVersion": 1,
	  "namespace": "billing",
	  "features": [{
	    "id": "billing/invoicing",
	    "type": "feature",
	    "name": "Invoicing",
	    "outcome": "A customer receives an invoice they can pay",
	    "owner": "billing",
	    "target": "coded",
	    "relations": [{"kind": "parent", "target": "platform/commerce"}],
	    "requirements": [{"id": "issue", "stage": "coded", "evidenceKinds": ["artifact"]}]
	  }]
	}`)

	sibling := pctx.ProjectRef{
		ID: "/platform", Name: "@acme/platform",
		Path: "platform", FullPath: filepath.Join(root, "platform"),
	}
	blind, _ := workspace.FromContext(projectScopedContext(root))
	blindReport, blindErr := BuildFeatureValidationResult(blind, Selection{
		Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"},
	})
	if blindErr == nil || blindReport.Valid {
		t.Fatal("a one-project identity tier accepted a relation it cannot resolve; this assertion no longer measures anything")
	}
	if !hasDiagnosticCode(blindReport.Diagnostics, "features.dangling_relation") {
		t.Fatalf("diagnostics = %+v, want the dangling-relation report the narrow view produces", blindReport.Diagnostics)
	}

	seeing, _ := workspace.FromContext(projectScopedContext(root, sibling))
	seeingReport, seeingErr := BuildFeatureValidationResult(seeing, Selection{
		Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/billing"},
	})
	if seeingErr != nil || !seeingReport.Valid {
		t.Fatalf("a real cross-project relation was reported as dangling: %v\n%+v", seeingErr, seeingReport.Diagnostics)
	}
	// The projection stays the project's own: seeing the sibling must not make
	// this task report on the sibling's features.
	if seeingReport.Summary.Features != 1 {
		t.Fatalf("features reported = %d, want only the job's own project's", seeingReport.Summary.Features)
	}
}
