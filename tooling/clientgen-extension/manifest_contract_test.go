package clientgen

import (
	"fmt"
	"os"
	"reflect"
	"slices"
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
// `putnami extensions validate` and the package-time gate do.
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

// TestExtensionManifestPassesFullValidation is the `putnami extensions
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
// Each generation task claims only what its emitter writes: client.gen.go for a
// Go client project, the src tree for a TypeScript one, and the client.gen.go
// an in-module target reports. No output preserves an authored file beside the
// generated one, because none claims a directory that holds one.
func TestDeclaredOutputOwnerTable(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "protocol-conformant-manifest", "every-declared-output-has-exactly-one-owner")
	spectest.Proves(t, clientgenFeature, "dynamic-output-ownership", "a-client-owns-only-its-generated-closure")
	manifest := loadExtensionManifest(t)
	want := []ownerRow{
		{task: "client-generate-go", id: "client", kind: proto.OutputKindFile, root: proto.OutputRootProject, path: "client.gen.go", optionalEmpty: true},
		{task: "client-generate-module", id: "client", kind: proto.OutputKindFile, root: proto.OutputRootProject, path: "moduleClientOutput", optionalEmpty: true},
		{task: "client-generate-ts", id: "client", kind: proto.OutputKindDirectory, root: proto.OutputRootProject, path: "src", optionalEmpty: true},
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
	for _, name := range sortedTaskNames(manifest) {
		for id, output := range manifest.Tasks[name].Declares.Outputs {
			if len(output.Preserves) != 0 || len(output.Excludes) != 0 {
				t.Errorf("task %q output %q carves out %v and %v; it claims only the generated closure, so nothing "+
					"authored lies inside it", name, id, output.Preserves, output.Excludes)
			}
		}
	}
	// The TypeScript tree holds only what the last generation wrote: the engine
	// empties it before the task runs and on a restore of an empty entry.
	if !manifest.Tasks["client-generate-ts"].Declares.Outputs["client"].Emptied {
		t.Error("client-generate-ts does not empty src; a file a previous contract generated would outlive it")
	}
}

// TestAConfigChosenOutputIsNeverClaimedAtALiteralPath keeps the in-module
// target honest. Its directory is chosen by the provider's committed
// clientgen.json, so a literal path would be a claim the configuration can
// move out from under. The task reports the file it wrote, and the output
// takes its path from that port. A client project's output is fixed by the
// project's language, so it is the one place a literal path is right.
func TestAConfigChosenOutputIsNeverClaimedAtALiteralPath(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "unclaimed-outputs-are-declared-unclaimed", "a-config-chosen-output-is-never-claimed-at-a-literal-path")
	manifest := loadExtensionManifest(t)
	task, ok := manifest.Tasks["client-generate-module"]
	if !ok || task.Declares == nil {
		t.Fatal("manifest task client-generate-module is missing or lost its v3 declaration")
	}
	for id, output := range task.Declares.Outputs {
		if output.PathFrom == "" {
			t.Errorf("client-generate-module declares output %q at the literal path %q; the directory is chosen by the "+
				"provider's clientgen.json, so it is declarable only through a pathFrom port", id, output.Path)
		}
		if _, reported := task.Outputs[output.PathFrom]; !reported {
			t.Errorf("client-generate-module declares output %q from the port %q, which the task does not report", id, output.PathFrom)
		}
	}
}

// TestNoClientOutputPolicesDrift pins that a generated client is built, not
// committed. A drift policy compares what a task writes with the bytes present
// before, which only means something for a file Git holds.
func TestNoClientOutputPolicesDrift(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "no-committed-client", "no-client-output-polices-drift")
	manifest := loadExtensionManifest(t)
	for _, name := range sortedTaskNames(manifest) {
		for id, output := range manifest.Tasks[name].Declares.Outputs {
			if output.DriftPolicy() != "" {
				t.Errorf("task %q output %q declares drift %q; a generated client is never committed, so there is "+
					"nothing to compare it with", name, id, output.DriftPolicy())
			}
		}
	}
}

