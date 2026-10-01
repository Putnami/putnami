package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// TestLoad_Memoizes verifies the per-root Load cache: two Load
// calls with the same root return the same *Workspace pointer, and a
// post-cache mutation of the on-disk config is NOT observed until
// InvalidateLoadCache is called.
func TestLoad_Memoizes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{"name":"first"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	InvalidateLoadCache(dir) // start clean

	a, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("Load (2nd): %v", err)
	}
	if a != b {
		t.Error("expected memoized Load to return the same *Workspace pointer")
	}
	if a.Name != "first" {
		t.Errorf("Name = %q, want %q", a.Name, "first")
	}

	// Mutate config on disk; without invalidation Load must still return
	// the cached *Workspace from before the mutation.
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{"name":"second"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(dir)
	if c.Name != "first" {
		t.Errorf("cached Load should still see %q, got %q", "first", c.Name)
	}

	// After invalidation, the next Load picks up the mutation.
	InvalidateLoadCache(dir)
	d, _ := Load(dir)
	if d.Name != "second" {
		t.Errorf("post-invalidation Load Name = %q, want %q", d.Name, "second")
	}
	if d == a {
		t.Error("post-invalidation Load should return a fresh *Workspace")
	}
}

func TestLoadReloadsAnIndexCreatedAfterSessionStart(t *testing.T) {
	spectest.Proves(t, "cli/workspace-initialization", "graph-readiness", "mcp-reloads-an-index-created-after-startup")
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, wsproto.WorkspaceConfigFilename), `{"includes":["app"]}`)
	writeFileAt(t, filepath.Join(root, "app", "package.json"), `{"name":"@acme/app"}`)
	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })

	before, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := before.ProjectByID("/app").Name; got != "app" {
		t.Fatalf("authored-only name = %q, want app", got)
	}

	// Build the value an external CLI process would publish without touching
	// this process's cached pointer. The cache must notice the atomic snapshot
	// appearance by itself on the next MCP-shaped Load.
	writer, err := loadUncached(root)
	if err != nil {
		t.Fatal(err)
	}
	provider := wsproto.ProbeResult{
		Version: wsproto.ProbeProtocolVersion, Extension: "@putnami/typescript",
		Projects: []wsproto.ProbeProject{{Path: "app", SourceName: "@acme/app"}},
	}
	merged, diags := wsproto.MergeProbeResults([]wsproto.ProbeResult{provider}, explicitProjects(writer))
	if len(diags) != 0 {
		t.Fatalf("merge diagnostics: %v", diags)
	}
	writer.AdoptProbeView(merged)
	snapshot := NewSnapshotWithProviders(writer, aggregateProbeDigest(writer, []wsproto.ProbeResult{provider}), []SnapshotProvider{{
		Extension: provider.Extension,
		Digest:    wsproto.ProbeResultDigest(provider),
		Result:    provider,
	}})
	if err := WriteSnapshot(root, snapshot, SnapshotWritePolicy{}); err != nil {
		t.Fatal(err)
	}

	after, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("Load returned the pre-index cached workspace after another process published the index")
	}
	if got := after.ProjectByID("/app").Name; got != "@acme/app" {
		t.Fatalf("reloaded provider name = %q, want @acme/app", got)
	}
}

