package workspace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- Config tests ---

func TestLoad_Empty(t *testing.T) {
	root := t.TempDir()
	cfg := Load(root)
	if cfg == nil {
		t.Fatal("Load should never return nil")
	}
}

func TestLoad_WorkspaceFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, WorkspaceConfigFilename), `{
		"name": "test-ws",
		"includes": ["app", "lib"]
	}`)

	// Chdir to root so cwd == workspaceRoot (avoids picking up the test module's putnami.json).
	origDir, _ := os.Getwd()
	os.Chdir(root)
	defer os.Chdir(origDir)

	cfg := Load(root)
	if cfg.Name != "test-ws" {
		t.Errorf("Name = %q, want test-ws", cfg.Name)
	}
	if len(cfg.Includes) != 2 {
		t.Errorf("Includes = %v, want [app, lib]", cfg.Includes)
	}
}

// TestLoad_DoesNotMergeCwdProjectOptions guards against a project's own
// putnami.json leaking into the workspace-wide command defaults. Before this
// was fixed, running `--all` from inside a Docker-packaged workload merged that
// project's options.package.docker into the shared Config, scheduling docker
// packaging for every project (libraries included → "compile output not found").
func TestLoad_DoesNotMergeCwdProjectOptions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, WorkspaceConfigFilename), `{"name": "test-ws"}`)
	// A project subdirectory that opts into docker packaging.
	sub := filepath.Join(root, "workloads", "app")
	writeFile(t, filepath.Join(sub, ConfigFilename), `{"name": "workloads/app", "options": {"package": {"docker": true}}}`)

	origDir, _ := os.Getwd()
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)

	cfg := Load(root)

	if v, ok := cfg.GetCommandDefaults("package", "@putnami/typescript")["docker"]; ok && v == true {
		t.Errorf("cwd project options leaked into workspace package defaults: docker=%v", v)
	}
	if cfg.Name != "test-ws" {
		t.Errorf("workspace Name = %q, want test-ws (cwd project must not override workspace config)", cfg.Name)
	}
}

func TestGetCommandDefaults_MergeOrder(t *testing.T) {
	c := &Config{
		Options: map[string]map[string]any{
			"*":                         {"a": 1, "b": 2, "c": 3, "d": 4},
			"build":                     {"b": 20, "c": 30},
			"@putnami/typescript":       {"c": 300},
			"@putnami/typescript:build": {"d": 4000},
		},
	}
	defaults := c.GetCommandDefaults("build", "@putnami/typescript")

	if defaults["a"] != 1 {
		t.Errorf("a = %v, want 1", defaults["a"])
	}
	if defaults["b"] != 20 {
		t.Errorf("b = %v, want 20", defaults["b"])
	}
	if defaults["c"] != 300 {
		t.Errorf("c = %v, want 300", defaults["c"])
	}
	if defaults["d"] != 4000 {
		t.Errorf("d = %v, want 4000", defaults["d"])
	}
}

func TestGetCommandDefaults_Empty(t *testing.T) {
	c := &Config{}
	defaults := c.GetCommandDefaults("build", "")
	if len(defaults) != 0 {
		t.Errorf("expected empty defaults, got %v", defaults)
	}
}

func TestMergeWorkspaceConfig(t *testing.T) {
	target := &Config{
		Name:           "ws",
		Baseline:       "origin/main",
		EpicBranches:   []string{"epic/a"},
		Includes:       []string{"scope-a", "a", "b"},
		AgentArtifacts: []string{"global-workflows"},
	}
	source := &Config{
		Baseline:       "origin/release",
		EpicBranches:   []string{"epic/a", "epic/b"},
		Includes:       []string{"scope-a", "scope-b", "b", "c"},
		AgentArtifacts: []string{"global-workflows", "team-workflows"},
		Aliases:        map[string]string{"x": "build"},
	}

	MergeWorkspaceConfig(target, source)

	if target.Name != "ws" {
		t.Errorf("name should not be overwritten, got %q", target.Name)
	}
	if target.Baseline != "origin/release" {
		t.Errorf("baseline should be overwritten, got %q", target.Baseline)
	}
	if len(target.EpicBranches) != 2 {
		t.Errorf("epicBranches should be merged uniquely: %v", target.EpicBranches)
	}
	if len(target.Includes) != 5 {
		t.Errorf("includes should be merged uniquely: %v", target.Includes)
	}
	if target.Aliases["x"] != "build" {
		t.Error("aliases should be merged")
	}
	if got := strings.Join(target.AgentArtifacts, ","); got != "global-workflows,team-workflows" {
		t.Errorf("agentArtifacts should be merged uniquely, got %q", got)
	}
}

