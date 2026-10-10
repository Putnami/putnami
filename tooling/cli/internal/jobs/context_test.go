package jobs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestBuildJobContext(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{
		Name:   "test-workspace",
		Root:   "/workspace",
		Config: &wsproto.Config{},
	}

	proj := &workspace.Project{
		Name:    "my-app",
		Path:    "packages/my-app",
		Publish: []string{"npm"},
	}

	ext := &extension.ExtensionDescription{
		Name: "@putnami/typescript",
		Path: "/workspace/node_modules/@putnami/typescript",
	}

	job := &ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef: &extension.JobDefinition{
			Name:          "build",
			ExtensionName: "@putnami/typescript",
			Defaults:      map[string]string{"target": "es2022"},
		},
	}

	params := map[string]any{"fast": true}
	defaults := map[string]any{"target": "es2020"}

	ctx := BuildJobContext(ws, job, params, defaults, nil)

	if ctx.WorkspaceRoot != "/workspace" {
		t.Errorf("WorkspaceRoot = %q, want /workspace", ctx.WorkspaceRoot)
	}
	if ctx.Workspace.Name != "test-workspace" {
		t.Errorf("Workspace.Name = %q, want test-workspace", ctx.Workspace.Name)
	}
	if ctx.Project.Name != "my-app" {
		t.Errorf("Project.Name = %q, want my-app", ctx.Project.Name)
	}
	if ctx.Project.FullPath != filepath.FromSlash("/workspace/packages/my-app") {
		t.Errorf("Project.FullPath = %q, want /workspace/packages/my-app", ctx.Project.FullPath)
	}
	if ctx.Project.Options != nil {
		t.Errorf("Project.Options = %v, want nil when no project options configured", ctx.Project.Options)
	}
	if ctx.Extension.Name != "@putnami/typescript" {
		t.Errorf("Extension.Name = %q, want @putnami/typescript", ctx.Extension.Name)
	}
	if ctx.Job.Name != "build" {
		t.Errorf("Job.Name = %q, want build", ctx.Job.Name)
	}
	// Params should be merged: defaults → job defaults → command params
	if ctx.Params["fast"] != true {
		t.Errorf("Params[fast] = %v, want true", ctx.Params["fast"])
	}
	// Job default "target" should override config default
	if ctx.Params["target"] != "es2022" {
		t.Errorf("Params[target] = %v, want es2022 (job default overrides config default)", ctx.Params["target"])
	}
}

func TestBuildJobContextCarriesCommittedWorkspaceOptions(t *testing.T) {
	ws := &workspace.Workspace{
		Name: "test-workspace", Root: "/workspace",
		Config: &wsproto.Config{Options: map[string]map[string]any{
			"sdd": {"verification": map[string]any{"architecture": "off"}},
		}},
	}
	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:   &workspace.Project{Name: "workspace", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/sdd", Path: "/workspace/sdd"},
		JobDef:    &extension.JobDefinition{Name: "architecture-validate"},
	}, nil, nil, nil)
	verification := ctx.Workspace.Options["sdd"]["verification"].(map[string]any)
	if verification["architecture"] != "off" {
		t.Errorf("workspace architecture policy = %#v, want off", verification["architecture"])
	}
}

func TestBuildJobContext_LiteralBindingsOverrideSharedParamLayers(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{Name: "test-workspace", Root: "/workspace", Config: &wsproto.Config{}}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/app", Name: "app", Path: "app"},
		Extension: &extension.ExtensionDescription{Name: "@test/runtime", Path: "/workspace/runtime"},
		JobDef: &extension.JobDefinition{
			Name: "test~generate",
			BoundParams: map[string]any{
				"mode":               "test",
				"nullable":           nil,
				"coverage-threshold": float64(80),
				"coverageThreshold":  float64(90),
			},
		},
	}
	commandParams := map[string]any{
		"mode":               "build",
		"coverage-threshold": float64(20),
		"coverageThreshold":  float64(30),
	}

	configDefaults := map[string]any{"mode": "default"}
	ctx := BuildJobContext(ws, job, commandParams, configDefaults, nil)
	if got := ctx.Params["mode"]; got != "test" {
		t.Errorf("mode = %#v, want bound value test", got)
	}
	if got, present := ctx.Params["nullable"]; !present || got != nil {
		t.Errorf("nullable = (%#v, %v), want explicit nil", got, present)
	}
	if got := ctx.Params["coverage-threshold"]; got != float64(80) {
		t.Errorf("coverage-threshold = %#v, want exact bound value 80", got)
	}
	for i := 0; i < 128; i++ {
		ctx = BuildJobContext(ws, job, commandParams, configDefaults, nil)
		if got := ctx.Params["coverageThreshold"]; got != float64(90) {
			t.Fatalf("iteration %d: coverageThreshold = %#v, want exact bound value 90", i, got)
		}
	}
	if got := commandParams["mode"]; got != "build" {
		t.Errorf("shared command params were mutated: mode = %#v", got)
	}
	if _, leaked := commandParams["nullable"]; leaked {
		t.Error("bound nullable value leaked into shared command params")
	}
}

