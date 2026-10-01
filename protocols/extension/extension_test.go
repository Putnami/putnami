package extension

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

func TestLoadManifest_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)

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
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "@putnami/test" {
		t.Errorf("Name = %q, want @putnami/test", m.Name)
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
		t.Error("expected error for missing file")
	}
}

func TestLoadManifest_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)
	os.WriteFile(path, []byte("{invalid}"), 0o644)

	_, err := LoadManifest(path)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// shadowManifest declares reserved-global shadows on every flag surface:
// a command flag name ("json"), a command flag short form ("-v"), a group
// shared flag ("output"), a subcommand flag negated form ("no-color"), and a
// nested subcommand flag ("quiet").
const shadowManifest = `{
	"name": "@putnami/test",
	%s
	"commands": {
		"test": {
			"flags": {
				"json": { "type": "boolean" },
				"details": { "type": "boolean", "short": "-v" },
				"target": { "type": "string" }
			},
			"run": [{"id": "test", "task": "test-exec"}]
		}
	},
	"commandGroups": {
		"cloud": {
			"flags": {
				"output": { "type": "string" },
				"env": { "type": "string" }
			},
			"subcommands": {
				"login": {
					"command": "test",
					"flags": {
						"no-color": { "type": "boolean" }
					},
					"subcommands": {
						"sso": {
							"flags": {
								"quiet": { "type": "boolean" },
								"region": { "type": "string" }
							}
						}
					}
				}
			}
		}
	},
	"tasks": {
		"test-exec": { "kind": "command", "command": "echo" }
	}
}`