func TestMergeWorkspaceConfig_Hooks(t *testing.T) {
	target := &Config{
		Hooks: &HooksConfig{
			Commands: map[string]*HookPhaseConfig{
				"publish": {Before: []string{"echo global"}},
			},
		},
	}
	source := &Config{
		Hooks: &HooksConfig{
			CLI: &HookPhaseConfig{Before: []string{"echo cli-before"}},
			Commands: map[string]*HookPhaseConfig{
				"publish": {Before: []string{"echo workspace"}},
			},
		},
	}

	MergeWorkspaceConfig(target, source)

	if target.Hooks.CLI == nil || len(target.Hooks.CLI.Before) != 1 {
		t.Errorf("CLI hooks not merged: %+v", target.Hooks.CLI)
	}
	publishBefore := target.Hooks.Commands["publish"].Before
	if len(publishBefore) != 2 || publishBefore[0] != "echo global" || publishBefore[1] != "echo workspace" {
		t.Errorf("publish before hooks = %v, want [echo global, echo workspace]", publishBefore)
	}
}

func TestMergeWorkspaceConfig_Disable(t *testing.T) {
	target := &Config{}
	source := &Config{
		Disable: &DisableConfig{
			Extensions: []string{"@putnami/ci"},
			Tags:       []string{"deprecated"},
		},
	}

	MergeWorkspaceConfig(target, source)

	if target.Disable == nil {
		t.Fatal("disable should be set")
	}
	if len(target.Disable.Extensions) != 1 || target.Disable.Extensions[0] != "@putnami/ci" {
		t.Errorf("disabled extensions = %v", target.Disable.Extensions)
	}
}

func TestMergeWorkspaceConfig_Store(t *testing.T) {
	mb := func(n int64) *int64 { return &n }

	// Global scope sets a budget + grace; workspace scope overrides the budget
	// and adds an idle threshold. Later scope wins per field; unset fields persist.
	target := &Config{Store: &StoreConfig{MaxBytes: mb(1000), GCGrace: "2h"}}
	source := &Config{Store: &StoreConfig{MaxBytes: mb(5000), MaxIdleBuilds: mb(50)}}

	MergeWorkspaceConfig(target, source)

	if target.Store == nil {
		t.Fatal("store config should be set")
	}
	if target.Store.MaxBytes == nil || *target.Store.MaxBytes != 5000 {
		t.Errorf("MaxBytes = %v, want 5000 (workspace overrides global)", target.Store.MaxBytes)
	}
	if target.Store.GCGrace != "2h" {
		t.Errorf("GCGrace = %q, want 2h (unset in workspace, retained from global)", target.Store.GCGrace)
	}
	if target.Store.MaxIdleBuilds == nil || *target.Store.MaxIdleBuilds != 50 {
		t.Errorf("MaxIdleBuilds = %v, want 50", target.Store.MaxIdleBuilds)
	}
}