// TestTaskEffectTable pins HONEST EFFECTS per task: what each task does beyond
// writing its declared outputs, and — for the effects a cache hit cannot
// reproduce — that the task is not cacheable.
//
// The generation tasks invoke emitters packaged beside the extension runtime or
// installed by the workspace, so none resolves a module over the network. The
// TypeScript one declares the Bun transpiler cache. The check reads committed
// files and spawns nothing, so it has no effect to declare.
func TestTaskEffectTable(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "honest-effects", "each-task-declares-exactly-the-effects-it-has")
	spectest.Proves(t, clientgenFeature, "honest-effects", "cacheable-generation-has-no-unreplayed-external-effect")
	spectest.Proves(t, clientgenFeature, "portable-emitter-resolution", "tasks-use-the-compiled-runtime-and-package-resolved-language-emitters")
	manifest := loadExtensionManifest(t)

	want := map[string][]string{
		"client-generate-go":        {},
		"client-generate-module":    {},
		"client-generate-ts":        {"toolchain-cache"},
		"clientgen-workspace-check": {},
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
// holds no bun of that release gets it from `putnami install`, which unpacks
// the archive @putnami/typescript pins under the Putnami home: the archive
// holds bun in a bun-<target> directory, one per Bun target. The emitter
// toolchain lists the program of each target after the host's own locations,
// so the emitter runs on a host where nobody installed Bun. It lists no
// provisioned candidate, since @putnami/clientgen answers for no bun pin.
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
		{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/bun/bun-{version}/bun-darwin-aarch64/bun"},
		{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/bun/bun-{version}/bun-darwin-x64/bun"},
		{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/bun/bun-{version}/bun-linux-aarch64/bun"},
		{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/bun/bun-{version}/bun-linux-x64/bun"},
		{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/bun/bun-{version}/bun-windows-aarch64/bun.exe"},
		{From: proto.RuntimeToolchainCandidatePutnamiHome, Path: "toolchains/bun/bun-{version}/bun-windows-x64/bun.exe"},
	}
	if !reflect.DeepEqual(emitter.Candidates, want) {
		t.Fatalf("typescriptEmitter candidates = %+v, want %+v", emitter.Candidates, want)
	}
}

// TestNoTaskWritesAnAuthoredSource pins that generation writes generated
// clients only, never the sources it reads. No task declares mutatesSources,
// the project-scoped sources write, the workspace-scoped authored-source write
// or the workspace-files effect.
func TestNoTaskWritesAnAuthoredSource(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "no-source-mutation",
		"every-generation-and-verification-task-leaves-authored-sources-alone")
	spectest.Proves(t, clientgenFeature, "no-source-mutation", "no-task-declares-an-authored-source-write")
	manifest := loadExtensionManifest(t)
	for _, name := range sortedTaskNames(manifest) {
		task := manifest.Tasks[name]
		if task.Declares.MutatesSources {
			t.Errorf("task %q declares mutatesSources; clientgen writes generated clients, not the sources it reads", name)
		}
		if declaresSourceWrite(task) {
			t.Errorf("task %q declares the project sources write", name)
		}
		for _, write := range task.Writes {
			if write.ID == "workspace-authored-sources" {
				t.Errorf("task %q claims the authored-source write resource", name)
			}
		}
		if slices.Contains(task.Declares.Effects, proto.EffectWorkspaceFiles) {
			t.Errorf("task %q declares the workspace-files effect; no clientgen task edits an authored source", name)
		}
	}
}

// TestClientGenerateReadsCommittedFilesAndBuildsNoProvider pins the
// generation command. It depends on no build: each task reads a committed
// contract, which the core resolves from the client's generatedFrom service or
// which the provider holds for its in-module target. It activates for every
// project, so a client project need not list this extension, and each step
// activates on the file that makes a project one of its kind. A project that
// matches no step plans nothing.
func TestClientGenerateReadsCommittedFilesAndBuildsNoProvider(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "spec-mediated-generation", "generation-reads-the-committed-contract-and-builds-no-provider")
	spectest.Proves(t, clientgenFeature, "client-owned-generation", "a-consumer-only-selection-generates-or-restores-its-client")
	manifest := loadExtensionManifest(t)

	command, ok := manifest.Commands["client-generate"]
	if !ok {
		t.Fatal("manifest declares no client-generate command")
	}
	if command.Activation != "workspace" || len(command.DependsOn) != 0 {
		t.Errorf("client-generate activation=%q dependsOn=%v, want workspace activation and no dependency: a client "+
			"project generates itself whatever extensions it lists, and generation builds no provider",
			command.Activation, command.DependsOn)
	}
	generatedFrom := map[string]string{"putnami.json": `"generatedFrom"`}
	want := []proto.PipelineStep{
		{ID: "go", Task: "client-generate-go", Activation: &proto.StepActivation{Files: []string{"go.mod"}, Contains: generatedFrom}},
		{ID: "ts", Task: "client-generate-ts", Activation: &proto.StepActivation{Files: []string{"package.json"}, Contains: generatedFrom}},
		{ID: "module", Task: "client-generate-module", Activation: &proto.StepActivation{Files: []string{"clientgen.json"}}},
	}
	if !reflect.DeepEqual(command.Run, want) {
		t.Errorf("client-generate runs %+v, want %+v", command.Run, want)
	}
	for _, step := range command.Run {
		task := manifest.Tasks[step.Task]
		for _, ref := range task.Reads {
			if ref.ID == "gen" {
				t.Errorf("task %q reads the gen resource; generation reads no file a build writes", step.Task)
			}
		}
		for _, glob := range task.Cache.Key.Files {
			if glob == ".gen" || strings.HasPrefix(glob, ".gen/") || strings.HasPrefix(glob, "git:.gen/") {
				t.Errorf("task %q keys on %q, a file a build writes", step.Task, glob)
			}
		}
	}

	check, ok := manifest.Commands["clientgen-check"]
	if !ok || check.Activation != "workspace-once" || len(check.DependsOn) != 0 {
		t.Errorf("clientgen-check = %+v (declared %t), want a workspace-once command that depends on nothing", check, ok)
	}
}

// TestLanguageCommandsWaitForClientGenerate reads the Go and TypeScript
// extension manifests. Their lifecycle commands depend on client-generate, so
// a generated client exists before a compiler, a linter or a packager reads
// it. The dependency is a plain command name, not a session barrier: it plans
// the project's own client-generate, which plans nothing where no step
// activates.
func TestLanguageCommandsWaitForClientGenerate(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "language-commands-wait-for-generation", "go-and-typescript-lifecycle-commands-depend-on-client-generate")
	spectest.Proves(t, clientgenFeature, "language-commands-wait-for-generation", "a-project-without-client-generate-plans-nothing-more")
	for _, manifestPath := range []string{"../../go/extension/putnami.extension.json", "../../typescript/extension/putnami.extension.json"} {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		manifest, diags := proto.ParseManifest(data)
		if manifest == nil || diag.HasErrors(diags) {
			t.Fatalf("parse %s: %s", manifestPath, formatDiagnostics(diags))
		}
		for _, name := range []string{"build", "test", "lint", "serve", "package"} {
			command, ok := manifest.Commands[name]
			if !ok {
				t.Errorf("%s declares no %s command", manifestPath, name)
				continue
			}
			if !slices.Contains(command.DependsOn, "client-generate") {
				t.Errorf("%s %s dependsOn = %v, want client-generate", manifestPath, name, command.DependsOn)
			}
			for _, dependency := range command.DependsOn {
				if strings.TrimLeft(dependency, "!^") == "client-generate" && dependency != "client-generate" {
					t.Errorf("%s %s depends on %q; a barrier or an upstream edge plans more than the project's own generation",
						manifestPath, name, dependency)
				}
			}
		}
	}
	for _, step := range loadExtensionManifest(t).Commands["client-generate"].Run {
		if step.Activation == nil {
			t.Errorf("client-generate step %q activates for every project; a project with no client would plan it", step.ID)
		}
	}
}

// TestProjectGeneratorsAreReadByTheClientProjectsDependents pins the
// declaration that orders a client's consumers after its generator. The
// generated client lies in the client project, or in the provider for an
// in-module target, and a project that depends on it compiles it. A resource
// conflicts only within one project, so only readByDependents on the client
// output makes the planner sequence the generator against those projects' jobs
// (protocols/extension ADR 0014). The planning half is asserted against the
// real planner in tooling/cli/internal/jobs/planner/clientgen_dependents_test.go.
func TestProjectGeneratorsAreReadByTheClientProjectsDependents(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "dynamic-output-ownership", "generated-outputs-are-read-by-the-client-projects-dependents")
	manifest := loadExtensionManifest(t)
	for _, name := range []string{"client-generate-go", "client-generate-module", "client-generate-ts"} {
		task, ok := manifest.Tasks[name]
		if !ok || task.Declares == nil {
			t.Fatalf("manifest task %q is missing or lost its v3 declaration", name)
		}
		client, ok := task.Declares.Outputs["client"]
		if !ok {
			t.Fatalf("task %q declares no client output", name)
		}
		if !client.ReadByDependents {
			t.Errorf("task %q client output does not declare readByDependents: a project that depends on the client "+
				"project can compile the client while %s rewrites it", name, name)
		}
		if client.EffectiveRoot() != proto.OutputRootProject {
			t.Errorf("task %q client output root = %q; readByDependents applies only under the project root", name, client.EffectiveRoot())
		}
	}
}