func writeShadowManifest(t *testing.T, contractField string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)
	if err := os.WriteFile(path, []byte(fmt.Sprintf(shadowManifest, contractField)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadManifest_EqualContract_EnforcesReservedShadows pins the ENFORCE arm:
// a manifest that claims the current contract but shadows reserved globals is
// a hard reject, exactly as before the reserved-flag registry landed.
func TestLoadManifest_EqualContract_EnforcesReservedShadows(t *testing.T) {
	path := writeShadowManifest(t, fmt.Sprintf(`"cliContract": %d,`, protocolcli.CurrentContract))

	_, err := LoadManifest(path)
	if err == nil {
		t.Fatal("expected reserved global flag error for a current-contract manifest")
	}
	for _, want := range []string{"commands.test.flags.details.short", "commands.test.flags.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestLoadManifest_NewerContract_FailsLoud pins the REJECT arm: a manifest
// from the future is never half-interpreted. The future starts above the
// latest contract this CLI reads, not above its base contract.
func TestLoadManifest_NewerContract_FailsLoud(t *testing.T) {
	path := writeShadowManifest(t, fmt.Sprintf(`"cliContract": %d,`, protocolcli.LatestContract+1))

	_, err := LoadManifest(path)
	if err == nil {
		t.Fatal("expected contract-too-new error")
	}
	want := fmt.Sprintf("requires a newer putnami (contract %d > %d)", protocolcli.LatestContract+1, protocolcli.LatestContract)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error missing %q: %v", want, err)
	}
}

// TestLoadManifest_OlderContract_IsAHardError converts B0b's tolerate-v2 loader
// test for contract 3. It used to pin the ADAPT arm: a manifest without
// a cliContract field loaded with every shadowed reserved flag dropped. There is
// no adapt arm any more — a manifest that declares a contract surface must
// declare THIS contract — so the same fixture now pins the rejection, and the
// message must name both numbers and the command that fixes it.
func TestLoadManifest_OlderContract_IsAHardError(t *testing.T) {
	for _, contract := range []struct {
		name  string
		field string
		found int
	}{
		{"unstamped", "", 0},
		{"one behind", fmt.Sprintf(`"cliContract": %d,`, protocolcli.CurrentContract-1), protocolcli.CurrentContract - 1},
	} {
		t.Run(contract.name, func(t *testing.T) {
			m, err := LoadManifest(writeShadowManifest(t, contract.field))
			if err == nil {
				t.Fatal("a manifest below the current CLI contract must not load")
			}
			if m != nil {
				t.Error("a rejected manifest must not be returned; a caller could use it")
			}
			for _, want := range []string{
				fmt.Sprintf("declares CLI contract %d", contract.found),
				fmt.Sprintf("requires %d", protocolcli.CurrentContract),
				"re-package the extension",
				"putnami extensions update",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
		})
	}
}

// TestLoadManifest_HookOnlyManifestIsOutsideTheLadder pins the one exemption.
// The package-time gate deliberately leaves a manifest with no contract surface
// UNSTAMPED (gateAndStampManifestContract), so requiring a stamp from it would
// reject exactly the manifests the packager refuses to stamp: every hook-only
// framework package would vanish from the workspace.
func TestLoadManifest_HookOnlyManifestIsOutsideTheLadder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFilename)
	hookOnly := `{
		"extensionDependencies": ["@putnami/application"],
		"hooks": {
			"preBuild": {"kind": "command", "command": "bun", "args": ["run", "generate"]}
		},
		"commands": {}
	}`
	if err := os.WriteFile(path, []byte(hookOnly), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("a hook-only manifest must still load: %v", err)
	}
	if m == nil || m.Hooks == nil || m.Hooks.PreBuild == nil {
		t.Fatalf("hook-only manifest lost its hook: %+v", m)
	}
	if DeclaresContractSurface(m) {
		t.Error("a manifest with no commands, groups or tools declares no contract surface")
	}
}

// TestDeclaresContractSurface pins the predicate the loader shares with the
// package-time stamper: anything the contract governs makes a manifest subject
// to it, and nothing else does.
func TestDeclaresContractSurface(t *testing.T) {
	cases := []struct {
		name string
		m    *Manifest
		want bool
	}{
		{"nil", nil, false},
		{"empty", &Manifest{}, false},
		{"empty commands", &Manifest{Commands: map[string]CommandDefinition{}}, false},
		{"hooks only", &Manifest{Hooks: &ManifestHooks{}}, false},
		{"a command", &Manifest{Commands: map[string]CommandDefinition{"build": {}}}, true},
		{"a command group", &Manifest{CommandGroups: map[string]CommandGroupDefinition{"cloud": {}}}, true},
		{"a tool", &Manifest{Tools: map[string]ToolDefinition{"x.y": {}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeclaresContractSurface(tc.m); got != tc.want {
				t.Errorf("DeclaresContractSurface = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtensionDependencies_UnmarshalJSON_Array(t *testing.T) {
	var d Dependencies
	if err := json.Unmarshal([]byte(`["@putnami/ts", "@putnami/go"]`), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.List) != 2 {
		t.Fatalf("len = %d, want 2", len(d.List))
	}
	if _, ok := d.List["@putnami/ts"]; !ok {
		t.Error("missing @putnami/ts")
	}
	if d.List["@putnami/ts"] != "" {
		t.Errorf("constraint = %q, want empty", d.List["@putnami/ts"])
	}
}

func TestExtensionDependencies_UnmarshalJSON_Object(t *testing.T) {
	var d Dependencies
	if err := json.Unmarshal([]byte(`{"@putnami/ts": "^1.0.0"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.List["@putnami/ts"] != "^1.0.0" {
		t.Errorf("constraint = %q, want ^1.0.0", d.List["@putnami/ts"])
	}
}

func TestExtensionDependencies_MarshalJSON(t *testing.T) {
	d := Dependencies{List: map[string]string{"@putnami/ts": "^1.0.0"}}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["@putnami/ts"] != "^1.0.0" {
		t.Errorf("roundtrip = %q", raw["@putnami/ts"])
	}
}

func TestExtensionDependencies_MarshalJSON_Nil(t *testing.T) {
	d := Dependencies{List: nil}
	data, _ := json.Marshal(d)
	if string(data) != "null" {
		t.Errorf("got %s, want null", data)
	}
}

func TestTaskDefinition_UnmarshalJSON_CacheBool(t *testing.T) {
	var task TaskDefinition
	if err := json.Unmarshal([]byte(`{"kind":"command","command":"go","args":["build"],"cache":false}`), &task); err != nil {
		t.Fatal(err)
	}
	if task.Cache == nil {
		t.Fatal("Cache should not be nil")
	}
	if task.Cache.IsEnabled() {
		t.Error("Cache.IsEnabled() should be false")
	}
}

func TestTaskDefinition_UnmarshalJSON_CacheObject(t *testing.T) {
	var task TaskDefinition
	if err := json.Unmarshal([]byte(`{"kind":"command","command":"go","args":["build"],"cache":{"enabled":true,"deterministic":true}}`), &task); err != nil {
		t.Fatal(err)
	}
	if !task.Cache.IsEnabled() {
		t.Error("Cache.IsEnabled() should be true")
	}
	if !task.Cache.Deterministic {
		t.Error("Deterministic should be true")
	}
}

func TestTaskCachePolicy_IsEnabled(t *testing.T) {
	var p *TaskCachePolicy
	if !p.IsEnabled() {
		t.Error("nil policy should be enabled")
	}

	tr := true
	p = &TaskCachePolicy{Enabled: &tr}
	if !p.IsEnabled() {
		t.Error("explicitly enabled should be enabled")
	}

	f := false
	p = &TaskCachePolicy{Enabled: &f}
	if p.IsEnabled() {
		t.Error("explicitly disabled should be disabled")
	}

	p = &TaskCachePolicy{}
	if !p.IsEnabled() {
		t.Error("nil Enabled should default to enabled")
	}
}

func TestInputBinding_UnmarshalJSON_Value(t *testing.T) {
	var b InputBinding
	if err := json.Unmarshal([]byte(`{"value": "hello"}`), &b); err != nil {
		t.Fatal(err)
	}
	if !b.HasValue {
		t.Error("HasValue should be true")
	}
}

func TestInputBinding_UnmarshalJSON_FromStep(t *testing.T) {
	var b InputBinding
	if err := json.Unmarshal([]byte(`{"fromStep": "build", "output": "binary"}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.FromStep != "build" {
		t.Errorf("FromStep = %q, want build", b.FromStep)
	}
	if b.Output != "binary" {
		t.Errorf("Output = %q, want binary", b.Output)
	}
}

func TestInputBinding_UnmarshalJSON_FromStepPathRejected(t *testing.T) {
	var b InputBinding
	if err := json.Unmarshal([]byte(`{"fromStep":"build","path":"data.binaryPath"}`), &b); err == nil {
		t.Fatal("fromStep input binding accepted removed JSONPath selector")
	}
}

func TestInputBinding_UnmarshalJSON_From(t *testing.T) {
	var b InputBinding
	if err := json.Unmarshal([]byte(`{"from": "command", "path": "params.target"}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.From != "command" {
		t.Errorf("From = %q, want command", b.From)
	}
	if b.Path != "params.target" {
		t.Errorf("Path = %q, want params.target", b.Path)
	}
}

// TestStepRunOn pins the run-condition vocabulary and its default. The default
// is what keeps the field additive: every step written before runOn existed
// runs on success, exactly as it did.
func TestStepRunOn(t *testing.T) {
	step := PipelineStep{ID: "build", Task: "build-exec"}
	if got := step.EffectiveRunOn(); got != StepRunOnSuccess {
		t.Errorf("absent runOn = %q, want %q", got, StepRunOnSuccess)
	}
	if step.IsFinalizer() {
		t.Error("a step without runOn is not a finalizer")
	}

	step.RunOn = StepRunOnFinally
	if got := step.EffectiveRunOn(); got != StepRunOnFinally {
		t.Errorf("runOn = %q, want %q", got, StepRunOnFinally)
	}
	if !step.IsFinalizer() {
		t.Error("a runOn=finally step is a finalizer")
	}

	for _, value := range ValidStepRunOn {
		if !IsValidStepRunOn(value) {
			t.Errorf("IsValidStepRunOn(%q) = false for a member of the vocabulary", value)
		}
	}
	for _, value := range []string{"", "failure", "always", "Success"} {
		if IsValidStepRunOn(value) {
			t.Errorf("IsValidStepRunOn(%q) = true outside the closed vocabulary", value)
		}
	}
	for i := 1; i < len(ValidStepRunOn); i++ {
		if ValidStepRunOn[i-1] >= ValidStepRunOn[i] {
			t.Errorf("ValidStepRunOn is not in canonical (sorted) order: %v", ValidStepRunOn)
		}
	}
}

// TestPipelineStepHasOneRuntimeGate is the deletion ratchet for `when`. Epic
// contract v3 removed it — a runtime expression over step results, with zero
// consumers and zero manifest usage — leaving runOn as the single field that
// decides what happens to a PLANNED step. Re-adding a second one is a design
// decision, not a convenience, and this test is where it has to be argued.
func TestPipelineStepHasOneRuntimeGate(t *testing.T) {
	for _, field := range goTypeJSONFields(t, PipelineStep{}) {
		if field == "when" {
			t.Fatal(`PipelineStep declares "when" again; runOn is the single step-gating runtime field`)
		}
	}

	// A manifest that still spells `when` must be REJECTED rather than
	// silently ignored: silence would let an extension believe a gate is
	// active while every step runs unconditionally.
	_, diags := ParseManifest([]byte(`{
		"commands": {"test": {"run": [{"id": "run", "task": "t", "when": "steps.build.status == 'success'"}]}},
		"tasks": {"t": {"kind": "command", "command": "echo"}}
	}`))
	if !diag.HasErrors(diags) {
		t.Fatal("a manifest using the deleted `when` field must fail strict parsing")
	}
}

func TestExpandTemplateVars(t *testing.T) {
	vars := map[string]string{
		"workspaceRoot": "/workspace",
		"projectRoot":   "/workspace/app",
		"extensionRoot": "/workspace/ext",
	}

	got := ExpandTemplateVars("{workspaceRoot}/out", vars)
	if got != "/workspace/out" {
		t.Errorf("got %q, want /workspace/out", got)
	}

	got = ExpandTemplateVars("no-vars", vars)
	if got != "no-vars" {
		t.Errorf("got %q, want no-vars", got)
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
	for _, contextSpecific := range []string{
		TemplateVarExtensionRuntime,
		TemplateVarRuntimeOutput,
		TemplateVarInvocationArtifactRoot,
	} {
		if _, ok := vars[contextSpecific]; ok {
			t.Errorf("generic template variables unexpectedly include context-specific %q", contextSpecific)
		}
	}

	invocationVars := map[string]string{
		TemplateVarInvocationArtifactRoot: "/private/inv-123/artifacts",
	}
	if got := ExpandTemplateVars("{invocationArtifactRoot}/database/lease.json", invocationVars); got != "/private/inv-123/artifacts/database/lease.json" {
		t.Errorf("invocation artifact root expansion = %q", got)
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

// TestDeriveTaskCacheKey_ClosurePortIsItsOwnBucket pins the `closure` input
// source.
//
// A closure port must NOT collapse into Files: those are hashed against the
// task's own project root, and a task that reads its dependencies' committed
// manifests would then be keyed on a strict subset of what it read — the hole
// that let a requirements-only change in a dependency leave a test verdict's
// cache key unmoved.
func TestDeriveTaskCacheKey_ClosurePortIsItsOwnBucket(t *testing.T) {
	key := DeriveTaskCacheKey(map[string]TaskInputPort{
		"sources":      {From: TaskInputFromProject, Files: []string{"**/*.go"}},
		"requirements": {From: TaskInputFromClosure, Files: []string{"infra/requirements.json"}},
	})
	if len(key.ClosureFiles) != 1 || key.ClosureFiles[0] != "infra/requirements.json" {
		t.Errorf("ClosureFiles = %v, want [infra/requirements.json]", key.ClosureFiles)
	}
	for _, own := range key.Files {
		if own == "infra/requirements.json" {
			t.Error("a closure port leaked into the project-local Files bucket")
		}
	}
	if len(key.WorkspaceFiles) != 0 {
		t.Errorf("WorkspaceFiles = %v, want none", key.WorkspaceFiles)
	}
}

// TestValidateManifest_AcceptsAClosureInputPort pins that `closure` is part of
// the closed input vocabulary the strict validator enforces — the manifests
// that declare it must load through the same gate every other port does.
func TestValidateManifest_AcceptsAClosureInputPort(t *testing.T) {
	manifest := &Manifest{
		Name:    "@acme/provider",
		Version: "1.0.0",
		Commands: map[string]CommandDefinition{
			"test": {Run: []PipelineStep{{ID: "env", Task: "env-up"}}},
		},
		Tasks: map[string]TaskDefinition{
			"env-up": {
				Kind:    "command",
				Command: "provider",
				Inputs: map[string]TaskInputPort{
					"requirements": {From: TaskInputFromClosure, Files: []string{"infra/requirements.json"}},
				},
			},
		},
	}
	invalidSource := func(diags []diag.Diagnostic) *diag.Diagnostic {
		for i := range diags {
			if diags[i].Severity == diag.Error && strings.Contains(diags[i].Message, "invalid input source") {
				return &diags[i]
			}
		}
		return nil
	}
	if rejected := invalidSource(ValidateManifest(manifest)); rejected != nil {
		t.Fatalf("the closure input source was rejected: %s", rejected.Message)
	}

	manifest.Tasks["env-up"].Inputs["requirements"] = TaskInputPort{From: "galaxy"}
	rejected := invalidSource(ValidateManifest(manifest))
	if rejected == nil {
		t.Fatal("an unknown input source was accepted")
	}
	if !strings.Contains(rejected.Message, "closure") {
		t.Errorf("the rejection message does not name the closure source: %s", rejected.Message)
	}
}

// TestTaskInputPort_RequiresProducer pins the typed producer requirement that
// replaced the config-schema preflight trait. Only a
// `from: "task"` port states a producer dependency at all, and `optional` is
// the sole way to soften it: a file-shaped input is not a producer edge, and a
// consumer that can handle absence says so instead of core guessing from the
// workload's sources.
func TestTaskInputPort_RequiresProducer(t *testing.T) {
	cases := []struct {
		name string
		port TaskInputPort
		want bool
	}{
		{"required task port", TaskInputPort{From: TaskInputFromTask}, true},
		{"optional task port", TaskInputPort{From: TaskInputFromTask, Optional: true}, false},
		{"project files", TaskInputPort{From: TaskInputFromProject, Files: []string{"**/*.go"}}, false},
		{"workspace files", TaskInputPort{From: TaskInputFromWorkspace, Files: []string{"go.work"}}, false},
		{"params", TaskInputPort{From: TaskInputFromParams}, false},
		{"env", TaskInputPort{From: TaskInputFromEnv}, false},
		{"runtime", TaskInputPort{From: TaskInputFromRuntime}, false},
		// `optional` is documented as meaningful only for task ports; a
		// non-task port must never become a producer edge by carrying it.
		{"optional project files", TaskInputPort{From: TaskInputFromProject, Optional: true}, false},
		{"unset source", TaskInputPort{}, false},
	}
	for _, c := range cases {
		if got := c.port.RequiresProducer(); got != c.want {
			t.Errorf("%s: RequiresProducer() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestTaskInputSourceConstants pins the wire spellings the manifest schema's
// `from` enum lists. A drift between the constant and the schema silently
// turns a declared port into an unrecognized one.
func TestTaskInputSourceConstants(t *testing.T) {
	want := map[string]string{
		TaskInputFromProject:   "project",
		TaskInputFromWorkspace: "workspace",
		TaskInputFromTask:      "task",
		TaskInputFromParams:    "params",
		TaskInputFromEnv:       "env",
		TaskInputFromRuntime:   "runtime",
	}
	if len(want) != 6 {
		t.Fatalf("task input sources = %d distinct spellings, want 6", len(want))
	}
	for got, expected := range want {
		if got != expected {
			t.Errorf("task input source constant = %q, want %q", got, expected)
		}
	}
}
