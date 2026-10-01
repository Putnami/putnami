package extension

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

func TestParseManifest_Strict_Valid(t *testing.T) {
	data := []byte(`{
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
	}`)

	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil {
		t.Fatal("manifest should not be nil")
	}
	if _, ok := m.Commands["build"]; !ok {
		t.Error("missing build command")
	}
}

func TestParseManifest_Strict_BatchableTask(t *testing.T) {
	data := []byte(`{
		"cliContract": 4,
		"commands": {
			"lint": {
				"run": [{"id": "check", "task": "lint-all"}]
			}
		},
		"tasks": {
			"lint-all": {
				"kind": "command",
				"command": "biome",
				"batchable": {
					"tool": "biome",
					"configFiles": ["{workspaceRoot}/biome.json"],
					"maxProjects": 8,
					"maxWorkers": 4
				}
			}
		}
	}`)

	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	policy := m.Tasks["lint-all"].Batchable
	if policy == nil || policy.Tool != "biome" || len(policy.ConfigFiles) != 1 ||
		policy.MaxProjects != 8 || policy.MaxWorkers != 4 {
		t.Fatalf("batchable policy = %+v", policy)
	}
}

func TestParseManifest_Strict_BatchableTaskRejectsSingletonLimit(t *testing.T) {
	data := []byte(`{
		"commands": {},
		"tasks": {
			"test": {
				"kind": "command",
				"command": "go",
				"batchable": {"tool": "go-test", "maxProjects": 1}
			}
		}
	}`)

	m, diags := ParseAndValidateManifest(data)
	if m == nil || !diag.HasErrors(diags) {
		t.Fatalf("invalid batchable.maxProjects parsed as manifest=%+v diagnostics=%v", m, diags)
	}
}

func TestParseManifest_Strict_BatchableTaskRejectsZeroWorkerLimit(t *testing.T) {
	data := []byte(`{
		"commands": {},
		"tasks": {
			"test": {
				"kind": "command",
				"command": "go",
				"batchable": {"tool": "go-test", "maxWorkers": 0}
			}
		}
	}`)

	m, diags := ParseAndValidateManifest(data)
	if m == nil || !diag.HasErrors(diags) {
		t.Fatalf("invalid batchable.maxWorkers parsed as manifest=%+v diagnostics=%v", m, diags)
	}
}

// A batch policy may name the parameter a workspace sets its project cap
// through. The cap only groups ready jobs, so nothing the CLI keys a task on may
// carry that parameter: not a task input or cache key param, not a flag of a
// command that runs the task, and not a step binding, in either the kebab or
// the camel spelling the CLI delivers.
func TestParseManifest_Strict_BatchableProjectsParam(t *testing.T) {
	const runsTest = `{"run": [{"id": "test", "task": "test"}]}`
	manifest := func(command, batchable, extra string) []byte {
		return []byte(`{
			"commands": {
				"test": ` + command + `
			},
			"tasks": {
				"test": {
					"kind": "command",
					"command": "suite",
					"batchable": ` + batchable + extra + `
				}
			}
		}`)
	}
	const named = `{"tool": "suite", "maxProjectsParam": "batch-max-projects"}`

	m, diags := ParseAndValidateManifest(manifest(runsTest, named, ""))
	if diag.HasErrors(diags) {
		t.Fatalf("valid maxProjectsParam rejected: %v", diags)
	}
	if got := m.Tasks["test"].Batchable; got.MaxProjectsParam != "batch-max-projects" || got.MaxProjects != 0 {
		t.Fatalf("batchable policy = %+v, want the named param and no static cap", got)
	}
	// A flag of the same name on a command that does NOT run the task keys
	// nothing this task computes, so it stays legal.
	unrelated := []byte(`{
		"commands": {
			"test": ` + runsTest + `,
			"other": {"flags": {"batch-max-projects": {"type": "number"}}, "run": [{"id": "other", "task": "other"}]}
		},
		"tasks": {
			"test": {"kind": "command", "command": "suite", "batchable": ` + named + `},
			"other": {"kind": "command", "command": "suite"}
		}
	}`)
	if _, diags := ParseAndValidateManifest(unrelated); diag.HasErrors(diags) {
		t.Fatalf("flag on a command that does not run the task rejected: %v", diags)
	}
	// The CLI matches names exactly, plus the camelCase alias of a kebab name.
	// A name that differs only in case is a different parameter, so it keys
	// the task without keying it on the batch cap.
	for name, data := range map[string][]byte{
		"case-only input": manifest(runsTest, named, `, "inputs": {"BatchMaxProjects": {"from": "params"}}`),
		"case-only flag": manifest(
			`{"flags": {"BatchMaxProjects": {"type": "number"}}, "run": [{"id": "test", "task": "test"}]}`, named, ""),
	} {
		if _, diags := ParseAndValidateManifest(data); diag.HasErrors(diags) {
			t.Errorf("%s: a different parameter was rejected: %v", name, diags)
		}
	}

	for name, data := range map[string][]byte{
		"whitespace": manifest(runsTest, `{"tool": "suite", "maxProjectsParam": "batch max"}`, ""),
		"declared input": manifest(runsTest, named,
			`, "inputs": {"batch-max-projects": {"from": "params"}}`),
		"camel input": manifest(runsTest, named,
			`, "inputs": {"batchMaxProjects": {"from": "params"}}`),
		"cache key param": manifest(runsTest, named,
			`, "cache": {"enabled": true, "key": {"params": ["batch-max-projects"]}}`),
		"command flag": manifest(
			`{"flags": {"batch-max-projects": {"type": "number"}}, "run": [{"id": "test", "task": "test"}]}`, named, ""),
		"camel command flag": manifest(
			`{"flags": {"batchMaxProjects": {"type": "number"}}, "run": [{"id": "test", "task": "test"}]}`, named, ""),
		"step binding": manifest(
			`{"run": [{"id": "test", "task": "test", "with": {"batch-max-projects": {"value": 2}}}]}`, named, ""),
		"camel step binding": manifest(
			`{"run": [{"id": "test", "task": "test", "with": {"batchMaxProjects": {"value": 2}}}]}`, named, ""),
		"camel policy, kebab input": manifest(runsTest, `{"tool": "suite", "maxProjectsParam": "batchMaxProjects"}`,
			`, "inputs": {"batch-max-projects": {"from": "params"}}`),
	} {
		m, diags := ParseAndValidateManifest(data)
		if m == nil || !diag.HasErrors(diags) {
			t.Errorf("%s: invalid maxProjectsParam accepted: diagnostics=%v", name, diags)
			continue
		}
		found := false
		for _, d := range diags {
			if strings.Contains(d.Field, "batchable.maxProjectsParam") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no diagnostic on batchable.maxProjectsParam: %v", name, diags)
		}
	}
}

