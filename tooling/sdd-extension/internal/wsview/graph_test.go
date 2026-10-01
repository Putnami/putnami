package wsview

import (
	"go.putnami.dev/protocol/features/spectest"

	"strings"
	"testing"

	workspaceproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
)

// TestBuildGraphResolvesNamesToIDsAndDropsOutsideEdges pins the translation the
// architecture detector depends on: a dependency declared by NAME must become
// the ID of the member it names, one already in "/path" form is kept, and one
// naming nothing in this membership is dropped rather than kept as a dangling
// node — an edge to a project that is not here has nothing to point at.
func TestBuildGraphResolvesNamesToIDsAndDropsOutsideEdges(t *testing.T) {
	ws := NewWorkspace("/repo", nil, []*Project{
		{ID: "/producer", Name: "@acme/producer", Path: "producer"},
		{ID: "/consumer", Name: "@acme/consumer", Path: "consumer",
			Dependencies: []string{"@acme/producer", "/producer", "@acme/absent"}},
	})

	// Two declarations resolve to the same member and the third is outside the
	// membership, so the answer is that member twice — core does not dedup
	// either, and matching it is the point.
	if got := ws.Graph.DependenciesOf("/consumer"); strings.Join(got, ",") != "/producer,/producer" {
		t.Fatalf("DependenciesOf = %v, want both spellings resolved and the outside edge dropped", got)
	}
	if deps := ws.Graph.DependenciesOf("/producer"); len(deps) != 0 {
		t.Fatalf("DependenciesOf(/producer) = %v, want none", deps)
	}
	if deps := ws.Graph.DependenciesOf("/absent"); deps != nil {
		t.Fatalf("DependenciesOf on a non-member = %v, want nil", deps)
	}
}

// TestBuildGraphAddsTheImplicitScopeEdgeOnce mirrors core: an activated scope's
// includes gain an edge back to the scope-self project, and a child that
// already declares it is left alone rather than gaining a duplicate.
func TestBuildGraphAddsTheImplicitScopeEdgeOnce(t *testing.T) {
	ws := NewWorkspace("/repo", nil, []*Project{
		{ID: "/scope", Name: "scope", Path: "scope", ActivatedScope: true,
			ScopeIncludes: []string{"/scope/a", "/scope/b", "/absent"}},
		{ID: "/scope/a", Name: "a", Path: "scope/a"},
		{ID: "/scope/b", Name: "b", Path: "scope/b", Dependencies: []string{"/scope"}},
	})

	if got := ws.Graph.DependenciesOf("/scope/a"); strings.Join(got, ",") != "/scope" {
		t.Fatalf("implicit scope edge = %v, want /scope", got)
	}
	if got := ws.Graph.DependenciesOf("/scope/b"); strings.Join(got, ",") != "/scope" {
		t.Fatalf("declared scope edge = %v, want exactly one /scope", got)
	}
}

// TestNilGraphAnswersEmpty keeps a caller that was handed no graph from
// panicking: an engine reading edges must see "none", not crash.
func TestNilGraphAnswersEmpty(t *testing.T) {
	var graph *DependencyGraph
	if deps := graph.DependenciesOf("/anything"); deps != nil {
		t.Fatalf("DependenciesOf on a nil graph = %v, want nil", deps)
	}
}

func TestProjectLookupsIndexIDAndName(t *testing.T) {
	ws := NewWorkspace("/repo", &workspaceproto.Config{Name: "repo"}, []*Project{
		nil,
		{ID: "/api", Name: "@acme/api", Path: "api"},
		{ID: "/unnamed", Path: "unnamed"},
	})

	if project := ws.ProjectByID("/api"); project == nil || project.Name != "@acme/api" {
		t.Fatalf("ProjectByID(/api) = %+v", project)
	}
	if project := ws.ProjectByName("@acme/api"); project == nil || project.ID != "/api" {
		t.Fatalf("ProjectByName(@acme/api) = %+v", project)
	}
	if project := ws.ProjectByName(""); project != nil {
		t.Fatalf("an unnamed project is indexed under the empty name: %+v", project)
	}
	if project := ws.ProjectByID("/nope"); project != nil {
		t.Fatalf("ProjectByID on a non-member = %+v", project)
	}
	var absent *Workspace
	if absent.ProjectByID("/api") != nil || absent.ProjectByName("@acme/api") != nil {
		t.Fatal("lookups on a nil workspace must answer nil")
	}
}

