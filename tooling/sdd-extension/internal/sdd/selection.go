package sdd

import (
	"strings"

	pctx "go.putnami.dev/sdk/extension/context"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// Selection is the resolved project selection the orchestrator wrote on the job
// context wire.
//
// It replaces the pair the CLI engines used to take — `shared.ProjectSelection`
// (the parsed flags) and `shared.ResolvedSelection` (what resolving them
// produced). An extension never sees the first and never performs the
// resolution: the orchestrator resolved the selection once, against the
// workspace it loaded, and published the ANSWER. Re-resolving it here would be
// a second selection contract, and a validator that disagreed with the run that
// scheduled it about what was in scope is worse than one that cannot answer.
//
// The wire member marshals identically to `shared.ResolvedSelection`, which is
// what makes the swap a type change and not a semantic one.
type Selection = pctx.Selection

// selectedScope builds the engine scope one evaluation runs under.
//
// It is the second half of core's `loadSelectedWorkspace`, minus the load and
// minus the resolve. The branch is preserved exactly: a scope is built only
// when the run was NARROWED, so an unscoped run keeps its exact whole-workspace
// meaning rather than being scoped to "everything", which reads the same but
// reports differently.
func selectedScope(ws *workspace.Workspace, selection Selection) *featureengine.Scope {
	if !selection.Scoped {
		return nil
	}
	return featureengine.NewProjectScope(selectedProjects(ws, selection))
}

// selectedProjects returns the workspace members the resolved selection names,
// in workspace order.
//
// On a narrowed run the view IS the selection — `selectedProjects` on the wire
// carries the selected projects and nothing else — so this filter is normally
// an identity. It is applied anyway because the two members are independent on
// the wire: taking `ws.Projects` on faith would let a producer that populated
// one and not the other silently widen a scope.
func selectedProjects(ws *workspace.Workspace, selection Selection) []*workspace.Project {
	if ws == nil {
		return nil
	}
	named := make(map[string]bool, len(selection.ProjectIDs))
	for _, id := range selection.ProjectIDs {
		named[id] = true
	}
	selected := make([]*workspace.Project, 0, len(named))
	for _, project := range ws.Projects {
		if project != nil && named[project.ID] {
			selected = append(selected, project)
		}
	}
	return selected
}

// resolveProjectSelector resolves selector against ws: a leading "/" is tried
// as a project ID first, then the selector is tried as a project name, and
// finally as a path-shaped ID ("/" + trimmed selector).
//
// Copied from `shared.ResolveProjectSelector`. The design projection mints a
// project node from a design graph's `project` field, which is a NAME, and this
// is how that name is turned back into the member it identifies.
func resolveProjectSelector(ws *workspace.Workspace, selector string) *workspace.Project {
	if ws == nil {
		return nil
	}
	if strings.HasPrefix(selector, "/") {
		if project := ws.ProjectByID(selector); project != nil {
			return project
		}
	}
	if project := ws.ProjectByName(selector); project != nil {
		return project
	}
	return ws.ProjectByID("/" + strings.Trim(selector, "/"))
}
