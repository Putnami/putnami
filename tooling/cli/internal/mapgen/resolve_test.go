package mapgen

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

func renderedJSON(t *testing.T, resolution Resolution) string {
	t.Helper()
	data, err := RenderJSON(resolution.Map)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	return string(data)
}

// TestResolveMap_WritesNothing pins that a map build, cold or warm, leaves the
// workspace tree exactly as it found it: no fragment, no document, no state
// directory, and nothing under a project's .gen.
func TestResolveMap_WritesNothing(t *testing.T) {
	ws, _, _ := fixtureWorkspace(t)
	before := workspaceFiles(t, ws.Root)

	cache := NewFragmentCache()
	for range 2 {
		if _, err := ResolveMap(ws, cache); err != nil {
			t.Fatalf("ResolveMap: %v", err)
		}
	}
	if _, err := ResolveMap(ws, nil); err != nil {
		t.Fatalf("ResolveMap without a cache: %v", err)
	}

	if after := workspaceFiles(t, ws.Root); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("ResolveMap changed the workspace tree:\n--- before:\n%s\n--- after:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
}

// TestResolveMap_ReusesCachedFragmentsUntilTheirInputsChange pins the warm path
// a long-lived MCP session relies on: a cold call builds every fragment, a warm
// call reuses every one, an edit rebuilds only the edited project's, and the
// cache never changes a byte of the render.
func TestResolveMap_ReusesCachedFragmentsUntilTheirInputsChange(t *testing.T) {
	ws, _, _ := fixtureWorkspace(t)
	cache := NewFragmentCache()

	cold, err := ResolveMap(ws, cache)
	if err != nil {
		t.Fatalf("ResolveMap cold: %v", err)
	}
	if cold.FragmentsBuilt != 2 || cold.FragmentsReused != 0 {
		t.Errorf("cold: built %d / reused %d, want 2 / 0", cold.FragmentsBuilt, cold.FragmentsReused)
	}

	warm, err := ResolveMap(ws, cache)
	if err != nil {
		t.Fatalf("ResolveMap warm: %v", err)
	}
	if warm.FragmentsBuilt != 0 || warm.FragmentsReused != 2 {
		t.Errorf("warm: built %d / reused %d, want 0 / 2", warm.FragmentsBuilt, warm.FragmentsReused)
	}
	if renderedJSON(t, warm) != renderedJSON(t, cold) {
		t.Error("a warm render differs from the cold one over an unchanged tree")
	}

	writeFile(t, ws.Root, "svc/lib/README.md", "# Lib\n\nA shared library.\n")
	edited, err := ResolveMap(ws, cache)
	if err != nil {
		t.Fatalf("ResolveMap after an edit: %v", err)
	}
	if edited.FragmentsBuilt != 1 || edited.FragmentsReused != 1 {
		t.Errorf("after an edit: built %d / reused %d, want 1 / 1 — the reduce must be O(changed)",
			edited.FragmentsBuilt, edited.FragmentsReused)
	}
	if !strings.Contains(renderedJSON(t, edited), "A shared library.") {
		t.Error("the render did not pick up a working-tree edit")
	}

	uncached, err := ResolveMap(ws, nil)
	if err != nil {
		t.Fatalf("ResolveMap without a cache: %v", err)
	}
	if renderedJSON(t, uncached) != renderedJSON(t, edited) {
		t.Error("a cached render and an uncached render of the same tree disagree")
	}
}

// TestResolveMap_DropsTheFragmentsOfRemovedProjects keeps a session's cache
// bounded by the live set: a project that leaves the workspace leaves the
// cache, and the map no longer lists it.
func TestResolveMap_DropsTheFragmentsOfRemovedProjects(t *testing.T) {
	ws, api, _ := fixtureWorkspace(t)
	cache := NewFragmentCache()
	if _, err := ResolveMap(ws, cache); err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}

	narrowed := workspace.NewWorkspace(ws.Root, &wsproto.Config{Name: "example"}, []*workspace.Project{api})
	resolution, err := ResolveMap(narrowed, cache)
	if err != nil {
		t.Fatalf("ResolveMap over the narrowed workspace: %v", err)
	}
	if resolution.FragmentsReused != 1 || resolution.FragmentsBuilt != 0 {
		t.Errorf("built %d / reused %d, want 0 / 1", resolution.FragmentsBuilt, resolution.FragmentsReused)
	}
	if len(resolution.Map.Projects) != 1 || resolution.Map.Projects[0].ID != api.ID {
		t.Errorf("projects = %+v, want only %s", resolution.Map.Projects, api.ID)
	}
	cache.mu.Lock()
	_, kept := cache.fragments["/svc/lib"]
	size := len(cache.fragments)
	cache.mu.Unlock()
	if kept || size != 1 {
		t.Errorf("the cache holds %d fragments (removed project kept: %v), want only the live one", size, kept)
	}
}

// TestResolveMap_SharedCacheIsSafeUnderConcurrentCalls pins that concurrent
// MCP calls sharing one cache each get the complete, identical render.
func TestResolveMap_SharedCacheIsSafeUnderConcurrentCalls(t *testing.T) {
	ws, _, _ := fixtureWorkspace(t)
	want, err := ResolveMap(ws, nil)
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	wantJSON := renderedJSON(t, want)

	cache := NewFragmentCache()
	const calls = 8
	renders := make([]string, calls)
	errs := make([]error, calls)
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolution, err := ResolveMap(ws, cache)
			if err != nil {
				errs[i] = err
				return
			}
			data, err := RenderJSON(resolution.Map)
			errs[i] = err
			renders[i] = string(data)
		}()
	}
	wg.Wait()
	for i := range calls {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if renders[i] != wantJSON {
			t.Errorf("call %d rendered a different map", i)
		}
	}
}

// TestResolveMap_RendersAWorkspaceWithNoProjects keeps the empty case total:
// the reduce still renders a document that says it is empty.
func TestResolveMap_RendersAWorkspaceWithNoProjects(t *testing.T) {
	ws := workspace.NewWorkspace(t.TempDir(), &wsproto.Config{Name: "empty"}, nil)

	resolution, err := ResolveMap(ws, NewFragmentCache())
	if err != nil {
		t.Fatalf("ResolveMap: %v", err)
	}
	if len(resolution.Map.Projects) != 0 || resolution.FragmentsBuilt != 0 {
		t.Fatalf("resolution = %+v, want a render of zero projects", resolution)
	}
	if !strings.Contains(string(RenderMarkdown(resolution.Map)), "_No projects._") {
		t.Error("the empty map does not say it is empty")
	}
}