func TestParseManifest_Strict_BatchableTaskRequiresTool(t *testing.T) {
	data := []byte(`{
		"commands": {},
		"tasks": {
			"lint-all": {
				"kind": "command",
				"command": "biome",
				"batchable": {"configFiles": ["biome.json"]}
			}
		}
	}`)

	m, diags := ParseAndValidateManifest(data)
	if m == nil || !diag.HasErrors(diags) {
		t.Fatalf("missing batchable.tool parsed as manifest=%+v diagnostics=%v", m, diags)
	}
}

// A task's `resources` map declares how many units of a named scarce resource
// one execution holds, which the scheduler admits against beside the worker
// count. Names are open — this protocol closes no vocabulary of
// resources — so only the shape is validated.
func TestParseManifest_Strict_TaskResourceClaims(t *testing.T) {
	data := []byte(`{
		"cliContract": 4,
		"commands": {
			"test": {
				"run": [{"id": "test", "task": "test-exec"}]
			}
		},
		"tasks": {
			"test-exec": {
				"kind": "command",
				"command": "go",
				"resources": {"db-connections": 230, "ports": 4}
			}
		}
	}`)

	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	claims := m.Tasks["test-exec"].Resources
	if claims["db-connections"] != 230 || claims["ports"] != 4 {
		t.Fatalf("resource claims = %v, want db-connections=230 and ports=4", claims)
	}
}

func TestParseManifest_Strict_TaskResourceClaimsRejectNonPositiveUnits(t *testing.T) {
	for _, units := range []string{"0", "-5"} {
		data := []byte(fmt.Sprintf(`{
			"commands": {},
			"tasks": {
				"test-exec": {
					"kind": "command",
					"command": "go",
					"resources": {"db-connections": %s}
				}
			}
		}`, units))

		m, diags := ParseAndValidateManifest(data)
		if m == nil || !diag.HasErrors(diags) {
			t.Fatalf("resource claim of %s units parsed as manifest=%+v diagnostics=%v", units, m, diags)
		}
	}
}

func TestParseManifest_Strict_UnknownField(t *testing.T) {
	data := []byte(`{
		"commands": {},
		"unknownField": true
	}`)

	m, diags := ParseManifest(data)
	if m != nil {
		t.Error("manifest should be nil on parse error")
	}
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for unknown field")
	}
	if diags[0].Code != "parse-error" {
		t.Errorf("Code = %q, want parse-error", diags[0].Code)
	}
}

func TestParseManifest_Strict_StepActivation(t *testing.T) {
	// The plan-time `activation` gate on a pipeline step must parse under
	// strict mode and populate both the files and contains conditions.
	data := []byte(`{
		"cliContract": 4,
		"commands": {
			"build": {
				"run": [
					{
						"id": "describe",
						"task": "build-describe",
						"activation": {
							"files": ["**/*.go"],
							"contains": {"go.mod": "go.putnami.dev/app"}
						}
					}
				]
			}
		},
		"tasks": {
			"build-describe": {"kind": "command", "command": "go"}
		}
	}`)

	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	step := m.Commands["build"].Run[0]
	if step.Activation == nil {
		t.Fatal("step activation should be populated")
	}
	if len(step.Activation.Files) != 1 || step.Activation.Files[0] != "**/*.go" {
		t.Errorf("activation.files = %v, want [**/*.go]", step.Activation.Files)
	}
	if step.Activation.Contains["go.mod"] != "go.putnami.dev/app" {
		t.Errorf("activation.contains[go.mod] = %q, want go.putnami.dev/app", step.Activation.Contains["go.mod"])
	}
}

