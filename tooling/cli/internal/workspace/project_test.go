package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
)

// What CORE still discovers.
//
// Everything that used to be tested here about package.json, go.mod and
// pyproject.toml is gone with the parsers: `readPackageJSON`, `readGoMod`,
// `parseGoMod`, `deriveGoDependencies`, `extractWorkspaceDeps` and
// `readPyProjectName` no longer exist, and the facts they produced are now the
// three language probes' answers, tested in each extension's own suite.
//
// What remains is core's half, and these tests pin exactly it: Putnami
// membership (workspace includes, scope includes, activated scopes), the version
// cascade, the scope conventions, and a full-tree scan that knows one marker of
// its own — putnami.json — plus whatever the adapters declare.

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- discoverProject: core-owned identity ---------------------------------

func TestDiscoverProject_FromPutnamiJSON(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "identities-and-dependencies-come-from-discovery")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", wsproto.ConfigFilename),
		`{"name":"my-app","type":"application","tags":["web"],"publish":["npm"],"runsWith":["db"]}`)

	proj := discoverProject(dir, "app")
	if proj == nil {
		t.Fatal("discoverProject returned nil")
	}
	if proj.Name != "my-app" || proj.SourceName != "my-app" {
		t.Errorf("name/sourceName = %q/%q, want my-app", proj.Name, proj.SourceName)
	}
	if proj.Type != "application" {
		t.Errorf("type = %q, want application", proj.Type)
	}
	if !slices.Equal(proj.Tags, []string{"web"}) || !slices.Equal(proj.Publish, []string{"npm"}) ||
		!slices.Equal(proj.RunsWith, []string{"db"}) {
		t.Errorf("lists = %v/%v/%v, want the authored values", proj.Tags, proj.Publish, proj.RunsWith)
	}
}

func TestDiscoverProject_DockerBaseProjectIsAGraphDependency(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "identities-and-dependencies-come-from-discovery")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "base", wsproto.ConfigFilename),
		`{"name":"base","type":"image","extensions":["@putnami/go"]}`)
	writeFile(t, filepath.Join(dir, "app", wsproto.ConfigFilename),
		`{"name":"app","dependencies":["other"],"options":{"package":{"dockerBaseProject":"/base"}}}`)

	base := discoverProject(dir, "base")
	app := discoverProject(dir, "app")
	ws := NewWorkspace(dir, &wsproto.Config{}, []*Project{base, app})

	if !slices.Equal(app.Dependencies, []string{"other", "/base"}) {
		t.Fatalf("dependencies = %v, want authored dependency plus docker base project", app.Dependencies)
	}
	if got := ws.Graph.DependencyPath("/app", "/base"); !slices.Equal(got, []string{"/app", "/base"}) {
		t.Fatalf("dependency path = %v, want [/app /base]", got)
	}
	ws.AdoptProbeView(map[string]wsproto.MergedProject{
		"base": {Path: "base", SourceName: "base", Type: "image"},
		"app":  {Path: "app", SourceName: "app"},
	})
	if got := ws.Graph.DependencyPath("/app", "/base"); !slices.Equal(got, []string{"/app", "/base"}) {
		t.Fatalf("dependency path after provider-view adoption = %v, want [/app /base]", got)
	}
	impacted := ProjectsForChangedFiles(ws, []string{"base/toolchain.tar"}, ChangeImpactOptions{})
	if len(impacted) != 2 || impacted[0] != base || impacted[1] != app {
		t.Fatalf("base change impacted %v, want base then dependent app", projectIDs(impacted))
	}
}

// A directory with nothing but a language manifest resolves to its basename
// here. Its real identity arrives with the probe view; core alone cannot read a
// go.mod any more, and pretending otherwise is what this slice deleted.
func TestDiscoverProject_FallsBackToTheDirectoryName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "my-service", "go.mod"), "module example.com/svc\n\ngo 1.26\n")

	proj := discoverProject(dir, "my-service")
	if proj == nil {
		t.Fatal("discoverProject returned nil")
	}
	if proj.Name != "my-service" {
		t.Errorf("name = %q, want the directory basename", proj.Name)
	}
}