func TestFindRoot_WithPutnamiRC(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "discovery-walks-up-to-a-supported-marker")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{"includes":["apps/*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := FindRoot(dir)
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	wantRoot := canonicalRoot(dir)
	if root != wantRoot {
		t.Errorf("FindRoot = %q, want %q", root, wantRoot)
	}
}

func TestFindRoot_WithPackageJSON(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "discovery-walks-up-to-a-supported-marker")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"test","workspaces":["packages/*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := FindRoot(dir)
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	wantRoot := canonicalRoot(dir)
	if root != wantRoot {
		t.Errorf("FindRoot = %q, want %q", root, wantRoot)
	}
}

func TestFindRoot_WalksUp(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "discovery-walks-up-to-a-supported-marker")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{"includes":["apps/*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	root, err := FindRoot(nested)
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	wantRoot := canonicalRoot(dir)
	if root != wantRoot {
		t.Errorf("FindRoot = %q, want %q", root, wantRoot)
	}
}

func TestFindRoot_CanonicalizesSymlinkedWorkspaceRoot(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "the-discovered-root-is-canonicalized")
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realRoot, "putnami.workspace.json"), []byte(`{"includes":["apps/*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(parent, "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}
	wantRoot := canonicalRoot(realRoot)

	root, err := FindRoot(linkRoot)
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	if root != wantRoot {
		t.Errorf("FindRoot = %q, want %q", root, wantRoot)
	}

	ws, err := Load(linkRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if ws.Root != wantRoot {
		t.Errorf("Load Root = %q, want %q", ws.Root, wantRoot)
	}
}

func TestFindRoot_NotFound(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "workspace-root", "a-missing-workspace-yields-an-actionable-init-error")
	dir := t.TempDir()
	// Create a deep nested dir with no marker files anywhere
	nested := filepath.Join(dir, "deep", "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := FindRoot(nested)
	if err == nil {
		t.Error("FindRoot should return error when no workspace found")
	}
}

func TestProject_ID(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "identities-and-dependencies-come-from-discovery")
	projects := []*Project{
		{Name: "@putnami/application", Path: "typescript/frameworks/application"},
		{Name: "go.putnami.dev/http", Path: "go/frameworks/http"},
		{Name: "cli", Path: "tooling/cli"},
	}

	// Simulate what discoverProject does.
	for _, p := range projects {
		p.ID = ProjectIDFromPath(p.Path)
	}

	tests := []struct {
		path string
		want string
	}{
		{"typescript/frameworks/application", "/typescript/frameworks/application"},
		{"go/frameworks/http", "/go/frameworks/http"},
		{"tooling/cli", "/tooling/cli"},
	}

	for _, tt := range tests {
		for _, p := range projects {
			if p.Path == tt.path {
				if p.ID != tt.want {
					t.Errorf("Project at %q: ID = %q, want %q", tt.path, p.ID, tt.want)
				}
			}
		}
	}
}

func TestLoad_TransparentGroupFoldersPreservePathsAndUseLogicalIDs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "putnami.workspace.json"), `{"includes":["identity"]}`)
	writeFile(t, filepath.Join(dir, "identity", "putnami.json"), `{"includes":["(workloads)/auth-server","(libs)/identity-client"]}`)
	writeFile(t, filepath.Join(dir, "identity", "(workloads)", "auth-server", "putnami.json"), `{"name":"auth-server","dependencies":["/identity/identity-client"]}`)
	writeFile(t, filepath.Join(dir, "identity", "(libs)", "identity-client", "putnami.json"), `{"name":"identity-client"}`)

	ws, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	auth := ws.ProjectByID("/identity/auth-server")
	if auth == nil {
		t.Fatal("logical project ID /identity/auth-server was not discovered")
	}
	if auth.Path != "identity/(workloads)/auth-server" {
		t.Errorf("auth Path = %q, want physical grouped path", auth.Path)
	}
	if got := ws.ProjectByPath("identity/(workloads)/auth-server"); got != auth {
		t.Errorf("ProjectByPath returned %p, want auth project %p", got, auth)
	}
	if old := ws.ProjectByID("/identity/(workloads)/auth-server"); old != nil {
		t.Errorf("physical path-derived ID unexpectedly resolves to %q", old.ID)
	}
	if got := ws.Graph.DependenciesOf(auth.ID); len(got) != 1 || got[0] != "/identity/identity-client" {
		t.Errorf("auth graph dependencies = %v, want logical ID /identity/identity-client", got)
	}
}

func TestLoad_TransparentGroupFolderIDCollision(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "putnami.workspace.json"), `{"includes":["identity/(workloads)/shared","identity/(libs)/shared"]}`)
	writeFile(t, filepath.Join(dir, "identity", "(workloads)", "shared", "package.json"), `{"name":"workload-shared"}`)
	writeFile(t, filepath.Join(dir, "identity", "(libs)", "shared", "package.json"), `{"name":"library-shared"}`)

	ws, err := Load(dir)
	if err == nil {
		t.Fatalf("Load returned workspace %#v; want duplicate logical ID error", ws)
	}
	for _, want := range []string{"/identity/shared", "identity/(libs)/shared", "identity/(workloads)/shared"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("collision error %q missing %q", err, want)
		}
	}
}

func TestLoad_PopulatesScopeIndex(t *testing.T) {
	dir := t.TempDir()

	// Create workspace root config with a scope
	if err := os.WriteFile(filepath.Join(dir, "putnami.workspace.json"), []byte(`{
		"includes": ["libs", "standalone"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create scope with groups and aliases
	libsDir := filepath.Join(dir, "libs")
	if err := os.MkdirAll(libsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libsDir, "putnami.json"), []byte(`{
		"includes": ["core", "utils"],
		"tags": ["lib"],
		"groups": {"all-libs": "/libs/..."},
		"projectAliases": {"c": "/libs/core"}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create project dirs with package.json
	for _, name := range []string{"libs/core", "libs/utils", "standalone"} {
		pDir := filepath.Join(dir, name)
		if err := os.MkdirAll(pDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pDir, "package.json"), []byte(`{"name":"`+filepath.Base(name)+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ws, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if ws.ScopeIndex == nil {
		t.Fatal("ScopeIndex should not be nil")
	}

	if got, ok := ws.ScopeIndex.Groups["all-libs"]; !ok || got != "/libs/..." {
		t.Errorf("ScopeIndex.Groups[all-libs] = %q, want /libs/...", got)
	}

	if got, ok := ws.ScopeIndex.ProjectAliases["c"]; !ok || got != "/libs/core" {
		t.Errorf("ScopeIndex.ProjectAliases[c] = %q, want /libs/core", got)
	}
}

// TestCheckDuplicateNames covers the workspace-load guard that rejects two or
// more projects resolving to the same name — an identity collision that would
// otherwise silently overwrite projectMap and clobber config-server storage.
func TestCheckDuplicateNames(t *testing.T) {
	t.Run("no duplicates", func(t *testing.T) {
		projects := []*Project{
			{ID: "/a", Name: "@acme/a", Path: "a"},
			{ID: "/b", Name: "@acme/b", Path: "b"},
		}
		if err := checkDuplicateNames(projects); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("duplicate name is an error listing both paths", func(t *testing.T) {
		projects := []*Project{
			{ID: "/x", Name: "@acme/dup", Path: "pkg/x"},
			{ID: "/y", Name: "@acme/dup", Path: "pkg/y"},
			{ID: "/z", Name: "@acme/ok", Path: "pkg/z"},
		}
		err := checkDuplicateNames(projects)
		if err == nil {
			t.Fatal("expected duplicate-name error, got nil")
		}
		msg := err.Error()
		for _, want := range []string{"@acme/dup", "pkg/x", "pkg/y"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q missing %q", msg, want)
			}
		}
		if strings.Contains(msg, "@acme/ok") {
			t.Errorf("error should not name the non-duplicate project: %q", msg)
		}
	})

	t.Run("empty names are skipped, not reported as a collision", func(t *testing.T) {
		projects := []*Project{
			{ID: "/a", Name: "", Path: "a"},
			{ID: "/b", Name: "", Path: "b"},
		}
		if err := checkDuplicateNames(projects); err != nil {
			t.Errorf("empty names should be skipped, got: %v", err)
		}
	})
}

// TestLoad_WarnsOnIgnoredProjectJobs pins the descope of the project-level
// `jobs` key. The key parses — older configs must keep loading — but
// planning never reads it, so a load that says nothing leaves the author
// believing their block runs. The warning IS the contract.
func TestLoad_WarnsOnIgnoredProjectJobs(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"ws","includes":["zeta","alpha","beta"]}`)
	// Declared out of sorted order on purpose: the warning must not inherit
	// discovery order.
	writeFileAt(t, filepath.Join(root, "zeta", "putnami.json"),
		`{"name":"zeta","jobs":{"typecheck":{"kind":"command","command":"tsc"}}}`)
	writeFileAt(t, filepath.Join(root, "alpha", "putnami.json"),
		`{"name":"alpha","jobs":{"gen":{}}}`)
	// A project on the SUPPORTED surfaces must stay silent.
	writeFileAt(t, filepath.Join(root, "beta", "putnami.json"),
		`{"name":"beta","tasks":{"lint":{"cpuWeight":2}},"disable":{"jobs":["serve"]}}`)

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var warning string
	for _, w := range ws.Warnings {
		if strings.Contains(w, "`jobs`") {
			if warning != "" {
				t.Fatalf("two projects declaring jobs produced two warnings, want one:\n%v", ws.Warnings)
			}
			warning = w
		}
	}
	if warning == "" {
		t.Fatalf("a project declaring `jobs` warned nothing:\n%v", ws.Warnings)
	}

	// It names the offending projects...
	alpha, zeta := strings.Index(warning, "alpha"), strings.Index(warning, "zeta")
	if alpha < 0 || zeta < 0 {
		t.Fatalf("the warning does not name both declaring projects: %s", warning)
	}
	if alpha > zeta {
		t.Errorf("project paths are not sorted; Go map/discovery order leaked into the message: %s", warning)
	}
	if strings.Contains(warning, "beta") {
		t.Errorf("the warning names a project that only uses the supported surfaces: %s", warning)
	}
	// ...says the key does nothing...
	if !strings.Contains(warning, "ignored") {
		t.Errorf("the warning does not say the key is ignored: %s", warning)
	}
	// ...and tells the operator where to go instead.
	for _, want := range []string{"disable.jobs", "`tasks`"} {
		if !strings.Contains(warning, want) {
			t.Errorf("the warning does not point at %s: %s", want, warning)
		}
	}

	// Deterministic: the same tree yields byte-identical warnings across loads.
	InvalidateLoadCache(root)
	again, err := Load(root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reflect.DeepEqual(again.Warnings, ws.Warnings) {
		t.Errorf("warnings are not stable across loads:\n%v\n%v", ws.Warnings, again.Warnings)
	}
}

// TestLoad_WarnsOnEmptyProjectJobsBlock pins that the warning triggers on
// PRESENCE of the key, not on how many entries it holds.
//
// `{"jobs": {}}` is enough to mark a directory as a project (the marker scan
// tests `Jobs != nil`), so a size test here would let an empty block create a
// project and then say nothing about the ignored surface that created it —
// reintroducing, in miniature, the exact silent no-op the warning guards against. An absent
// key and an explicit `"jobs": null` both leave the map nil and stay quiet.
func TestLoad_WarnsOnEmptyProjectJobsBlock(t *testing.T) {
	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"),
		`{"name":"ws","includes":["empty","nulled","absent"]}`)
	writeFileAt(t, filepath.Join(root, "empty", "putnami.json"), `{"name":"empty","jobs":{}}`)
	writeFileAt(t, filepath.Join(root, "nulled", "putnami.json"), `{"name":"nulled","jobs":null}`)
	writeFileAt(t, filepath.Join(root, "absent", "putnami.json"), `{"name":"absent"}`)

	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var warning string
	for _, w := range ws.Warnings {
		if strings.Contains(w, "`jobs`") {
			warning = w
		}
	}
	if warning == "" {
		t.Fatalf("an empty `jobs` block warned nothing; it still declares the ignored surface:\n%v", ws.Warnings)
	}
	if !strings.Contains(warning, "empty") {
		t.Errorf("the warning does not name the project declaring an empty `jobs` block: %s", warning)
	}
	for _, quiet := range []string{"nulled", "absent"} {
		if strings.Contains(warning, quiet) {
			t.Errorf("the warning names %q, which does not declare `jobs` at all: %s", quiet, warning)
		}
	}
}

// A putnami.json whose ONLY project-specific key is `jobs` must still make the
// directory a project. Dropping the marker would make an ignored key also make
// its project vanish from the workspace — a second silent failure on top of the
// one this descope exists to surface.
func TestProjectMarker_JobsOnlyConfigIsStillAProject(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, filepath.Join(dir, "putnami.json"),
		`{"includes":["nested"],"jobs":{"gen":{"kind":"command","command":"go"}}}`)

	if isScopeOnlyConfig(dir) {
		t.Error("a config declaring only `jobs` was treated as scope-only; its project would disappear silently")
	}
}

func TestLoad_RejectsNonPositiveTaskTimeout(t *testing.T) {
	for _, timeout := range []string{"0", "-1", "-50"} {
		t.Run(timeout, func(t *testing.T) {
			root := t.TempDir()
			writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["apps/api"]}`)
			writeFileAt(t, filepath.Join(root, "apps", "api", "putnami.json"),
				`{"name":"api","tasks":{"lint~golangci-lint":{"timeoutMs":`+timeout+`}}}`)
			InvalidateLoadCache(root)
			t.Cleanup(func() { InvalidateLoadCache(root) })

			if ws, err := Load(root); err == nil {
				t.Fatalf("Load returned %#v, want an invalid configuration error", ws)
			} else {
				for _, want := range []string{
					filepath.Join(root, "apps", "api", "putnami.json"),
					"tasks.lint~golangci-lint.timeoutMs",
					timeout,
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
			}
		})
	}

	root := t.TempDir()
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["apps/api"]}`)
	writeFileAt(t, filepath.Join(root, "apps", "api", "putnami.json"),
		`{"name":"api","tasks":{"lint":{"timeoutMs":1,"cpuWeight":4}}}`)
	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("positive task deadline should load: %v", err)
	}
	if got := ws.ProjectByName("api").Config.Tasks["lint"].TimeoutMs; got == nil || *got != 1 {
		t.Fatalf("loaded timeout = %v, want 1", got)
	}
}

func TestLoad_RejectsMalformedTaskTuning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tasks string
		want  string
	}{
		{
			name:  "wrong timeout type",
			tasks: `{"lint~golangci-lint":{"timeoutMs":"900000"}}`,
			want:  "tasks.lint~golangci-lint.timeoutMs",
		},
		{
			name:  "wrong timeout spelling",
			tasks: `{"lint~golangci-lint":{"timeoutMS":900000}}`,
			want:  "tasks.lint~golangci-lint.timeoutMS",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), `{"includes":["apps/api"]}`)
			writeFileAt(t, filepath.Join(root, "apps", "api", "putnami.json"),
				`{"name":"api","tasks":`+tc.tasks+`}`)
			InvalidateLoadCache(root)
			t.Cleanup(func() { InvalidateLoadCache(root) })

			if ws, err := Load(root); err == nil {
				t.Fatalf("Load returned %#v, want an invalid configuration error", ws)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}