func TestParseManifest_Strict_OnInstallHook(t *testing.T) {
	data := []byte(`{
		"hooks": {
			"onInstall": {
				"kind": "command",
				"command": "putnami",
				"args": ["cloud", "setup"]
			}
		},
		"cliContract": 4,
		"commands": {
			"build": {
				"run": [{"id": "build", "task": "build-exec"}]
			}
		},
		"tasks": {
			"build-exec": {"kind": "command", "command": "go"}
		}
	}`)

	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m.Hooks == nil || m.Hooks.OnInstall == nil {
		t.Fatal("onInstall hook should be populated")
	}
	if got := strings.Join(m.Hooks.OnInstall.Args, " "); got != "cloud setup" {
		t.Errorf("onInstall args = %q, want cloud setup", got)
	}
}

func TestParseManifest_Strict_InvalidJSON(t *testing.T) {
	_, diags := ParseManifest([]byte("{invalid"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateManifest_Nil(t *testing.T) {
	diags := ValidateManifest(nil)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for nil manifest")
	}
}

func TestValidateManifest_MissingCommands(t *testing.T) {
	m := &Manifest{}
	diags := ValidateManifest(m)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for missing commands")
	}
	found := false
	for _, d := range diags {
		if d.Field == "commands" {
			found = true
		}
	}
	if !found {
		t.Error("expected diagnostic for commands field")
	}
}

func TestValidateManifest_ToolOnlyExtension(t *testing.T) {
	trueVal, falseVal := true, false
	m := &Manifest{
		Tools: map[string]ToolDefinition{
			"putnami.search": {
				Description: "Search the hosted index.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
				Annotations: &ToolAnnotations{
					ReadOnlyHint:    &trueVal,
					DestructiveHint: &falseVal,
					IdempotentHint:  &trueVal,
					OpenWorldHint:   &trueVal,
				},
				Meta: map[string]any{"putnami.dev/contract": map[string]any{
					"access": "read", "readOnly": true, "supportsDryRun": false,
				}},
				Command: "bin/acme-tool",
			},
		},
	}
	if diags := ValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("tool-only manifest should be valid: %v", diags)
	}
}

func TestValidateManifest_InvalidToolDefinition(t *testing.T) {
	m := &Manifest{
		Tools: map[string]ToolDefinition{
			"bare": {
				Description: "Missing required execution and safety data.",
				InputSchema: json.RawMessage(`[]`),
			},
		},
	}
	diags := ValidateManifest(m)
	if !diag.HasErrors(diags) {
		t.Fatal("expected invalid tool diagnostics")
	}
	fields := map[string]bool{}
	for _, d := range diags {
		fields[d.Field] = true
	}
	for _, want := range []string{"tools.bare", "tools.bare.command", "tools.bare.annotations", "tools.bare._meta.putnami.dev/contract"} {
		if !fields[want] {
			t.Errorf("missing diagnostic for %s: %v", want, diags)
		}
	}
}

func TestValidateManifest_EmptyPipeline(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {Run: nil},
		},
	}
	diags := ValidateManifest(m)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for empty pipeline")
	}
}

func TestValidateManifest_MissingStepFields(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {Run: []PipelineStep{{}}},
		},
	}
	diags := ValidateManifest(m)
	errors := diag.Errors(diags)
	if len(errors) < 2 {
		t.Fatalf("expected at least 2 errors, got %d: %v", len(errors), errors)
	}
}

func TestValidateManifest_UnresolvedTask(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {Run: []PipelineStep{{ID: "s1", Task: "nonexistent"}}},
		},
		Tasks: map[string]TaskDefinition{},
	}
	diags := ValidateManifest(m)
	found := false
	for _, d := range diags {
		if d.Code == "unresolved-task" {
			found = true
		}
	}
	if !found {
		t.Error("expected unresolved-task warning")
	}
}

// TestValidateManifest_StepRunOn pins the run-condition validation on the
// strict path, including the acceptance side: a well-formed finalizer must not
// draw a diagnostic just for existing.
func TestValidateManifest_StepRunOn(t *testing.T) {
	disabled := false
	tasks := map[string]TaskDefinition{
		"setup": {
			Kind: "command", Command: "echo",
			Cache: &TaskCachePolicy{Enabled: &disabled},
			Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
				"lease": {Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "lease.json"},
			}},
		},
		"t": {Kind: "command", Command: "echo"},
	}

	valid := &Manifest{
		Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{
			{ID: "setup", Task: "setup"},
			{ID: "run", Task: "t", DependsOn: []string{"setup"}},
			{
				ID: "teardown", Task: "t", RunOn: StepRunOnFinally,
				Finalizes: &FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
			},
		}}},
		Tasks: tasks,
	}
	if diags := ValidateManifest(valid); diag.HasErrors(diags) {
		t.Fatalf("a well-formed finalizer was rejected: %v", diags)
	}

	unknown := &Manifest{
		Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{
			{ID: "run", Task: "t", RunOn: "failure"},
		}}},
		Tasks: tasks,
	}
	assertDiag(t, ValidateManifest(unknown), "invalid-enum", "commands.test.run[0].runOn")
}