// TestValidateContributesTheWorkspaceGuard is the activation half of the native
// validation gate, read from the manifest that ships.
//
// The guard reaches a session through `validate`, with workspace-once
// activation so the planner never consults a project's extension list. The
// command depends on nothing, so it plans no generation: the guard reads the
// committed tree. The task declares a workspace-scoped read (it answers about
// the whole workspace) and is keyed on the input `git:**`: it reads the
// workspace's Git candidate cut and nothing else, so that cut is its whole
// read set, and it stores no output. The planning half — exactly one node for
// any selection, and none for another command — is asserted against the real
// planner in tooling/cli/internal/jobs/planner/clientgen_native_gate_test.go.
func TestValidateContributesTheWorkspaceGuard(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "validate-contributes-exactly-one-workspace-guard-for-any-selection")
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "the-guard-reads-the-git-candidate-cut-and-keys-on-it")
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "the-guard-task-is-reachable-only-from-validate-and-the-explicit-check-command")
	spectest.Proves(t, clientgenFeature, "native-validation-gate", "validate-plans-no-generation")
	manifest := loadExtensionManifest(t)

	command, ok := manifest.Commands["validate"]
	if !ok {
		t.Fatal("the manifest contributes no validate command; `putnami validate` runs no generated-client guard")
	}
	if command.Activation != "workspace-once" {
		t.Fatalf("validate activation = %q, want workspace-once; any other activation lets a project opt out of the "+
			"guard by not declaring this extension", command.Activation)
	}
	if len(command.DependsOn) != 0 {
		t.Fatalf("validate dependsOn = %v, want none: validate plans no generation", command.DependsOn)
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
	// A `git:` input needs the CLI contract whose key holds the executable bit;
	// the manifest carries the stamp the packager earns for it.
	if manifest.CLIContract != proto.RequiredCLIContract(manifest) {
		t.Fatalf("cliContract = %d, want the contract the manifest's vocabulary requires, %d",
			manifest.CLIContract, proto.RequiredCLIContract(manifest))
	}
	for _, phrase := range []string{"Git candidate cut", "`git:**`", "neither read nor keyed"} {
		if !strings.Contains(task.Description, phrase) {
			t.Fatalf("the guard description does not say what its key reads (%q): %s", phrase, task.Description)
		}
	}
	if len(task.Toolchains) != 0 {
		t.Fatalf("the guard binds toolchains %v; it generates nothing, so it needs no emitter", task.Toolchains)
	}
	for _, ref := range task.Writes {
		t.Fatalf("the guard declares the write %+v; it builds nothing and generates nothing", ref)
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
				for _, name := range []string{"client-generate-go", "client-generate-ts"} {
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
				task := manifest.Tasks["client-generate-go"]
				task.Declares.Outputs = map[string]proto.DeclaredOutput{
					"client": {Kind: proto.OutputKindFile, PathFrom: "moduleClientOutput"},
				}
				manifest.Tasks["client-generate-go"] = task
			},
			wantMsg: "which the task does not declare",
		},
		{
			// An effect outside the closed vocabulary is a protocol change, not
			// a free-form string.
			name: "effect outside the vocabulary",
			mutate: func(manifest *proto.Manifest) {
				task := manifest.Tasks["client-generate-ts"]
				task.Declares.Effects = []string{"filesystem"}
				manifest.Tasks["client-generate-ts"] = task
			},
			wantMsg: "invalid task effect",
		},
		{
			// A cacheable task cannot claim an effect a cache hit replays.
			name: "cache with an unreplayable effect",
			mutate: func(manifest *proto.Manifest) {
				task := manifest.Tasks["client-generate-go"]
				task.Declares.Effects = []string{"registry"}
				manifest.Tasks["client-generate-go"] = task
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