func TestBuildJobContext_SelectedProjects(t *testing.T) {
	machineCacheParent := filepath.Join(t.TempDir(), "extension-caches")
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv(extensionproto.MachineCacheDirEnv, machineCacheParent)
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	ws := &workspace.Workspace{
		Name:   "test-workspace",
		Root:   "/workspace",
		Config: &wsproto.Config{},
	}
	job := &ScheduledJob{
		Project: &workspace.Project{Name: "test-workspace", Path: "."},
		SelectedProjects: []*workspace.Project{
			{ID: "/apps/api", Name: "api", Path: "apps/api"},
			{ID: "/sites/web", Name: "web", Path: "sites/web"},
		},
		Extension: &extension.ExtensionDescription{Name: "@putnami/cloud", Path: "/workspace/cloud"},
		JobDef:    &extension.JobDefinition{Name: "deploy", ExtensionName: "@putnami/cloud"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)

	if got, want := len(ctx.SelectedProjects), 2; got != want {
		t.Fatalf("selected projects = %d, want %d", got, want)
	}
	if got, want := ctx.SelectedProjects[0].FullPath, filepath.FromSlash("/workspace/apps/api"); got != want {
		t.Fatalf("selected[0].fullPath = %q, want %q", got, want)
	}
	env := BuildEnvVars(ctx)
	for _, want := range []string{
		// One generic per-extension machine root replaces the three
		// language-specific variables core used to export into every job.
		extensionproto.MachineCacheRootEnv + "=" + filepath.Join(machineCacheParent, "@putnami-cloud"),
		extensionproto.SharedOCILayerCacheRootEnv + "=" + filepath.Join(storeRoot, "oci"),
		"PUTNAMI_SELECTED_PROJECTS=api,web",
		"PUTNAMI_SELECTED_PROJECT_IDS=/apps/api,/sites/web",
		"PUTNAMI_SELECTED_PROJECT_PATHS=apps/api,sites/web",
		"PUTNAMI_SELECTED_PROJECT_ROOTS=" + filepath.FromSlash("/workspace/apps/api,/workspace/sites/web"),
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("env missing %q in %v", want, env)
		}
	}
	// The deleted variables must not come back: core exported them into every
	// job of every extension, so a Python task learned where the Bun cache was
	// and a fourth ecosystem got nothing.
	for _, banned := range []string{"PUTNAMI_GO_CACHE_DIR=", "PUTNAMI_BUN_CACHE_DIR=", "BUN_INSTALL_CACHE_DIR="} {
		for _, entry := range env {
			if strings.HasPrefix(entry, banned) {
				t.Fatalf("core still exports the language cache variable %q (%s)", banned, entry)
			}
		}
	}
}

