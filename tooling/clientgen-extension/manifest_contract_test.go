package clientgen

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// The v3 task contract makes this extension's manifest load-bearing: a declared
// output is what the cache captures and restores, and two tasks claiming one
// path is a correctness bug that surfaces in a consumer's workspace at plan
// time, far from here. These tests are the extension's own conformance harness —
// they run the SAME predicates the CLI and the extension SDK run
// (proto.FullValidateManifest, which calls ValidateTaskContracts and
// ValidateOutputOwnership), against the REAL manifest, so a declaration that
// would be rejected downstream is rejected in this project's own test run first.

const clientgenFeature = "tooling/cross-language-rest-clients"

// loadExtensionManifest parses the shipped putnami.extension.json exactly as
// `putnami dev extension validate` and the package-time gate do.
func loadExtensionManifest(t *testing.T) *proto.Manifest {
	t.Helper()
	data, err := os.ReadFile("putnami.extension.json")
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	manifest, diags := proto.ParseManifest(data)
	if manifest == nil || diag.HasErrors(diags) {
		t.Fatalf("parse extension manifest: %s", formatDiagnostics(diags))
	}
	return manifest
}

func formatDiagnostics(diags []diag.Diagnostic) string {
	messages := make([]string, 0, len(diags))
	for _, value := range diags {
		messages = append(messages, value.String())
	}
	if len(messages) == 0 {
		return "<none>"
	}
	return strings.Join(messages, "\n  ")
}

// TestExtensionManifestPassesFullValidation is the `putnami dev extension
// validate` gate as a unit test: the real manifest must produce ZERO
// diagnostics under the comprehensive protocol entry point, which includes the
// v3 task-contract harness (exactness, one owner per output, honest effects).
func TestExtensionManifestPassesFullValidation(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "protocol-conformant-manifest", "the-manifest-produces-zero-diagnostics-under-full-validation")
	manifest := loadExtensionManifest(t)
	if diags := proto.FullValidateManifest(manifest); len(diags) > 0 {
		t.Fatalf("manifest is not conformant:\n  %s", formatDiagnostics(diags))
	}
}

// TestManifestExercisesTheV3TaskContract keeps the migration from silently
// regressing to v2: a manifest whose tasks lose their `declares` blocks would
// still validate (v3 is additive), so the version itself is asserted.
func TestManifestExercisesTheV3TaskContract(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "protocol-conformant-manifest", "the-manifest-exercises-the-v3-task-contract-harness")
	manifest := loadExtensionManifest(t)
	if got := proto.ManifestProtocolVersion(manifest); got != proto.ProtocolVersionV3 {
		t.Fatalf("manifest protocol version = %d, want %d (v3 task declarations)", got, proto.ProtocolVersionV3)
	}
}

// TestEveryTaskCarriesAV3Declaration pins that the migration is COMPLETE. v3 is
// additive per task, so a task that quietly loses (or never gains) its
// `declares` block keeps v2 inferred capture while the rest of the manifest is
// declared — the mixed state this contract exists to end.
func TestEveryTaskCarriesAV3Declaration(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "protocol-conformant-manifest", "every-task-carries-a-v3-declaration")
	manifest := loadExtensionManifest(t)
	for _, name := range sortedTaskNames(manifest) {
		if manifest.Tasks[name].Declares == nil {
			t.Errorf("task %q has no v3 declaration; every task in this manifest must state its outputs, effects and source mutation (an empty object states \"none of the three\")", name)
		}
	}
}

// ownerRow is one declared output and the task that owns it, in the form a
// reviewer reads the ownership decision in.
type ownerRow struct {
	task          string
	id            string
	kind          string
	root          string
	path          string
	optionalEmpty bool
}

func (row ownerRow) String() string {
	return fmt.Sprintf("%s.%s = %s %s:%s optionalEmpty=%t", row.task, row.id, row.kind, row.root, row.path, row.optionalEmpty)
}