// TestMergeWorkspaceConfig_Sessions pins session retention through the same
// per-field overlay the store block uses: a later scope replaces the value it
// authors, and a scope that authors nothing leaves the earlier one intact.
func TestMergeWorkspaceConfig_Sessions(t *testing.T) {
	keep := func(n int) *int { return &n }

	target := &Config{Sessions: &SessionsConfig{Keep: keep(20)}}
	MergeWorkspaceConfig(target, &Config{Sessions: &SessionsConfig{Keep: keep(500)}})
	if target.Sessions == nil || target.Sessions.Keep == nil || *target.Sessions.Keep != 500 {
		t.Fatalf("Keep = %v, want 500 (workspace overrides global)", target.Sessions)
	}

	// A source with no sessions block must not clear the one already merged:
	// "absent" is not "reset to default".
	MergeWorkspaceConfig(target, &Config{Name: "ws"})
	if target.Sessions == nil || target.Sessions.Keep == nil || *target.Sessions.Keep != 500 {
		t.Fatalf("Keep = %v, want 500 retained when a later scope authors no sessions block", target.Sessions)
	}
}

// TestWorkspaceConfig_SessionsRoundtrip pins the wire spelling and the
// pointer's purpose: an absent block stays nil so a reader can tell "unset"
// (use the default) from an authored value.
func TestWorkspaceConfig_SessionsRoundtrip(t *testing.T) {
	var authored Config
	if err := json.Unmarshal([]byte(`{"sessions": {"keep": 200}}`), &authored); err != nil {
		t.Fatalf("unmarshal sessions: %v", err)
	}
	if authored.Sessions == nil || authored.Sessions.Keep == nil || *authored.Sessions.Keep != 200 {
		t.Fatalf("sessions = %+v, want keep 200", authored.Sessions)
	}

	var absent Config
	if err := json.Unmarshal([]byte(`{"name": "ws"}`), &absent); err != nil {
		t.Fatalf("unmarshal without sessions: %v", err)
	}
	if absent.Sessions != nil {
		t.Fatalf("sessions = %+v, want nil when the config authors none", absent.Sessions)
	}

	data, err := json.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sessions") {
		t.Errorf("an unset sessions block must be omitted from the wire, got %s", data)
	}
}

// --- ProjectConfig tests ---

func TestLoadProjectConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ConfigFilename), `{
		"name": "my-app",
		"tags": ["ts", "framework"],
		"dependencies": ["@putnami/utils"]
	}`)

	cfg := LoadProjectConfig(root)
	if cfg == nil {
		t.Fatal("expected project config")
	}
	if cfg.Name != "my-app" {
		t.Errorf("Name = %q, want my-app", cfg.Name)
	}
	if len(cfg.Tags) != 2 {
		t.Errorf("Tags = %v, want [ts, framework]", cfg.Tags)
	}
}

func TestLoadProjectConfig_Missing(t *testing.T) {
	cfg := LoadProjectConfig(t.TempDir())
	if cfg != nil {
		t.Error("expected nil for missing project config")
	}
}

func TestLoadProjectConfig_Invalid(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ConfigFilename), `{invalid}`)

	cfg := LoadProjectConfig(root)
	if cfg != nil {
		t.Error("expected nil for invalid JSON")
	}
}

func TestLoadProjectConfigWithDiagnostics_StrictTaskTuning(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		field  string
		want   string
	}{
		{
			name:   "wrong timeout type",
			config: `{"name":"app","tasks":{"lint":{"timeoutMs":"900000"}}}`,
			field:  "tasks.lint.timeoutMs",
			want:   "cannot unmarshal string",
		},
		{
			name:   "wrong timeout spelling",
			config: `{"name":"app","tasks":{"lint":{"timeoutMS":900000}}}`,
			field:  "tasks.lint.timeoutMS",
			want:   "unknown task tuning field",
		},
		{
			name:   "task tuning is not an object",
			config: `{"name":"app","tasks":{"lint":null}}`,
			field:  "tasks.lint",
			want:   "task tuning must be an object",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ConfigFilename), tc.config)

			cfg, diags := LoadProjectConfigWithDiagnostics(root)
			if cfg != nil {
				t.Fatalf("malformed task tuning returned %#v, want no usable config", cfg)
			}
			if len(diags) != 1 {
				t.Fatalf("diagnostics = %v, want one", diags)
			}
			if diags[0].Field != tc.field || !strings.Contains(diags[0].Message, tc.want) {
				t.Fatalf("diagnostic = %+v, want field %q containing %q", diags[0], tc.field, tc.want)
			}
			if cfg := LoadProjectConfig(root); cfg != nil {
				t.Fatalf("legacy convenience loader returned %#v for malformed task tuning, want nil", cfg)
			}
		})
	}
}