func TestBuildJobContext_SelectedProjectsOutputPath(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{Name: "ws", Root: "/workspace", Config: &wsproto.Config{}}
	job := &ScheduledJob{
		Project: &workspace.Project{Name: "api", Path: "apps/api"},
		SelectedProjects: []*workspace.Project{
			{ID: "/apps/api", Name: "api", Path: "apps/api"},
			{ID: "/sites/web", Name: "web", Path: "sites/web"},
		},
		Extension: &extension.ExtensionDescription{Name: "@putnami/typescript", Path: "/workspace/ts"},
		// Pipeline step suffix must be stripped so OutputPath uses the command
		// name ("test"), exactly like the leader's OutputPath.
		JobDef: &extension.JobDefinition{Name: "test~test", ExtensionName: "@putnami/typescript"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)

	if got, want := ctx.SelectedProjects[0].OutputPath, filepath.FromSlash("/workspace/.putnami/out/apps/api/test"); got != want {
		t.Fatalf("selected[0].outputPath = %q, want %q", got, want)
	}
	if got, want := ctx.SelectedProjects[1].OutputPath, filepath.FromSlash("/workspace/.putnami/out/sites/web/test"); got != want {
		t.Fatalf("selected[1].outputPath = %q, want %q", got, want)
	}
	// The leader project's own OutputPath must share the same derivation, so a
	// batch loop that reconstructs a peer's dir from the command name matches.
	if got, want := ctx.OutputPath, filepath.FromSlash("/workspace/.putnami/out/apps/api/test"); got != want {
		t.Fatalf("leader outputPath = %q, want %q", got, want)
	}
}

// REGRESSION: the resolved selection must carry each project's DECLARED
// identity, not just the resolved name.
//
// A provider's workspace-sync task aligns native manifest names, and the only
// way it can rename exactly the manifests core reports as diverging — rather
// than every manifest whose name merely differs from the resolved name — is if
// core tells it what the project declared. Without this member the TypeScript
// task rewrote every package.json in the workspace to its project path,
// orphaning every `workspace:*` dependency that still spelled the npm name.
func TestBuildJobContext_SelectedProjectsCarrySourceName(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{Name: "ws", Root: "/workspace", Config: &wsproto.Config{}}
	job := &ScheduledJob{
		Project: &workspace.Project{Name: "ws", Path: "."},
		SelectedProjects: []*workspace.Project{
			// Declared its own identity: nothing to align.
			{ID: "/libs/core", Name: "@acme/core", SourceName: "@acme/core", Path: "libs/core"},
			// Declared nothing; a scope namePattern resolved the name. This is
			// the only member whose manifest is out of alignment.
			{ID: "/apps/web", Name: "@acme/web", SourceName: "web", Path: "apps/web"},
		},
		Extension: &extension.ExtensionDescription{Name: "@putnami/typescript", Path: "/workspace/ts"},
		JobDef:    &extension.JobDefinition{Name: "workspace-sync", ExtensionName: "@putnami/typescript"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)

	if got, want := ctx.SelectedProjects[0].SourceName, "@acme/core"; got != want {
		t.Errorf("selected[0].sourceName = %q, want %q", got, want)
	}
	if got, want := ctx.SelectedProjects[1].SourceName, "web"; got != want {
		t.Errorf("selected[1].sourceName = %q, want %q", got, want)
	}
	// It must survive the wire, since the task reads the serialized document.
	encoded, err := json.Marshal(ctx.SelectedProjects[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"sourceName":"web"`) {
		t.Errorf("encoded selected project = %s, want a sourceName member", encoded)
	}
	// A project that declared nothing AND had no scope rename carries no
	// divergence to report; omitempty keeps those refs byte-identical to what
	// an older orchestrator produced.
	plain, err := json.Marshal(JobContextSelectedProject{Name: "a", Path: "a", FullPath: "/workspace/a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "sourceName") {
		t.Errorf("encoded ref = %s, want sourceName omitted when empty", plain)
	}
}

// The protocol defines ONE project-reference shape, used by both
// selectedProjects[] and project.dependencyClosure[]. A member populated in
// only one of them makes that false in the direction that bites: a consumer
// reading a closure member's declared identity would see "" and take it for
// "nothing was declared" rather than "nobody said".
func TestBuildJobContext_DependencyClosureCarriesSourceName(t *testing.T) {
	t.Parallel()
	core := &workspace.Project{ID: "/libs/core", Name: "@acme/core", SourceName: "@acme/core", Path: "libs/core"}
	web := &workspace.Project{
		ID: "/apps/web", Name: "@acme/web", SourceName: "web", Path: "apps/web",
		Dependencies: []string{"@acme/core"},
	}
	// NewWorkspace, not a struct literal: it is what builds the id index and the
	// graph that projectDependencyClosure walks.
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{web, core})
	ws.Name = "ws"

	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:   web,
		Extension: &extension.ExtensionDescription{Name: "@putnami/typescript", Path: "/workspace/ts"},
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "@putnami/typescript"},
	}, nil, nil, nil)

	closure := ctx.Project.DependencyClosure
	if len(closure) != 2 {
		t.Fatalf("closure = %+v, want the seed and its one dependency", closure)
	}
	declared := map[string]string{}
	for _, ref := range closure {
		declared[ref.Name] = ref.SourceName
	}
	if got, want := declared["@acme/web"], "web"; got != want {
		t.Errorf("closure sourceName for @acme/web = %q, want %q", got, want)
	}
	if got, want := declared["@acme/core"], "@acme/core"; got != want {
		t.Errorf("closure sourceName for @acme/core = %q, want %q", got, want)
	}

	// It must survive serialization — the closure reaches a task as bytes.
	encoded, err := json.Marshal(ctx.Project.DependencyClosure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"sourceName":"web"`) {
		t.Errorf("encoded closure = %s, want a sourceName member", encoded)
	}
	// omitempty keeps a ref with nothing to declare byte-identical to what an
	// older orchestrator produced.
	plain, err := json.Marshal(JobContextProjectRef{Name: "a", Path: "a", FullPath: "/workspace/a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "sourceName") {
		t.Errorf("encoded ref = %s, want sourceName omitted when empty", plain)
	}
}

// TestBuildJobContext_ProjectReferencesCarryTheResolvedVersion pins the second
// half of the package-reference pair.
//
// The SDD features engine resolves a package-root source binding by name AND
// version. Without the version on the wire, the versioned half of a corpus
// resolved to nothing and degraded to "stale source binding unavailable" while
// the versionless half kept working — a silent narrowing, which is why it is
// asserted rather than left to the first consumer to notice.
//
// It is asserted on BOTH project-reference sites, for the same one-shape reason
// as sourceName above, and the version is each project's LINE version: a
// project declares none of its own, so a project of another line reports that
// line's version rather than a single answer for the run.
func TestBuildJobContext_ProjectReferencesCarryTheResolvedVersion(t *testing.T) {
	t.Parallel()
	core := &workspace.Project{
		ID: "/libs/core", Name: "@acme/core", Path: "libs/core", Line: "libs",
	}
	web := &workspace.Project{
		ID: "/apps/web", Name: "@acme/web", Path: "apps/web", Line: "apps",
		Dependencies: []string{"@acme/core"},
	}
	ws := workspace.NewWorkspace("/workspace",
		&wsproto.Config{}, []*workspace.Project{web, core})
	ws.Name = "ws"
	versions := RunVersions{
		"libs": &JobContextVersion{Base: "2.1.0", Full: "2.1.0", Line: "libs"},
		"apps": &JobContextVersion{Base: "9.9.9", Full: "9.9.9", Line: "apps"},
	}

	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:          web,
		SelectedProjects: []*workspace.Project{web, core},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/sdd", Path: "/workspace/sdd"},
		JobDef:           &extension.JobDefinition{Name: "validate", ExtensionName: "@putnami/sdd"},
	}, nil, nil, versions)

	selected := map[string]string{}
	for _, ref := range ctx.SelectedProjects {
		selected[ref.Name] = ref.Version
	}
	if got, want := selected["@acme/core"], "2.1.0"; got != want {
		t.Errorf("selected version for @acme/core = %q, want %q", got, want)
	}
	if got, want := selected["@acme/web"], "9.9.9"; got != want {
		t.Errorf("selected version for @acme/web = %q, want its own line's %q", got, want)
	}

	closure := map[string]string{}
	for _, ref := range ctx.Project.DependencyClosure {
		closure[ref.Name] = ref.Version
	}
	if got, want := closure["@acme/core"], "2.1.0"; got != want {
		t.Errorf("closure version for @acme/core = %q, want %q", got, want)
	}
	if got, want := closure["@acme/web"], "9.9.9"; got != want {
		t.Errorf("closure version for @acme/web = %q, want its own line's %q", got, want)
	}

	// It must survive the wire: the task reads the serialized document.
	encoded, err := json.Marshal(ctx.SelectedProjects)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"version":"2.1.0"`) {
		t.Errorf("encoded selection = %s, want a version member", encoded)
	}
	// omitempty keeps a reference with no version byte-identical to what an
	// orchestrator that predates the member produced.
	for _, ref := range []any{
		JobContextSelectedProject{Name: "a", Path: "a", FullPath: "/workspace/a"},
		JobContextProjectRef{Name: "a", Path: "a", FullPath: "/workspace/a"},
	} {
		plain, err := json.Marshal(ref)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(plain), "version") {
			t.Errorf("encoded ref = %s, want version omitted when empty", plain)
		}
	}
}

func TestBuildJobContext_RebasesWorkspaceExtensionRootFromRelPath(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{Name: "ws", Root: "/workspace", Config: &wsproto.Config{}}
	job := &ScheduledJob{
		Project: &workspace.Project{Name: "pkg", Path: "pkg"},
		Extension: &extension.ExtensionDescription{
			Name:    "@putnami/go",
			Path:    "/other-workspace/go/extension",
			RelPath: "/go/extension",
		},
		JobDef: &extension.JobDefinition{Name: "build", ExtensionName: "@putnami/go"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)

	if ctx.Extension.Root != filepath.FromSlash("/workspace/go/extension") {
		t.Fatalf("Extension.Root = %q, want /workspace/go/extension", ctx.Extension.Root)
	}
}

func TestBuildJobContext_ProjectOptionsMergeOrder(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{
		Name:   "ws",
		Root:   "/workspace",
		Config: &wsproto.Config{},
	}

	proj := &workspace.Project{
		Name: "my-cli",
		Path: "tooling/cli",
		Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{
				"build":                     {"lib": true, "compile": true, "installPath": "from-cmd"},
				"@putnami/typescript":       {"compile": false, "extra": "ext"},
				"@putnami/typescript:build": {"installPath": "from-ext-cmd"},
			},
		},
	}

	ext := &extension.ExtensionDescription{
		Name: "@putnami/typescript",
		Path: "/ext",
	}

	job := &ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "@putnami/typescript"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)

	if ctx.Project.Options == nil {
		t.Fatal("Project.Options is nil")
	}
	if got := ctx.Project.Options["generate"]["schema"]; got != nil {
		t.Errorf("Project.Options[generate][schema] = %v, want nil for unrelated options", got)
	}
	if got := ctx.Project.Options["@putnami/typescript"]["extra"]; got != "ext" {
		t.Errorf("Project.Options[@putnami/typescript][extra] = %v, want ext", got)
	}

	// "lib" from command-keyed options
	if ctx.Params["lib"] != true {
		t.Errorf("Params[lib] = %v, want true (from build key)", ctx.Params["lib"])
	}
	// "compile" from extension-keyed overrides command-keyed
	if ctx.Params["compile"] != false {
		t.Errorf("Params[compile] = %v, want false (extension overrides command)", ctx.Params["compile"])
	}
	// "extra" from extension-keyed
	if ctx.Params["extra"] != "ext" {
		t.Errorf("Params[extra] = %v, want ext", ctx.Params["extra"])
	}
	// "installPath" from extension:command overrides command-keyed
	if ctx.Params["installPath"] != "from-ext-cmd" {
		t.Errorf("Params[installPath] = %v, want from-ext-cmd (ext:cmd overrides cmd)", ctx.Params["installPath"])
	}
}

func TestBuildJobContext_PipelineStep(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{
		Name:   "ws",
		Root:   "/workspace",
		Config: &wsproto.Config{},
	}

	proj := &workspace.Project{Name: "pkg", Path: "packages/pkg"}
	ext := &extension.ExtensionDescription{Name: "ext", Path: "/ext"}

	job := &ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef: &extension.JobDefinition{
			Name:          "build~transpile",
			ExtensionName: "ext",
		},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)

	// jobCommandName should strip the step suffix
	if ctx.Job.Name != "build" {
		t.Errorf("Job.Name = %q, want build (strip ~transpile)", ctx.Job.Name)
	}
}

func TestWriteContextFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ctx := &JobCommandContext{
		WorkspaceRoot: "/workspace",
		Workspace:     JobContextWorkspace{Name: "test", RootPath: "/workspace"},
		Extension:     JobContextExtension{Name: "ext", Root: "/ext"},
		Job:           JobContextJob{Name: "build"},
		Params:        map[string]any{"key": "value"},
		OutputPath:    "/out",
		CacheRoot:     dir,
	}

	path, err := WriteContextFile(ctx, dir)
	if err != nil {
		t.Fatalf("WriteContextFile: %v", err)
	}
	defer os.Remove(path)

	// Verify file exists and is valid JSON
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var parsed JobCommandContext
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if parsed.Workspace.Name != "test" {
		t.Errorf("parsed Workspace.Name = %q, want test", parsed.Workspace.Name)
	}
	if parsed.Params["key"] != "value" {
		t.Errorf("parsed Params[key] = %v, want value", parsed.Params["key"])
	}

	// Verify file is in the correct directory
	if filepath.Dir(path) != dir {
		t.Errorf("context file dir = %q, want %q", filepath.Dir(path), dir)
	}
}

func TestJobCommandName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{"build", "build"},
		{"build~transpile", "build"},
		{"test~unit", "test"},
		{"lint", "lint"},
	}

	for _, tt := range tests {
		job := &ScheduledJob{
			JobDef: &extension.JobDefinition{Name: tt.name},
		}
		got := jobCommandName(job)
		if got != tt.want {
			t.Errorf("jobCommandName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestWriteContextFile_FullRoundtrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ctx := &JobCommandContext{
		WorkspaceRoot: "/workspace",
		Workspace: JobContextWorkspace{
			Name:     "test-ws",
			RootPath: "/workspace",
			Version:  "2.0.0",
		},
		Project: &JobContextProject{
			Name:     "my-app",
			Path:     "packages/my-app",
			FullPath: "/workspace/packages/my-app",
			Publish:  []string{"npm", "docker"},
			Options: map[string]map[string]any{
				"generate": {"schema": false},
			},
		},
		Extension: JobContextExtension{
			Name: "@putnami/typescript",
			Root: "/ext/ts",
		},
		Job:        JobContextJob{Name: "build"},
		Params:     map[string]any{"target": "es2022", "fast": true, "count": float64(5)},
		OutputPath: "/workspace/.putnami/out/my-app/build",
		CacheRoot:  dir,
	}

	path, err := WriteContextFile(ctx, dir)
	if err != nil {
		t.Fatalf("WriteContextFile: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var parsed JobCommandContext
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Verify all fields roundtrip correctly
	if parsed.WorkspaceRoot != "/workspace" {
		t.Errorf("WorkspaceRoot = %q", parsed.WorkspaceRoot)
	}
	if parsed.Workspace.Version != "2.0.0" {
		t.Errorf("Workspace.Version = %q", parsed.Workspace.Version)
	}
	if parsed.Project == nil {
		t.Fatal("Project is nil")
	}
	if parsed.Project.FullPath != "/workspace/packages/my-app" {
		t.Errorf("Project.FullPath = %q", parsed.Project.FullPath)
	}
	if len(parsed.Project.Publish) != 2 || parsed.Project.Publish[0] != "npm" {
		t.Errorf("Project.Publish = %v", parsed.Project.Publish)
	}
	if parsed.Project.Options["generate"]["schema"] != false {
		t.Errorf("Project.Options[generate][schema] = %v, want false", parsed.Project.Options["generate"]["schema"])
	}
	if parsed.Extension.Name != "@putnami/typescript" {
		t.Errorf("Extension.Name = %q", parsed.Extension.Name)
	}
	if parsed.OutputPath != "/workspace/.putnami/out/my-app/build" {
		t.Errorf("OutputPath = %q", parsed.OutputPath)
	}
	if parsed.CacheRoot != dir {
		t.Errorf("CacheRoot = %q", parsed.CacheRoot)
	}
	if parsed.Params["target"] != "es2022" {
		t.Errorf("Params[target] = %v", parsed.Params["target"])
	}
	if parsed.Params["fast"] != true {
		t.Errorf("Params[fast] = %v", parsed.Params["fast"])
	}
	if parsed.Params["count"] != float64(5) {
		t.Errorf("Params[count] = %v", parsed.Params["count"])
	}
}

func TestWriteContextFile_NilProject(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ctx := &JobCommandContext{
		WorkspaceRoot: "/ws",
		Workspace:     JobContextWorkspace{Name: "ws"},
		Extension:     JobContextExtension{Name: "ext"},
		Job:           JobContextJob{Name: "generate"},
		Params:        map[string]any{},
		CacheRoot:     dir,
	}

	path, err := WriteContextFile(ctx, dir)
	if err != nil {
		t.Fatalf("WriteContextFile: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var parsed JobCommandContext
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Project should be omitted in JSON when nil
	if parsed.Project != nil {
		t.Error("Project should be nil when omitted")
	}
}

func TestBuildJobContext_OutputPath(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{
		Name:   "ws",
		Root:   "/workspace",
		Config: &wsproto.Config{},
	}

	proj := &workspace.Project{Name: "my-lib", Path: "packages/my-lib"}
	runtimePath := filepath.Join("/artifacts", "runtime", "compiled", "extension")
	ext := &extension.ExtensionDescription{Name: "ext", Path: "/ext", RuntimeExecutable: runtimePath}

	job := &ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef: &extension.JobDefinition{
			Name:          "build~transpile",
			ExtensionName: "ext",
		},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)

	// OutputPath should use Path (not Name) and the base command name (not the step name)
	expected := filepath.Join("/workspace", ".putnami", "out", "packages/my-lib", "build")
	if ctx.OutputPath != expected {
		t.Errorf("OutputPath = %q, want %q", ctx.OutputPath, expected)
	}

	// CacheRoot is the per-workspace mutable scratch dir, NOT the (now
	// machine-global) content-addressed store.
	expectedCache := filepath.Join("/workspace", ".putnami", "cache")
	if ctx.CacheRoot != expectedCache {
		t.Errorf("CacheRoot = %q, want %q", ctx.CacheRoot, expectedCache)
	}
	if ctx.Extension.RuntimePath != runtimePath {
		t.Errorf("Extension.RuntimePath = %q, want synchronized %q", ctx.Extension.RuntimePath, runtimePath)
	}
	// Slice C5 wires extension.cacheRoot: the machine-global directory THIS
	// extension owns. It is per-extension and independent of the workspace, so
	// two worktrees of one repo share it and two extensions never do.
	wantExtensionCache := extensionproto.MachineCacheRoot(ext.Name, ws.Root)
	if wantExtensionCache == "" {
		t.Fatal("MachineCacheRoot resolved to nothing for a named extension")
	}
	if ctx.Extension.CacheRoot != wantExtensionCache {
		t.Errorf("Extension.CacheRoot = %q, want %q", ctx.Extension.CacheRoot, wantExtensionCache)
	}
	if !filepath.IsAbs(ctx.Extension.CacheRoot) {
		t.Errorf("Extension.CacheRoot = %q, want an absolute path (the job context contract requires one)",
			ctx.Extension.CacheRoot)
	}
}

func TestBuildJobContext_VersionInfo(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{
		Name:   "ws",
		Root:   "/workspace",
		Config: &wsproto.Config{},
	}

	proj := &workspace.Project{Name: "app", Path: "packages/app"}
	ext := &extension.ExtensionDescription{Name: "ext", Path: "/ext"}

	job := &ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "ext"},
	}

	vi := &JobContextVersion{
		Base:    "0.1.0",
		Full:    "0.1.0-abc1234",
		SHA:     "abc1234",
		Branch:  "main",
		Suffix:  "abc1234",
		IsDirty: false,
	}

	ctx := BuildJobContext(ws, job, nil, nil, rootLineVersions(vi))

	if ctx.Version == nil {
		t.Fatal("Version should not be nil")
	}
	if ctx.Version.Full != "0.1.0-abc1234" {
		t.Errorf("Version.Full = %q, want 0.1.0-abc1234", ctx.Version.Full)
	}
	if ctx.Version.Tag != "" {
		t.Errorf("Version.Tag = %q, want no tag on an untagged commit", ctx.Version.Tag)
	}

	// nil version info should result in nil Version
	ctx2 := BuildJobContext(ws, job, nil, nil, nil)
	if ctx2.Version != nil {
		t.Error("Version should be nil when no version info provided")
	}
}

// A project's version is its LINE's, and a package version the probe happened
// to report does not displace it: the artifact is stamped with what the release
// publishes, not with what a manifest on disk says.
func TestBuildJobContext_VersionComesFromTheProjectsLine(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{
		Name:   "ws",
		Root:   "/workspace",
		Config: &wsproto.Config{},
	}
	proj := &workspace.Project{Name: "app", Path: "packages/app", Version: "0.2.0", Line: "packages"}
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "ext", Path: "/ext"},
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "ext"},
	}
	versions := RunVersions{"packages": &JobContextVersion{
		Base:   "0.1.0",
		Full:   "0.1.0-abc1234",
		SHA:    "abc1234",
		Branch: "main",
		Suffix: "abc1234",
		Line:   "packages",
	}}

	ctx := BuildJobContext(ws, job, nil, nil, versions)
	if ctx.Workspace.Version != "0.1.0" {
		t.Errorf("Workspace.Version = %q, want the line's base 0.1.0", ctx.Workspace.Version)
	}
	if ctx.Version.Full != "0.1.0-abc1234" {
		t.Errorf("Version.Full = %q, want 0.1.0-abc1234", ctx.Version.Full)
	}
}

func TestKebabToCamel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"config-path", "configPath"},
		{"put-registry-url", "putRegistryUrl"},
		{"also-branch-tag", "alsoBranchTag"},
		{"dry-run", "dryRun"},
		{"simple", "simple"},
		{"a-b-c-d", "aBCD"},
		{"", ""},
	}
	for _, tt := range tests {
		got := kebabToCamel(tt.input)
		if got != tt.want {
			t.Errorf("kebabToCamel(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestAddCamelCaseKeys(t *testing.T) {
	t.Parallel()
	params := map[string]any{
		"config-path":      "/some/path",
		"dry-run":          true,
		"simple":           "value",
		"put-registry-url": "https://registry.example.test",
	}
	addCamelCaseKeys(params)

	// Kebab-case originals preserved
	if params["config-path"] != "/some/path" {
		t.Error("kebab key config-path should be preserved")
	}
	if params["dry-run"] != true {
		t.Error("kebab key dry-run should be preserved")
	}

	// CamelCase variants added
	if params["configPath"] != "/some/path" {
		t.Errorf("configPath = %v, want /some/path", params["configPath"])
	}
	if params["dryRun"] != true {
		t.Errorf("dryRun = %v, want true", params["dryRun"])
	}
	if params["putRegistryUrl"] != "https://registry.example.test" {
		t.Errorf("putRegistryUrl = %v, want https://registry.example.test", params["putRegistryUrl"])
	}

	// Non-kebab key unchanged, no spurious camelCase
	if params["simple"] != "value" {
		t.Error("simple key should be preserved")
	}
}

func TestAddCamelCaseKeys_NoOverwrite(t *testing.T) {
	t.Parallel()
	// If camelCase key already exists, don't overwrite it
	params := map[string]any{
		"config-path": "from-kebab",
		"configPath":  "from-camel",
	}
	addCamelCaseKeys(params)

	if params["configPath"] != "from-camel" {
		t.Errorf("existing camelCase key should not be overwritten, got %v", params["configPath"])
	}
}

func TestBuildJobContext_CamelCaseParams(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{
		Name: "test-ws",
		Root: "/tmp/test-ws",
	}

	job := &ScheduledJob{
		Project: &workspace.Project{
			Name: "test-project",
			Path: "packages/test",
		},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: "extensions/test",
		},
		JobDef: &extension.JobDefinition{
			Name:          "build",
			ExtensionName: "@test/ext",
		},
	}

	commandParams := map[string]any{
		"config-path":      "/custom/path",
		"put-registry-url": "https://registry.example.test",
		"simple":           "value",
	}

	ctx := BuildJobContext(ws, job, commandParams, nil, nil)

	// Both kebab and camelCase should exist
	if ctx.Params["config-path"] != "/custom/path" {
		t.Errorf("Params[config-path] = %v", ctx.Params["config-path"])
	}
	if ctx.Params["configPath"] != "/custom/path" {
		t.Errorf("Params[configPath] = %v", ctx.Params["configPath"])
	}
	if ctx.Params["put-registry-url"] != "https://registry.example.test" {
		t.Errorf("Params[put-registry-url] = %v", ctx.Params["put-registry-url"])
	}
	if ctx.Params["putRegistryUrl"] != "https://registry.example.test" {
		t.Errorf("Params[putRegistryUrl] = %v", ctx.Params["putRegistryUrl"])
	}
	if ctx.Params["simple"] != "value" {
		t.Errorf("Params[simple] = %v", ctx.Params["simple"])
	}
}

// THE JOB-CONTEXT CUTOVER.
//
// Job context v2 no longer carries `main`, `bin` and `exports`: they were
// package.json fields core parsed and stamped into EVERY task's context, so a Go
// test job received a `bin` member it could not have. They are replaced by
// `project.metadata[<extension>]`, produced by the workspace probe of the
// extension that understands them and carried verbatim.
func TestBuildJobContext_CarriesNamespacedProviderMetadata(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{Name: "ws", Root: "/workspace", Config: &wsproto.Config{}}
	proj := &workspace.Project{
		Name: "my-app",
		Path: "packages/my-app",
		Metadata: map[string]json.RawMessage{
			"@putnami/typescript": json.RawMessage(`{"main":"dist/index.js","bin":{"cli":"dist/cli.js"}}`),
			"@putnami/go":         json.RawMessage(`{"module":"example.com/app"}`),
		},
	}
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "@putnami/typescript"},
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "@putnami/typescript"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)
	if len(ctx.Project.Metadata) != 2 {
		t.Fatalf("metadata = %v, want both providers' blocks (ownership, not read isolation)", ctx.Project.Metadata)
	}
	if string(ctx.Project.Metadata["@putnami/typescript"]) != `{"main":"dist/index.js","bin":{"cli":"dist/cli.js"}}` {
		t.Errorf("metadata block = %s, want it carried verbatim", ctx.Project.Metadata["@putnami/typescript"])
	}

	// The document must not have grown the members back under another name: the
	// encoded shape is what an extension validates.
	encoded, err := json.Marshal(ctx.Project)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"main", "bin", "exports"} {
		if _, present := raw[gone]; present {
			t.Errorf("job context project still carries the npm-shaped %q member: %s", gone, encoded)
		}
	}
}

// A project no provider reported metadata for omits the member entirely. An
// omitted member and an empty object encode differently, and the context
// document's BYTES are what a consumer validates.
func TestBuildJobContext_OmitsEmptyProviderMetadata(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{Name: "ws", Root: "/workspace", Config: &wsproto.Config{}}
	job := &ScheduledJob{
		Project:   &workspace.Project{Name: "svc", Path: "svc", Metadata: map[string]json.RawMessage{}},
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef:    &extension.JobDefinition{Name: "test", ExtensionName: "@putnami/go"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)
	if ctx.Project.Metadata != nil {
		t.Fatalf("metadata = %v, want nil so the member is omitted", ctx.Project.Metadata)
	}
	encoded, err := json.Marshal(ctx.Project)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"metadata"`)) {
		t.Errorf("empty metadata reached the wire: %s", encoded)
	}
}

