package mapgen

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// workspaceFiles lists every path under root, so a test can prove a call
// created, removed, or renamed nothing.
func workspaceFiles(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)
	return paths
}

func renderedJSON(t *testing.T, repoMap *RepoMap) string {
	t.Helper()
	data, err := RenderJSON(repoMap)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	return string(data)
}

// TestResolveMap_WritesNothing pins that a map build leaves the workspace tree
// exactly as it found it: no fragment, no document, no state directory, and
// nothing under a project's .gen.
func TestResolveMap_WritesNothing(t *testing.T) {
	ws, _, _ := fixtureWorkspace(t)
	before := workspaceFiles(t, ws.Root)

	for range 2 {
		if _, err := ResolveMap(ws); err != nil {
			t.Fatalf("ResolveMap: %v", err)
		}
	}

	if after := workspaceFiles(t, ws.Root); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("ResolveMap changed the workspace tree:\n--- before:\n%s\n--- after:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}

// TestResolveMap_FollowsTheWorkingTree pins that every build reads the inputs
// on disk: an unchanged tree renders the same bytes, and an edit reaches the
// next render.
func TestResolveMap_FollowsTheWorkingTree(t *testing.T) {
	ws, _, _ := fixtureWorkspace(t)

	first, err := ResolveMap(ws)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	again, err := ResolveMap(ws)
	if err != nil {
		t.Fatalf("ResolveMap again: %v", err)
	}
	if renderedJSON(t, again) != renderedJSON(t, first) {
		t.Error("two renders of an unchanged tree differ")
	}

	writeFile(t, ws.Root, "svc/lib/README.md", "# Lib\n\nA shared library.\n")
	edited, err := ResolveMap(ws)
	if err != nil {
		t.Fatalf("ResolveMap after an edit: %v", err)
	}
	if !strings.Contains(renderedJSON(t, edited), "A shared library.") {
		t.Error("the render did not pick up a working-tree edit")
	}
}

// TestResolveMap_ListsOnlyTheLiveProjects pins that a project that leaves the
// workspace leaves the map.
func TestResolveMap_ListsOnlyTheLiveProjects(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)
	if _, err := ResolveMap(ws); err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}

	narrowed := workspace.NewWorkspace(ws.Root, &wsproto.Config{Name: "example"}, []*workspace.Project{api})
	repoMap, err := ResolveMap(narrowed)
	if err != nil {
		t.Fatalf("ResolveMap over the narrowed workspace: %v", err)
	}
	if len(repoMap.Projects) != 1 || repoMap.Projects[0].ID != api.ID {
		t.Errorf("projects = %+v, want only %s", repoMap.Projects, api.ID)
	}
}

// TestResolveMap_RendersAWorkspaceWithNoProjects keeps the empty case total:
// the reduce still renders a document that says it is empty.
func TestResolveMap_RendersAWorkspaceWithNoProjects(t *testing.T) {
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "empty"}, nil)

	repoMap, err := ResolveMap(ws)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	if len(repoMap.Projects) != 0 {
		t.Fatalf("map = %+v, want a render of zero projects", repoMap)
	}
	if !strings.Contains(string(RenderMarkdown(repoMap)), "_No projects._") {
		t.Error("the empty map does not say it is empty")
	}
}