// TestWarningCodesAreCarriedNotInferred pins the fail-closed input the
// architecture engine branches on. The wire cannot supply it today, so a view
// carries what its builder attached and nothing else.
func TestWarningCodesAreCarriedNotInferred(t *testing.T) {
	ws := NewWorkspace("/repo", nil, nil)
	if ws.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatal("a fresh view invented a warning")
	}

	ws.AddWarning(WarningCodeProviderViewUnavailable, "this human wording may change freely")
	ws.AddWarning(WarningCodeProviderViewUnavailable, "and may be repeated")
	if !ws.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatal("the attached warning code is not readable")
	}
	if len(ws.WarningCodes) != 1 || len(ws.Warnings) != 2 {
		t.Fatalf("codes = %v, messages = %v; a repeated code must be recorded once", ws.WarningCodes, ws.Warnings)
	}
	ws.AddWarning("", "prose with no machine meaning")
	if len(ws.WarningCodes) != 1 {
		t.Fatalf("an empty code was recorded: %v", ws.WarningCodes)
	}

	var absent *Workspace
	absent.AddWarning(WarningCodeProviderViewUnavailable, "no receiver")
	if absent.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatal("a nil workspace reported a warning")
	}
}

// TestWireBuiltViewIndexesTheResolvedEdges is the positive counterpart of the
// gap this test used to state.
//
// A wire-built view carried no dependency edges at all, so the architecture
// detector walked an empty graph and reported every undeclared cross-domain
// dependency as absent. `workspaceProjects[].dependencies` carries the resolved
// direct edges — already translated from declared names to workspace ids — and
// BuildGraph indexes them the same way it indexes a loader-built membership.
func TestWireBuiltViewIndexesTheResolvedEdges(t *testing.T) {
	ws, _ := FromContext(&pctx.Context{
		WorkspaceRoot: "/repo",
		WorkspaceProjects: []pctx.ProjectRef{
			{ID: "/api", Name: "@acme/api", Path: "api", Dependencies: []string{"/core"}},
			{ID: "/core", Name: "@acme/core", Path: "core"},
		},
	})
	if ws == nil || len(ws.Projects) != 2 {
		t.Fatalf("view = %+v", ws)
	}
	if deps := ws.Graph.DependenciesOf("/api"); len(deps) != 1 || deps[0] != "/core" {
		t.Fatalf("edges of /api = %v, want the resolved [/core]", deps)
	}
	if deps := ws.Graph.DependenciesOf("/core"); len(deps) != 0 {
		t.Fatalf("edges of /core = %v, want none", deps)
	}
	// A published membership is a COMPLETE view: no incomplete-graph warning,
	// so the architecture engine enforces rather than fails closed.
	if len(ws.WarningCodes) != 0 {
		t.Fatalf("a complete membership carries warnings: %v", ws.WarningCodes)
	}
}

// TestWireBuiltViewFailsClosedOnTheSelectionAlone pins the other half: a
// workspace-scoped job handed the SELECTION and not the membership must not
// report a subset as the whole workspace.
//
// Both wrong answers were measured before workspaceProjects existed. Under
// `--impacted`, a project an architecture manifest names but the run did not
// select read as absent (architecture.unknown_project on a real member), and an
// edge into it was never walked (a missed undeclared dependency). The warning
// code is what turns both into one refusal.
func TestWireBuiltViewFailsClosedOnTheSelectionAlone(t *testing.T) {
	spectest.Proves(t, "tooling/specification-driven-development", "refuse-rather-than-widen", "a-selection-only-view-fails-closed-for-workspace-evaluation")
	ws, _ := FromContext(&pctx.Context{
		WorkspaceRoot:    "/repo",
		SelectedProjects: []pctx.ProjectRef{{ID: "/api", Name: "@acme/api", Path: "api"}},
		Selection:        &pctx.Selection{Mode: pctx.SelectionModeImpacted, Scoped: true, ProjectIDs: []string{"/api"}},
	})
	if ws == nil || len(ws.Projects) != 1 {
		t.Fatalf("view = %+v", ws)
	}
	if !ws.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatalf("a selection-only view reports a complete project graph: %v", ws.WarningCodes)
	}
	if ws.Projects[0].Config != nil {
		t.Fatalf("a view carries a config for a project the wire said nothing about: %+v", ws.Projects[0].Config)
	}
}