// The map is COPIED. The *workspace.Project is memoized for the process and read
// by every scheduled job; sharing the map would let one task's mutation of its
// own context reach another's.
func TestBuildJobContext_CopiesProviderMetadata(t *testing.T) {
	t.Parallel()
	ws := &workspace.Workspace{Name: "ws", Root: "/workspace", Config: &wsproto.Config{}}
	proj := &workspace.Project{
		Name:     "app",
		Path:     "app",
		Metadata: map[string]json.RawMessage{"@putnami/typescript": json.RawMessage(`{"main":"a.js"}`)},
	}
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "@putnami/typescript"},
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "@putnami/typescript"},
	}

	ctx := BuildJobContext(ws, job, nil, nil, nil)
	ctx.Project.Metadata["@acme/other"] = json.RawMessage(`{}`)
	if len(proj.Metadata) != 1 {
		t.Errorf("mutating one context's metadata reached the shared project: %v", proj.Metadata)
	}
}

// TestBuildJobContext_WorkspaceProjectsCarryTheWholeMembership pins the member
// that makes a workspace-wide verdict provable from the wire.
//
// The measurement it encodes: `architecture validate` reads the resolved
// membership to decide whether a project an architecture manifest names is a
// real member, and reads the direct edges to decide whether one domain's
// project depends on another's without a declared binding. Given only
// `selectedProjects`, a narrowed run reported a real member as absent and an
// unnarrowed one observed zero edges — a false violation and a missed one.
func TestBuildJobContext_WorkspaceProjectsCarryTheWholeMembership(t *testing.T) {
	t.Parallel()
	core := &workspace.Project{
		ID: "/libs/core", Name: "@acme/core", Path: "libs/core", Version: "2.1.0",
		Config: &wsproto.ProjectConfig{
			Name: "@acme/core",
			FeatureAuthority: &wsproto.ProjectFeatureAuthority{
				None: "this library deliberately has no standalone user-facing feature",
			},
		},
	}
	web := &workspace.Project{
		ID: "/apps/web", Name: "@acme/web", Path: "apps/web",
		Dependencies: []string{"@acme/core"},
		Extensions:   []string{"/typescript/extension"},
	}
	unselected := &workspace.Project{ID: "/libs/legacy", Name: "@acme/legacy", Path: "libs/legacy"}
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{web, core, unselected})
	ws.Name = "ws"

	// A NARROWED run: only /apps/web is selected. The membership must still be
	// the whole workspace, which is the entire point of the member.
	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:          web,
		SelectedProjects: []*workspace.Project{web},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/sdd", Path: "/workspace/sdd"},
		JobDef:           &extension.JobDefinition{Name: "validate-workspace", ExtensionName: "@putnami/sdd"},
	}, nil, nil, nil)

	if got, want := len(ctx.SelectedProjects), 1; got != want {
		t.Fatalf("selectedProjects = %d, want %d — the run is narrowed", got, want)
	}
	ids := make([]string, 0, len(ctx.WorkspaceProjects))
	for _, ref := range ctx.WorkspaceProjects {
		ids = append(ids, ref.ID)
	}
	// Sorted by id, because a membership answer must be the same bytes for the
	// same workspace whichever run produced it.
	if want := []string{"/apps/web", "/libs/core", "/libs/legacy"}; !slices.Equal(ids, want) {
		t.Fatalf("workspaceProjects = %v, want the whole membership in id order %v", ids, want)
	}
	for _, ref := range ctx.WorkspaceProjects {
		if ref.Name == "" || ref.Path == "" || ref.FullPath == "" {
			t.Errorf("workspace project %q is not fully located: %+v", ref.ID, ref)
		}
	}
	if got, want := ctx.WorkspaceProjects[0].Dependencies, []string{"/libs/core"}; !slices.Equal(got, want) {
		t.Errorf("direct dependencies of /apps/web = %v, want %v", got, want)
	}
	if got := ctx.WorkspaceProjects[1].Dependencies; len(got) != 0 {
		t.Errorf("direct dependencies of /libs/core = %v, want none", got)
	}
	var coreConfig wsproto.ProjectConfig
	if err := json.Unmarshal(ctx.WorkspaceProjects[1].Config, &coreConfig); err != nil {
		t.Fatalf("decode /libs/core config: %v", err)
	}
	if coreConfig.FeatureAuthority == nil || coreConfig.FeatureAuthority.None == "" {
		t.Errorf("/libs/core config lost featureAuthority: %+v", coreConfig)
	}
	// The resolved extensions, so a workspace task knows which extension's
	// option blocks apply to a sibling.
	if got, want := ctx.WorkspaceProjects[0].Extensions, []string{"/typescript/extension"}; !slices.Equal(got, want) {
		t.Errorf("extensions of /apps/web = %v, want %v", got, want)
	}
	if got := ctx.WorkspaceProjects[1].Extensions; got != nil {
		t.Errorf("extensions of /libs/core = %v, want none", got)
	}
	if got, want := ctx.WorkspaceProjects[2].FullPath, filepath.Join("/workspace", "libs/legacy"); got != want {
		t.Errorf("unselected member fullPath = %q, want %q", got, want)
	}
}

