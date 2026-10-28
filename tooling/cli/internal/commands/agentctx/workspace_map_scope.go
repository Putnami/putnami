package agentctx

import "go.putnami.dev/tooling/cli/internal/mapgen"

// scopeWorkspaceMap narrows the full map to the projects whose id keep holds;
// a nil keep keeps every project. The result is always a SUBSET of the full
// document: it drops entries, never adds or rewrites one, so a narrowed map and
// the full map can never disagree.
//
// The header (schema version, workspace, provenance strings) always survives,
// so a narrowed document describes itself. The docs index is workspace-scoped,
// so the project filter leaves it whole.
func scopeWorkspaceMap(full *mapgen.RepoMap, keep map[string]bool) *mapgen.RepoMap {
	scoped := &mapgen.RepoMap{
		SchemaVersion: full.SchemaVersion,
		Workspace:     full.Workspace,
		GeneratedBy:   full.GeneratedBy,
		Regenerate:    full.Regenerate,
		Docs:          full.Docs,
		// Non-nil so a filter that matches nothing serializes as [] rather than
		// null.
		Projects: []mapgen.ProjectEntry{},
	}
	for _, entry := range full.Projects {
		if keep != nil && !keep[entry.ID] {
			continue
		}
		scoped.Projects = append(scoped.Projects, entry)
	}
	return scoped
}