// TestDeclaredOutputOwnerTable pins the declared output ownership. A row
// appearing or moving here must be a reviewed change rather than a silent one.
func TestDeclaredOutputOwnerTable(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "protocol-conformant-manifest", "every-declared-output-has-exactly-one-owner")
	spectest.Proves(t, clientgenFeature, "dynamic-output-ownership", "both-language-targets-report-and-own-their-configured-output-directory")
	manifest := loadExtensionManifest(t)
	want := []ownerRow{
		{task: "clientgen-go", id: "client", kind: proto.OutputKindDirectory, root: proto.OutputRootProject, path: "goClientOutput", optionalEmpty: true},
		{task: "clientgen-ts", id: "client", kind: proto.OutputKindDirectory, root: proto.OutputRootProject, path: "typescriptClientOutput", optionalEmpty: true},
	}
	got := declaredOwnerRows(manifest)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("declared owners = %s\nwant %s", rowsString(got), rowsString(want))
	}

	// The manifest-local half of ONE OWNER PER OUTPUT, run exactly as the CLI
	// and the SDK run it.
	if diags := proto.ValidateOutputOwnership(manifest); len(diags) > 0 {
		t.Fatalf("declared outputs are not exclusively owned:\n  %s", formatDiagnostics(diags))
	}
	for name, outputPort := range map[string]string{"clientgen-go": "goClientOutput", "clientgen-ts": "typescriptClientOutput"} {
		if _, ok := manifest.Tasks[name].Outputs[outputPort]; !ok {
			t.Errorf("task %q does not report the %s port its declared output takes its pathFrom", name, outputPort)
		}
	}
}

// TestAConfigChosenOutputIsNeverClaimedAtALiteralPath is the descendant of the
// recorded gap this project used to carry. The generated client directory is
// chosen by the provider's `.gen/clientgen/config.json`, so a literal `path`
// would be a claim the configuration can move out from under. Until the
// generators reported their output the only honest answer was to claim nothing;
// now they report it, and the only honest answer is a pathFrom naming that
// port. Both readings forbid the same thing, and this test forbids it.
func TestAConfigChosenOutputIsNeverClaimedAtALiteralPath(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "unclaimed-outputs-are-declared-unclaimed", "a-config-chosen-output-is-never-claimed-at-a-literal-path")
	manifest := loadExtensionManifest(t)
	for _, name := range []string{"clientgen-go", "clientgen-ts"} {
		task, ok := manifest.Tasks[name]
		if !ok {
			t.Fatalf("manifest task %q is missing", name)
		}
		if task.Declares == nil {
			t.Fatalf("task %q lost its v3 declaration", name)
		}
		for id, output := range task.Declares.Outputs {
			if output.PathFrom == "" {
				t.Errorf("task %q declares output %q at the literal path %q; the client directory is chosen by the "+
					"provider's clientgen config, so it is declarable only through a pathFrom port", name, id, output.Path)
			}
			if _, reported := task.Outputs[output.PathFrom]; !reported {
				t.Errorf("task %q declares output %q from the port %q, which the task does not report", name, id, output.PathFrom)
			}
		}
	}
}

// TestGeneratedClientDirectoriesPreserveWhatTheWorkspaceOwns pins the
// preserved subpaths of both client outputs (protocol ADR 0005). The project
// document and the Go module files make the client directory a workspace
// project of its own. The TypeScript install tree is the workspace's
// too: a consumer that lists the client as a package installs its dependencies
// inside the output, and the engine's capture walked into that tree, failed on
// its first symlinked directory and cached nothing for any provider.
// A client directory that is a project has its own .gen, which the engine and
// that project's tasks write, so the generator neither captures nor restores
// it.
func TestGeneratedClientDirectoriesPreserveWhatTheWorkspaceOwns(t *testing.T) {
	manifest := loadExtensionManifest(t)
	want := map[string][]string{
		"clientgen-go": {".gen", "go.mod", "go.sum", "putnami.json"},
		"clientgen-ts": {".gen", "node_modules", "putnami.json"},
	}
	for name, preserved := range want {
		task, ok := manifest.Tasks[name]
		if !ok || task.Declares == nil {
			t.Fatalf("manifest task %q is missing or lost its v3 declaration", name)
		}
		if got := proto.DecidablePreserves(task.Declares.Outputs["client"]); !reflect.DeepEqual(got, preserved) {
			t.Errorf("task %q preserves %v inside its client output, want %v", name, got, preserved)
		}
	}
}

