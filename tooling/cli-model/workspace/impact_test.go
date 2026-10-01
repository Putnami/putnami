package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

// TestGenerateAssetSources_SkipsMalformedEntries pins the shape the untyped
// options.generate.assets decoder accepts, and that one bad entry costs only
// itself. Before this test, these branches were an indentation staircase with no
// test, so a mis-set assertion could silently drop every source in the list.
func TestGenerateAssetSources_SkipsMalformedEntries(t *testing.T) {
	// The fixtures are decoded from JSON rather than built as Go literals so
	// they are the exact shape putnami.json parsing produces — a hand-built
	// approximation could pass while the real config shape does not.
	tests := []struct {
		name   string
		assets string
		want   []string
	}{
		{name: "absent", assets: `null`},
		{name: "not a list", assets: `"docs/x.md"`},
		{name: "empty list", assets: `[]`},
		{
			name: "malformed entries are skipped, valid neighbors survive",
			assets: `[
				"a-bare-string-not-an-object",
				{"from": "keep/first.md"},
				{"to": "only-a-destination.md"},
				{"from": 42},
				{"from": ""},
				null,
				{"from": "keep/second.md", "to": "out/second.md"}
			]`,
			want: []string{"keep/first.md", "keep/second.md"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var assetsRaw any
			if err := json.Unmarshal([]byte(tc.assets), &assetsRaw); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			got := generateAssetSources(assetsRaw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d sources %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i, want := range tc.want {
				if got[i] != want {
					t.Errorf("source %d = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestCollectProjectAssetPaths(t *testing.T) {
	ws := &Workspace{
		Projects: []*Project{
			{
				ID:   "/web/app",
				Name: "web-app",
				Path: "web/app",
				Config: &wsproto.ProjectConfig{
					Build: &wsproto.BuildConfig{
						Assets: []wsproto.BuildAsset{{From: "/tooling/cli/scripts/install.sh"}},
					},
					Options: map[string]map[string]any{
						"generate": {
							"assets": []any{
								map[string]any{"from": "/docs/README.md", "to": "public/README.md"},
								map[string]any{"from": "local/file.txt", "to": "out/file.txt"},
							},
						},
					},
				},
			},
			{
				ID:     "/lib/core",
				Name:   "no-assets",
				Path:   "lib/core",
				Config: &wsproto.ProjectConfig{},
			},
			{
				ID:   "/lib/nil",
				Name: "nil-config",
				Path: "lib/nil",
			},
		},
	}

	result := CollectProjectAssetPaths(ws)

	// web-app should have 3 asset paths (keyed by ID "/web/app")
	webAssets := result["/web/app"]
	if len(webAssets) != 3 {
		t.Fatalf("expected 3 asset paths for web-app, got %d: %v", len(webAssets), webAssets)
	}
	// Workspace-root-relative paths (leading / stripped)
	if webAssets[0] != "tooling/cli/scripts/install.sh" {
		t.Errorf("expected build asset 'tooling/cli/scripts/install.sh', got %q", webAssets[0])
	}
	if webAssets[1] != "docs/README.md" {
		t.Errorf("expected generate asset 'docs/README.md', got %q", webAssets[1])
	}
	// Project-relative path
	if webAssets[2] != "web/app/local/file.txt" {
		t.Errorf("expected generate asset 'web/app/local/file.txt', got %q", webAssets[2])
	}

	// no-assets project should have no entries (keyed by ID "/lib/core")
	if len(result["/lib/core"]) != 0 {
		t.Errorf("expected 0 asset paths for no-assets, got %d", len(result["/lib/core"]))
	}

	// nil-config project should have no entries (keyed by ID "/lib/nil")
	if len(result["/lib/nil"]) != 0 {
		t.Errorf("expected 0 asset paths for nil-config, got %d", len(result["/lib/nil"]))
	}
}

func TestProjectOwnersForPathUsesProjectAndAssetPaths(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "owners-resolve-by-project-and-asset-paths")
	ws := NewWorkspace("", nil, []*Project{
		{
			ID:   "/web/app",
			Name: "web-app",
			Path: "web/app",
			Config: &wsproto.ProjectConfig{
				Build: &wsproto.BuildConfig{
					Assets: []wsproto.BuildAsset{{From: "/shared/schema.json"}},
				},
			},
		},
		{ID: "/lib/core", Name: "core", Path: "lib/core"},
	})

	owners := ProjectOwnersForPath(ws, "web/app/src/main.ts")
	if len(owners) != 1 || owners[0].ID != "/web/app" {
		t.Fatalf("ProjectOwnersForPath(project file) = %v, want /web/app", ownerProjectIDs(owners))
	}

	owners = ProjectOwnersForPath(ws, "shared/schema.json")
	if len(owners) != 1 || owners[0].ID != "/web/app" {
		t.Fatalf("ProjectOwnersForPath(asset file) = %v, want /web/app", ownerProjectIDs(owners))
	}

	owners = ProjectOwnersForPath(ws, "README.md")
	if len(owners) != 0 {
		t.Fatalf("ProjectOwnersForPath(unowned) = %v, want none", ownerProjectIDs(owners))
	}
}

func TestProjectOwnersForPath_GroupedPhysicalPathReturnsLogicalID(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "owners-resolve-by-project-and-asset-paths")
	ws := NewWorkspace("", nil, []*Project{
		{ID: "/identity/auth-server", Name: "auth-server", Path: "identity/(workloads)/auth-server"},
	})

	owners := ProjectOwnersForPath(ws, "identity/(workloads)/auth-server/src/main.go")
	if got := ownerProjectIDs(owners); len(got) != 1 || got[0] != "/identity/auth-server" {
		t.Fatalf("ProjectOwnersForPath(grouped file) = %v, want [/identity/auth-server]", got)
	}
}

func TestProjectOwnersForPathCanonicalizesSymlinkedAbsolutePath(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "owners-resolve-by-project-and-asset-paths")
	realRoot := CanonicalRoot(t.TempDir())
	if err := os.MkdirAll(filepath.Join(realRoot, "web", "app", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ws := NewWorkspace(realRoot, nil, []*Project{
		{ID: "/web/app", Name: "web-app", Path: "web/app"},
	})

	owners := ProjectOwnersForPath(ws, filepath.Join(aliasRoot, "web", "app", "src", "new.ts"))
	if len(owners) != 1 || owners[0].ID != "/web/app" {
		t.Fatalf("ProjectOwnersForPath(symlinked absolute path) = %v, want /web/app", ownerProjectIDs(owners))
	}
}

// --- ProjectsForChangedFiles: the ONE change→project calculation -------------
//
// An earlier cleanup deleted internal/watch/classifier.go, a second algorithm the
// watch loop used to map changed files onto projects. These tests pin what the
// shared calculation must keep covering, including the one rule the classifier
// had and --impacted did not.

func changeImpactWorkspace() *Workspace {
	return NewWorkspace("/workspace", nil, []*Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/utils", Name: "utils", Path: "packages/utils"},
	})
}

func TestProjectsForChangedFiles_DirectAndTransitive(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "impact-includes-the-transitive-dependent-closure")
	ws := changeImpactWorkspace()

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"packages/lib/src/index.ts"}, ChangeImpactOptions{}))

	want := []string{"/packages/app", "/packages/lib"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("changed lib file selected %v, want %v (lib plus its dependent app)", got, want)
	}
}

// The gap the deleted classifier covered and --impacted does not: a lockfile or
// root compiler config belongs to NO project by path, so the prefix mapping
// attributes it to nobody — yet every project that runs the task is invalidated.
// Watch passes the patterns; --impacted passes none, deliberately, because
// widening it is a terminal behavior change and not a refactor.
func TestProjectsForChangedFiles_WorkspaceInputPatternsWidenToEveryProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "workspace-input-patterns-widen-to-every-project")
	ws := changeImpactWorkspace()
	changed := []string{"bun.lock"}

	widened := ownerProjectIDs(ProjectsForChangedFiles(ws, changed, ChangeImpactOptions{
		WorkspaceInputPatterns: []string{"bun.lock", "tsconfig.base.json"},
	}))
	if len(widened) != len(ws.Projects) {
		t.Fatalf("workspace-scoped input change selected %v, want every project", widened)
	}

	if plain := ownerProjectIDs(ProjectsForChangedFiles(ws, changed, ChangeImpactOptions{})); len(plain) != 0 {
		t.Fatalf("without workspace input patterns a root lockfile selected %v, want none "+
			"(--impacted's behavior, unchanged)", plain)
	}
}

// A caller that DID declare an answer for workspace-root files — the watch
// loop's workspace-scoped task inputs — attributes them to every project, so
// there is nothing left unclaimed to report. The evidence describes the mapping
// that RAN, not the one some other caller would have performed.
func TestChangeImpact_WidenedRootFilesAreNotReportedUnowned(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "workspace-input-patterns-widen-to-every-project")
	ws := changeImpactWorkspace()

	widened, widenedUnowned := ChangeImpact(ws, []string{"bun.lock"}, ChangeImpactOptions{
		WorkspaceInputPatterns: []string{"bun.lock"},
	})
	if len(widened) != len(ws.Projects) {
		t.Fatalf("widened selection = %v, want every project", ownerProjectIDs(widened))
	}
	if len(widenedUnowned) != 0 {
		t.Errorf("unowned root files = %v, want none: the declared pattern claimed the file", widenedUnowned)
	}

	if _, plainUnowned := ChangeImpact(ws, []string{"bun.lock"}, ChangeImpactOptions{}); len(plainUnowned) != 1 {
		t.Errorf("unowned root files without patterns = %v, want the one unclaimed root path", plainUnowned)
	}
}

