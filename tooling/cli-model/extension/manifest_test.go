package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	proto "go.putnami.dev/protocol/extension"
)

func TestLoadManifest_Valid(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "putnami.extension.json")

	data := `{
		"name": "@putnami/test",
		"version": "1.0.0",
		"cliContract": 4,
		"commands": {
			"build": {
				"run": [{"id": "build", "task": "build-exec"}]
			}
		},
		"tasks": {
			"build-exec": {
				"kind": "command",
				"command": "node",
				"args": ["build.js"]
			}
		}
	}`
	if err := os.WriteFile(manifestPath, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "@putnami/test" {
		t.Errorf("Name = %q, want %q", m.Name, "@putnami/test")
	}
	if _, ok := m.Commands["build"]; !ok {
		t.Error("missing 'build' command")
	}
	if _, ok := m.Tasks["build-exec"]; !ok {
		t.Error("missing 'build-exec' task")
	}
}

func TestLoadManifest_FileNotFound(t *testing.T) {
	_, err := LoadManifest("/nonexistent/manifest.json")
	if err == nil {
		t.Error("LoadManifest with missing file should error")
	}
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "putnami.extension.json")
	if err := os.WriteFile(manifestPath, []byte("{invalid}"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadManifest(manifestPath)
	if err == nil {
		t.Error("LoadManifest with invalid JSON should error")
	}
}

func TestResolve_BasicCommand(t *testing.T) {
	manifest := &Manifest{
		Name:    "@putnami/test",
		Version: "1.0.0",
		Commands: map[string]CommandDefinition{
			"build": {
				Description: "Build project",
				Run:         []PipelineStep{{ID: "build", Task: "build-exec"}},
			},
		},
		Tasks: map[string]TaskDefinition{
			"build-exec": {
				Kind:      "command",
				Command:   "bun",
				Args:      []string{"run", "build.ts"},
				Cwd:       "{projectRoot}",
				Batchable: &TaskBatchPolicy{Tool: "biome"},
				Resources: map[string]int{"db-connections": 230},
			},
		},
	}

	desc := Resolve(manifest, "/ext")
	if desc.Name != "@putnami/test" {
		t.Errorf("Name = %q, want %q", desc.Name, "@putnami/test")
	}
	if desc.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", desc.Version, "1.0.0")
	}
	if desc.Path != "/ext" {
		t.Errorf("Path = %q, want %q", desc.Path, "/ext")
	}

	job, ok := desc.Jobs["build"]
	if !ok {
		t.Fatal("missing 'build' job")
	}
	if job.Command != "bun" {
		t.Errorf("Command = %q, want %q", job.Command, "bun")
	}
	if job.ExtensionName != "@putnami/test" {
		t.Errorf("ExtensionName = %q, want %q", job.ExtensionName, "@putnami/test")
	}
	if job.Batchable == nil || job.Batchable.Tool != "biome" {
		t.Errorf("Batchable = %+v, want biome policy", job.Batchable)
	}
	// A single-step command resolves through Resolve rather than ExpandPipeline,
	// so the named budget claims have to be carried on this path too.
	if job.Resources["db-connections"] != 230 {
		t.Errorf("Resources = %v, want db-connections=230", job.Resources)
	}
}