// TestTaskEffectTable pins HONEST EFFECTS per task: what each task does beyond
// writing its declared outputs, and — for the effects a cache hit cannot
// reproduce — that the task is not cacheable.
//
// Both project tasks now invoke emitter binaries packaged beside the extension
// runtime, so neither resolves a module over the network. The TypeScript half
// still declares the bun transpile cache. Sync and adopt spawn the CLI to
// build every provider, which does resolve modules; adoption additionally
// rewrites authored sources, which is the workspace-files effect. The check
// reads committed files and spawns nothing, so it has no effect to declare.
func TestTaskEffectTable(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "honest-effects", "each-task-declares-exactly-the-effects-it-has")
	spectest.Proves(t, clientgenFeature, "honest-effects", "cacheable-generation-has-no-unreplayed-external-effect")
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "tasks-use-the-compiled-runtime-and-package-resolved-language-emitters")
	manifest := loadExtensionManifest(t)

	want := map[string][]string{
		"clientgen-go":              {},
		"clientgen-ts":              {"toolchain-cache"},
		"clientgen-workspace-adopt": {"network", "toolchain-cache", "workspace-files"},
		"clientgen-workspace-check": {},
		"clientgen-workspace-sync":  {"network", "toolchain-cache"},
	}

	for _, name := range sortedTaskNames(manifest) {
		task := manifest.Tasks[name]
		if task.Command != "{extensionRuntime}" {
			t.Errorf("task %q command = %q, want the compiled extension runtime", name, task.Command)
		}
		got := []string(nil)
		if task.Declares != nil {
			got = append(got, task.Declares.Effects...)
		}
		sort.Strings(got)
		expected := append([]string(nil), want[name]...)
		sort.Strings(expected)
		if strings.Join(got, ",") != strings.Join(expected, ",") {
			t.Errorf("task %q effects = %v, want %v", name, got, expected)
		}
		for _, effect := range got {
			if proto.IsExternalTaskEffect(effect) && task.Cache.IsEnabled() {
				t.Errorf("task %q declares the external effect %q while caching is enabled", name, effect)
			}
		}
	}
}

// The TypeScript emitter runs with the bun the workspace lock pins. A host that
// holds no bun of that release gets it from `putnami install`, which puts it
// under the Putnami home: the emitter toolchain lists that install after the
// host's own locations, so the emitter runs on a host where nobody installed
// Bun.
func TestTypeScriptEmitterResolvesTheBunUnderThePutnamiHome(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution",
		"the-typescript-emitter-resolves-the-bun-under-the-putnami-home")
	manifest := loadExtensionManifest(t)
	emitter, declared := manifest.Runtime.Toolchains["typescriptEmitter"]
	if !declared || emitter.Lock != "bun" {
		t.Fatalf("typescriptEmitter = %+v (declared %t), want a toolchain that resolves the bun lock", emitter, declared)
	}
	want := []proto.RuntimeToolchainCandidate{
		{From: proto.RuntimeToolchainCandidatePath, Path: "bun"},
		{From: proto.RuntimeToolchainCandidateEnvironment, Environment: "BUN_INSTALL", Path: "bin/bun"},
		{From: proto.RuntimeToolchainCandidateHome, Path: ".bun/bin/bun"},
		{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/bun/bun-{version}/bin/bun"},
	}
	if !reflect.DeepEqual(emitter.Candidates, want) {
		t.Fatalf("typescriptEmitter candidates = %+v, want %+v", emitter.Candidates, want)
	}
}

// TestOnlyTheAdoptionCommandWritesAuthoredSources pins where source mutation
// is allowed to live. Generation writes generated clients, never the authored
// sources it reads, so neither mutatesSources nor the project-scoped "sources"
// write resource belongs to any of those tasks. Adoption does rewrite authored
// imports and binding constructions, and it is the one task that declares the
// workspace-scoped authored-source write and the workspace-files effect.
func TestOnlyTheAdoptionCommandWritesAuthoredSources(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "no-source-mutation",
		"every-generation-and-verification-task-leaves-authored-sources-alone")
	spectest.Proves(t, clientgenFeature, "no-source-mutation",
		"only-the-adoption-command-declares-and-performs-an-authored-source-write")
	const adoptionTask = "clientgen-workspace-adopt"
	manifest := loadExtensionManifest(t)
	adoptionWrites, adoptionEffect := false, false
	for _, name := range sortedTaskNames(manifest) {
		task := manifest.Tasks[name]
		mutates := task.Declares != nil && task.Declares.MutatesSources
		if mutates {
			t.Errorf("task %q declares mutatesSources; clientgen writes generated clients, not the sources it reads", name)
		}
		if mutates != declaresSourceWrite(task) {
			t.Errorf("task %q mutatesSources = %t but sources write resource = %t; the two must agree",
				name, mutates, declaresSourceWrite(task))
		}
		writesAuthored := false
		for _, write := range task.Writes {
			if write.ID == "workspace-authored-sources" {
				writesAuthored = true
			}
		}
		declaresEffect := false
		for _, effect := range task.Declares.Effects {
			if effect == proto.EffectWorkspaceFiles {
				declaresEffect = true
			}
		}
		if name == adoptionTask {
			adoptionWrites, adoptionEffect = writesAuthored, declaresEffect
			continue
		}
		if writesAuthored {
			t.Errorf("task %q claims the authored-source write resource without owning a codemod", name)
		}
		if declaresEffect {
			t.Errorf("task %q declares the workspace-files effect; only adoption edits an authored source", name)
		}
	}
	if !adoptionWrites || !adoptionEffect {
		t.Errorf("%s applies source codemods but declares authored-source write = %t, workspace-files effect = %t",
			adoptionTask, adoptionWrites, adoptionEffect)
	}
}