// Providers answer root-file invalidation through project-level watchedFiles in
// the v1 probe protocol. Core matches only those opaque paths, then applies its
// ordinary dependent closure. A TypeScript lockfile must therefore select
// TypeScript projects and their consumers without widening to an unrelated Go
// project, and the claimed root path must disappear from the unowned evidence.
func TestChangeImpact_ProviderRootWatchedFilesSelectOnlyClaimedProjects(t *testing.T) {
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/web", Name: "web", Path: "apps/web", Dependencies: []string{"ui"}},
		{ID: "/packages/ui", Name: "ui", Path: "packages/ui"},
		{ID: "/services/api", Name: "api", Path: "services/api"},
	})
	ws.AdoptProbeView(map[string]wsproto.MergedProject{
		"apps/web":    {Path: "apps/web", Dependencies: []string{"packages/ui"}},
		"packages/ui": {Path: "packages/ui", WatchedFiles: []string{"bun.lock", "package.json"}},
	})

	projects, unowned := ChangeImpact(ws, []string{"bun.lock"}, ChangeImpactOptions{})
	got := ownerProjectIDs(projects)
	want := []string{"/apps/web", "/packages/ui"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("provider-claimed bun.lock selected %v, want %v", got, want)
	}
	if len(unowned) != 0 {
		t.Fatalf("provider-claimed bun.lock reported unowned: %v", unowned)
	}

	projects, unowned = ChangeImpact(ws, []string{"go.work.sum"}, ChangeImpactOptions{})
	if len(projects) != 0 {
		t.Fatalf("unclaimed go.work.sum selected %v, want none", ownerProjectIDs(projects))
	}
	if len(unowned) != 1 || unowned[0] != "go.work.sum" {
		t.Fatalf("unclaimed root evidence = %v, want [go.work.sum]", unowned)
	}
}

// Multiple providers may claim the same root file for different projects. The
// merged answer is a union and core must seed every claimant before propagating
// dependents; provider order cannot make one language's project disappear.
func TestChangeImpact_ProviderRootWatchedFilesUnionAcrossProjects(t *testing.T) {
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/a", Name: "a", Path: "apps/a"},
		{ID: "/apps/b", Name: "b", Path: "apps/b"},
		{ID: "/apps/c", Name: "c", Path: "apps/c"},
	})
	ws.AdoptProbeView(map[string]wsproto.MergedProject{
		"apps/a": {Path: "apps/a", WatchedFiles: []string{"shared.lock"}},
		"apps/b": {Path: "apps/b", WatchedFiles: []string{"shared.lock"}},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"shared.lock"}, ChangeImpactOptions{}))
	want := []string{"/apps/a", "/apps/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("shared provider root file selected %v, want %v", got, want)
	}
}

func TestProjectsForChangedFiles_WorkspaceInputPatternsMatchRecursiveGlobs(t *testing.T) {
	ws := changeImpactWorkspace()

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"config/tools/biome.json"}, ChangeImpactOptions{
		WorkspaceInputPatterns: []string{"**/biome.json"},
	}))
	if len(got) != len(ws.Projects) {
		t.Fatalf("`**/` pattern selected %v, want every project", got)
	}
}

func TestProjectsForChangedFiles_CrossProjectAssetReachesConsumer(t *testing.T) {
	ws := NewWorkspace("/workspace", nil, []*Project{
		{
			ID:   "/sites/docs",
			Name: "docs",
			Path: "sites/docs",
			Config: &wsproto.ProjectConfig{
				Options: map[string]map[string]any{
					"generate": {
						"assets": []any{map[string]any{"from": "/tooling/doc", "to": "public/docs"}},
					},
				},
			},
		},
		{ID: "/tooling/cli", Name: "cli", Path: "tooling/cli"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"tooling/doc/index.md"}, ChangeImpactOptions{}))
	if len(got) != 1 || got[0] != "/sites/docs" {
		t.Fatalf("asset source change selected %v, want [/sites/docs]", got)
	}
}

// The prefix trap: "packages/library" must not be read as a file inside
// "packages/lib".
func TestProjectsForChangedFiles_PathPrefixIsNotSubstringPrefix(t *testing.T) {
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/library", Name: "library", Path: "packages/library"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"packages/library/src/main.ts"}, ChangeImpactOptions{}))
	if len(got) != 1 || got[0] != "/packages/library" {
		t.Fatalf("changed file selected %v, want [/packages/library] only", got)
	}
}

// A nested project's file is owned by the nearest project only. An earlier version kept the
// union of every ancestor so --watch would not narrow when it adopted the
// shared calculation; a later fix replaced it with the nearest-owner rule after a
// one-line edit under an activated scope selected 106 of 111 projects. The
// enclosing project is selected through a declared dependency or a declared
// file input — the two companion tests below show both routes.
func TestProjectsForChangedFiles_NestedProjectFileIsOwnedByTheNearestProjectOnly(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "nearest-project-owns-a-nested-path")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/site", Name: "site", Path: "apps/site"},
		{ID: "/apps/site/theme", Name: "theme", Path: "apps/site/theme"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"apps/site/theme/main.css"}, ChangeImpactOptions{}))
	if want := []string{"/apps/site/theme"}; !slices.Equal(got, want) {
		t.Fatalf("nested change selected %v, want %v (the nearest project only)", got, want)
	}
}

// The declared-dependency route: an enclosing project that consumes its nested
// project declares it, and propagation selects the enclosing project.
func TestProjectsForChangedFiles_EnclosingProjectSelectedThroughDeclaredDependency(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "nearest-project-owns-a-nested-path")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/site", Name: "site", Path: "apps/site", Dependencies: []string{"theme"}},
		{ID: "/apps/site/theme", Name: "theme", Path: "apps/site/theme"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"apps/site/theme/main.css"}, ChangeImpactOptions{}))
	if want := []string{"/apps/site", "/apps/site/theme"}; !slices.Equal(got, want) {
		t.Fatalf("nested change selected %v, want %v (enclosing project through its declared dependency)", got, want)
	}
}