func TestLoadProjectConfigWithDiagnostics_RemainsLenientOutsideTasks(t *testing.T) {
	root := t.TempDir()
	// `name` remains a legacy-lenient field, while the valid tasks block must
	// not accidentally make the whole config strict.
	writeFile(t, filepath.Join(root, ConfigFilename), `{"name":42,"tasks":{"lint":{"timeoutMs":900000}}}`)

	cfg, diags := LoadProjectConfigWithDiagnostics(root)
	if cfg != nil || len(diags) != 0 {
		t.Fatalf("unrelated malformed field changed legacy leniency: cfg=%#v diags=%v", cfg, diags)
	}
}

func TestProjectConfig_Roundtrip(t *testing.T) {
	cfg := &ProjectConfig{
		Name:         "test",
		Tags:         []string{"go"},
		Dependencies: []string{"dep1"},
		Jobs: map[string]json.RawMessage{
			"build": json.RawMessage(`{"kind":"command","command":"go"}`),
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var parsed ProjectConfig
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Name != "test" {
		t.Errorf("Name = %q", parsed.Name)
	}
	if len(parsed.Jobs) != 1 {
		t.Errorf("Jobs = %v", parsed.Jobs)
	}
}

// --- ScopeConfig tests ---

func TestLoadScopeChain_NoFiles(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "typescript", "framework", "app")
	os.MkdirAll(project, 0o755)

	sc := LoadScopeChain(root, project)
	if sc != nil {
		t.Error("expected nil when no scopes exist")
	}
}

func TestLoadScopeChain_SingleAncestor(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "typescript", "framework", "app")

	writeFile(t, filepath.Join(root, "typescript", "framework", ConfigFilename), `{
		"tags": ["ts"],
		"extensions": ["/typescript/extension"],
		"publishConfig": { "npm": { "namePattern": "@putnami/{name}" } }
	}`)
	os.MkdirAll(project, 0o755)

	sc := LoadScopeChain(root, project)
	if sc == nil {
		t.Fatal("expected scope config")
	}
	if len(sc.Tags) != 1 || sc.Tags[0] != "ts" {
		t.Errorf("tags = %v, want [ts]", sc.Tags)
	}
	if sc.PublishConfig["npm"]["namePattern"] != "@putnami/{name}" {
		t.Errorf("namePattern = %v", sc.PublishConfig["npm"]["namePattern"])
	}
}

func TestLoadScopeChain_DeepestWins(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "typescript", "framework", "app")

	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{
		"tags": ["lang-ts"],
		"extensions": ["/typescript/extension-base"]
	}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", ConfigFilename), `{
		"tags": ["framework"],
		"extensions": ["/typescript/extension"]
	}`)
	os.MkdirAll(project, 0o755)

	sc := LoadScopeChain(root, project)
	if sc == nil {
		t.Fatal("expected scope config")
	}
	if len(sc.Tags) != 2 {
		t.Errorf("tags = %v, want [lang-ts, framework]", sc.Tags)
	}
	if len(sc.Extensions) != 1 || sc.Extensions[0] != "/typescript/extension" {
		t.Errorf("extensions = %v, want [/typescript/extension]", sc.Extensions)
	}
}

// The sources are the files the merge read, shallowest first, as
// workspace-relative slash paths. A putnami.json with no scope field is not a
// source, because it contributed nothing.
func TestLoadScopeChainWithSources_NamesTheFilesItMerged(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "typescript", "framework", "libs", "app")

	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{"tags": ["lang-ts"]}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", ConfigFilename), `{"extensions": ["/typescript/extension"]}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", "libs", ConfigFilename), `{"name": "libs-holder"}`)
	os.MkdirAll(project, 0o755)

	sc, sources := LoadScopeChainWithSources(root, project)
	if sc == nil {
		t.Fatal("expected scope config")
	}
	want := []string{"typescript/putnami.json", "typescript/framework/putnami.json"}
	if !slices.Equal(sources, want) {
		t.Errorf("sources = %v, want %v", sources, want)
	}
	if merged := LoadScopeChain(root, project); !slices.Equal(merged.Tags, sc.Tags) || !slices.Equal(merged.Extensions, sc.Extensions) {
		t.Errorf("LoadScopeChain = %+v, want the same merge as LoadScopeChainWithSources %+v", merged, sc)
	}
	if _, sources := LoadScopeChainWithSources(root, filepath.Join(root, "go", "service")); sources != nil {
		t.Errorf("sources under no scope = %v, want nil", sources)
	}
}

