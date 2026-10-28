package agentctx

import (
	"reflect"
	"testing"

	"go.putnami.dev/tooling/cli/internal/mapgen"
)

// fullMapDocument resolves the complete map of the two-project fixture.
func fullMapDocument(t *testing.T) *mapgen.RepoMap {
	t.Helper()
	return resolvedMapDocument(t, mapCommandWorkspace(t), nil)
}

// TestScopeWorkspaceMapKeepsEveryProjectWithoutAFilter pins that a nil filter
// returns the full document.
func TestScopeWorkspaceMapKeepsEveryProjectWithoutAFilter(t *testing.T) {
	full := fullMapDocument(t)
	if !reflect.DeepEqual(scopeWorkspaceMap(full, nil), full) {
		t.Error("an unfiltered scope differs from the full map")
	}
}

// TestScopeWorkspaceMapProjectFilterNarrowsToOneEntry pins the project scope:
// one entry, equal to that entry in the full map, and the header and the docs
// index still there so the document describes itself.
func TestScopeWorkspaceMapProjectFilterNarrowsToOneEntry(t *testing.T) {
	full := fullMapDocument(t)
	fullEntries := entriesByID(full)

	scoped := scopeWorkspaceMap(full, map[string]bool{"/svc/api": true})
	if len(scoped.Projects) != 1 || scoped.Projects[0].ID != "/svc/api" {
		t.Fatalf("scope returned %+v, want only /svc/api", scoped.Projects)
	}
	if scoped.Workspace != full.Workspace || scoped.SchemaVersion != full.SchemaVersion {
		t.Error("the scoped document lost its self-describing header")
	}
	if !reflect.DeepEqual(scoped.Docs, full.Docs) {
		t.Error("the project filter narrowed the workspace docs index")
	}
	if !reflect.DeepEqual(scoped.Projects[0], fullEntries["/svc/api"]) {
		t.Errorf("scoped entry = %+v, want the full map's entry", scoped.Projects[0])
	}

	none := scopeWorkspaceMap(full, map[string]bool{})
	if none.Projects == nil || len(none.Projects) != 0 {
		t.Errorf("a filter that matches nothing = %#v, want an empty, non-nil list", none.Projects)
	}
}

// entriesByID indexes the full document's project entries so a scoped answer can
// be checked field-by-field against the map it claims to be a subset of.
func entriesByID(doc *mapgen.RepoMap) map[string]mapgen.ProjectEntry {
	index := make(map[string]mapgen.ProjectEntry, len(doc.Projects))
	for _, entry := range doc.Projects {
		index[entry.ID] = entry
	}
	return index
}