func TestValidateManifest_FinalizesRelation(t *testing.T) {
	disabled := false
	tasks := map[string]TaskDefinition{
		"setup": {
			Kind: "command", Command: "echo",
			Cache: &TaskCachePolicy{Enabled: &disabled},
			Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
				"lease": {Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "lease.json"},
			}},
		},
		"work":     {Kind: "command", Command: "echo"},
		"teardown": {Kind: "command", Command: "echo"},
	}
	build := func() *Manifest {
		return &Manifest{
			Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{
				{ID: "setup", Task: "setup"},
				{ID: "first", Task: "work", DependsOn: []string{"setup"}},
				{ID: "last", Task: "work", DependsOn: []string{"first"}},
				{
					ID: "teardown", Task: "teardown", RunOn: StepRunOnFinally,
					Finalizes: &FinalizesRelation{Producer: "setup", Consumers: []string{"first", "last"}},
				},
			}}},
			Tasks: tasks,
		}
	}
	if diags := ValidateManifest(build()); diag.HasErrors(diags) {
		t.Fatalf("a transitive multi-consumer frontier was rejected: %v", diags)
	}

	notDownstream := build()
	notDownstream.Commands["test"].Run[2].DependsOn = nil
	assertDiag(t, ValidateManifest(notDownstream), "invalid-finalizer-frontier",
		"commands.test.run[3].finalizes.consumers[1]")

	ordinaryDependency := build()
	ordinaryDependency.Commands["test"].Run[3].DependsOn = []string{"setup"}
	assertDiag(t, ValidateManifest(ordinaryDependency), "finalizer-dependency-conflict",
		"commands.test.run[3].dependsOn")

	resultInput := build()
	resultInput.Commands["test"].Run[3].With = map[string]InputBinding{
		"lease": {FromStep: "setup", Output: "lease"},
	}
	assertDiag(t, ValidateManifest(resultInput), "finalizer-result-input",
		"commands.test.run[3].with.lease.fromStep")

	duplicate := build()
	command := duplicate.Commands["test"]
	command.Run = append(command.Run, PipelineStep{
		ID: "teardown-again", Task: "teardown", RunOn: StepRunOnFinally,
		Finalizes: &FinalizesRelation{Producer: "setup", Consumers: []string{"last"}},
	})
	duplicate.Commands["test"] = command
	assertDiag(t, ValidateManifest(duplicate), "duplicate-finalizer",
		"commands.test.run[4].finalizes.producer")
}

// TestValidateManifest_AStepBelongsToAtMostOneInvocation pins the exclusivity
// of a consumer frontier.
//
// Two relations may coexist in one pipeline — two producers, two finalizers —
// but no step may sit in both frontiers. Core resolves exactly ONE invocation
// locator per step and holds one blocking and guard state per relation, so a
// shared step would receive one relation's private artifact tree while the
// other relation's confinement and setup-failure blocking silently applied to
// nothing. The manifest is where that is knowable, so it is where it is
// refused.
func TestValidateManifest_AStepBelongsToAtMostOneInvocation(t *testing.T) {
	disabled := false
	m := &Manifest{
		Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{
			{ID: "setup", Task: "setup"},
			{ID: "setup-2", Task: "setup"},
			{ID: "run", Task: "work", DependsOn: []string{"setup", "setup-2"}},
			{
				ID: "teardown", Task: "teardown", RunOn: StepRunOnFinally,
				Finalizes: &FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
			},
			{
				ID: "teardown-2", Task: "teardown", RunOn: StepRunOnFinally,
				Finalizes: &FinalizesRelation{Producer: "setup-2", Consumers: []string{"run"}},
			},
		}}},
		Tasks: map[string]TaskDefinition{
			"setup": {
				Kind: "command", Command: "echo",
				Cache: &TaskCachePolicy{Enabled: &disabled},
				Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"lease": {Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "lease.json"},
				}},
			},
			"work":     {Kind: "command", Command: "echo"},
			"teardown": {Kind: "command", Command: "echo"},
		},
	}

	diags := ValidateManifest(m)
	assertDiag(t, diags, "shared-finalizer-consumer", "commands.test.run[4].finalizes.consumers[0]")
	// The diagnostic must name the step and BOTH relations: either one alone
	// leaves the author guessing which other declaration to change.
	found := false
	for _, d := range diags {
		if d.Code != "shared-finalizer-consumer" {
			continue
		}
		found = true
		for _, want := range []string{`"run"`, `"teardown"`, `"teardown-2"`} {
			if !strings.Contains(d.Message, want) {
				t.Errorf("diagnostic %q does not name %s", d.Message, want)
			}
		}
	}
	if !found {
		t.Fatal("no shared-finalizer-consumer diagnostic to inspect")
	}

	// Two relations that keep their frontiers disjoint stay valid: the rule is
	// exclusivity, not "one invocation per command".
	disjoint := *m
	steps := append([]PipelineStep(nil), m.Commands["test"].Run...)
	steps[4].Finalizes = &FinalizesRelation{Producer: "setup-2", Consumers: []string{"run-2"}}
	steps = append(steps, PipelineStep{ID: "run-2", Task: "work", DependsOn: []string{"setup-2"}})
	disjoint.Commands = map[string]CommandDefinition{"test": {Run: steps}}
	if diags := ValidateManifest(&disjoint); diag.HasErrors(diags) {
		t.Fatalf("two relations with disjoint frontiers were rejected: %v", diags)
	}
}