// TestWorkspaceCommandsOwnTheirBuildAndTheProjectCommandDependsOnBuild pins the
// ordering contract. The project command is scheduler-mediated through the
// provider's build; sync and adopt take no dependsOn because they must snapshot
// the worktree BEFORE any build of their own can rewrite it and then build the
// providers themselves; the check builds nothing, so the explicit check command
// depends on nothing either, while the validate contribution waits on the
// `!clientgen` session barrier (TestValidateContributesTheWorkspaceGuard).
func TestWorkspaceCommandsOwnTheirBuildAndTheProjectCommandDependsOnBuild(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "spec-mediated-generation", "the-clientgen-command-depends-on-build")
	spectest.Proves(t, clientgenFeature, "workspace-one-pass", "sync-and-adopt-materialize-all-provider-contracts-themselves")
	manifest := loadExtensionManifest(t)

	projectCommand, ok := manifest.Commands["clientgen"]
	if !ok {
		t.Fatal("manifest declares no clientgen command")
	}
	if got := append([]string(nil), projectCommand.DependsOn...); !reflect.DeepEqual(got, []string{"build"}) {
		t.Errorf("clientgen dependsOn = %v, want [build]; without it a generator can run before the OpenAPI document exists", got)
	}
	for _, name := range []string{"clientgen-sync", "clientgen-adopt", "clientgen-check"} {
		command, exists := manifest.Commands[name]
		if !exists {
			t.Errorf("manifest declares no %q command", name)
			continue
		}
		if command.Activation != "workspace-once" || len(command.DependsOn) != 0 {
			t.Errorf("workspace command %q activation=%q dependsOn=%v; it owns its own build or needs none",
				name, command.Activation, command.DependsOn)
		}
	}
}

// TestProjectGeneratorsReadTheGeneratedTree pins the declaration that orders a
// generator against every rewrite of its provider's .gen. Each generator reads
// .gen/clientgen/config.json, and `package` rewrites .gen through a generate
// step no functional edge orders against `clientgen`. Reading the project `gen`
// resource, which the language extensions' generate steps write, is what makes
// the planner sequence the two. The planning half is asserted against
// the real planner in tooling/cli/internal/jobs/clientgen_native_gate_test.go.
func TestProjectGeneratorsReadTheGeneratedTree(t *testing.T) {
	manifest := loadExtensionManifest(t)
	for _, name := range []string{"clientgen-go", "clientgen-ts"} {
		task, ok := manifest.Tasks[name]
		if !ok {
			t.Fatalf("manifest declares no %s task", name)
		}
		readsGen := false
		for _, ref := range task.Reads {
			if ref.ID == "gen" && ref.Scope != proto.ResourceScopeWorkspace {
				readsGen = true
			}
		}
		if !readsGen {
			t.Errorf("%s reads %+v, want the project gen resource: without it the generator can run while "+
				"another step rewrites .gen and find no generation contract", name, task.Reads)
		}
	}
}