// TestBuildJobContext_WorkspaceProjectsReachAProjectScopedJobToo pins the
// presence rule, and the rule is deliberately NOT "only where selectedProjects
// is".
//
// That narrower rule was tried and measured wrong. A per-project task can own a
// workspace-wide READ: the SDD feature contract resolves a relation target
// against every authored manifest in the workspace, so a project-scoped
// validation shown only its own project reported a real cross-project relation
// as dangling — a false failure in the canonical gate. What decides whether a
// task may make a workspace-wide CLAIM is `selection`, which every document
// already carries.
func TestBuildJobContext_WorkspaceProjectsReachAProjectScopedJobToo(t *testing.T) {
	t.Parallel()
	core := &workspace.Project{ID: "/libs/core", Name: "@acme/core", Path: "libs/core"}
	sibling := &workspace.Project{ID: "/libs/other", Name: "@acme/other", Path: "libs/other"}
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{core, sibling})
	ws.Name = "ws"

	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:   core,
		Extension: &extension.ExtensionDescription{Name: "@putnami/sdd", Path: "/workspace/sdd"},
		JobDef:    &extension.JobDefinition{Name: "validate", ExtensionName: "@putnami/sdd"},
	}, nil, nil, nil)

	// The selection stays absent: this job runs for one project and the
	// orchestrator does not attach a run selection to a project-scoped node.
	if ctx.SelectedProjects != nil {
		t.Fatalf("selectedProjects = %+v, want none on a project-scoped job", ctx.SelectedProjects)
	}
	ids := make([]string, 0, len(ctx.WorkspaceProjects))
	for _, ref := range ctx.WorkspaceProjects {
		ids = append(ids, ref.ID)
	}
	if want := []string{"/libs/core", "/libs/other"}; !slices.Equal(ids, want) {
		t.Fatalf("workspaceProjects = %v, want the whole membership %v", ids, want)
	}
	encoded, err := json.Marshal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"workspaceProjects"`)) {
		t.Errorf("encoded context omits workspaceProjects: %s", encoded)
	}
}

// TestBuildJobContext_ProjectReferencesCarryDirectDependencies pins the edge
// list on every project-reference carrier, for the same one-shape reason
// sourceName and version are pinned on both above.
func TestBuildJobContext_ProjectReferencesCarryDirectDependencies(t *testing.T) {
	t.Parallel()
	core := &workspace.Project{ID: "/libs/core", Name: "@acme/core", Path: "libs/core"}
	web := &workspace.Project{
		ID: "/apps/web", Name: "@acme/web", Path: "apps/web",
		// Declared by NAME; the wire must carry the resolved ID, because the
		// name→id table is not on the wire for a consumer to apply itself.
		Dependencies: []string{"@acme/core", "@acme/absent"},
	}
	ws := workspace.NewWorkspace("/workspace", &wsproto.Config{}, []*workspace.Project{web, core})
	ws.Name = "ws"

	ctx := BuildJobContext(ws, &ScheduledJob{
		Project:          web,
		SelectedProjects: []*workspace.Project{web, core},
		Extension:        &extension.ExtensionDescription{Name: "@putnami/sdd", Path: "/workspace/sdd"},
		JobDef:           &extension.JobDefinition{Name: "validate-workspace", ExtensionName: "@putnami/sdd"},
	}, nil, nil, nil)

	// An out-of-workspace edge is dropped: there is no member to point at.
	if got, want := ctx.SelectedProjects[0].Dependencies, []string{"/libs/core"}; !slices.Equal(got, want) {
		t.Errorf("selected dependencies = %v, want the resolved in-workspace ids %v", got, want)
	}
	closure := map[string][]string{}
	for _, ref := range ctx.Project.DependencyClosure {
		closure[ref.ID] = ref.Dependencies
	}
	if got, want := closure["/apps/web"], []string{"/libs/core"}; !slices.Equal(got, want) {
		t.Errorf("closure dependencies for /apps/web = %v, want %v", got, want)
	}
	if got := closure["/libs/core"]; len(got) != 0 {
		t.Errorf("closure dependencies for /libs/core = %v, want none", got)
	}

	encoded, err := json.Marshal(ctx.SelectedProjects)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"dependencies":["/libs/core"]`) {
		t.Errorf("encoded selection = %s, want a dependencies member", encoded)
	}
	// omitempty keeps a reference with no edges byte-identical to what an
	// orchestrator that predates the member produced.
	for _, ref := range []any{
		JobContextSelectedProject{Name: "a", Path: "a", FullPath: "/workspace/a"},
		JobContextProjectRef{Name: "a", Path: "a", FullPath: "/workspace/a"},
	} {
		plain, err := json.Marshal(ref)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(plain), "dependencies") {
			t.Errorf("encoded ref = %s, want dependencies omitted when empty", plain)
		}
	}
}