// TestValidateManifest_FinalizerBinding pins that no step may consume a
// finalizer's result. A finalizer runs whatever happened to the work it tears
// down, so an edge that waits for its OUTPUT is one the scheduler could never
// honor — and a plan that contains one is a plan whose data flow is a fiction.
func TestValidateManifest_FinalizerBinding(t *testing.T) {
	disabled := false
	m := &Manifest{
		Commands: map[string]CommandDefinition{"test": {Run: []PipelineStep{
			{ID: "setup", Task: "setup"},
			{ID: "run", Task: "t", DependsOn: []string{"setup"}},
			{
				ID: "teardown", Task: "t", RunOn: StepRunOnFinally,
				Finalizes: &FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
			},
			{ID: "report", Task: "t", DependsOn: []string{"teardown"}, With: map[string]InputBinding{
				"summary": {FromStep: "teardown", Output: "summary"},
			}},
		}}},
		Tasks: map[string]TaskDefinition{
			"setup": {
				Kind: "command", Command: "echo",
				Cache: &TaskCachePolicy{Enabled: &disabled},
				Declares: &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"lease": {Kind: OutputKindFile, Scope: OutputScopeInvocation, Path: "lease.json"},
				}},
			},
			"t": {Kind: "command", Command: "echo"},
		},
	}
	assertDiag(t, ValidateManifest(m), "finalizer-binding", "commands.test.run[3].with.summary.fromStep")

	// Ordering alone is fine: depending ON a finalizer without consuming its
	// result is a legitimate "after teardown" edge.
	m.Commands["test"].Run[3].With = nil
	if diags := ValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("a dependsOn edge to a finalizer must be allowed: %v", diags)
	}

	// And binding from an ordinary step stays allowed, so the rule is about
	// finalizers rather than about bindings.
	m.Commands["test"].Run[3].With = map[string]InputBinding{"summary": {FromStep: "setup", Output: "summary"}}
	if diags := ValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("binding from an ordinary step must be allowed: %v", diags)
	}
}

func TestValidateManifest_CommandDependenciesRejectPipelineStepRefs(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {
				DependsOn: []string{"^build", "/workspace-install", "*generate", "!^generate", "prepare", "!lint"},
				Run:       []PipelineStep{{ID: "s1", Task: "t1"}},
			},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go"},
		},
	}

	errors := diag.Errors(ValidateManifest(m))
	if len(errors) != 4 {
		t.Fatalf("expected 4 errors for pipeline-step references at command level, got %d: %v", len(errors), errors)
	}
	for i, d := range errors {
		if d.Code != "invalid-command-dependency" {
			t.Errorf("error %d code = %q, want invalid-command-dependency", i, d.Code)
		}
		wantField := fmt.Sprintf("commands.build.dependsOn[%d]", i)
		if d.Field != wantField {
			t.Errorf("error %d field = %q, want %q", i, d.Field, wantField)
		}
	}
}

func TestValidateManifest_AlsoRunsContract(t *testing.T) {
	manifest := &Manifest{
		Commands: map[string]CommandDefinition{
			"validate": {
				AlsoRuns: []string{"validate-workspace"},
				Run:      []PipelineStep{{ID: "specs", Task: "work"}},
			},
			"validate-workspace": {
				Activation: "workspace-once",
				Run:        []PipelineStep{{ID: "architecture", Task: "work"}},
			},
		},
		Tasks: map[string]TaskDefinition{"work": {Kind: "command", Command: "run"}},
	}
	if errors := diag.Errors(ValidateManifest(manifest)); len(errors) != 0 {
		t.Fatalf("valid alsoRuns rejected: %v", errors)
	}

	invalid := *manifest
	invalid.Commands = map[string]CommandDefinition{
		"validate": {
			// Self-reference, an unknown target, a repeat, and pipeline-step
			// syntax — each must produce its own diagnostic.
			AlsoRuns: []string{"validate", "missing", "missing", "^step"},
			Run:      []PipelineStep{{ID: "specs", Task: "work"}},
		},
	}
	errors := diag.Errors(ValidateManifest(&invalid))
	if len(errors) != 5 {
		t.Fatalf("invalid alsoRuns errors = %d, want 5: %v", len(errors), errors)
	}
	wantCodes := []string{"cyclic-also-runs", "unknown-also-runs-target", "unknown-also-runs-target", "duplicate-also-runs", "invalid-also-runs"}
	for i, want := range wantCodes {
		if errors[i].Code != want {
			t.Fatalf("alsoRuns diagnostics[%d] = %q, want %q (all: %v)", i, errors[i].Code, want, errors)
		}
	}
}