// TestValidateContributesTheWorkspaceGuard is the activation half of the native
// validation gate, read from the manifest that ships.
//
// The guard reaches a session through `validate`, with workspace-once
// activation so the planner never consults a project's extension list. The
// command waits on the `!clientgen` session barrier, which is what plans every
// selected provider's generation — the engine judging drift on the generators'
// declared outputs — before the guard reads the tree. The task declares a
// workspace-scoped read (it answers about the whole workspace) and is keyed on
// the input `git:**`: it reads the workspace's Git candidate cut and nothing
// else, so that cut is its whole read set, and it stores no output. The planning
// half — exactly one node for any selection, the barrier's edges, and none
// for another command — is asserted against the real planner in
// tooling/cli/internal/jobs/clientgen_native_gate_test.go.
func TestValidateContributesTheWorkspaceGuard(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "validate-contributes-exactly-one-workspace-guard-for-any-selection")
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "the-guard-reads-the-git-candidate-cut-and-keys-on-it")
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "the-guard-task-is-reachable-only-from-validate-and-the-explicit-check-command")
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "validate-waits-for-the-selected-providers-generation")
	manifest := loadExtensionManifest(t)

	command, ok := manifest.Commands["validate"]
	if !ok {
		t.Fatal("the manifest contributes no validate command; `putnami validate` runs no generated-client guard")
	}
	if command.Activation != "workspace-once" {
		t.Fatalf("validate activation = %q, want workspace-once; any other activation lets a project opt out of the "+
			"guard by not declaring this extension", command.Activation)
	}
	if !reflect.DeepEqual(append([]string(nil), command.DependsOn...), []string{"!clientgen"}) {
		t.Fatalf("validate dependsOn = %v, want [!clientgen]: the barrier is what regenerates a selected provider's "+
			"clients, under the engine's drift judgment, before the guard reads them", command.DependsOn)
	}
	if len(command.Run) != 1 || command.Run[0].Task != "clientgen-workspace-check" {
		t.Fatalf("validate runs %+v, want exactly the workspace check", command.Run)
	}

	reachable := []string{}
	for name, definition := range manifest.Commands {
		for _, step := range definition.Run {
			if step.Task == "clientgen-workspace-check" {
				reachable = append(reachable, name)
			}
		}
	}
	sort.Strings(reachable)
	if !reflect.DeepEqual(reachable, []string{"clientgen-check", "validate"}) {
		t.Fatalf("the guard task is reachable from %v; a whole-workspace verification behind build or lint is a "+
			"cost nobody asked for", reachable)
	}

	task := manifest.Tasks["clientgen-workspace-check"]
	workspaceScoped := false
	for _, ref := range task.Reads {
		if ref.Scope == proto.ResourceScopeWorkspace {
			workspaceScoped = true
		}
	}
	if !workspaceScoped {
		t.Fatalf("the guard reads %+v; it answers about the whole workspace and must say so", task.Reads)
	}
	if !task.Cache.IsEnabled() || !task.Cache.NoOutput {
		t.Fatalf("the guard cache policy is %+v, want enabled with noOutput: it is a pure verdict over the candidate cut "+
			"its key holds, and it writes nothing to restore", task.Cache)
	}
	// The port is project-scoped because a workspace-once task's project is
	// rooted at the workspace root, so `git:**` holds the whole repository's
	// candidate cut. A workspace port, a narrower glob or a second port would
	// key on a set other than the one the check reads.
	wantInputs := map[string]proto.TaskInputPort{"repository": {From: "project", Files: []string{"git:**"}}}
	if !reflect.DeepEqual(task.Inputs, wantInputs) {
		t.Fatalf("the guard inputs are %+v, want %+v: the check reads the candidate cut, and `git:**` is the key that holds it",
			task.Inputs, wantInputs)
	}
	if task.Cache.Key != nil {
		t.Fatalf("the guard declares the cache key %+v; its key is its `git:**` input and nothing else", task.Cache.Key)
	}
	for _, phrase := range []string{"Git candidate cut", "`git:**`", "neither read nor keyed"} {
		if !strings.Contains(task.Description, phrase) {
			t.Fatalf("the guard description does not say what its key reads (%q): %s", phrase, task.Description)
		}
	}
	if len(task.Toolchains) != 0 {
		t.Fatalf("the guard binds toolchains %v; it renders nothing, so it needs no emitter", task.Toolchains)
	}
	for _, ref := range task.Writes {
		t.Fatalf("the guard declares the write %+v; it builds nothing and renders nothing", ref)
	}
}

// TestNoGeneratorInvokesTheOtherLanguagesExtension keeps generation
// scheduler-mediated. Each task reaches its own packaged emitter through the
// extension runtime and nothing else — no CLI recursion, no cross-extension
// exec.
func TestNoGeneratorInvokesTheOtherLanguagesExtension(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "spec-mediated-generation", "neither-generator-invokes-the-other-languages-extension")
	manifest := loadExtensionManifest(t)
	for id, task := range manifest.Tasks {
		line := strings.Join(append([]string{task.Command}, task.Args...), " ")
		for _, forbidden := range []string{"putnami-ts", "putnami-go", "putnami "} {
			if strings.Contains(line, forbidden) {
				t.Errorf("task %q invokes %q (%s); generation is scheduler-mediated, not cross-extension", id, forbidden, line)
			}
		}
	}
}