// A scope's distribution level reaches the projects below it, the deepest
// scope wins, and a file that declares only that level is a scope source: the
// level is inherited, so a change to it is a change to every project below.
func TestLoadScopeChain_DistributionDeepestWins(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "typescript", "framework", "app")

	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{"distribution": {"visibility": "private"}}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", ConfigFilename), `{"distribution": {"visibility": "public"}}`)
	os.MkdirAll(project, 0o755)

	sc, sources := LoadScopeChainWithSources(root, project)
	if sc == nil || sc.Distribution == nil || sc.Distribution.Visibility != "public" {
		t.Fatalf("scope chain = %+v, want the deepest distribution level", sc)
	}
	want := []string{"typescript/putnami.json", "typescript/framework/putnami.json"}
	if !slices.Equal(sources, want) {
		t.Errorf("sources = %v, want %v", sources, want)
	}

	sibling := filepath.Join(root, "typescript", "tools", "gen")
	os.MkdirAll(sibling, 0o755)
	if sc := LoadScopeChain(root, sibling); sc == nil || sc.Distribution == nil || sc.Distribution.Visibility != "private" {
		t.Fatalf("scope chain = %+v, want the shallower scope's level where no deeper scope states one", sc)
	}
}

func TestScanAllScopes_Aggregation(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, WorkspaceConfigFilename), `{
		"projectAliases": { "cli": "/tooling/cli" }
	}`)
	writeFile(t, filepath.Join(root, "typescript", "framework", ConfigFilename), `{
		"projectAliases": { "app": "/typescript/framework/app" },
		"groups": { "ts-fw": "/typescript/framework/..." }
	}`)

	index, err := ScanAllScopes(root, []string{"typescript/framework/app"})
	if err != nil {
		t.Fatalf("ScanAllScopes: %v", err)
	}
	if index.ProjectAliases["cli"] != "/tooling/cli" {
		t.Errorf("cli alias = %q", index.ProjectAliases["cli"])
	}
	if index.ProjectAliases["app"] != "/typescript/framework/app" {
		t.Errorf("app alias = %q", index.ProjectAliases["app"])
	}
	if index.Groups["ts-fw"] != "/typescript/framework/..." {
		t.Errorf("ts-fw group = %q", index.Groups["ts-fw"])
	}
}

func TestScanAllScopes_DuplicateAlias(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "a", ConfigFilename), `{
		"projectAliases": { "app": "/a/app" }
	}`)
	writeFile(t, filepath.Join(root, "b", ConfigFilename), `{
		"projectAliases": { "app": "/b/app" }
	}`)

	_, err := ScanAllScopes(root, []string{"a/app", "b/app"})
	if err == nil {
		t.Error("expected error for duplicate alias")
	}
}

func TestReadScopeConfig_WithIncludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ConfigFilename), `{
		"includes": ["app", "http"],
		"tags": ["go"]
	}`)

	sc := ReadScopeConfig(root)
	if sc == nil {
		t.Fatal("expected scope config")
	}
	if len(sc.IncludePaths()) != 2 {
		t.Errorf("includes = %v", sc.IncludePaths())
	}
}