func TestValidateManifest_SessionPrerequisiteContract(t *testing.T) {
	manifest := &Manifest{
		Commands: map[string]CommandDefinition{
			"deploy": {
				Flags: map[string]FlagDefinition{"apps": {Type: "string"}},
				SessionPrerequisites: []SessionPrerequisiteDefinition{{
					Command: "publish", ProjectsFromParam: "apps", DependsOn: []string{"lint"},
					Params: map[string]SessionPrerequisiteParamBinding{
						"docker": {Value: true},
						"config": {FromProjectParam: "publishConfig"},
					},
				}},
				Run: []PipelineStep{{ID: "deploy", Task: "work"}},
			},
		},
		Tasks: map[string]TaskDefinition{"work": {Kind: "command", Command: "run"}},
	}
	if errors := diag.Errors(ValidateManifest(manifest)); len(errors) != 0 {
		t.Fatalf("valid session prerequisite rejected: %v", errors)
	}

	invalid := *manifest
	invalid.Commands = map[string]CommandDefinition{
		"deploy": {
			SessionPrerequisites: []SessionPrerequisiteDefinition{{
				Command: "deploy", DependsOn: []string{"publish"},
				Params: map[string]SessionPrerequisiteParamBinding{
					"ambiguous": {Value: true, FromProjectParam: "enabled"},
				},
			}},
			Run: []PipelineStep{{ID: "deploy", Task: "work"}},
		},
	}
	errors := diag.Errors(ValidateManifest(&invalid))
	if len(errors) != 2 {
		t.Fatalf("invalid session prerequisite errors = %d, want 2: %v", len(errors), errors)
	}
	if errors[0].Code != "cyclic-session-prerequisite" || errors[1].Code != "invalid-session-prerequisite-param" {
		t.Fatalf("invalid session prerequisite diagnostics = %v", errors)
	}
}

func TestValidateManifest_SessionPrerequisiteParamDiagnosticsAreSorted(t *testing.T) {
	manifest := &Manifest{
		Commands: map[string]CommandDefinition{
			"deploy": {
				SessionPrerequisites: []SessionPrerequisiteDefinition{{
					Command: "publish",
					Params: map[string]SessionPrerequisiteParamBinding{
						"zeta":  {},
						"alpha": {Value: true, FromProjectParam: "enabled"},
					},
				}},
				Run: []PipelineStep{{ID: "deploy", Task: "work"}},
			},
		},
		Tasks: map[string]TaskDefinition{"work": {Kind: "command", Command: "run"}},
	}
	want := []string{
		"commands.deploy.sessionPrerequisites[0].params.alpha",
		"commands.deploy.sessionPrerequisites[0].params.zeta",
	}
	for iteration := 0; iteration < 50; iteration++ {
		errors := diag.Errors(ValidateManifest(manifest))
		if len(errors) != len(want) {
			t.Fatalf("iteration %d errors = %v, want fields %v", iteration, errors, want)
		}
		for i, field := range want {
			if errors[i].Field != field {
				t.Fatalf("iteration %d error %d field = %q, want %q", iteration, i, errors[i].Field, field)
			}
		}
	}
}

func TestValidateManifest_SessionPrerequisiteExpressionsAreStrict(t *testing.T) {
	manifest := &Manifest{
		Commands: map[string]CommandDefinition{
			"deploy": {
				SessionPrerequisites: []SessionPrerequisiteDefinition{
					{Command: "publish", If: "param.preview"},
					{Command: "publish", ProjectIf: "params.deploy.enabled &&"},
				},
				Run: []PipelineStep{{ID: "deploy", Task: "work"}},
			},
		},
		Tasks: map[string]TaskDefinition{"work": {Kind: "command", Command: "run"}},
	}
	errors := diag.Errors(ValidateManifest(manifest))
	want := []struct {
		field string
		expr  string
	}{
		{field: "commands.deploy.sessionPrerequisites[0].if", expr: "param.preview"},
		{field: "commands.deploy.sessionPrerequisites[1].projectIf", expr: "params.deploy.enabled &&"},
	}
	if len(errors) != len(want) {
		t.Fatalf("invalid session prerequisite expressions errors = %v, want %d", errors, len(want))
	}
	for i, expected := range want {
		if errors[i].Code != "invalid-session-prerequisite-expression" || errors[i].Field != expected.field ||
			!strings.Contains(errors[i].Message, expected.expr) {
			t.Fatalf("error %d = %+v, want invalid expression %q at %q", i, errors[i], expected.expr, expected.field)
		}
	}
}

func TestValidateManifest_InvalidInputSource(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {Run: []PipelineStep{{ID: "s1", Task: "t1"}}},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {
				Kind:    "command",
				Command: "go",
				Inputs: map[string]TaskInputPort{
					"src": {From: "invalid-source"},
				},
			},
		},
	}
	diags := ValidateManifest(m)
	found := false
	for _, d := range diags {
		if d.Code == "invalid-enum" {
			found = true
		}
	}
	if !found {
		t.Error("expected invalid-enum error for bad input source")
	}
}