// TestHarnessRejectsADishonestDeclaration is the mutation check that keeps the
// conformance tests above from being vacuous: flip one row of the real manifest
// and the SAME predicates that pass on it must fail. Without this, a harness
// that silently validated nothing would look identical to a passing one.
func TestHarnessRejectsADishonestDeclaration(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "honest-effects", "a-dishonest-effect-declaration-is-rejected")
	spectest.Proves(t, clientgenFeature, "protocol-conformant-manifest", "the-contract-harness-rejects-forged-ownership-and-effects")
	cases := []struct {
		name    string
		mutate  func(m *proto.Manifest)
		wantMsg string
	}{
		{
			// Both generators run in the SAME command, so a shared
			// command-output path is a real collision, not a theoretical one.
			name: "two tasks of one command claiming one path",
			mutate: func(manifest *proto.Manifest) {
				for _, name := range []string{"clientgen-go", "clientgen-ts"} {
					task := manifest.Tasks[name]
					task.Declares.Outputs = map[string]proto.DeclaredOutput{
						"client": {Kind: proto.OutputKindDirectory, Root: proto.OutputRootCommandOutput, Path: "clients"},
					}
					manifest.Tasks[name] = task
				}
			},
			wantMsg: "exactly one owner",
		},
		{
			// A pathFrom must name a port the task actually reports on.
			name: "pathFrom without a port",
			mutate: func(manifest *proto.Manifest) {
				task := manifest.Tasks["clientgen-go"]
				task.Declares.Outputs = map[string]proto.DeclaredOutput{
					"client": {Kind: proto.OutputKindDirectory, PathFrom: "typescriptClientOutput"},
				}
				manifest.Tasks["clientgen-go"] = task
			},
			wantMsg: "which the task does not declare",
		},
		{
			// An effect outside the closed vocabulary is a protocol change, not
			// a free-form string.
			name: "effect outside the vocabulary",
			mutate: func(manifest *proto.Manifest) {
				task := manifest.Tasks["clientgen-ts"]
				task.Declares.Effects = []string{"filesystem"}
				manifest.Tasks["clientgen-ts"] = task
			},
			wantMsg: "invalid task effect",
		},
		{
			// A cacheable task cannot claim an effect a cache hit replays.
			name: "cache with an unreplayable effect",
			mutate: func(manifest *proto.Manifest) {
				task := manifest.Tasks["clientgen-go"]
				task.Declares.Effects = []string{"registry"}
				manifest.Tasks["clientgen-go"] = task
			},
			wantMsg: "cache",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := loadExtensionManifest(t)
			if diags := proto.ValidateTaskContracts(manifest); len(diags) > 0 {
				t.Fatalf("the real manifest must be clean before it is mutated:\n  %s", formatDiagnostics(diags))
			}
			tc.mutate(manifest)
			diags := proto.ValidateTaskContracts(manifest)
			if len(diags) == 0 {
				t.Fatal("the mutated manifest produced no diagnostics; the conformance harness is not checking anything")
			}
			if !strings.Contains(strings.ToLower(formatDiagnostics(diags)), strings.ToLower(tc.wantMsg)) {
				t.Errorf("diagnostics do not mention %q:\n  %s", tc.wantMsg, formatDiagnostics(diags))
			}
		})
	}
}

func declaresSourceWrite(task proto.TaskDefinition) bool {
	for _, ref := range task.Writes {
		if ref.ID == proto.ResourceIDSources && ref.EffectiveScope() == proto.ResourceScopeProject {
			return true
		}
	}
	return false
}

func declaredOwnerRows(manifest *proto.Manifest) []ownerRow {
	var rows []ownerRow
	for name, task := range manifest.Tasks {
		if task.Declares == nil {
			continue
		}
		for id, output := range task.Declares.Outputs {
			rows = append(rows, ownerRow{
				task: name, id: id, kind: output.Kind, root: output.EffectiveRoot(),
				path: output.Path + output.PathFrom, optionalEmpty: output.OptionalEmpty,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].task+"\x00"+rows[i].id < rows[j].task+"\x00"+rows[j].id
	})
	return rows
}

func rowsString(rows []ownerRow) string {
	if len(rows) == 0 {
		return "\n  <none>"
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.String())
	}
	return "\n  " + strings.Join(out, "\n  ")
}

func sortedTaskNames(manifest *proto.Manifest) []string {
	names := make([]string, 0, len(manifest.Tasks))
	for name := range manifest.Tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