func TestReadScopeConfig_Empty(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ConfigFilename), `{}`)

	sc := ReadScopeConfig(root)
	if sc != nil {
		t.Error("expected nil for empty config")
	}
}

// TestReadScopeConfig_ActivatedExample mirrors the activated-scope example from
// docs/05-configuration.md. Both ReadScopeConfig and LoadProjectConfig must
// successfully parse the same file, since the schema accepts project fields
// alongside scope fields when activate is true.
func TestReadScopeConfig_ActivatedExample(t *testing.T) {
	root := t.TempDir()
	// Verbatim from tooling/cli/doc/05-configuration.md.
	writeFile(t, filepath.Join(root, ConfigFilename), `{
		"name": "cloud",
		"activate": true,
		"includes": ["workloads/api", "workloads/worker"],
		"extensions": ["/cloud/extension"]
	}`)

	sc := ReadScopeConfig(root)
	if sc == nil {
		t.Fatal("ReadScopeConfig returned nil for activated scope example")
	}
	if !sc.Activate {
		t.Error("Activate = false; want true")
	}
	if len(sc.IncludePaths()) != 2 {
		t.Errorf("IncludePaths = %v; want 2 entries", sc.IncludePaths())
	}
	if len(sc.Extensions) != 1 || sc.Extensions[0] != "/cloud/extension" {
		t.Errorf("Extensions = %v; want [/cloud/extension]", sc.Extensions)
	}

	pc := LoadProjectConfig(root)
	if pc == nil {
		t.Fatal("LoadProjectConfig returned nil for activated scope example")
	}
	if pc.Name != "cloud" {
		t.Errorf("project Name = %q; want cloud", pc.Name)
	}
	if len(pc.Extensions) != 1 || pc.Extensions[0] != "/cloud/extension" {
		t.Errorf("project Extensions = %v; want [/cloud/extension]", pc.Extensions)
	}
}

func TestResolveNamePattern(t *testing.T) {
	sc := &ScopeConfig{
		PublishConfig: map[string]map[string]any{
			"npm": {"namePattern": "@putnami/{name}"},
		},
	}
	got := sc.ResolveNamePattern("application")
	if got != "@putnami/application" {
		t.Errorf("ResolveNamePattern = %q, want @putnami/application", got)
	}
}

func TestResolveNamePattern_Nil(t *testing.T) {
	var sc *ScopeConfig
	if got := sc.ResolveNamePattern("app"); got != "" {
		t.Errorf("nil scope should return empty, got %q", got)
	}
}

// --- Resolve helpers ---

func TestIsWorkspaceConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, WorkspaceConfigFilename), `{}`)

	if !IsWorkspaceConfig(root) {
		t.Error("expected true when workspace config exists")
	}
}

func TestIsWorkspaceConfig_Missing(t *testing.T) {
	if IsWorkspaceConfig(t.TempDir()) {
		t.Error("expected false when no workspace config")
	}
}

// --- Cases inherited from the deleted CLI alias package ---
//
// tooling/cli/internal/config was a pure `type X = workspace.X` alias of this
// package with its own test file. Deleting that shim moved the cases it
// covered that this suite did not. Each one pins a rule
// this package OWNS — merge precedence, scope recognition, duplicate detection
// — so losing them with the alias would have quietly lowered the protocol's
// own coverage.

func TestGetCommandDefaults_GlobalOnly(t *testing.T) {
	c := &Config{
		Options: map[string]map[string]any{
			"*": {"verbose": true, "coverage": true},
		},
	}
	defaults := c.GetCommandDefaults("build", "")
	if defaults["verbose"] != true {
		t.Error("global verbose should be true")
	}
	if defaults["coverage"] != true {
		t.Error("global coverage should be true")
	}
}

func TestGetCommandDefaults_CommandOverridesGlobal(t *testing.T) {
	c := &Config{
		Options: map[string]map[string]any{
			"*":    {"fix": false, "coverage": true},
			"lint": {"fix": true},
		},
	}
	defaults := c.GetCommandDefaults("lint", "")
	if defaults["fix"] != true {
		t.Errorf("lint fix should be true (command overrides global), got %v", defaults["fix"])
	}
	if defaults["coverage"] != true {
		t.Error("coverage should be inherited from global")
	}
}