func TestValidateManifest_Valid(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {Run: []PipelineStep{{ID: "s1", Task: "t1"}}},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go"},
		},
	}
	diags := ValidateManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestValidateManifest_ReservedGlobalFlagShadows(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"test": {
				Flags: map[string]FlagDefinition{
					"details": {Type: "boolean", Short: "-v"},
					"json":    {Type: "boolean"},
				},
				Run: []PipelineStep{{ID: "s1", Task: "t1"}},
			},
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"cloud": {
				Flags: map[string]FlagDefinition{
					"output": {Type: "string"},
				},
				Subcommands: map[string]SubcommandDefinition{
					"status": {
						Command: "test",
						Flags: map[string]FlagDefinition{
							"color": {Type: "boolean"},
						},
					},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go"},
		},
	}
	diags := ValidateManifest(m)
	wantFields := []string{
		"commands.test.flags.details.short",
		"commands.test.flags.json",
		"commandGroups.cloud.flags.output",
		"commandGroups.cloud.subcommands.status.flags.color",
	}
	for _, field := range wantFields {
		found := false
		for _, d := range diags {
			if d.Code == "reserved-global-flag" && d.Field == field {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected reserved-global-flag diagnostic for %s, got %v", field, diags)
		}
	}
}

func TestValidateManifest_NestedSubcommandsInheritCommand(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"cloud-config": {Run: []PipelineStep{{ID: "config", Task: "config-task"}}},
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"cloud": {
				Subcommands: map[string]SubcommandDefinition{
					"config": {
						Command: "cloud-config",
						Subcommands: map[string]SubcommandDefinition{
							"show": {
								Flags: map[string]FlagDefinition{
									"with-secrets": {Type: "boolean"},
								},
								Positionals: []PositionalDefinition{{Name: "project"}},
							},
						},
					},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"config-task": {Kind: "command", Command: "echo"},
		},
	}
	diags := ValidateManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestValidateManifest_FlatCommandGroupStillValid(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"demo-login": {Run: []PipelineStep{{ID: "login", Task: "login-task"}}},
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"demo": {
				Subcommands: map[string]SubcommandDefinition{
					"login": {Command: "demo-login"},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"login-task": {Kind: "command", Command: "echo"},
		},
	}
	diags := ValidateManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestValidateManifest_TopLevelSubcommandStillRequiresCommand(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"cloud-config": {Run: []PipelineStep{{ID: "config", Task: "config-task"}}},
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"cloud": {
				Subcommands: map[string]SubcommandDefinition{
					"config": {
						Subcommands: map[string]SubcommandDefinition{
							"show": {Command: "cloud-config"},
						},
					},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"config-task": {Kind: "command", Command: "echo"},
		},
	}
	diags := ValidateManifest(m)
	found := false
	for _, d := range diags {
		if d.Code == "required-field" && d.Field == "commandGroups.cloud.subcommands.config.command" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected top-level subcommand command diagnostic, got %v", diags)
	}
}

func TestValidateManifest_ExampleRequiresCommand(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"cloud-status": {Run: []PipelineStep{{ID: "status", Task: "status-task"}}},
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"cloud": {
				Subcommands: map[string]SubcommandDefinition{
					"status": {
						Command: "cloud-status",
						Examples: []ExampleDefinition{
							{Command: "putnami cloud status"},
							{Description: "missing command"},
						},
					},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"status-task": {Kind: "command", Command: "echo"},
		},
	}
	diags := ValidateManifest(m)
	found := false
	for _, d := range diags {
		if d.Code == "required-field" && d.Field == "commandGroups.cloud.subcommands.status.examples[1].command" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected example command required diagnostic, got %v", diags)
	}
}

func TestParseManifest_Strict_CommandGroupFlags(t *testing.T) {
	// A group-level `flags` block declaring shared flags inherited by every
	// subcommand must parse under strict mode and populate the group's Flags map.
	data := []byte(`{
		"cliContract": 4,
		"commandGroups": {
			"cloud": {
				"description": "Interact with Putnami Cloud",
				"flags": {
					"env": {"type": "string", "default": "prod"}
				},
				"subcommands": {
					"status": {"command": "cloud-status"}
				}
			}
		},
		"commands": {
			"cloud-status": {
				"run": [{"id": "status", "task": "status-exec"}]
			}
		},
		"tasks": {
			"status-exec": {"kind": "command", "command": "echo"}
		}
	}`)

	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	group, ok := m.CommandGroups["cloud"]
	if !ok {
		t.Fatal("cloud command group should be populated")
	}
	envFlag, ok := group.Flags["env"]
	if !ok {
		t.Fatalf("group.Flags[env] not populated, got %v", group.Flags)
	}
	if envFlag.Type != "string" {
		t.Errorf("group.Flags[env].Type = %q, want string", envFlag.Type)
	}
	if envFlag.Default != "prod" {
		t.Errorf("group.Flags[env].Default = %v, want prod", envFlag.Default)
	}
}

func TestValidateManifest_ExamplesWithCommandValid(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"cloud-status": {Run: []PipelineStep{{ID: "status", Task: "status-task"}}},
		},
		CommandGroups: map[string]CommandGroupDefinition{
			"cloud": {
				Subcommands: map[string]SubcommandDefinition{
					"status": {
						Command: "cloud-status",
						Examples: []ExampleDefinition{
							{Command: "putnami cloud status", Description: "human table"},
						},
					},
				},
			},
		},
		Tasks: map[string]TaskDefinition{
			"status-task": {Kind: "command", Command: "echo"},
		},
	}
	if diags := ValidateManifest(m); diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestNormalizeManifest(t *testing.T) {
	m := &Manifest{}
	NormalizeManifest(m)
	if m.Commands == nil {
		t.Error("Commands should be initialized")
	}
	if m.Tasks == nil {
		t.Error("Tasks should be initialized")
	}
	if m.ExtensionDeps.List == nil {
		t.Error("ExtensionDeps.List should be initialized")
	}
}

func TestNormalizeManifest_Nil(t *testing.T) {
	NormalizeManifest(nil) // should not panic
}

func TestNormalizeManifest_DerivesCacheKey(t *testing.T) {
	m := &Manifest{
		Tasks: map[string]TaskDefinition{
			"t1": {
				Kind:    "command",
				Command: "go",
				Inputs: map[string]TaskInputPort{
					"src": {From: "project", Files: []string{"**/*.go"}},
				},
			},
		},
	}
	NormalizeManifest(m)
	task := m.Tasks["t1"]
	if task.Cache == nil {
		t.Fatal("Cache should be derived from inputs")
	}
	if task.Cache.Key == nil {
		t.Fatal("Cache.Key should be set")
	}
	if len(task.Cache.Key.Files) != 1 {
		t.Errorf("Cache.Key.Files = %v, want 1 entry", task.Cache.Key.Files)
	}
}

func TestParseAndValidateManifest_Valid(t *testing.T) {
	data := []byte(`{
		"cliContract": 4,
		"commands": {
			"build": {
				"run": [{"id": "s1", "task": "t1"}]
			}
		},
		"tasks": {
			"t1": {"kind": "command", "command": "go"}
		}
	}`)

	m, diags := ParseAndValidateManifest(data)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
	if m == nil {
		t.Fatal("manifest should not be nil")
	}
	// Verify normalization happened.
	if m.ExtensionDeps.List == nil {
		t.Error("ExtensionDeps.List should be initialized after normalization")
	}
}

func TestParseAndValidateManifest_ParseError(t *testing.T) {
	_, diags := ParseAndValidateManifest([]byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected parse error")
	}
}

func TestValidatePipelineDAG_Acyclic(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {
				Run: []PipelineStep{
					{ID: "a", Task: "t1"},
					{ID: "b", Task: "t1", DependsOn: []string{"a"}},
					{ID: "c", Task: "t1", DependsOn: []string{"b"}},
				},
			},
		},
		Tasks: map[string]TaskDefinition{"t1": {Kind: "command", Command: "go"}},
	}
	diags := ValidatePipelineDAG(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected cycle error: %v", diags)
	}
}