func TestResolve_TwoManifestsCanContributeSameCommand(t *testing.T) {
	flag := FlagDefinition{Type: "boolean", Default: false, Description: "Show the plan without applying"}
	cloud := Resolve(&Manifest{
		Name: "@putnami/cloud",
		Commands: map[string]CommandDefinition{
			"deploy": {
				Flags: map[string]FlagDefinition{"dry-run": flag},
				Run:   []PipelineStep{{ID: "release", Task: "deploy-release"}},
			},
		},
		Tasks: map[string]TaskDefinition{
			"deploy-release": {Kind: "command", Command: "cloud"},
		},
	}, "/cloud")
	pulumi := Resolve(&Manifest{
		Name: "@putnami/pulumi",
		Commands: map[string]CommandDefinition{
			"deploy": {
				Flags: map[string]FlagDefinition{"dry-run": flag},
				Run:   []PipelineStep{{ID: "apply", Task: "deploy-apply"}},
			},
		},
		Tasks: map[string]TaskDefinition{
			"deploy-apply": {Kind: "command", Command: "pulumi"},
		},
	}, "/pulumi")

	jobs := []*JobDefinition{cloud.Jobs["deploy"], pulumi.Jobs["deploy"]}
	merged, err := MergeCommandFlags("deploy", jobs)
	if err != nil {
		t.Fatalf("MergeCommandFlags: %v", err)
	}
	if _, ok := merged["dry-run"]; !ok {
		t.Fatalf("merged flags missing dry-run: %#v", merged)
	}
}

func TestResolve_AutoServeDefault(t *testing.T) {
	manifest := &Manifest{
		Commands: map[string]CommandDefinition{},
	}

	desc := Resolve(manifest, "/ext")
	if !desc.AutoServe {
		t.Error("AutoServe should be true by default (nil AutoServe)")
	}
}

func TestResolve_AutoServeFalse(t *testing.T) {
	f := false
	manifest := &Manifest{
		AutoServe: &f,
		Commands:  map[string]CommandDefinition{},
	}

	desc := Resolve(manifest, "/ext")
	if desc.AutoServe {
		t.Error("AutoServe should be false when explicitly set to false")
	}
}