// The registry endpoints reach a publish task through the job context and only
// through it. Routing them through the resolved parameter bag would put them in
// the exact projection the cache-key builder hashes, so pointing the workspace
// at a mirror would evict a store that holds the same bytes.
func TestRegistriesParamIsNotACacheKeyInput(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj", Registries: map[string]json.RawMessage{
		"oci": json.RawMessage(`{"publish":"oci.example.test/team"}`),
	}}
	job := declareCacheTestTask(&ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
		JobDef: &extension.JobDefinition{
			Name: "publish~docker", CommandName: "publish", ExtensionName: "@test/ext", Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{
				Params: []string{"registries", "docker"},
			}},
		},
	}, "docker")

	ctx := BuildJobContext(ws, job, nil, nil, nil)
	entry, present := ctx.Params["registries"].(map[string]json.RawMessage)
	if !present || string(entry["oci"]) != `{"publish":"oci.example.test/team"}` {
		t.Fatalf("context registries = %#v, want the project's effective entries", ctx.Params["registries"])
	}
	if _, leaked := resolvedJobParams(job, nil, nil)["registries"]; leaked {
		t.Fatal("registries reached the resolved parameter bag the cache key hashes")
	}

	hash := func() string {
		t.Helper()
		value, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return value
	}
	before := hash()
	project.Registries["oci"] = json.RawMessage(`{"publish":"mirror.example.test/team"}`)
	if after := hash(); after != before {
		t.Fatalf("moving the registry endpoint changed the task cache key: %s → %s", before, after)
	}
}

