package lifecycle

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// anyStrings converts a decoded JSON list of strings; an absent key is empty.
func anyStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

// projects list and projects describe print the identities, paths and edges of
// the graph workspace.Load returns: the list holds every project in path
// order, and a description names the project's dependencies and the projects
// that depend on it.
func TestProjectsListAndDescribe_ServeTheLoadedGraph(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "listing-and-description-serve-the-loaded-graph")
	dir := makeProjectGraphWorkspace(t)

	ws, err := workspace.Load(dir)
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}

	output, err := captureStdout(t, func() error { return ProjectsList(dir, nil, "jsonl", nil) })
	if err != nil {
		t.Fatalf("ProjectsList: %v", err)
	}
	rows := jsonlObjects(t, output)
	if want := []string{"/apps/docs", "/apps/web", "/libs/core", "/libs/util"}; !sameStrings(jsonlIDs(t, output), want) {
		t.Fatalf("listed ids = %v, want %v", jsonlIDs(t, output), want)
	}
	for _, row := range rows {
		id, _ := row["id"].(string)
		project := ws.ProjectByID(id)
		if project == nil {
			t.Fatalf("listed %s, which the loaded workspace does not hold", id)
		}
		if row["name"] != project.Name || row["path"] != project.Path {
			t.Errorf("row %v, want name %q and path %q", row, project.Name, project.Path)
		}
		if got := anyStrings(row["dependencies"]); !sameStrings(got, project.Dependencies) {
			t.Errorf("%s dependencies = %v, want %v", id, got, project.Dependencies)
		}
	}

	output, err = captureStdout(t, func() error { return ProjectsDescribe(dir, nil, []string{"util"}, "jsonl", nil) })
	if err != nil {
		t.Fatalf("ProjectsDescribe: %v", err)
	}
	described := jsonlObjects(t, output)
	if len(described) != 1 {
		t.Fatalf("describe = %v, want one object", described)
	}
	util := described[0]
	if util["id"] != "/libs/util" || util["name"] != "util" || util["path"] != "libs/util" {
		t.Errorf("describe util = %v, want /libs/util at libs/util", util)
	}
	if got, want := anyStrings(util["dependencies"]), ws.ProjectByID("/libs/util").Dependencies; len(got) != 1 || !sameStrings(got, want) {
		t.Errorf("util dependencies = %v, want the one loaded dependency %v", got, want)
	}
	if got := anyStrings(util["dependents"]); !sameStrings(got, []string{"/apps/web"}) || !sameStrings(got, ws.Graph.DependentsOf("/libs/util")) {
		t.Errorf("util dependents = %v, want [/apps/web] from the loaded graph", got)
	}
}