// The scope contribution is captured ONCE, at discovery, so applying a probe
// view stays a pure function of (authored config, scope chain, view). Re-reading
// the chain on every application would make identity depend on how many times a
// run happened to probe.
func TestDiscoverProject_CapturesTheScopeContribution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "typescript", "framework", wsproto.ConfigFilename),
		`{"tags":["ts"],"extensions":["@putnami/typescript"],`+
			`"publishConfig":{"npm":{"namePattern":"@putnami/{name}"}},`+
			`"distribution":{"visibility":"public"}}`)
	writeFile(t, filepath.Join(dir, "typescript", "framework", "application", "package.json"), `{"version":"0.0.0"}`)

	proj := discoverProject(dir, "typescript/framework/application")
	if proj == nil {
		t.Fatal("discoverProject returned nil")
	}
	if proj.Scope.Name != "@putnami/application" {
		t.Errorf("scope name = %q, want the resolved namePattern", proj.Scope.Name)
	}
	if proj.Name != "@putnami/application" {
		t.Errorf("name = %q, want the scope pattern applied over the basename", proj.Name)
	}
	if !slices.Equal(proj.Tags, []string{"ts"}) {
		t.Errorf("tags = %v, want the scope's", proj.Tags)
	}
	if !slices.Equal(proj.Extensions, []string{"@putnami/typescript"}) {
		t.Errorf("extensions = %v, want the scope's", proj.Extensions)
	}
	// The file the contribution came from is recorded, so change impact
	// can select this project when the scope config changes.
	if !slices.Equal(proj.Scope.ConfigPaths, []string{"typescript/framework/putnami.json"}) {
		t.Errorf("scope config paths = %v, want the merged scope file", proj.Scope.ConfigPaths)
	}
	if proj.Scope.Distribution == nil || proj.Scope.Distribution.Visibility != "public" {
		t.Errorf("scope distribution = %+v, want the scope's level", proj.Scope.Distribution)
	}
}

// A project that ships its own package identity keeps it, and the scope
// namePattern defers. The identity now arrives from the probe, so the rule is
// exercised through adoption — which is the only path it has left.
func TestAdoption_DeclaredSourceIdentityDefersTheScopePattern(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go", "sites", "examples", wsproto.ConfigFilename),
		`{"publishConfig":{"go":{"namePattern":"go.putnami.dev/examples/{name}"}}}`)
	writeFile(t, filepath.Join(dir, "go", "sites", "examples", "ts", "package.json"),
		`{"name":"@example/s2s-client","version":"0.0.0"}`)

	proj := discoverProject(dir, "go/sites/examples/ts")
	ws := NewWorkspace(dir, &wsproto.Config{}, []*Project{proj})
	ws.AdoptProbeView(map[string]wsproto.MergedProject{
		"go/sites/examples/ts": {Path: "go/sites/examples/ts", SourceName: "@example/s2s-client"},
	})

	if proj.Name != "@example/s2s-client" {
		t.Errorf("name = %q, want the declared identity", proj.Name)
	}
	// No divergence: SourceName equals Name, so no spurious "source name differs
	// from resolved name" warning fires.
	if proj.SourceName != proj.Name {
		t.Errorf("sourceName = %q, want it to equal name %q", proj.SourceName, proj.Name)
	}
	if findings := NameDivergences(ws); len(findings) != 0 {
		t.Errorf("divergences = %v, want none", findings)
	}
}

// A scope declares its LINE, not a version, and discovery records which line a
// project belongs to. The version itself is derived from that line's git tags
// at run time, so nothing here resolves one.
func TestDiscoverProject_RecordsTheNearestAncestorLine(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename),
		`{"name":"test","includes":["tooling","go"]}`)
	writeFile(t, filepath.Join(dir, "tooling", wsproto.ConfigFilename), `{"line":{},"includes":["cli"]}`)
	writeFile(t, filepath.Join(dir, "tooling", "cli", wsproto.ConfigFilename), `{"name":"@putnami/cli"}`)
	writeFile(t, filepath.Join(dir, "go", wsproto.ConfigFilename), `{"line":{"tag":"go/v{version}"},"includes":["extension"]}`)
	writeFile(t, filepath.Join(dir, "go", "extension", "go.mod"), "module go.putnami.dev/go/extension\n\ngo 1.26\n")

	if got := discoverProject(dir, "tooling/cli").Line; got != "tooling" {
		t.Errorf("tooling/cli line = %q, want tooling", got)
	}
	if got := discoverProject(dir, "go/extension").Line; got != "go" {
		t.Errorf("go/extension line = %q, want go", got)
	}
	if got := discoverProject(dir, "unscoped").Line; got != "" {
		t.Errorf("unscoped line = %q, want the implicit root line", got)
	}
	// A version is never resolved at discovery: it comes from git per line.
	if got := discoverProject(dir, "tooling/cli").Version; got != "" {
		t.Errorf("tooling/cli version = %q, want none at discovery", got)
	}
}