func TestMergeWorkspaceConfig_HooksFromNil(t *testing.T) {
	target := &Config{}
	source := &Config{
		Hooks: &HooksConfig{
			Commands: map[string]*HookPhaseConfig{
				"publish": {Before: []string{"npx auth"}},
			},
		},
	}

	MergeWorkspaceConfig(target, source)

	if target.Hooks == nil {
		t.Fatal("hooks should be set")
	}
	if len(target.Hooks.Commands["publish"].Before) != 1 {
		t.Errorf("publish hooks = %v", target.Hooks.Commands["publish"])
	}
}

func TestMergeWorkspaceConfig_ProjectAliasesAndGroups(t *testing.T) {
	target := &Config{}
	source := &Config{
		ProjectAliases: map[string]string{"app": "/ts/fw/app"},
		Groups:         map[string]string{"ts": "/typescript/..."},
	}

	MergeWorkspaceConfig(target, source)

	if target.ProjectAliases["app"] != "/ts/fw/app" {
		t.Errorf("projectAliases not merged: %v", target.ProjectAliases)
	}
	if target.Groups["ts"] != "/typescript/..." {
		t.Errorf("groups not merged: %v", target.Groups)
	}
}

// TestLoadScopeChain_PublishConfigDeepestWins: a publish channel is replaced
// wholesale by the deepest scope that names it, not merged key by key — a
// half-inherited registry/namePattern pair would publish to the wrong place.
func TestLoadScopeChain_PublishConfigDeepestWins(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "typescript", "samples", "app")

	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{
		"publishConfig": {
			"npm": { "namePattern": "@putnami/{name}", "registry": "https://registry.npmjs.org" }
		}
	}`)
	writeFile(t, filepath.Join(root, "typescript", "samples", ConfigFilename), `{
		"publishConfig": {
			"npm": { "namePattern": "@example/{name}" }
		}
	}`)
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	sc := LoadScopeChain(root, project)
	if sc == nil {
		t.Fatal("expected scope config")
	}
	if sc.PublishConfig["npm"]["namePattern"] != "@example/{name}" {
		t.Errorf("namePattern = %v, want @example/{name}", sc.PublishConfig["npm"]["namePattern"])
	}
}

func TestScanAllScopes_DuplicateGroup(t *testing.T) {
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "typescript", ConfigFilename), `{
		"groups": { "all": "/typescript/..." }
	}`)
	writeFile(t, filepath.Join(root, "go", ConfigFilename), `{
		"groups": { "all": "/go/..." }
	}`)

	if _, err := ScanAllScopes(root, []string{"typescript/framework/app", "go/framework/http"}); err == nil {
		t.Error("expected error for duplicate group")
	}
}

// TestReadScopeConfig_IncludesOnlyIsScope: a directory config carrying only
// includes is still a scope. TestReadScopeConfig_Empty pins the other side —
// `{}` is not — so the recognition rule is bounded from both directions.
func TestReadScopeConfig_IncludesOnlyIsScope(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ConfigFilename), `{
		"includes": ["app"]
	}`)

	sc := ReadScopeConfig(root)
	if sc == nil {
		t.Fatal("expected scope config with includes-only to be recognized")
	}
	if includes := sc.IncludePaths(); len(includes) != 1 || includes[0] != "app" {
		t.Errorf("includes = %v, want [app]", includes)
	}
}

func TestReadScopeConfig_LineOnlyIsScope(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ConfigFilename), `{
		"line": {"tag": "ts/v{version}"}
	}`)

	sc := ReadScopeConfig(root)
	if sc == nil {
		t.Fatal("expected line-only scope config to be recognized")
	}
	if !sc.IsLine() || sc.Line.Tag != "ts/v{version}" {
		t.Errorf("line = %+v, want tag ts/v{version}", sc.Line)
	}
}