func TestExpandTemplateVars(t *testing.T) {
	vars := map[string]string{
		"workspaceRoot": "/workspace",
		"projectRoot":   "/workspace/app",
		"extensionRoot": "/workspace/node_modules/@putnami/ts",
	}

	tests := []struct {
		input string
		want  string
	}{
		{"{workspaceRoot}/out", "/workspace/out"},
		{"{projectRoot}/dist", "/workspace/app/dist"},
		{"{extensionRoot}/bin/build", "/workspace/node_modules/@putnami/ts/bin/build"},
		{"no-vars-here", "no-vars-here"},
		{"{workspaceRoot}/{projectRoot}", "/workspace//workspace/app"},
	}

	for _, tt := range tests {
		got := ExpandTemplateVars(tt.input, vars)
		if got != tt.want {
			t.Errorf("ExpandTemplateVars(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildTemplateVars(t *testing.T) {
	vars := BuildTemplateVars("/ws", "/ws/app", "/ws/ext", "/ws/out")
	if vars["workspaceRoot"] != "/ws" {
		t.Errorf("workspaceRoot = %q", vars["workspaceRoot"])
	}
	if vars["projectRoot"] != "/ws/app" {
		t.Errorf("projectRoot = %q", vars["projectRoot"])
	}
	if vars["extensionRoot"] != "/ws/ext" {
		t.Errorf("extensionRoot = %q", vars["extensionRoot"])
	}
	if vars["outputRoot"] != "/ws/out" {
		t.Errorf("outputRoot = %q", vars["outputRoot"])
	}
	if vars["cacheRoot"] == "" {
		t.Error("cacheRoot should not be empty")
	}
}

func TestExtensionDependencies_UnmarshalJSON_Array(t *testing.T) {
	var d Dependencies
	data := `["@putnami/ts", "@putnami/go"]`
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if len(d.List) != 2 {
		t.Fatalf("len = %d, want 2", len(d.List))
	}
	if _, ok := d.List["@putnami/ts"]; !ok {
		t.Error("missing @putnami/ts")
	}
	if _, ok := d.List["@putnami/go"]; !ok {
		t.Error("missing @putnami/go")
	}
	// Array values should have empty constraints
	if d.List["@putnami/ts"] != "" {
		t.Errorf("constraint = %q, want empty", d.List["@putnami/ts"])
	}
}

func TestExtensionDependencies_UnmarshalJSON_Object(t *testing.T) {
	var d Dependencies
	data := `{"@putnami/ts": "^1.0.0", "@putnami/go": ">=2.0.0"}`
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if d.List["@putnami/ts"] != "^1.0.0" {
		t.Errorf("ts constraint = %q, want %q", d.List["@putnami/ts"], "^1.0.0")
	}
	if d.List["@putnami/go"] != ">=2.0.0" {
		t.Errorf("go constraint = %q, want %q", d.List["@putnami/go"], ">=2.0.0")
	}
}

func TestExtensionDependencies_MarshalJSON(t *testing.T) {
	d := Dependencies{List: map[string]string{"@putnami/ts": "^1.0.0"}}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal roundtrip: %v", err)
	}
	if raw["@putnami/ts"] != "^1.0.0" {
		t.Errorf("roundtrip = %q, want %q", raw["@putnami/ts"], "^1.0.0")
	}
}

func TestExtensionDependencies_MarshalJSON_Nil(t *testing.T) {
	d := Dependencies{List: nil}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(data) != "null" {
		t.Errorf("MarshalJSON nil = %s, want null", data)
	}
}

func TestTaskDefinition_UnmarshalJSON_CacheBool(t *testing.T) {
	data := `{"kind":"command","command":"go","args":["build"],"cache":false}`
	var task TaskDefinition
	if err := json.Unmarshal([]byte(data), &task); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if task.Cache == nil {
		t.Fatal("Cache should not be nil")
	}
	if task.Cache.IsEnabled() {
		t.Error("Cache.IsEnabled() should be false")
	}
}

func TestTaskDefinition_UnmarshalJSON_CacheObject(t *testing.T) {
	data := `{"kind":"command","command":"go","args":["build"],"cache":{"enabled":true,"deterministic":true}}`
	var task TaskDefinition
	if err := json.Unmarshal([]byte(data), &task); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if task.Cache == nil {
		t.Fatal("Cache should not be nil")
	}
	if !task.Cache.IsEnabled() {
		t.Error("Cache.IsEnabled() should be true")
	}
	if !task.Cache.Deterministic {
		t.Error("Deterministic should be true")
	}
}

func TestTaskCachePolicy_IsEnabled(t *testing.T) {
	// nil policy → enabled by default
	var p *TaskCachePolicy
	if !p.IsEnabled() {
		t.Error("nil policy should be enabled")
	}

	// Enabled explicitly true
	tr := true
	p = &TaskCachePolicy{Enabled: &tr}
	if !p.IsEnabled() {
		t.Error("explicitly enabled should be enabled")
	}

	// Enabled explicitly false
	f := false
	p = &TaskCachePolicy{Enabled: &f}
	if p.IsEnabled() {
		t.Error("explicitly disabled should be disabled")
	}

	// Enabled field nil → default true
	p = &TaskCachePolicy{}
	if !p.IsEnabled() {
		t.Error("nil Enabled should default to enabled")
	}
}

func TestInputBinding_UnmarshalJSON_Value(t *testing.T) {
	data := `{"value": "hello"}`
	var b InputBinding
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if !b.HasValue {
		t.Error("HasValue should be true")
	}
}

func TestInputBinding_UnmarshalJSON_From(t *testing.T) {
	data := `{"from": "command", "path": "params.target"}`
	var b InputBinding
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if b.From != "command" {
		t.Errorf("From = %q, want %q", b.From, "command")
	}
	if b.Path != "params.target" {
		t.Errorf("Path = %q, want %q", b.Path, "params.target")
	}
}

func TestInputBinding_UnmarshalJSON_FromStepPathRejected(t *testing.T) {
	data := `{"fromStep": "build", "path": "data.binaryPath"}`
	var b InputBinding
	if err := json.Unmarshal([]byte(data), &b); err == nil {
		t.Fatal("fromStep input binding accepted removed JSONPath selector")
	}
}

func TestInputBinding_UnmarshalJSON_FromStepOutput(t *testing.T) {
	data := `{"fromStep": "build", "output": "binary"}`
	var b InputBinding
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if b.Output != "binary" {
		t.Errorf("Output = %q, want %q", b.Output, "binary")
	}
}

func TestDeriveTaskCacheKey(t *testing.T) {
	inputs := map[string]TaskInputPort{
		"sources":    {From: "project", Files: []string{"**/*.go", "go.mod"}},
		"workspace":  {From: "workspace", Files: []string{"go.work", "go.work.sum"}},
		"target":     {From: "params"},
		"go_version": {From: "env"},
		"platform":   {From: "runtime"},
		"upstream":   {From: "task"},
	}

	key := DeriveTaskCacheKey(inputs)
	if len(key.Files) != 2 {
		t.Errorf("Files len = %d, want 2", len(key.Files))
	}
	if len(key.WorkspaceFiles) != 2 || key.WorkspaceFiles[0] != "go.work" || key.WorkspaceFiles[1] != "go.work.sum" {
		t.Errorf("WorkspaceFiles = %v, want [go.work go.work.sum]", key.WorkspaceFiles)
	}
	if len(key.Params) != 1 || key.Params[0] != "target" {
		t.Errorf("Params = %v, want [target]", key.Params)
	}
	if len(key.Env) != 1 || key.Env[0] != "go_version" {
		t.Errorf("Env = %v, want [go_version]", key.Env)
	}
	if len(key.Runtime) != 1 || key.Runtime[0] != "platform" {
		t.Errorf("Runtime = %v, want [platform]", key.Runtime)
	}
}

// TestResolveCopiesEcosystemProfiles pins that the resolved description carries
// the manifest's ecosystem declarations through. It is the only path by which
// the CLI learns an ecosystem exists — it names none itself — so a profile
// dropped here would leave the ecosystem invisible to release-set assembly with
// no error anywhere.
func TestResolveCopiesEcosystemProfiles(t *testing.T) {
	manifest, diags := proto.ParseManifest([]byte(`{
		"name": "@putnami/typescript",
		"ecosystems": [{
			"id": "npm",
			"coordinate": {"pattern": "^(@[a-z0-9-]+/)?[a-z0-9-]+$"},
			"version": {"pattern": "^\\d+\\.\\d+\\.\\d+$", "ordering": "semver"},
			"channel": "native",
			"registries": {"type": "object"},
			"publish": "publish-npm"
		}],
		"uses": ["oci"],
		"commands": {"publish-npm": {"run": [{"id": "publish", "task": "publish-exec"}]}},
		"tasks": {"publish-exec": {"kind": "command", "command": "echo"}}
	}`))
	if len(diags) != 0 {
		t.Fatalf("parse: %v", diags)
	}

	desc := Resolve(manifest, "/ext")
	if len(desc.Ecosystems) != 1 {
		t.Fatalf("Ecosystems = %v, want one profile", desc.Ecosystems)
	}
	profile := desc.Ecosystems[0]
	if profile.ID != "npm" || profile.Channel != proto.ChannelNative || profile.Publish != "publish-npm" {
		t.Errorf("profile = %+v, want the manifest's npm profile", profile)
	}
	if profile.Version.Ordering != proto.OrderingSemver {
		t.Errorf("Version.Ordering = %q, want %q", profile.Version.Ordering, proto.OrderingSemver)
	}
	if len(desc.Uses) != 1 || desc.Uses[0] != "oci" {
		t.Errorf("Uses = %v, want [oci]", desc.Uses)
	}

	// Control: a manifest without profiles resolves to nothing rather than to
	// an empty-but-present declaration.
	bare, diags := proto.ParseManifest([]byte(`{
		"commands": {"build": {"run": [{"id": "build", "task": "build-exec"}]}},
		"tasks": {"build-exec": {"kind": "command", "command": "echo"}}
	}`))
	if len(diags) != 0 {
		t.Fatalf("parse bare: %v", diags)
	}
	if got := Resolve(bare, "/ext"); got.Ecosystems != nil || got.Uses != nil {
		t.Errorf("bare manifest resolved to %v / %v, want nil / nil", got.Ecosystems, got.Uses)
	}
}