// --- discoverProjects: Putnami membership ---------------------------------

func TestDiscoverProjects_Includes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename),
		`{"includes":["go/framework","tooling/cli"]}`)
	writeFile(t, filepath.Join(dir, "go", "framework", wsproto.ConfigFilename),
		`{"includes":["app","http"],"tags":["go"]}`)
	writeFile(t, filepath.Join(dir, "go", "framework", "app", "go.mod"), "module go.putnami.dev/app\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "go", "framework", "http", "go.mod"), "module go.putnami.dev/http\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "tooling", "cli", "go.mod"), "module go.putnami.dev/tooling/cli\n\ngo 1.26\n")

	projects := discoverProjects(dir, &wsproto.Config{Includes: []string{"go/framework", "tooling/cli"}})
	if len(projects) != 3 {
		t.Fatalf("projects = %v, want 3", projectIDs(projects))
	}
	paths := make(map[string]bool, len(projects))
	for _, p := range projects {
		paths[p.Path] = true
	}
	for _, expected := range []string{"go/framework/app", "go/framework/http", "tooling/cli"} {
		if !paths[expected] {
			t.Errorf("missing project path %q", expected)
		}
	}
}

// The root package.json `workspaces` array is NO LONGER a discovery source: it
// is npm membership, the TypeScript adapter's own sync task owns it, and reading
// it back in a language-neutral core is the same layering mistake in the other
// direction. A directory that is not a Putnami member becomes one through
// `projects sync`.
func TestDiscoverProjects_IgnoresThePackageJSONWorkspacesArray(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"workspaces":["packages/*"]}`)
	writeFile(t, filepath.Join(dir, "packages", "web", "package.json"), `{"name":"@acme/web"}`)

	projects := discoverProjects(dir, &wsproto.Config{})
	if len(projects) != 0 {
		t.Errorf("projects = %v, want none: npm membership is not Putnami membership", projectIDs(projects))
	}
}

func TestDiscoverProjects_ScopeDedup(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename),
		`{"includes":["go/framework","go/framework/app"]}`)
	writeFile(t, filepath.Join(dir, "go", "framework", wsproto.ConfigFilename), `{"includes":["app"]}`)
	writeFile(t, filepath.Join(dir, "go", "framework", "app", "go.mod"), "module go.putnami.dev/app\n\ngo 1.26\n")

	projects := discoverProjects(dir, &wsproto.Config{Includes: []string{"go/framework", "go/framework/app"}})
	if len(projects) != 1 {
		t.Errorf("projects = %v, want 1 after dedup", projectIDs(projects))
	}
}

func TestDiscoverProjects_ScopeNonexistentProject(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), `{"includes":["go/framework"]}`)
	writeFile(t, filepath.Join(dir, "go", "framework", wsproto.ConfigFilename), `{"includes":["app","nonexistent"]}`)
	writeFile(t, filepath.Join(dir, "go", "framework", "app", "go.mod"), "module go.putnami.dev/app\n\ngo 1.26\n")

	projects := discoverProjects(dir, &wsproto.Config{Includes: []string{"go/framework"}})
	if len(projects) != 1 {
		t.Errorf("projects = %v, want 1 (the nonexistent include is skipped)", projectIDs(projects))
	}
}

func TestDiscoverProjects_ActivatedScope(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), `{"includes":["cloud"]}`)
	writeFile(t, filepath.Join(dir, "cloud", wsproto.ConfigFilename),
		`{"name":"cloud","activate":true,"includes":["workloads/api"],"extensions":["/cloud/extension"]}`)
	writeFile(t, filepath.Join(dir, "cloud", "workloads", "api", "go.mod"), "module go.putnami.dev/cloud/api\n\ngo 1.26\n")

	projects := discoverProjects(dir, &wsproto.Config{Includes: []string{"cloud"}})
	byID := make(map[string]*Project, len(projects))
	for _, p := range projects {
		byID[p.ID] = p
	}

	scope, ok := byID["/cloud"]
	if !ok {
		t.Fatalf("scope-self project /cloud missing; got %v", projectIDs(projects))
	}
	if !scope.ActivatedScope {
		t.Error("scope.ActivatedScope = false; want true")
	}
	if !slices.Equal(scope.ScopeIncludes, []string{"/cloud/workloads/api"}) {
		t.Errorf("scope.ScopeIncludes = %v; want [/cloud/workloads/api]", scope.ScopeIncludes)
	}
	if _, ok := byID["/cloud/workloads/api"]; !ok {
		t.Errorf("child /cloud/workloads/api missing; got %v", projectIDs(projects))
	}
}

func TestDiscoverProjects_ScopeWithoutActivate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wsproto.WorkspaceConfigFilename), `{"includes":["cloud"]}`)
	writeFile(t, filepath.Join(dir, "cloud", wsproto.ConfigFilename),
		`{"name":"cloud","includes":["workloads/api"],"extensions":["/cloud/extension"]}`)
	writeFile(t, filepath.Join(dir, "cloud", "workloads", "api", "go.mod"), "module go.putnami.dev/cloud/api\n\ngo 1.26\n")

	projects := discoverProjects(dir, &wsproto.Config{Includes: []string{"cloud"}})
	for _, p := range projects {
		if p.ID == "/cloud" {
			t.Error("scope /cloud should not be a project without activate: true")
		}
	}
	if len(projects) != 1 {
		t.Errorf("projects = %v; want 1", projectIDs(projects))
	}
}

// --- the full-tree scan ---------------------------------------------------

// Core knows ONE marker of its own. A directory carrying only a language
// manifest is discoverable exactly when an adapter declares that manifest, which
// is what the second half of this test exercises.
func TestScanProjectPaths_CoreKnowsOnlyItsOwnMarker(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", "package.json"), "{}")
	writeFile(t, filepath.Join(dir, "lib", "go.mod"), "module example.com/lib\n")
	writeFile(t, filepath.Join(dir, "py", "pyproject.toml"), "[project]\nname = \"py\"\n")
	writeFile(t, filepath.Join(dir, "svc", wsproto.ConfigFilename), `{"name":"svc"}`)
	// Scope-only putnami.json files are breadcrumbs, not projects.
	writeFile(t, filepath.Join(dir, "scope", wsproto.ConfigFilename), `{"tags":["scope"]}`)
	writeFile(t, filepath.Join(dir, "autonomous", wsproto.ConfigFilename), `{"includes":["app"]}`)

	bare, err := ScanProjectPaths(dir)
	if err != nil {
		t.Fatalf("ScanProjectPaths: %v", err)
	}
	if !slices.Equal(bare, []string{"svc"}) {
		t.Fatalf("bare scan = %v, want only the putnami.json project", bare)
	}

	widened, err := ScanProjectPathsWithProviders(dir, []ProviderScope{
		{Extension: "@putnami/typescript", Markers: []string{"package.json"}, Inputs: []string{"package.json"}},
		{Extension: "@putnami/go", Markers: []string{"go.mod"}, Inputs: []string{"go.mod"}},
		{Extension: "@putnami/python", Markers: []string{"pyproject.toml"}, Inputs: []string{"pyproject.toml"}},
	})
	if err != nil {
		t.Fatalf("ScanProjectPathsWithProviders: %v", err)
	}
	if !slices.Equal(widened, []string{"app", "lib", "py", "svc"}) {
		t.Errorf("widened scan = %v, want every declared marker discovered", widened)
	}
}

// `.git` and `.putnami` are core's, unconditionally. Everything else an adapter
// refuses to descend into is honored as a WALK exclusion, so the cost of not
// matching a million files is never paid.
func TestScanProjectPaths_HonorsCoreAndAdapterExclusions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", wsproto.ConfigFilename), `{"name":"app"}`)
	for _, excluded := range []string{".git", ".putnami", "node_modules", "dist"} {
		writeFile(t, filepath.Join(dir, excluded, "sub", wsproto.ConfigFilename), `{"name":"nope"}`)
	}

	paths, err := ScanProjectPathsWithProviders(dir, []ProviderScope{{
		Extension: "@putnami/typescript",
		Markers:   []string{"package.json"},
		Inputs:    []string{"package.json"},
		Excludes:  []string{"node_modules", "dist"},
	}})
	if err != nil {
		t.Fatalf("ScanProjectPathsWithProviders: %v", err)
	}
	if !slices.Equal(paths, []string{"app"}) {
		t.Errorf("paths = %v, want only app", paths)
	}
}

func TestScanProjectPaths_Empty(t *testing.T) {
	paths, err := ScanProjectPaths(t.TempDir())
	if err != nil {
		t.Fatalf("ScanProjectPaths: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want empty", paths)
	}
}

func TestIsScopeOnlyConfig_ActivateMakesItAProject(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wsproto.ConfigFilename), `{"activate":true,"includes":["child"]}`)
	if isScopeOnlyConfig(dir) {
		t.Error("isScopeOnlyConfig should be false when activate: true")
	}
}

// --- the core-owned putnami.json writer -----------------------------------

// `projects tag` writes into putnami.json, core's OWN file, and nowhere else.
// Writing it into a package.json `putnami` section — as it used to — made the
// answer depend on which language the project happened to be written in, and put
// core's authored values inside a manifest another writer owns.
func TestUpdateProjectConfigField_WritesPutnamiJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", wsproto.ConfigFilename), `{"name":"app","type":"library"}`)
	proj := &Project{Name: "app", Path: "app"}

	if err := UpdateProjectConfigField(dir, proj, "tags", []string{"web"}); err != nil {
		t.Fatalf("UpdateProjectConfigField: %v", err)
	}
	cfg := wsproto.LoadProjectConfig(filepath.Join(dir, "app"))
	if cfg == nil || !slices.Equal(cfg.Tags, []string{"web"}) || cfg.Name != "app" || cfg.Type != "library" {
		t.Fatalf("config = %+v, want the tag added and the rest preserved", cfg)
	}

	if err := UpdateProjectConfigField(dir, proj, "tags", nil); err != nil {
		t.Fatalf("UpdateProjectConfigField(nil): %v", err)
	}
	if cfg := wsproto.LoadProjectConfig(filepath.Join(dir, "app")); cfg == nil || len(cfg.Tags) != 0 {
		t.Errorf("config = %+v, want the tags removed", cfg)
	}
}

// A project with no putnami.json gets one: core's authored surface must be
// writable for every project, including one whose only manifest belongs to a
// provider.
func TestUpdateProjectConfigField_CreatesTheConfigWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app", "go.mod"), "module example.com/app\n")

	if err := UpdateProjectConfigField(dir, &Project{Name: "app", Path: "app"}, "tags", []string{"go"}); err != nil {
		t.Fatalf("UpdateProjectConfigField: %v", err)
	}
	cfg := wsproto.LoadProjectConfig(filepath.Join(dir, "app"))
	if cfg == nil || !slices.Equal(cfg.Tags, []string{"go"}) {
		t.Fatalf("config = %+v, want a created putnami.json carrying the tag", cfg)
	}
}

func TestUpdateProjectConfigField_MissingProjectDirectoryIsAnError(t *testing.T) {
	if err := UpdateProjectConfigField(t.TempDir(), &Project{Name: "x", Path: "gone"}, "tags", nil); err == nil {
		t.Error("want an error for a project directory that does not exist")
	}
}

// projectIDs is the discovery assertions' shorthand. It duplicates the helper
// of the same name in the model package's target_test.go: a test helper cannot
// be re-exported across modules the way the model symbols themselves are.
func projectIDs(projects []*Project) []string {
	ids := make([]string, len(projects))
	for i, p := range projects {
		ids[i] = p.ID
	}
	return ids
}
