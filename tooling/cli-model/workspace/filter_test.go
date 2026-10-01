package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func makeTestWorkspace() *Workspace {
	projects := []*Project{
		{ID: "/app", Name: "app", Path: "app", Tags: []string{"web"}},
		{ID: "/lib-a", Name: "lib-a", Path: "lib-a", Tags: []string{"core"}},
		{ID: "/lib-b", Name: "lib-b", Path: "lib-b", Tags: []string{"core", "deprecated"}},
		{ID: "/cli", Name: "cli", Path: "cli"},
		{ID: "/docs", Name: "docs", Path: "docs", Tags: []string{"docs"}},
	}
	byID := make(map[string]*Project, len(projects))
	for _, p := range projects {
		byID[p.ID] = p
	}
	return &Workspace{
		Name:        "test-ws",
		Root:        "/workspace",
		Projects:    projects,
		projectByID: byID,
	}
}

func projectNames(projects []*Project) []string {
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names
}

func TestFilterProjects_All(t *testing.T) {
	ws := makeTestWorkspace()
	result := FilterProjects(ws, FilterOptions{Projects: "*"})
	if len(result) != 5 {
		t.Errorf("expected 5 projects, got %d", len(result))
	}
}

// TestFilterProjects_BareDefault_SymlinkedRoot guards a symlinked-root regression:
// FindRoot canonicalizes ws.Root through EvalSymlinks, but the bare default
// (selectCurrentProject) captured cwd un-resolved. When the workspace is
// reached through a symlinked path — e.g. Conductor's per-worktree alias dirs —
// cwd != ws.Root, the cwd==root shortcut failed, and the bare default selected
// zero projects ("No projects matched"). Canonicalizing cwd restores parity.
func TestFilterProjects_BareDefault_SymlinkedRoot(t *testing.T) {
	realRoot := CanonicalRoot(t.TempDir())
	for _, p := range []string{"app", "lib-a"} {
		if err := os.MkdirAll(filepath.Join(realRoot, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A symlink alias pointing at the real workspace root, mirroring how
	// Conductor exposes a worktree under a human-friendly name.
	linkRoot := filepath.Join(t.TempDir(), "ws-alias")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ws := &Workspace{
		Root: realRoot, // canonical, as FindRoot would resolve it
		Projects: []*Project{
			{ID: "/app", Name: "app", Path: "app"},
			{ID: "/lib-a", Name: "lib-a", Path: "lib-a"},
		},
	}

	// Enter the workspace through the symlinked alias, as the user does.
	t.Chdir(linkRoot)

	// Bare default (Projects: "") at the workspace root must select every
	// project, exactly as it does when entered through the canonical path.
	result := FilterProjects(ws, FilterOptions{Projects: ""})
	if len(result) != 2 {
		t.Fatalf("bare default through symlinked root: got %d projects %v, want 2",
			len(result), projectNames(result))
	}
}

func TestFilterProjects_ByName(t *testing.T) {
	ws := makeTestWorkspace()
	result := FilterProjects(ws, FilterOptions{Projects: "app,cli"})
	names := projectNames(result)
	if len(names) != 2 {
		t.Fatalf("expected 2 projects, got %d: %v", len(names), names)
	}
}

func TestFilterProjects_ByTag(t *testing.T) {
	ws := makeTestWorkspace()
	result := FilterProjects(ws, FilterOptions{Projects: "*", FilterTag: "core"})
	names := projectNames(result)
	if len(names) != 2 {
		t.Fatalf("expected 2 core projects, got %d: %v", len(names), names)
	}
}

func TestFilterProjects_ExcludeTag(t *testing.T) {
	ws := makeTestWorkspace()
	result := FilterProjects(ws, FilterOptions{Projects: "*", ExcludeTag: "deprecated"})
	names := projectNames(result)
	if len(names) != 4 {
		t.Fatalf("expected 4 projects, got %d: %v", len(names), names)
	}
	for _, n := range names {
		if n == "lib-b" {
			t.Error("lib-b should be excluded (deprecated tag)")
		}
	}
}

func TestFilterProjects_DefaultExcludeTags(t *testing.T) {
	ws := makeTestWorkspace()
	result := FilterProjects(ws, FilterOptions{
		Projects:           "*",
		DefaultExcludeTags: []string{"docs"},
	})
	names := projectNames(result)
	if len(names) != 4 {
		t.Fatalf("expected 4 projects, got %d: %v", len(names), names)
	}
	for _, n := range names {
		if n == "docs" {
			t.Error("docs should be excluded by default exclude tags")
		}
	}
}

func TestFilterProjects_IncludeOverridesDefaultExclude(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "selection-filters", "direct-targets-and-includes-override-default-excludes")
	ws := makeTestWorkspace()
	// Default excludes "docs", but --tag docs should override that
	result := FilterProjects(ws, FilterOptions{
		Projects:           "*",
		FilterTag:          "docs",
		DefaultExcludeTags: []string{"docs"},
	})
	names := projectNames(result)
	if len(names) != 1 || names[0] != "docs" {
		t.Errorf("expected [docs], got %v", names)
	}
}

func TestFilterProjects_IncludeOverridesAllDefaultExcludes(t *testing.T) {
	ws := makeTestWorkspace()
	// Project has both "core" and "deprecated" tags.
	// Default excludes "deprecated", but --tag core should include it anyway
	// because explicit --tag overrides all default exclusions.
	result := FilterProjects(ws, FilterOptions{
		Projects:           "*",
		FilterTag:          "core",
		DefaultExcludeTags: []string{"deprecated"},
	})
	names := projectNames(result)
	if len(names) != 2 {
		t.Errorf("expected [lib-a, lib-b], got %v", names)
	}
}

func TestFilterProjects_ExplicitExcludeTagStillAppliesWithInclude(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "selection-filters", "explicit-excludes-always-apply")
	ws := makeTestWorkspace()
	// --tag core --exclude-tag deprecated: explicit exclude should still filter
	result := FilterProjects(ws, FilterOptions{
		Projects:   "*",
		FilterTag:  "core",
		ExcludeTag: "deprecated",
	})
	names := projectNames(result)
	if len(names) != 1 || names[0] != "lib-a" {
		t.Errorf("expected [lib-a], got %v", names)
	}
}

func TestFilterProjects_DirectTargetBypassesDefaultExclude(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "selection-filters", "direct-targets-and-includes-override-default-excludes")
	ws := makeTestWorkspace()
	// When a project is directly targeted by name, default exclude tags should not apply.
	result := FilterProjects(ws, FilterOptions{
		Projects:           "docs",
		DefaultExcludeTags: []string{"docs"},
		DirectTarget:       true,
	})
	names := projectNames(result)
	if len(names) != 1 || names[0] != "docs" {
		t.Errorf("expected [docs] (direct target should bypass default excludes), got %v", names)
	}
}

func TestFilterProjects_ExcludeName(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "selection-filters", "explicit-excludes-always-apply")
	ws := makeTestWorkspace()
	result := FilterProjects(ws, FilterOptions{Projects: "*", Exclude: "app,docs"})
	names := projectNames(result)
	if len(names) != 3 {
		t.Fatalf("expected 3 projects, got %d: %v", len(names), names)
	}
}

func TestFilterProjects_Combined(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "selection-filters", "name-and-tag-filters-compose")
	ws := makeTestWorkspace()
	result := FilterProjects(ws, FilterOptions{
		Projects:   "*",
		FilterTag:  "core",
		ExcludeTag: "deprecated",
	})
	names := projectNames(result)
	if len(names) != 1 || names[0] != "lib-a" {
		t.Errorf("expected [lib-a], got %v", names)
	}
}