// The declared-input route: an enclosing project that reads a nested subtree
// declares it as a file input, and selection honors the declaration.
func TestProjectsForChangedFiles_EnclosingProjectSelectedThroughDeclaredInput(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "nearest-project-owns-a-nested-path")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/site", Name: "site", Path: "apps/site", Config: declaredInputConfig(t, "site", "build", "theme/**")},
		{ID: "/apps/site/theme", Name: "theme", Path: "apps/site/theme"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"apps/site/theme/main.css"}, ChangeImpactOptions{}))
	if want := []string{"/apps/site", "/apps/site/theme"}; !slices.Equal(got, want) {
		t.Fatalf("nested change selected %v, want %v (enclosing project through its declared input)", got, want)
	}
}

// find_owner and the change plan's direct-project list answer the nearest
// project for a nested path, and the enclosing project for its own files.
func TestProjectOwnersForPath_NestedProjectAnswersTheNearestOnly(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "nearest-project-owns-a-nested-path")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/site", Name: "site", Path: "apps/site"},
		{ID: "/apps/site/theme", Name: "theme", Path: "apps/site/theme"},
	})

	if got, want := ownerProjectIDs(ProjectOwnersForPath(ws, "apps/site/theme/main.css")), []string{"/apps/site/theme"}; !slices.Equal(got, want) {
		t.Fatalf("owners of a nested project's file = %v, want %v", got, want)
	}
	if got, want := ownerProjectIDs(ProjectOwnersForPath(ws, "apps/site/index.html")), []string{"/apps/site"}; !slices.Equal(got, want) {
		t.Fatalf("owners of the enclosing project's own file = %v, want %v", got, want)
	}
}

// A project at the workspace root registers under "." like any other
// directory, so the deepest-wins walk makes it the owner of last resort: it
// owns what no deeper project claims, and nothing more.
func TestProjectOwnersForPath_RootProjectIsTheOwnerOfLastResort(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "nearest-project-owns-a-nested-path")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/root", Name: "root", Path: "."},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	})

	if got, want := ownerProjectIDs(ProjectOwnersForPath(ws, "README.md")), []string{"/root"}; !slices.Equal(got, want) {
		t.Fatalf("owners of a root-level file = %v, want %v", got, want)
	}
	if got, want := ownerProjectIDs(ProjectOwnersForPath(ws, "packages/lib/x.ts")), []string{"/packages/lib"}; !slices.Equal(got, want) {
		t.Fatalf("owners of a nested project's file = %v, want %v (not the root project)", got, want)
	}
}

// Asset claims are additive: reading a file and owning it are different
// relations, so a project that claims another project's directory as an asset
// source is selected on top of the directory owner, never instead of it.
func TestProjectsForChangedFiles_AssetClaimInsideAnotherProjectSelectsBoth(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "owners-resolve-by-project-and-asset-paths")
	// Decoded from JSON rather than built as a Go literal so the fixture is the
	// exact shape putnami.json parsing produces for an extension option tree.
	source := `{
		"name": "docs",
		"options": {"generate": {"assets": [{"from": "/tooling/doc", "to": "public/docs"}]}}
	}`
	config, diags := wsproto.ParseProjectConfig([]byte(source))
	if diag.HasErrors(diags) {
		t.Fatalf("parse fixture: %v", diags)
	}
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/sites/docs", Name: "docs", Path: "sites/docs", Config: config},
		{ID: "/tooling/doc", Name: "doc", Path: "tooling/doc"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"tooling/doc/index.md"}, ChangeImpactOptions{}))
	if want := []string{"/sites/docs", "/tooling/doc"}; !slices.Equal(got, want) {
		t.Fatalf("asset source change selected %v, want %v (claimant plus directory owner)", got, want)
	}
}

func TestProjectsForChangedFiles_NoChangedFiles(t *testing.T) {
	if got := ProjectsForChangedFiles(changeImpactWorkspace(), nil, ChangeImpactOptions{}); len(got) != 0 {
		t.Fatalf("no changed files selected %v, want none", ownerProjectIDs(got))
	}
}

// --- Activated scopes ---------------------------------------------------------
//
// An activated scope's implicit include→scope-self edge orders the schedule
// (workspace-level artifacts before their workloads, ^job resolution) and
// carries no input relation. An earlier version of propagation could not tell it from a
// declared edge, so a one-line edit of a file only the scope-self owned reached
// every include, and from there — through an include that feeds an
// in-workspace extension — every project.

// Activated scope with one include feeding an in-workspace extension: the shape
// that selected 106 of 111 projects for a one-line doc edit.
func activatedScopeImpactWorkspace() *Workspace {
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"/tools/ext": ""}}}
	inherits := ScopeContribution{ConfigPaths: []string{"cloud/putnami.json"}}
	return NewWorkspace("/workspace", cfg, []*Project{
		{ID: "/cloud", Name: "cloud", Path: "cloud", ActivatedScope: true,
			ScopeIncludes: []string{"/cloud/workloads/api", "/cloud/workloads/worker", "/cloud/libs/cli"}},
		{ID: "/cloud/workloads/api", Name: "api", Path: "cloud/workloads/api", Scope: inherits},
		{ID: "/cloud/workloads/worker", Name: "worker", Path: "cloud/workloads/worker", Scope: inherits},
		{ID: "/cloud/libs/cli", Name: "cloud-cli", Path: "cloud/libs/cli", Scope: inherits},
		{ID: "/tools/ext", Name: "ext", Path: "tools/ext", Dependencies: []string{"/cloud/libs/cli"}},
		{ID: "/other/app", Name: "other", Path: "other/app"},
	})
}

func TestProjectsForChangedFiles_ActivatedScope(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "implicit-scope-edges-order-without-propagating-impact")
	all := []string{"/cloud", "/cloud/workloads/api", "/cloud/workloads/worker", "/cloud/libs/cli", "/tools/ext", "/other/app"}
	extTasks := []string{"/tools/ext"}
	cases := []struct {
		name    string
		changed string
		mutate  func(ws *Workspace)
		want    []string
		// scoped lists the selected projects that run the extension's tasks
		// only; the others are full.
		scoped []string
	}{
		{name: "scope-own-doc-selects-the-scope-self-only", changed: "cloud/doc/overview.md", want: []string{"/cloud"}},
		// The scope config is inherited by every include, and the include
		// feeding the extension reaches it through its dependency. The rebuilt
		// extension then reaches the one project left, for its tasks.
		{name: "scope-config-selects-the-scope-self-and-every-inheritor", changed: "cloud/putnami.json", want: all, scoped: []string{"/other/app"}},
		{name: "child-file-selects-the-child-only", changed: "cloud/workloads/api/main.go", want: []string{"/cloud/workloads/api"}},
		// Before this fix, this shape selected every project in full; the scope
		// self, its other includes and the unrelated app now run the rebuilt
		// extension's tasks and nothing else.
		{name: "child-feeding-the-extension-scopes-every-consumer", changed: "cloud/libs/cli/cli.go", want: all,
			scoped: []string{"/cloud", "/cloud/workloads/api", "/cloud/workloads/worker", "/other/app"}},
		{name: "extension-manifest-fans-out", changed: "tools/ext/putnami.extension.json", want: all,
			scoped: []string{"/cloud", "/cloud/workloads/api", "/cloud/workloads/worker", "/cloud/libs/cli", "/other/app"}},
		{name: "declared-dependency-on-the-scope-self-propagates", changed: "cloud/infra/main.go",
			mutate: func(ws *Workspace) {
				ws.ProjectByID("/cloud/workloads/api").Dependencies = []string{"/cloud"}
				ws.Graph = BuildGraph(ws.Projects)
			},
			want: []string{"/cloud", "/cloud/workloads/api"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := activatedScopeImpactWorkspace()
			if tc.mutate != nil {
				tc.mutate(ws)
			}
			r := TraceChangeImpact(ws, []string{tc.changed}, ChangeImpactOptions{})
			got := ownerProjectIDs(r.Projects)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s selected %v, want %v", tc.changed, got, tc.want)
			}
			for _, id := range tc.want {
				want := []string(nil)
				if slices.Contains(tc.scoped, id) {
					want = extTasks
				}
				if scope := r.Trace.ScopeOf(id); !slices.Equal(scope, want) {
					t.Errorf("%s scoped %s to %v, want %v", tc.changed, id, scope, want)
				}
			}
		})
	}
}

// The ordering family is untouched by the impact fix: every include still
// depends on the scope-self, so ^job resolution and the dependency-closure
// context keep their answers.
func TestProjectsForChangedFiles_ActivatedScopeKeepsTheOrderingEdge(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "implicit-scope-edges-order-without-propagating-impact")
	ws := activatedScopeImpactWorkspace()

	for _, include := range []string{"/cloud/workloads/api", "/cloud/workloads/worker", "/cloud/libs/cli"} {
		if deps := ws.Graph.DependenciesOf(include); !slices.Contains(deps, "/cloud") {
			t.Errorf("DependenciesOf(%s) = %v, want the implicit edge to /cloud kept for ordering", include, deps)
		}
	}
	ordering := ws.Graph.DependentsOf("/cloud")
	if want := []string{"/cloud/workloads/api", "/cloud/workloads/worker", "/cloud/libs/cli"}; !slices.Equal(ordering, want) {
		t.Errorf("DependentsOf(/cloud) = %v, want %v (the ordering family keeps the implicit edges)", ordering, want)
	}
	if impact := ws.Graph.ImpactDependentsOf("/cloud"); impact != nil {
		t.Errorf("ImpactDependentsOf(/cloud) = %v, want nil", impact)
	}
}

// --- Scope config inheritance ------------------------------------------------
//
// A scope's putnami.json is merged into every project below it: tags,
// extensions and the name pattern reach each include through the scope chain
// (Project.Scope.ConfigPaths). Editing it changes the children's job sets and
// cache keys, so it selects them — activated or not, and for every scope in a
// nested chain. The scope-self, when the scope is activated, stays the file's
// only OWNER; the inheritors are readers, like a declared file input.

// Two scopes, one nested in the other, plus a scope-self only when activated:
//
//	typescript/putnami.json           → inherited by every typescript project
//	typescript/framework/putnami.json → inherited by the framework projects
func scopeConfigImpactWorkspace(activated bool) *Workspace {
	outer := ScopeContribution{ConfigPaths: []string{"typescript/putnami.json"}}
	both := ScopeContribution{ConfigPaths: []string{"typescript/putnami.json", "typescript/framework/putnami.json"}}
	projects := []*Project{
		{ID: "/typescript/framework/app", Name: "app", Path: "typescript/framework/app", Scope: both},
		{ID: "/typescript/framework/lib", Name: "lib", Path: "typescript/framework/lib", Scope: both},
		{ID: "/typescript/samples/demo", Name: "demo", Path: "typescript/samples/demo", Scope: outer,
			Dependencies: []string{"/typescript/framework/lib"}},
		{ID: "/go/service", Name: "service", Path: "go/service"},
	}
	if activated {
		projects = append([]*Project{{ID: "/typescript", Name: "typescript", Path: "typescript", ActivatedScope: true,
			ScopeIncludes: []string{"/typescript/framework/app", "/typescript/framework/lib", "/typescript/samples/demo"},
			Scope:         ScopeContribution{}}}, projects...)
	}
	return NewWorkspace("/workspace", &wsproto.Config{}, projects)
}

func TestProjectsForChangedFiles_ScopeConfigSelectsEveryInheritor(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-scope-config-selects-every-project-that-inherits-it")
	typescript := []string{"/typescript/framework/app", "/typescript/framework/lib", "/typescript/samples/demo"}
	cases := []struct {
		name      string
		activated bool
		changed   string
		want      []string
	}{
		{name: "activated-scope-selects-the-scope-self-and-its-inheritors", activated: true,
			changed: "typescript/putnami.json", want: append([]string{"/typescript"}, typescript...)},
		{name: "non-activated-scope-selects-its-inheritors-alone", activated: false,
			changed: "typescript/putnami.json", want: typescript},
		// The inner scope reaches only what inherits from it; the outer include
		// that depends on an inner project follows through propagation, as it
		// would for any change to that project. The activated outer scope-self
		// is selected too, as the file's directory owner, not as an inheritor.
		{name: "nested-scope-selects-its-own-inheritors-then-their-dependents", activated: true,
			changed: "typescript/framework/putnami.json", want: append([]string{"/typescript"}, typescript...)},
		{name: "nested-scope-without-a-scope-self-selects-the-same", activated: false,
			changed: "typescript/framework/putnami.json", want: typescript},
		// Inheritance is per file, not per directory: another file under the
		// scope stays with its directory owner, which is nobody here.
		{name: "another-file-under-a-non-activated-scope-selects-nothing", activated: false,
			changed: "typescript/doc/overview.md", want: []string{}},
		{name: "another-file-under-an-activated-scope-selects-the-scope-self-only", activated: true,
			changed: "typescript/doc/overview.md", want: []string{"/typescript"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := scopeConfigImpactWorkspace(tc.activated)
			got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{tc.changed}, ChangeImpactOptions{}))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s selected %v, want %v", tc.changed, got, tc.want)
			}
		})
	}
}

// The trace names the relation: the inheritors are seeded by the scope
// config, the scope-self by its directory, and a project the seeds only
// propagate to keeps a dependency edge.
func TestTraceChangeImpact_ScopeConfigSeedNamesTheScopeDirectory(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-scope-config-selects-every-project-that-inherits-it")
	ws := scopeConfigImpactWorkspace(true)

	r := TraceChangeImpact(ws, []string{"typescript/framework/putnami.json"}, ChangeImpactOptions{})

	inherited := []ImpactSeed{{File: "typescript/framework/putnami.json", Kind: ImpactSeedScopeConfig, Via: "typescript/framework"}}
	for _, id := range []string{"/typescript/framework/app", "/typescript/framework/lib"} {
		if got := r.Trace.Seeds[id]; !reflect.DeepEqual(got, inherited) {
			t.Errorf("seeds of %s = %+v, want %+v", id, got, inherited)
		}
	}
	if got, want := r.Trace.Seeds["/typescript"], []ImpactSeed{{File: "typescript/framework/putnami.json", Kind: ImpactSeedPathOwner, Via: "typescript"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of the scope-self = %+v, want %+v (it owns the file by directory, not by inheritance)", got, want)
	}
	if got, want := r.Trace.Edges["/typescript/samples/demo"], (ImpactEdge{From: "/typescript/framework/lib", Kind: ImpactEdgeDependency}); got != want {
		t.Errorf("edge into the outer include = %+v, want %+v", got, want)
	}
}

// A seed records its file and its directory claim in slash form whatever
// separator the caller's changed paths use, so a trace reads the same on every
// host and ranks against a slash-form diff. On Unix FromSlash is the identity
// and this pins the slash-form answer; on Windows it feeds backslash paths.
func TestTraceChangeImpact_SeedsRecordPathsInSlashForm(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-path-that-claimed-a-seed")
	ws := scopeConfigImpactWorkspace(true)

	r := TraceChangeImpact(ws, []string{filepath.FromSlash("typescript/framework/putnami.json")}, ChangeImpactOptions{})

	inherited := []ImpactSeed{{File: "typescript/framework/putnami.json", Kind: ImpactSeedScopeConfig, Via: "typescript/framework"}}
	for _, id := range []string{"/typescript/framework/app", "/typescript/framework/lib"} {
		if got := r.Trace.Seeds[id]; !reflect.DeepEqual(got, inherited) {
			t.Errorf("seeds of %s = %+v, want %+v", id, got, inherited)
		}
	}
	nested := filepath.FromSlash("typescript/framework/app/src/main.ts")
	r = TraceChangeImpact(ws, []string{nested}, ChangeImpactOptions{})
	if got, want := r.Trace.Seeds["/typescript/framework/app"], []ImpactSeed{{File: "typescript/framework/app/src/main.ts", Kind: ImpactSeedPathOwner, Via: "typescript/framework/app"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of the path owner = %+v, want %+v", got, want)
	}

	r = TraceChangeImpact(ws, []string{filepath.FromSlash("config/base.json")}, ChangeImpactOptions{
		WorkspaceInputPatterns: []string{"config/*.json"},
	})
	want := []ImpactSeed{{File: "config/base.json", Kind: ImpactSeedWorkspaceInput, Via: "config/*.json"}}
	if got := r.Trace.Seeds["/go/service"]; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace-input seeds = %+v, want %+v", got, want)
	}
}

// Inheriting a file is reading it, not owning it: find_owner and the change
// plan's direct-project list keep answering the directory owner alone.
func TestProjectOwnersForPath_IgnoresScopeConfigInheritance(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "owners-resolve-by-project-and-asset-paths")

	if got := ownerProjectIDs(ProjectOwnersForPath(scopeConfigImpactWorkspace(true), "typescript/putnami.json")); !slices.Equal(got, []string{"/typescript"}) {
		t.Errorf("owners of an activated scope's config = %v, want exactly [/typescript]", got)
	}
	if got := ownerProjectIDs(ProjectOwnersForPath(scopeConfigImpactWorkspace(false), "typescript/putnami.json")); len(got) != 0 {
		t.Errorf("owners of a non-activated scope's config = %v, want none", got)
	}
}

// Impact-only extension edges. Every consuming project's jobs run
// the tasks an extension's manifest declares, so a change to that manifest must
// select the extension's consumers even though nobody declares a build
// dependency on it. A manifest change is the ONLY change that does: the
// extension's own sources and every library it imports are its implementation,
// and reach its dependents the way any project's sources do. The fixture
// mirrors this repository's real shape: three workspace-level local
// extensions, one project-level extension reference in each spelling (ID and
// name), and the protocol upstreams every extension reaches through the shared
// SDK's derived Go edges.
func extensionImpactWorkspace() *Workspace {
	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"/go/extension":         "",
			"/typescript/extension": "",
			"/python/extension":     "",
			"@putnami/cloud":        "", // registry-installed: resolves to no local project
		}},
	}
	return NewWorkspace("/workspace", cfg, []*Project{
		{ID: "/go/extension", Name: "@putnami/go", Path: "go/extension",
			Dependencies: []string{"/tooling/extension-sdk", "/protocols/extension"}},
		{ID: "/typescript/extension", Name: "@putnami/typescript", Path: "typescript/extension",
			Dependencies: []string{"/tooling/extension-sdk", "/protocols/extension"},
			Extensions:   []string{"@putnami/go"}},
		{ID: "/python/extension", Name: "@putnami/python", Path: "python/extension",
			Dependencies: []string{"/tooling/extension-sdk", "/protocols/extension"},
			Extensions:   []string{"/go/extension"}},
		{ID: "/tooling/extension-sdk", Name: "extension-sdk", Path: "tooling/extension-sdk",
			Dependencies: []string{"/protocols/cli", "/protocols/extension", "/protocols/job"}},
		{ID: "/protocols/cli", Name: "protocol-cli", Path: "protocols/cli"},
		{ID: "/protocols/extension", Name: "protocol-extension", Path: "protocols/extension"},
		{ID: "/protocols/job", Name: "protocol-job", Path: "protocols/job"},
		{ID: "/apps/web", Name: "web", Path: "apps/web"},
	})
}

// A change to an extension's manifest is a change to what every consumer runs,
// so it selects the plain consuming project through the impact-only edge, for
// that extension's tasks.
func TestProjectsForChangedFiles_ExtensionManifestChangeSelectsConsumers(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	ws := extensionImpactWorkspace()
	for ext, manifest := range map[string]string{
		"/go/extension":         "go/extension/putnami.extension.json",
		"/typescript/extension": "typescript/extension/putnami.extension.json",
		"/python/extension":     "python/extension/putnami.extension.json",
	} {
		r := TraceChangeImpact(ws, []string{manifest}, ChangeImpactOptions{})
		if got := ownerProjectIDs(r.Projects); !slices.Contains(got, "/apps/web") {
			t.Errorf("change to %s selected %v — the consuming project /apps/web is missing", manifest, got)
		}
		if got, want := r.Trace.ScopeOf("/apps/web"), []string{ext}; !slices.Equal(got, want) {
			t.Errorf("change to %s scoped /apps/web to %v, want %v", manifest, got, want)
		}
	}
}

// The extension's own sources, and every library it imports, rebuild the
// binary its consumers run. They select the extension in full, and every
// consumer for that extension's tasks only: a Go extension change re-runs the
// Go jobs of every Go project and nothing else. Before this fix, each of
// these origins selected every consumer in full, which is how one library
// imported by an extension that is also a product binary made every pull
// request a full run.
func TestProjectsForChangedFiles_ExtensionImplementationChangeScopesConsumersToItsTasks(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	ws := extensionImpactWorkspace()
	extensions := []string{"/go/extension", "/typescript/extension", "/python/extension"}
	// The three extensions are workspace-level: every project runs under
	// them, so every project not full is selected for the rebuilt
	// extensions' tasks. A project that activates none of those tasks then
	// plans no job at all.
	cases := []struct {
		origin    string
		wantFull  []string
		wantScope []string
	}{
		{origin: "go/extension/internal/run.go", wantFull: []string{"/go/extension"}, wantScope: []string{"/go/extension"}},
		{origin: "typescript/extension/cmd/putnami-ts/main.go", wantFull: []string{"/typescript/extension"}, wantScope: []string{"/typescript/extension"}},
		{origin: "python/extension/cmd/putnami-python/main.go", wantFull: []string{"/python/extension"}, wantScope: []string{"/python/extension"}},
		// The shared origins reach all three extensions through derived
		// dependency edges, protocols/job and protocols/cli included, and every
		// one of them fires: each binary was rebuilt.
		{origin: "tooling/extension-sdk/sdk.go", wantFull: append(slices.Clone(extensions), "/tooling/extension-sdk"), wantScope: extensions},
		{origin: "protocols/cli/result.go", wantFull: append(slices.Clone(extensions), "/tooling/extension-sdk", "/protocols/cli"), wantScope: extensions},
		{origin: "protocols/extension/manifest.go", wantFull: append(slices.Clone(extensions), "/tooling/extension-sdk", "/protocols/extension"), wantScope: extensions},
		{origin: "protocols/job/context.go", wantFull: append(slices.Clone(extensions), "/tooling/extension-sdk", "/protocols/job"), wantScope: extensions},
	}
	for _, tc := range cases {
		r := TraceChangeImpact(ws, []string{tc.origin}, ChangeImpactOptions{})
		if got, want := ownerProjectIDs(r.Projects), ownerProjectIDs(ws.Projects); !slices.Equal(got, want) {
			t.Errorf("change to %s selected %v, want every project (%v full, the rest task-scoped)", tc.origin, got, tc.wantFull)
			continue
		}
		wantScope := slices.Clone(tc.wantScope)
		slices.Sort(wantScope)
		for _, p := range ws.Projects {
			want := wantScope
			if slices.Contains(tc.wantFull, p.ID) {
				want = nil
			}
			if got := r.Trace.ScopeOf(p.ID); !slices.Equal(got, want) {
				t.Errorf("change to %s scoped %s to %v, want %v", tc.origin, p.ID, got, want)
			}
		}
	}
}

// The two states form a lattice: full when seeded or reached by a dependency
// edge from a full project, task-scoped when reached by extension-consumer
// edges only. Full dominates, an upgrade re-propagates, and nothing leaves a
// task-scoped project.
func TestTraceChangeImpact_TaskScopedProjectsFormALattice(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "extension-consumers-run-the-changed-extension-tasks")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/tools/lint", Name: "lint", Path: "tools/lint", Dependencies: []string{"/libs/core"}},
		{ID: "/tools/gen", Name: "gen", Path: "tools/gen", Dependencies: []string{"/libs/core"}},
		{ID: "/libs/core", Name: "core", Path: "libs/core"},
		{ID: "/libs/mid", Name: "mid", Path: "libs/mid", Dependencies: []string{"/libs/core"}},
		// Runs under gen AND depends on core: full, because full dominates.
		{ID: "/apps/api", Name: "api", Path: "apps/api", Dependencies: []string{"/libs/core"}, Extensions: []string{"/tools/gen"}},
		// Runs under lint and gen, depends on nothing changed: task-scoped to both.
		{ID: "/apps/cli", Name: "cli", Path: "apps/cli", Extensions: []string{"/tools/lint", "/tools/gen"}},
		// Reached task-scoped at depth 1 (lint) and full at depth 2 (core →
		// mid → web): the upgrade wins and propagates to web-e2e.
		{ID: "/apps/web", Name: "web", Path: "apps/web", Dependencies: []string{"/libs/mid"}, Extensions: []string{"/tools/lint"}},
		{ID: "/apps/web-e2e", Name: "web-e2e", Path: "apps/web-e2e", Dependencies: []string{"/apps/web"}},
		// Depends on a task-scoped project only: not selected.
		{ID: "/apps/cli-e2e", Name: "cli-e2e", Path: "apps/cli-e2e", Dependencies: []string{"/apps/cli"}},
	})

	r := TraceChangeImpact(ws, []string{"libs/core/core.go"}, ChangeImpactOptions{})

	want := []string{"/tools/lint", "/tools/gen", "/libs/core", "/libs/mid", "/apps/api", "/apps/cli", "/apps/web", "/apps/web-e2e"}
	if got := ownerProjectIDs(r.Projects); !slices.Equal(got, want) {
		t.Fatalf("library change selected %v, want %v", got, want)
	}
	wantScopes := map[string][]string{"/apps/cli": {"/tools/gen", "/tools/lint"}}
	if !reflect.DeepEqual(r.Trace.Scopes, wantScopes) {
		t.Errorf("scopes = %v, want %v", r.Trace.Scopes, wantScopes)
	}
	for id, want := range map[string]ImpactEdge{
		"/apps/api":     {From: "/libs/core", Kind: ImpactEdgeDependency},
		"/apps/cli":     {From: "/tools/lint", Kind: ImpactEdgeExtensionConsumer},
		"/apps/web":     {From: "/libs/mid", Kind: ImpactEdgeDependency},
		"/apps/web-e2e": {From: "/apps/web", Kind: ImpactEdgeDependency},
	} {
		if got := r.Trace.Edges[id]; got != want {
			t.Errorf("edge into %s = %+v, want %+v", id, got, want)
		}
	}
	// /apps/cli-e2e's only route is through a task-scoped project.
	if edge, ok := r.Trace.Edges["/apps/cli-e2e"]; ok {
		t.Errorf("a task-scoped project widened to its dependent through %+v", edge)
	}

	// A manifest nothing resolves to as an extension is an ordinary file.
	plain := TraceChangeImpact(ws, []string{"apps/web/putnami.extension.json"}, ChangeImpactOptions{})
	if got, want := ownerProjectIDs(plain.Projects), []string{"/apps/web", "/apps/web-e2e"}; !slices.Equal(got, want) {
		t.Errorf("manifest of a project no one runs under selected %v, want %v", got, want)
	}
	if len(plain.Trace.Scopes) != 0 {
		t.Errorf("scopes = %v, want none", plain.Trace.Scopes)
	}

	// why_impacted answers from the same propagation: the same reach and the
	// same scope for every pair, with no diff to read.
	reach := ImpactReach(ws, "/libs/core")
	if got := ownerProjectIDs(reach.Projects); !slices.Equal(got, want) {
		t.Errorf("ImpactReach selected %v, want the diff's %v", got, want)
	}
	if !reflect.DeepEqual(reach.Trace.Scopes, wantScopes) || !reflect.DeepEqual(reach.Trace.Edges, r.Trace.Edges) {
		t.Errorf("ImpactReach scopes %v edges %v, want the diff's %v %v", reach.Trace.Scopes, reach.Trace.Edges, wantScopes, r.Trace.Edges)
	}
	if got := ImpactReach(ws, "/nowhere"); got.Projects != nil || got.Trace.Edges != nil {
		t.Errorf("ImpactReach of an unknown project = %+v, want empty", got)
	}
}

func TestProjectsForChangedFiles_RelativeExtensionPathSelectsConsumerWithoutSchedulingEdge(t *testing.T) {
	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"./extensions/(local)/custom": "",
		}},
	}
	ws := NewWorkspace("/workspace", cfg, []*Project{
		{
			ID:   "/extensions/custom",
			Name: "@local/custom",
			Path: "extensions/(local)/custom",
		},
		{ID: "/apps/web", Name: "web", Path: "apps/web"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(
		ws,
		[]string{"extensions/(local)/custom/putnami.extension.json"},
		ChangeImpactOptions{},
	))
	if len(got) != 2 || got[0] != "/extensions/custom" || got[1] != "/apps/web" {
		t.Fatalf("relative extension manifest change selected %v, want extension and ordinary consumer", got)
	}
	source := TraceChangeImpact(ws, []string{"extensions/(local)/custom/run.go"}, ChangeImpactOptions{})
	if got := ownerProjectIDs(source.Projects); !slices.Equal(got, []string{"/extensions/custom", "/apps/web"}) {
		t.Fatalf("relative extension source change selected %v, want the extension and its consumer", got)
	}
	if got, want := source.Trace.ScopeOf("/apps/web"), []string{"/extensions/custom"}; !slices.Equal(got, want) {
		t.Fatalf("relative extension source change scoped /apps/web to %v, want %v", got, want)
	}
	if deps := ws.Graph.DependentsOf("/extensions/custom"); len(deps) != 0 {
		t.Fatalf("relative extension path leaked into scheduling graph: %v", deps)
	}
}

// The edges are one-directional: a change to a plain consumer must not select
// the extension projects, and must not balloon into the whole workspace.
func TestProjectsForChangedFiles_ConsumerChangeDoesNotSelectExtensions(t *testing.T) {
	ws := extensionImpactWorkspace()
	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"apps/web/src/main.ts"}, ChangeImpactOptions{}))
	if len(got) != 1 || got[0] != "/apps/web" {
		t.Fatalf("consumer change selected %v, want exactly [/apps/web]", got)
	}
}

// The edges stay OUT of the scheduling graph: workspace-level extensions apply
// to the extension projects themselves, so putting them in the dependency
// graph would create cycles and reorder runs. Impact widening must not leak
// into DependentsOf.
func TestExtensionImpactEdges_DoNotEnterSchedulingGraph(t *testing.T) {
	ws := extensionImpactWorkspace()
	if deps := ws.Graph.DependentsOf("/go/extension"); len(deps) != 0 {
		t.Fatalf("scheduling graph has dependents of /go/extension: %v — extension edges are impact-only", deps)
	}
	if cycle := ws.Graph.FindCycle(); cycle != nil {
		t.Fatalf("scheduling graph has a cycle: %v", cycle)
	}
}

func ownerProjectIDs(projects []*Project) []string {
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	return ids
}

// --- Declared file inputs -----------------------------------------------------
//
// A project declares its file inputs as `options.<layer>.filePatterns`, and the
// build cache keys on the files they select. An earlier version of selection ignored the
// same declaration, so a change to a file a project declares as an input landed
// with that project's jobs unrun under `--impacted`: `@putnami/cli` declares the
// agent-workflow sources and owns the only test that rebuilds each committed
// agent artifact and compares it, and that gate had never run on CI.
//
// These tests pin the RULE, not that pair of projects: a declared input selects
// its declaring project, wherever the file sits and whichever command declared
// it.

// The configs are decoded from JSON rather than built as Go literals so they
// are the exact shape putnami.json parsing produces — a hand-built
// approximation of the option layers could pass while the real config shape
// does not.
func declaredInputConfig(t *testing.T, name, layer string, patterns ...string) *wsproto.ProjectConfig {
	t.Helper()
	encodedPatterns, err := json.Marshal(patterns)
	if err != nil {
		t.Fatalf("encode fixture patterns: %v", err)
	}
	source := fmt.Sprintf(`{"name":%q,"options":{%q:{"filePatterns":%s}}}`, name, layer, encodedPatterns)
	config, diags := wsproto.ParseProjectConfig([]byte(source))
	if diag.HasErrors(diags) {
		t.Fatalf("parse fixture %s: %v", source, diags)
	}
	return config
}

func declaredInputWorkspace(t *testing.T) *Workspace {
	t.Helper()
	config := func(name, layer string, patterns ...string) *wsproto.ProjectConfig {
		return declaredInputConfig(t, name, layer, patterns...)
	}
	return NewWorkspace("/workspace", nil, []*Project{
		// Reads another project's sources, and is itself depended on.
		{
			ID: "/tools/gate", Name: "gate", Path: "tools/gate",
			Config: config("gate", "test", "internal/**", "../../content/src/**", "../../decisions.json"),
		},
		// Declares an input under a command other than test.
		{
			ID: "/tools/linter", Name: "linter", Path: "tools/linter",
			Config: config("linter", "lint", "../../content/rules.json"),
		},
		// Declares inputs under an extension-keyed layer, with an exclusion.
		{
			ID: "/sites/docs", Name: "docs", Path: "sites/docs",
			Config: config("docs", "/typescript/extension", "../../content/src/**", "!../../content/src/ignored/**"),
		},
		{ID: "/content", Name: "content", Path: "content"},
		{ID: "/tools/downstream", Name: "downstream", Path: "tools/downstream", Dependencies: []string{"gate"}},
		{ID: "/apps/unrelated", Name: "unrelated", Path: "apps/unrelated"},
	})
}

func TestProjectsForChangedFiles_DeclaredInputSelectsItsProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "declared-file-inputs-select-their-project")
	ws := declaredInputWorkspace(t)

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"content/src/skill.md"}, ChangeImpactOptions{}))

	// The owning project by path, both declaring readers, and the gate's own
	// dependent through the ordinary graph propagation.
	want := []string{"/tools/gate", "/sites/docs", "/content", "/tools/downstream"}
	if len(got) != len(want) {
		t.Fatalf("changed declared input selected %v, want %v", got, want)
	}
	for i, id := range want {
		if got[i] != id {
			t.Fatalf("changed declared input selected %v, want %v", got, want)
		}
	}
}

// The rule is not test-only: lint, build and validate read their inputs from
// the same option layers, so a declaration under any of them selects too.
func TestProjectsForChangedFiles_DeclaredInputUnderAnyCommandSelects(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "declared-file-inputs-select-their-project")
	ws := declaredInputWorkspace(t)

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"content/rules.json"}, ChangeImpactOptions{}))

	want := []string{"/tools/linter", "/content"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("changed lint input selected %v, want %v", got, want)
	}
}

func TestProjectsForChangedFiles_GitCandidateReadersIncludeRemovedPaths(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "declared-file-inputs-select-their-project")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/tooling/gate", Name: "gate", Path: "tooling/gate", Config: declaredInputConfig(t, "gate", "test", "git:**")},
		{ID: "/tooling/clientgen", Name: "clientgen", Path: "tooling/clientgen"},
	})
	// No fixture file exists: impact must include deleted and pre-rename names
	// without asking whether Git still lists them in the current candidate.
	for _, path := range []string{"tooling/clientgen/doc/new.md", "tooling/clientgen/doc/deleted.md", "tooling/clientgen/doc/renamed.md"} {
		got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{path}, ChangeImpactOptions{}))
		if len(got) != 2 || got[0] != "/tooling/gate" || got[1] != "/tooling/clientgen" {
			t.Errorf("candidate change %s selected %v", path, got)
		}
	}
}

// An exclusion declared in the pattern set applies to selection exactly as it
// applies to the cache key — one declaration read one way.
func TestProjectsForChangedFiles_DeclaredInputExclusionIsHonored(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "declared-file-inputs-select-their-project")
	ws := declaredInputWorkspace(t)

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"content/src/ignored/vendor.md"}, ChangeImpactOptions{}))

	for _, id := range got {
		if id == "/sites/docs" {
			t.Fatalf("excluded declared input selected %v, want /sites/docs absent", got)
		}
	}
}

// A workspace-root file a project declared is claimed: the evidence must not
// report it as belonging to nobody, because a project did answer for it.
func TestChangeImpact_DeclaredRootInputIsNotReportedUnowned(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "declared-file-inputs-select-their-project")
	ws := declaredInputWorkspace(t)

	selected, unowned := ChangeImpact(ws, []string{"decisions.json"}, ChangeImpactOptions{})

	got := ownerProjectIDs(selected)
	want := []string{"/tools/gate", "/tools/downstream"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("changed declared root input selected %v, want %v", got, want)
	}
	if len(unowned) != 0 {
		t.Errorf("unowned root files = %v, want none: a project declared the file as an input", unowned)
	}
}

// Reading a file is not owning it. find_owner and the change plan's direct
// project list answer "who owns this path", and a project that merely declared
// the file as an input is a worse answer there than none — @putnami/cli
// declares every putnami.json in the workspace.
func TestProjectOwnersForPath_IgnoresDeclaredInputs(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "owners-resolve-by-project-and-asset-paths")
	ws := declaredInputWorkspace(t)

	got := ownerProjectIDs(ProjectOwnersForPath(ws, "content/src/skill.md"))

	if len(got) != 1 || got[0] != "/content" {
		t.Fatalf("owners of a declared input = %v, want exactly [/content]", got)
	}
}

// An unrelated change must not be widened by the rule: only the projects whose
// patterns actually match are added.
func TestProjectsForChangedFiles_DeclaredInputsDoNotWidenUnrelatedChanges(t *testing.T) {
	ws := declaredInputWorkspace(t)

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"apps/unrelated/src/main.ts"}, ChangeImpactOptions{}))

	if len(got) != 1 || got[0] != "/apps/unrelated" {
		t.Fatalf("unrelated change selected %v, want exactly [/apps/unrelated]", got)
	}
}

// An exclusion belongs to the layer that declares it, and stops there.
//
// A cache key concatenates only the layers that apply to its own job, so
// `options.lint` saying "!../../shared/**" is not something a `test~test` key
// ever reads. Flattening the layers into one set would let it cancel
// `options.test`'s "../../shared/**", and the project would go unselected for a
// change its test key does read — the same bug, one layer down.
func TestProjectsForChangedFiles_DeclaredInputExclusionDoesNotCrossLayers(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "declared-file-inputs-select-their-project")

	source := `{
		"name": "two-layers",
		"options": {
			"test": {"filePatterns": ["../../shared/**"]},
			"lint": {"filePatterns": ["!../../shared/**"]}
		}
	}`
	config, diags := wsproto.ParseProjectConfig([]byte(source))
	if diag.HasErrors(diags) {
		t.Fatalf("parse fixture: %v", diags)
	}
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/tools/two-layers", Name: "two-layers", Path: "tools/two-layers", Config: config},
		{ID: "/shared", Name: "shared", Path: "shared"},
	})

	got := ownerProjectIDs(ProjectsForChangedFiles(ws, []string{"shared/schema.json"}, ChangeImpactOptions{}))

	want := []string{"/tools/two-layers", "/shared"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("changed file selected %v, want %v — the lint layer's exclusion must not "+
			"cancel the test layer's include, which the test cache key reads", got, want)
	}
}

// --- Impact trace --------------------------------------------------------------
//
// The one change→project pass keeps the evidence it computes: which file
// claimed which project and how, and which edge first reached each propagated
// project. Every surface that explains a selection renders this record; none
// computes its own answer.

func TestTraceChangeImpact_SeedNamesTheOwningPath(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-path-that-claimed-a-seed")
	ws := extensionImpactWorkspace()

	r := TraceChangeImpact(ws, []string{"tooling/extension-sdk/sdk.go"}, ChangeImpactOptions{})

	want := []ImpactSeed{{File: "tooling/extension-sdk/sdk.go", Kind: ImpactSeedPathOwner, Via: "tooling/extension-sdk"}}
	if got := r.Trace.Seeds["/tooling/extension-sdk"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("seeds of /tooling/extension-sdk = %+v, want %+v", got, want)
	}
	if edge, ok := r.Trace.Edges["/tooling/extension-sdk"]; ok {
		t.Fatalf("a seeded project carries an edge %+v, want none", edge)
	}
}

func TestTraceChangeImpact_EdgesNameDependencyAndExtensionConsumer(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-edge-that-reached-a-propagated-project")
	ws := extensionImpactWorkspace()

	r := TraceChangeImpact(ws, []string{"tooling/extension-sdk/sdk.go"}, ChangeImpactOptions{})

	if got, want := r.Trace.Edges["/go/extension"], (ImpactEdge{From: "/tooling/extension-sdk", Kind: ImpactEdgeDependency}); got != want {
		t.Errorf("edge into /go/extension = %+v, want %+v", got, want)
	}
	// The first full extension the walk processes names the edge; the scope
	// lists every extension that reached the consumer.
	if got, want := r.Trace.Edges["/apps/web"], (ImpactEdge{From: "/go/extension", Kind: ImpactEdgeExtensionConsumer}); got != want {
		t.Errorf("edge into /apps/web = %+v, want %+v", got, want)
	}
	if got, want := r.Trace.ScopeOf("/apps/web"), []string{"/go/extension", "/python/extension", "/typescript/extension"}; !slices.Equal(got, want) {
		t.Errorf("scope of /apps/web = %v, want %v", got, want)
	}

	r = TraceChangeImpact(ws, []string{"go/extension/putnami.extension.json"}, ChangeImpactOptions{})

	if got, want := r.Trace.Edges["/apps/web"], (ImpactEdge{From: "/go/extension", Kind: ImpactEdgeExtensionConsumer}); got != want {
		t.Errorf("edge into /apps/web = %+v, want %+v", got, want)
	}
	wantPath := []ImpactStep{
		{Project: "/go/extension"},
		{Project: "/apps/web", Kind: ImpactEdgeExtensionConsumer},
	}
	if got := r.Trace.PathTo("/apps/web"); !reflect.DeepEqual(got, wantPath) {
		t.Errorf("PathTo(/apps/web) = %+v, want %+v", got, wantPath)
	}
	if got := r.Trace.PathTo("/nowhere"); got != nil {
		t.Errorf("PathTo of an unknown project = %+v, want nil", got)
	}
}

// scopeImpactWorkspace is an activated scope with one include that declares
// the scope as a dependency and one that does not, so the graph carries one
// declared edge and one implicit scope-self edge into the same scope.
func scopeImpactWorkspace() *Workspace {
	return NewWorkspace("/workspace", nil, []*Project{
		{ID: "/go", Name: "go", Path: "go", ActivatedScope: true, ScopeIncludes: []string{"/go/framework/log", "/go/framework/http"}},
		{ID: "/go/framework/log", Name: "log", Path: "go/framework/log"},
		{ID: "/go/framework/http", Name: "http", Path: "go/framework/http", Dependencies: []string{"/go"}},
	})
}

// The implicit scope edge orders the schedule and carries no impact, so
// a change to the scope-self's own files does not reach the include that never
// declared it. Before a fix, this test asserted the opposite — that the edge was
// labeled `scope` — which was the defect: one doc edit under an
// activated scope selected 106 of 111 projects.
func TestTraceChangeImpact_ImplicitScopeEdgeDoesNotPropagate(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-edge-that-reached-a-propagated-project")
	ws := scopeImpactWorkspace()

	r := TraceChangeImpact(ws, []string{"go/README.md"}, ChangeImpactOptions{})

	if got, want := r.Trace.Seeds["/go"], []ImpactSeed{{File: "go/README.md", Kind: ImpactSeedPathOwner, Via: "go"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of /go = %+v, want %+v", got, want)
	}
	if edge, ok := r.Trace.Edges["/go/framework/log"]; ok {
		t.Errorf("the implicit include was reached through %+v, want no edge at all", edge)
	}
	if got := ownerProjectIDs(r.Projects); !slices.Contains(got, "/go") || slices.Contains(got, "/go/framework/log") {
		t.Errorf("selection = %v, want /go without the implicit include", got)
	}
	// The include that declared its scope keeps a declared edge, and a declared
	// edge is a full edge: it still propagates.
	if got, want := r.Trace.Edges["/go/framework/http"], (ImpactEdge{From: "/go", Kind: ImpactEdgeDependency}); got != want {
		t.Errorf("edge into the declaring include = %+v, want %+v", got, want)
	}
}

// A nested file is claimed by the nearest project and by no ancestor,
// and the trace names the directory that made the claim. Before a fix, this test
// asserted the enclosing project was seeded too, with its own directory as the
// reason: the ancestor ownership that made every child edit a scope-self edit.
func TestTraceChangeImpact_NestedFileSeedsTheNearestProjectOnly(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-path-that-claimed-a-seed")
	ws := scopeImpactWorkspace()

	r := TraceChangeImpact(ws, []string{"go/framework/log/log.go"}, ChangeImpactOptions{})

	if got, want := r.Trace.Seeds["/go/framework/log"], []ImpactSeed{{File: "go/framework/log/log.go", Kind: ImpactSeedPathOwner, Via: "go/framework/log"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of the nested /go/framework/log = %+v, want %+v", got, want)
	}
	if seeds, ok := r.Trace.Seeds["/go"]; ok {
		t.Errorf("the enclosing /go was seeded by %+v, want no seed", seeds)
	}
}

func TestTraceChangeImpact_DeclaredInputSeedCarriesThePatternSet(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-path-that-claimed-a-seed")
	ws := declaredInputWorkspace(t)

	r := TraceChangeImpact(ws, []string{"content/src/skill.md"}, ChangeImpactOptions{})

	if got, want := r.Trace.Seeds["/tools/gate"], []ImpactSeed{{
		File: "content/src/skill.md", Kind: ImpactSeedDeclaredInput,
		Via: "internal/** ../../content/src/** ../../decisions.json",
	}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of the declaring reader = %+v, want %+v", got, want)
	}
	// The whole set is named, exclusion included: SelectsPath answers per set.
	if got, want := r.Trace.Seeds["/sites/docs"], []ImpactSeed{{
		File: "content/src/skill.md", Kind: ImpactSeedDeclaredInput,
		Via: "../../content/src/** !../../content/src/ignored/**",
	}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of the extension-keyed reader = %+v, want %+v", got, want)
	}
	if got, want := r.Trace.Seeds["/content"], []ImpactSeed{{File: "content/src/skill.md", Kind: ImpactSeedPathOwner, Via: "content"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of the path owner = %+v, want %+v", got, want)
	}
}

func TestTraceChangeImpact_RootWatchedFileSeedCarriesTheEntry(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "trace-names-the-path-that-claimed-a-seed")
	ws := NewWorkspace("/workspace", nil, []*Project{
		{ID: "/apps/web", Name: "web", Path: "apps/web", Dependencies: []string{"ui"}},
		{ID: "/packages/ui", Name: "ui", Path: "packages/ui"},
	})
	// Adopting a probe view rebuilds the graph from it, so the dependent edge
	// has to be stated there too, exactly as the provider answers it.
	ws.AdoptProbeView(map[string]wsproto.MergedProject{
		"apps/web":    {Path: "apps/web", Dependencies: []string{"packages/ui"}},
		"packages/ui": {Path: "packages/ui", WatchedFiles: []string{"bun.lock", "package.json"}},
	})

	r := TraceChangeImpact(ws, []string{"bun.lock"}, ChangeImpactOptions{})

	if got, want := r.Trace.Seeds["/packages/ui"], []ImpactSeed{{File: "bun.lock", Kind: ImpactSeedRootWatchedFile, Via: "bun.lock"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeds of the claiming project = %+v, want %+v", got, want)
	}
	if got, want := r.Trace.Edges["/apps/web"], (ImpactEdge{From: "/packages/ui", Kind: ImpactEdgeDependency}); got != want {
		t.Errorf("edge into the dependent = %+v, want %+v", got, want)
	}
}

func TestTraceChangeImpact_WorkspaceInputSeedsEveryProject(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "workspace-input-patterns-widen-to-every-project")
	ws := changeImpactWorkspace()

	r := TraceChangeImpact(ws, []string{"bun.lock"}, ChangeImpactOptions{
		WorkspaceInputPatterns: []string{"tsconfig.base.json", "bun.lock"},
	})

	if len(r.Projects) != len(ws.Projects) {
		t.Fatalf("widened selection = %v, want every project", ownerProjectIDs(r.Projects))
	}
	want := []ImpactSeed{{File: "bun.lock", Kind: ImpactSeedWorkspaceInput, Via: "bun.lock"}}
	for _, p := range ws.Projects {
		if got := r.Trace.Seeds[p.ID]; !reflect.DeepEqual(got, want) {
			t.Errorf("seeds of %s = %+v, want %+v", p.ID, got, want)
		}
	}
	if len(r.Trace.Edges) != 0 {
		t.Errorf("edges = %+v, want none: every project was seeded", r.Trace.Edges)
	}
}

// Every selected project is explained exactly once, and the projection callers
// read is the traced set itself.
func TestTraceChangeImpact_EveryProjectHasExactlyOneExplanation(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "every-impacted-project-has-one-explanation")
	fixtures := []struct {
		name    string
		ws      *Workspace
		changed []string
		opts    ChangeImpactOptions
	}{
		{name: "dependents", ws: changeImpactWorkspace(), changed: []string{"packages/lib/src/index.ts"}},
		{name: "extension origins", ws: extensionImpactWorkspace(), changed: []string{"tooling/extension-sdk/sdk.go", "protocols/job/context.go"}},
		{name: "extension manifest", ws: extensionImpactWorkspace(), changed: []string{"tooling/extension-sdk/sdk.go", "python/extension/putnami.extension.json"}},
		{name: "scope", ws: scopeImpactWorkspace(), changed: []string{"go/README.md"}},
		{name: "declared inputs", ws: declaredInputWorkspace(t), changed: []string{"content/src/skill.md", "decisions.json"}},
		{name: "workspace input", ws: changeImpactWorkspace(), changed: []string{"bun.lock"}, opts: ChangeImpactOptions{WorkspaceInputPatterns: []string{"bun.lock"}}},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			r := TraceChangeImpact(f.ws, f.changed, f.opts)
			if len(r.Projects) == 0 {
				t.Fatal("fixture selected nothing")
			}
			for _, p := range r.Projects {
				_, seeded := r.Trace.Seeds[p.ID]
				_, propagated := r.Trace.Edges[p.ID]
				if seeded == propagated {
					t.Errorf("%s: seeded=%v propagated=%v, want exactly one", p.ID, seeded, propagated)
				}
				path := r.Trace.PathTo(p.ID)
				if len(path) == 0 || path[len(path)-1].Project != p.ID || path[0].Kind != "" {
					t.Errorf("%s: PathTo = %+v, want a seed-first path ending at the project", p.ID, path)
					continue
				}
				// why_impacted must agree with every selection: from the seed the
				// trace starts at, ImpactPath reaches the project too.
				if ImpactPath(f.ws, path[0].Project, p.ID) == nil {
					t.Errorf("%s: selected from seed %s, but ImpactPath finds no path", p.ID, path[0].Project)
				}
			}
			if len(r.Trace.Seeds)+len(r.Trace.Edges) != len(r.Projects) {
				t.Errorf("trace explains %d+%d projects, selection has %d", len(r.Trace.Seeds), len(r.Trace.Edges), len(r.Projects))
			}
			projected, unowned := ChangeImpact(f.ws, f.changed, f.opts)
			if !reflect.DeepEqual(ownerProjectIDs(projected), ownerProjectIDs(r.Projects)) {
				t.Errorf("ChangeImpact = %v, TraceChangeImpact = %v", ownerProjectIDs(projected), ownerProjectIDs(r.Projects))
			}
			if !reflect.DeepEqual(unowned, r.UnownedRootFiles) {
				t.Errorf("ChangeImpact unowned = %v, TraceChangeImpact = %v", unowned, r.UnownedRootFiles)
			}
		})
	}
}

// Two seeds that reach one project at the same depth: the recorded parent must
// be the same on every run, or the explanation would change each time it is
// read. Seeds come from a map, so this pins that the walk orders them.
func TestTraceChangeImpact_ParentIsStableAcrossRuns(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "every-impacted-project-has-one-explanation")
	ws := extensionImpactWorkspace()
	changed := []string{"typescript/extension/putnami.extension.json", "python/extension/putnami.extension.json"}

	first := TraceChangeImpact(ws, changed, ChangeImpactOptions{}).Trace.Edges
	if got, want := first["/apps/web"], (ImpactEdge{From: "/typescript/extension", Kind: ImpactEdgeExtensionConsumer}); got != want {
		t.Fatalf("edge into /apps/web = %+v, want the first seed in project order %+v", got, want)
	}
	for i := 0; i < 20; i++ {
		if again := TraceChangeImpact(ws, changed, ChangeImpactOptions{}).Trace.Edges; !reflect.DeepEqual(again, first) {
			t.Fatalf("run %d: edges = %+v, want %+v", i, again, first)
		}
	}
}

// ImpactPath walks the union `impacted` widens through, so it crosses the
// extension-consumer edge DependentPath does not know about — the gap that let
// `why_impacted` deny a pair `impacted` had selected.
func TestImpactPath_CrossesExtensionConsumerEdges(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "impact-path-walks-the-same-edges-as-the-closure")
	ws := extensionImpactWorkspace()

	want := []ImpactStep{
		{Project: "/go/extension"},
		{Project: "/apps/web", Kind: ImpactEdgeExtensionConsumer},
	}
	if got := ImpactPath(ws, "/go/extension", "/apps/web"); !reflect.DeepEqual(got, want) {
		t.Errorf("ImpactPath = %+v, want %+v", got, want)
	}
	if got := ws.Graph.DependentPath("/go/extension", "/apps/web"); got != nil {
		t.Errorf("DependentPath = %v, want nil: the scheduling graph has no extension edges", got)
	}
	// A library the extension imports rebuilds the extension, which reaches its
	// consumers for its tasks: the path crosses both edge kinds, and the reach
	// records the scope.
	if got, want := ImpactPath(ws, "/tooling/extension-sdk", "/apps/web"), []ImpactStep{
		{Project: "/tooling/extension-sdk"},
		{Project: "/go/extension", Kind: ImpactEdgeDependency},
		{Project: "/apps/web", Kind: ImpactEdgeExtensionConsumer},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("ImpactPath from a library an extension imports = %+v, want %+v", got, want)
	}
	if got, want := ImpactReach(ws, "/tooling/extension-sdk").Trace.ScopeOf("/apps/web"), []string{"/go/extension", "/python/extension", "/typescript/extension"}; !slices.Equal(got, want) {
		t.Errorf("reach from the library scopes /apps/web to %v, want %v", got, want)
	}
	if got, want := ImpactPath(ws, "/tooling/extension-sdk", "/go/extension"), []ImpactStep{
		{Project: "/tooling/extension-sdk"},
		{Project: "/go/extension", Kind: ImpactEdgeDependency},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("ImpactPath from the library to the extension = %+v, want %+v", got, want)
	}
	// Edges are one-directional: a consumer reaches no extension.
	if got := ImpactPath(ws, "/apps/web", "/go/extension"); got != nil {
		t.Errorf("ImpactPath from a consumer = %+v, want nil", got)
	}
	if got, want := ImpactPath(ws, "/apps/web", "/apps/web"), []ImpactStep{{Project: "/apps/web"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("self path = %+v, want %+v", got, want)
	}
	if got := ImpactPath(ws, "/apps/web", "/nowhere"); got != nil {
		t.Errorf("path to an unknown project = %+v, want nil", got)
	}
}

const providerChainDigest = "3333333333333333333333333333333333333333333333333333333333333333"

// providerChainWorkspace is the chain that turned one test file inside an API
// workload into a whole-workspace run: a provider, the committed client of its
// contract, an in-workspace extension that imports that client, the projects
// that run under the extension, and one dependency dependent of the provider.
func providerChainWorkspace() *Workspace {
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{"/tools/cli": ""}}}
	return NewWorkspace("/workspace", cfg, []*Project{
		{ID: "/services/api", Name: "api", Path: "services/api", ContractServiceID: "api.items",
			ContractPath: "services/api/schema/openapi.json", ContractSHA256: providerChainDigest},
		{ID: "/services/api/clients/sdk", Name: "api-sdk", Path: "services/api/clients/sdk",
			GeneratedClient: &GeneratedClientBinding{ServiceID: "api.items", ContractSHA256: providerChainDigest, Language: "ts"}},
		{ID: "/tools/cli", Name: "cli", Path: "tools/cli", Dependencies: []string{"/services/api/clients/sdk"}},
		{ID: "/apps/admin", Name: "admin", Path: "apps/admin", Dependencies: []string{"/services/api"}},
		{ID: "/apps/web", Name: "web", Path: "apps/web"},
		{ID: "/libs/shared", Name: "shared", Path: "libs/shared"},
	})
}

// providerChainIndex declares what the chain's tasks read. The provider's
// `build~describe` reads every source of its package, test files included, and
// a dependent's `^describe` reference carries it: the shape that let a test
// file cross every edge of the chain.
func providerChainIndex() fakeTaskIndex {
	reached := []string{"build~compile", "build~describe", "test~test"}
	return fakeTaskIndex{
		reading: map[string][]string{
			"/services/api|services/api/internal/store/store_test.go": {"build~describe", "test~test"},
			"/services/api|services/api/internal/store/store.go":      {"build~compile", "build~describe", "test~test"},
			"/services/api|services/api/schema/openapi.json":          {"build~describe"},
		},
		reached:        map[string][]string{"build~describe": reached},
		everyReached:   reached,
		extensionTasks: map[string][]string{"/tools/cli": {"/tools/cli#build~describe", "/tools/cli#test~test"}},
	}
}

// An implementation change of a provider, test files included, selects the
// provider and its dependency dependents only. With the committed contract
// unchanged, the contract edge does not fire, so neither the client nor the
// extension that imports it nor that extension's consumers are selected.
func TestTraceChangeImpact_AProviderImplementationChangeLeavesItsClientsAlone(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	ws := providerChainWorkspace()

	for _, changed := range [][]string{
		{"services/api/internal/store/store_test.go"},
		{"services/api/internal/store/store.go"},
		{"services/api/internal/store/store.go", "services/api/internal/store/store_test.go"},
	} {
		r := TraceChangeImpact(ws, changed, ChangeImpactOptions{Tasks: providerChainIndex()})

		if got, want := ownerProjectIDs(r.Projects), []string{"/services/api", "/apps/admin"}; !slices.Equal(got, want) {
			t.Errorf("%v selected %v, want the provider and its dependency dependent %v", changed, got, want)
		}
		if got, want := r.Trace.Edges["/apps/admin"], (ImpactEdge{From: "/services/api", Kind: ImpactEdgeDependency}); got != want {
			t.Errorf("%v reached /apps/admin by %+v, want %+v", changed, got, want)
		}
		for _, id := range []string{"/services/api/clients/sdk", "/tools/cli", "/apps/web", "/libs/shared"} {
			if edge, reached := r.Trace.Edges[id]; reached {
				t.Errorf("%v reached %s by %+v, want it unselected", changed, id, edge)
			}
		}
	}
}

// A change to the provider's committed contract does fire the contract edge,
// and the chain behind it follows on its own edges: the extension that imports
// the client, then every project that runs under it, for its tasks only.
func TestTraceChangeImpact_AProviderContractChangeReachesItsClients(t *testing.T) {
	spectest.Proves(t, "cli/workspace-model", "change-impact", "a-change-selects-only-the-tasks-that-read-it")
	ws := providerChainWorkspace()

	r := TraceChangeImpact(ws, []string{"services/api/schema/openapi.json"}, ChangeImpactOptions{Tasks: providerChainIndex()})

	want := []string{"/services/api", "/services/api/clients/sdk", "/tools/cli", "/apps/admin", "/apps/web", "/libs/shared"}
	if got := ownerProjectIDs(r.Projects); !slices.Equal(got, want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	wantEdges := map[string]ImpactEdge{
		"/services/api/clients/sdk": {From: "/services/api", Kind: ImpactEdgeContract,
			Via: "services/api/schema/openapi.json", ContractSHA256: providerChainDigest},
		"/tools/cli":   {From: "/services/api/clients/sdk", Kind: ImpactEdgeDependency},
		"/apps/admin":  {From: "/services/api", Kind: ImpactEdgeDependency},
		"/apps/web":    {From: "/tools/cli", Kind: ImpactEdgeExtensionConsumer},
		"/libs/shared": {From: "/tools/cli", Kind: ImpactEdgeExtensionConsumer},
	}
	if !reflect.DeepEqual(r.Trace.Edges, wantEdges) {
		t.Errorf("edges = %+v, want %+v", r.Trace.Edges, wantEdges)
	}
	if got, want := r.Trace.ScopeOf("/apps/web"), []string{"/tools/cli#build~describe", "/tools/cli#test~test"}; !slices.Equal(got, want) {
		t.Errorf("scoped /apps/web to %v, want the extension's tasks %v", got, want)
	}
}