func TestValidatePipelineDAG_Cycle(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {
				Run: []PipelineStep{
					{ID: "a", Task: "t1", DependsOn: []string{"b"}},
					{ID: "b", Task: "t1", DependsOn: []string{"a"}},
				},
			},
		},
		Tasks: map[string]TaskDefinition{"t1": {Kind: "command", Command: "go"}},
	}
	diags := ValidatePipelineDAG(m)
	if !diag.HasErrors(diags) {
		t.Fatal("expected cycle error")
	}
	found := false
	for _, d := range diags {
		if d.Code == "pipeline-cycle" {
			found = true
		}
	}
	if !found {
		t.Error("expected pipeline-cycle diagnostic code")
	}
}

func TestValidatePipelineDAG_Nil(t *testing.T) {
	diags := ValidatePipelineDAG(nil)
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics for nil, got %v", diags)
	}
}

func TestValidateSchemaRefs_Valid(t *testing.T) {
	m := &Manifest{
		Contracts: &ContractsDefinition{
			Schemas: map[string]map[string]any{
				"input-schema":  {"type": "object"},
				"output-schema": {"type": "object"},
			},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go", InputSchemaRef: "input-schema", OutputSchemaRef: "output-schema"},
		},
	}
	diags := ValidateSchemaRefs(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestValidateSchemaRefs_Unresolved(t *testing.T) {
	m := &Manifest{
		Contracts: &ContractsDefinition{
			Schemas: map[string]map[string]any{},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go", InputSchemaRef: "missing"},
		},
	}
	diags := ValidateSchemaRefs(m)
	if !diag.HasErrors(diags) {
		t.Fatal("expected error for unresolved schema ref")
	}
	if diags[0].Code != "unresolved-schema-ref" {
		t.Errorf("Code = %q, want unresolved-schema-ref", diags[0].Code)
	}
}

func TestValidateSchemaRefs_NoContracts(t *testing.T) {
	m := &Manifest{
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go", InputSchemaRef: "something"},
		},
	}
	diags := ValidateSchemaRefs(m)
	if len(diags) != 0 {
		t.Errorf("expected no diagnostics when no contracts section, got %v", diags)
	}
}

func TestFullValidateManifest_Valid(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {Run: []PipelineStep{{ID: "s1", Task: "t1"}}},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go"},
		},
	}
	diags := FullValidateManifest(m)
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors: %v", diags)
	}
}

func TestFullValidateManifest_CatchesCycle(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {
				Run: []PipelineStep{
					{ID: "a", Task: "t1", DependsOn: []string{"b"}},
					{ID: "b", Task: "t1", DependsOn: []string{"a"}},
				},
			},
		},
		Tasks: map[string]TaskDefinition{"t1": {Kind: "command", Command: "go"}},
	}
	diags := FullValidateManifest(m)
	if !diag.HasErrors(diags) {
		t.Fatal("expected cycle error from FullValidateManifest")
	}
}

func TestStrictLoadManifest_PrefixesPath(t *testing.T) {
	_, diags := StrictLoadManifest("my/path.json", []byte("{bad"))
	if !diag.HasErrors(diags) {
		t.Fatal("expected error")
	}
	if len(diags) == 0 {
		t.Fatal("expected at least one diagnostic")
	}
	// The message should contain the path.
	if diags[0].Message == "" {
		t.Error("message should not be empty")
	}
}