// Two lanes that build one tree resolve two versions of its line, so a task the
// cache can serve reads the line's version only when its key carries it (ADR
// 0060). Every other cached task reads the tree's version, 0.0.0, and no
// commit, in its own version and in every project reference.
func TestBuildJobContextGivesTheLineVersionOnlyToATaskWhoseKeyCarriesIt(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
	ws.Projects = []*workspace.Project{project}
	resolved := &JobContextVersion{
		Base: "0.4.0", Full: "0.4.0-20261004094924-a389c95", Suffix: "20261004094924-a389c95",
		SHA: "a389c95f00dfeed0123456789abcdef012345678", Branch: "main",
	}
	versions := rootLineVersions(resolved)

	cached := cacheableJob("lint", "/proj", "proj", "proj")
	versionAware := cacheableJob("package", "/proj", "proj", "proj")
	versionAware.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{VersionAware: true}
	uncached := cacheableJob("deploy", "/proj", "proj", "proj")
	uncached.JobDef.Cache = false

	for _, tc := range []struct {
		name string
		job  *ScheduledJob
		want JobContextVersion
	}{
		{"a cached task", cached, JobContextVersion{Base: "0.0.0"}},
		{"a version-aware task", versionAware, *resolved},
		{"an uncached task", uncached, *resolved},
	} {
		tc.job.Project = project
		ctx := BuildJobContext(ws, tc.job, nil, nil, versions)
		if ctx.Version == nil || *ctx.Version != tc.want {
			t.Errorf("%s: version = %+v, want %+v", tc.name, ctx.Version, tc.want)
		}
		if ctx.Workspace.Version != tc.want.Base {
			t.Errorf("%s: workspace version = %q, want %q", tc.name, ctx.Workspace.Version, tc.want.Base)
		}
		if len(ctx.WorkspaceProjects) != 1 || ctx.WorkspaceProjects[0].Version != tc.want.Base {
			t.Errorf("%s: workspace projects = %+v, want proj at %q", tc.name, ctx.WorkspaceProjects, tc.want.Base)
		}
	}
}
