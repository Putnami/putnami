package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/releaseset"
)

// The v3 task contract makes this extension's manifest
// load-bearing: a declared output is what the cache captures and restores, and
// two tasks claiming one path is a correctness bug that surfaces in a
// consumer's workspace at plan time, far from here. These tests are the
// extension's own conformance harness — they run the SAME predicates the CLI
// and the extension SDK run (proto.FullValidateManifest, which calls
// ValidateTaskContracts and ValidateOutputOwnership), against the REAL
// manifest, so a declaration that would be rejected downstream is rejected in
// this project's own test run first.

// loadExtensionManifest parses the shipped putnami.extension.json exactly as
// `putnami dev extension validate` and the package-time gate do.
func loadExtensionManifest(t *testing.T) *proto.Manifest {
	t.Helper()
	path := filepath.Join("..", "..", "putnami.extension.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	m, diags := proto.ParseManifest(data)
	if m == nil || diag.HasErrors(diags) {
		t.Fatalf("parse extension manifest: %s", formatDiagnostics(diags))
	}
	return m
}

func formatDiagnostics(diags []diag.Diagnostic) string {
	if len(diags) == 0 {
		return "<none>"
	}
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.String())
	}
	return strings.Join(msgs, "\n  ")
}

// TestNoPackageTaskDeclaresARestoreMode pins that no packaging task carries the
// `restoreMode` opt-in: the field is still accepted by the protocol so
// manifests that spelled it keep loading, but it decides nothing:
// a task earns declared capture by declaring what it owns, in every command.
func TestNoPackageTaskDeclaresARestoreMode(t *testing.T) {
	// Read the manifest BYTES rather than the decoded policy: the field is
	// deprecated in the protocol type, and what this test pins is that the
	// document does not spell it, not what a decoder makes of it.
	assertNoTaskDeclaresARestoreMode(t, filepath.Join("..", "..", "putnami.extension.json"))
}

func assertNoTaskDeclaresARestoreMode(t *testing.T, manifestPath string) {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	var document struct {
		// `cache` is a boolean OR an object in this contract, so it is decoded
		// as `any` and only the object form can carry the field.
		Tasks map[string]struct {
			Cache any `json:"cache"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse extension manifest: %v", err)
	}
	for name, task := range document.Tasks {
		policy, isObject := task.Cache.(map[string]any)
		if !isObject {
			continue
		}
		if mode, declared := policy["restoreMode"]; declared {
			t.Errorf("task %q declares restoreMode = %v; cache eligibility is the task's own declaration, "+
				"in every command, and the field decides nothing", name, mode)
		}
	}
}

// PackageDocker copies .gen/public after compile. Declaring the same project
// resource as its generator makes other commands' generators wait until that
// copy finishes; pipeline dependencies alone only order this command's writer.
func TestPackageDockerReadsGeneratorOwnedGenResource(t *testing.T) {
	m := loadExtensionManifest(t)
	for _, access := range []struct {
		name string
		refs []proto.ResourceRef
	}{
		{"build-generate writes", m.Tasks["build-generate"].Writes},
		{"package-docker reads", m.Tasks["package-docker"].Reads},
	} {
		if !slices.ContainsFunc(access.refs, func(ref proto.ResourceRef) bool {
			return ref.ID == "gen" && ref.EffectiveScope() == proto.ResourceScopeProject
		}) {
			t.Errorf("%s = %+v, want the shared project-scoped gen resource", access.name, access.refs)
		}
	}

	// Resource ordering attaches readers to their functional writer ancestor.
	// Keep docker -> compile -> generate so packaging consumes its own generation.
	steps := map[string]proto.PipelineStep{}
	for _, step := range m.Commands["package"].Run {
		steps[step.ID] = step
	}
	if steps["generate"].Task != "build-generate" {
		t.Errorf("package generate task = %q, want build-generate", steps["generate"].Task)
	}
	if steps["compile"].Task != "build-compile" || !slices.Contains(steps["compile"].DependsOn, "generate") {
		t.Errorf("package compile step = %+v, want build-compile depending on generate", steps["compile"])
	}
	if steps["docker"].Task != "package-docker" || !slices.Contains(steps["docker"].DependsOn, "compile") {
		t.Errorf("package docker step = %+v, want package-docker depending on compile", steps["docker"])
	}
}

// TestExtensionManifestPassesFullValidation is the `putnami dev extension
// validate` gate as a unit test: the real manifest must produce ZERO
// diagnostics under the comprehensive protocol entry point, which includes the
// v3 task-contract harness (exactness, one owner per output, honest effects).
func TestExtensionManifestPassesFullValidation(t *testing.T) {
	m := loadExtensionManifest(t)
	if diags := proto.FullValidateManifest(m); len(diags) > 0 {
		t.Fatalf("manifest is not conformant:\n  %s", formatDiagnostics(diags))
	}
}

// TestManifestExercisesTheV3TaskContract keeps the migration from silently
// regressing to v2: a manifest whose tasks lose their `declares` blocks would
// still validate (v3 is additive), so the version itself is asserted.
func TestManifestExercisesTheV3TaskContract(t *testing.T) {
	m := loadExtensionManifest(t)
	if got := proto.ManifestProtocolVersion(m); got != proto.ProtocolVersionV3 {
		t.Fatalf("manifest protocol version = %d, want %d (v3 task declarations)", got, proto.ProtocolVersionV3)
	}
}

func TestReleaseSetPlanIsTransportedToNpmPackageAndPublishTasks(t *testing.T) {
	m := loadExtensionManifest(t)
	for _, taskName := range []string{"package-npm", "publish-npm"} {
		port, ok := m.Tasks[taskName].Inputs[releaseset.ContextParamName]
		if !ok || port.From != "params" {
			t.Errorf("task %q release-set input = %+v, present=%v; want from=params", taskName, port, ok)
		}
	}
}

func TestImagePackagePathSkipsCompilerAndOrdersGraphBase(t *testing.T) {
	m := loadExtensionManifest(t)
	steps := map[string]proto.PipelineStep{}
	for _, step := range m.Commands["package"].Run {
		steps[step.ID] = step
	}
	image := steps["image"]
	if image.Task != "package-docker" || image.If != "params.image" || !slices.Contains(image.DependsOn, "^image") {
		t.Fatalf("image package step = %+v, want shared package task depending on upstream image", image)
	}
	docker := steps["docker"]
	if !slices.Contains(docker.DependsOn, "^image") || !strings.Contains(docker.If, "!params.image") {
		t.Fatalf("workload docker step = %+v, want upstream image ordering and image-project exclusion", docker)
	}
	for _, id := range []string{"generate", "transpile", "types", "compile"} {
		if !strings.Contains(steps[id].If, "!params.image") {
			t.Errorf("compiler step %q is not excluded for image projects: %+v", id, steps[id])
		}
	}
	if got := m.Commands["publish"].DependsOn; len(got) != 1 || got[0] != "package" {
		t.Fatalf("publish dependencies = %v, want package only so image publish cannot enter build compilers", got)
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
	// drift is the declaration's drift policy (protocols/extension ADR 0004).
	// The committed outputs build-generate writes carry it — the generated
	// client and the OpenAPI contract: this task is the first writer of both
	// in a session, so it is the only place the pre-write bytes can be
	// compared with.
	drift string
}

func (r ownerRow) String() string {
	return fmt.Sprintf("%s.%s = %s %s:%s optionalEmpty=%t drift=%q", r.task, r.id, r.kind, r.root, r.path, r.optionalEmpty, r.drift)
}

// TestDeclaredOutputOwnerTable pins the declared output ownership as a
// table. Every filesystem region this extension produces has exactly one owning
// task, and the table is what makes a change to that mapping a reviewed change
// rather than a silent one.
//
// The two collisions the TypeScript pipelines actually contain, and how they
// were resolved:
//
//   - Four build steps share ONE per-command output directory (the shared
//     directory baseline subtraction exists to disambiguate). Resolved by exact
//     disjoint subpaths: transpile owns lib/, types owns types/, compile owns
//     compile/, and package-npm/package-docker own npm/ and docker/. Each reads
//     its siblings' trees and writes only its own.
//   - `.gen/config-schema.json` (the config-extract fallback location) sits
//     inside the .gen subtree build-generate produces. Resolved by SINGLE
//     PRODUCER: build-generate owns .gen whole, so config-extract-exec claims
//     only the committed schema/ files and never a slice of .gen.
//   - test-env-up's two rows are the only INVOCATION-scoped outputs here (an
//     earlier migration). They collide with nothing by construction: they live in
//     the orchestrator's private per-invocation tree, not in the project or the
//     command-output directory, and they are never captured or restored. The
//     `bindings` row is the extension's only `sensitive` output — the database
//     credential — and its `lease` sibling is deliberately NOT sensitive,
//     because the finalizer's whole job is to read a record that cannot carry
//     one.
func TestDeclaredOutputOwnerTable(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-client-drift", "the-generate-client-output-declares-fail-on-drift")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "committed-openapi-drift", "the-generate-openapi-output-declares-fail-on-drift")
	m := loadExtensionManifest(t)

	want := []ownerRow{
		{task: "build-compile", id: "compile", kind: "directory", root: "command-output", path: "compile", optionalEmpty: true},
		{task: "build-generate", id: "capabilities", kind: "file", root: "project", path: "schema/capabilities.json", optionalEmpty: true},
		{task: "build-generate", id: "client", kind: "directory", root: "project", path: "clientOutput", optionalEmpty: true, drift: "fail"},
		{task: "build-generate", id: "gen", kind: "directory", root: "project", path: ".gen"},
		{task: "build-generate", id: "openapi", kind: "file", root: "project", path: "schema/openapi.json", optionalEmpty: true, drift: "fail"},
		{task: "build-transpile", id: "lib", kind: "directory", root: "command-output", path: "lib", optionalEmpty: true},
		{task: "build-types", id: "types", kind: "directory", root: "command-output", path: "types", optionalEmpty: true},
		{task: "config-extract-exec", id: "jsonSchema", kind: "file", root: "project", path: "schema/config.jsonschema.json", optionalEmpty: true},
		{task: "config-extract-exec", id: "schema", kind: "file", root: "project", path: "schema/config.json", optionalEmpty: true},
		{task: "package-docker", id: "docker", kind: "directory", root: "command-output", path: "docker", optionalEmpty: true},
		{task: "package-npm", id: "npm", kind: "directory", root: "command-output", path: "npm", optionalEmpty: true},
		{task: "publish-docker", id: "published-image", kind: "file", root: "command-output", path: "docker/published-image.json", optionalEmpty: true},
		{task: "test-env-up", id: "bindings", kind: "runtime-file", root: "invocation", path: "database/bindings.json", optionalEmpty: true},
		{task: "test-env-up", id: "lease", kind: "file", root: "invocation", path: "database/lease.json", optionalEmpty: true},
		{task: "test-run", id: "coverage", kind: "file", root: "command-output", path: "lcov.info", optionalEmpty: true},
		{task: "test-run", id: "featureVerification", kind: "file", root: "command-output", path: "putnami-feature-verification.json", optionalEmpty: true},
		{task: "test-run", id: "junit", kind: "file", root: "command-output", path: "results.junit.xml", optionalEmpty: true},
	}

	got := declaredOwnerRows(m)
	if len(got) != len(want) {
		t.Fatalf("declared output count = %d, want %d\n got: %s\nwant: %s",
			len(got), len(want), rowsString(got), rowsString(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("owner row %d = %s, want %s", i, got[i], want[i])
		}
	}

	// The manifest-local half of ONE OWNER PER OUTPUT, run exactly as the CLI
	// and the SDK run it.
	if diags := proto.ValidateOutputOwnership(m); len(diags) > 0 {
		t.Fatalf("declared outputs are not exclusively owned:\n  %s", formatDiagnostics(diags))
	}
}

func declaredOwnerRows(m *proto.Manifest) []ownerRow {
	var rows []ownerRow
	for name, task := range m.Tasks {
		if task.Declares == nil {
			continue
		}
		for id, output := range task.Declares.Outputs {
			rows = append(rows, ownerRow{
				task:          name,
				id:            id,
				kind:          output.Kind,
				root:          output.EffectiveRoot(),
				path:          output.Path + output.PathFrom,
				optionalEmpty: output.OptionalEmpty,
				drift:         output.Drift,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].task != rows[j].task {
			return rows[i].task < rows[j].task
		}
		return rows[i].id < rows[j].id
	})
	return rows
}

func rowsString(rows []ownerRow) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.String())
	}
	return "\n  " + strings.Join(out, "\n  ")
}

// TestPackageTasksWriteOnlyInsideTheDirectoriesTheyOwn pins the property that
// puts this extension's packaging under ordinary declared capture.
//
// `<command-output>/metadata.json` used to be a channel INDEX that package-npm
// and package-docker each read-merge-wrote and publish read back. Two producers
// of one file is what ONE OWNER PER OUTPUT forbids, so it stayed unclaimed by
// both — and the whole `package` command lost cache restore with it. Each task
// now records its channel inside the directory it already owns, and the index is
// derived over those records, so this extension writes NOTHING at the package
// root: it has no archive channel, so it does not even own the archive
// publication manifest that survives there for the other packagers.
func TestPackageTasksWriteOnlyInsideTheDirectoriesTheyOwn(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "package-channel-index", "no-package-task-claims-a-path-outside-the-directory-it-owns")
	m := loadExtensionManifest(t)
	commands := commandsByTask(m)

	producers := []string{"package-docker", "package-npm"}
	owned := map[string]string{"package-docker": "docker", "package-npm": "npm"}
	for _, name := range producers {
		task, ok := m.Tasks[name]
		if !ok {
			t.Fatalf("manifest task %q is missing", name)
		}
		if task.Declares == nil {
			t.Fatalf("task %q lost its v3 declaration", name)
		}
		for id, output := range task.Declares.Outputs {
			if output.Root != proto.OutputRootCommandOutput {
				continue
			}
			if output.Path != owned[name] {
				t.Errorf("task %q declares command-output %q as %q; a TypeScript packager owns only %q, "+
					"and a second writer at the package root is a merge point this contract forbids",
					name, output.Path, id, owned[name])
			}
		}
	}
	// The two producers remain in one command, which is what made a shared file
	// at the package root a collision in the first place.
	if !sharesAnyCommand(commands["package-npm"], commands["package-docker"]) {
		t.Errorf("package-npm (%v) and package-docker (%v) no longer share a command",
			commands["package-npm"], commands["package-docker"])
	}
}

func sharesAnyCommand(a, b []string) bool {
	for _, left := range a {
		for _, right := range b {
			if left == right {
				return true
			}
		}
	}
	return false
}

// TestTaskEffectTable pins HONEST EFFECTS per task: what each task does beyond
// writing its declared outputs, and — for the effects a cache hit cannot
// reproduce — that the task is not cacheable. A task missing from the table
// declares no effects.
func TestTaskEffectTable(t *testing.T) {
	m := loadExtensionManifest(t)

	want := map[string][]string{
		// The cache commands mutate the ts-types scratch and Bun's
		// machine-global package cache — the toolchain-cache effect exactly —
		// and no cache hit could reproduce that, so both are uncacheable.
		"cache-clean-exec":    {"toolchain-cache"},
		"cache-gc-exec":       {"toolchain-cache"},
		"build-compile":       {"toolchain-cache"},
		"build-transpile":     {"toolchain-cache"},
		"build-types":         {"toolchain-cache"},
		"config-extract-exec": {"toolchain-cache"},
		"deps-upgrade-exec":   {"network", "toolchain-cache", "workspace-files"},
		"package-docker":      {"network", "toolchain-cache"},
		"publish-docker":      {"network", "registry"},
		"publish-npm":         {"network", "registry"},
		"run-app":             {"process"},
		"serve-app":           {"network", "process"},
		"test-run":            {"toolchain-cache"},
		// The database test environment runs a container (process) and may pull
		// its pinned image (network). Both are external effects a cache hit
		// could not reproduce, which is also why the pair is uncacheable — and
		// the producer must be, since a task with an invocation-scoped output
		// is never captured. The finalizer only removes containers, so it
		// declares `process` alone.
		"test-env-up":   {"network", "process"},
		"test-env-down": {"process"},
		// workspace-fetch shapes the manifests workspace-install shapes and
		// downloads into bun's machine-global cache.
		"workspace-fetch-exec":   {"network", "toolchain-cache", "workspace-files"},
		"workspace-install-exec": {"network", "toolchain-cache", "workspace-files"},
		// workspace-sync edits the root package.json workspaces array and each
		// member's package.json name. It touches no toolchain cache and no
		// network: the sync is a pure rewrite of manifests core already
		// resolved, so a `projects sync` on an offline machine still converges.
		"workspace-sync-exec": {"workspace-files"},
		"build-generate":      {"toolchain-cache"},
	}

	for name, task := range m.Tasks {
		got := []string(nil)
		if task.Declares != nil {
			got = append(got, task.Declares.Effects...)
		}
		sort.Strings(got)
		expected := want[name]
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

// TestOnlyTheBiomeFixTasksMutateSources pins the check/fix split. `lint-all` is
// the read-only pass (`biome check`, never writes) and must declare neither
// source mutation nor the write resource; the two writer phases the fix
// pipeline runs (`biome format --write`, `biome lint --write --unsafe`) must
// declare BOTH, because that write resource — not the flag — is what the
// planner serializes conflicting jobs on.
func TestOnlyTheBiomeFixTasksMutateSources(t *testing.T) {
	m := loadExtensionManifest(t)
	fixTasks := map[string]bool{"lint-format": true, "lint-check": true}

	for name, task := range m.Tasks {
		mutates := task.Declares != nil && task.Declares.MutatesSources
		if mutates != fixTasks[name] {
			t.Errorf("task %q mutatesSources = %t, want %t", name, mutates, fixTasks[name])
		}
		if mutates != declaresSourceWrite(task) {
			t.Errorf("task %q mutatesSources = %t but sources write resource = %t; the two must agree",
				name, mutates, declaresSourceWrite(task))
		}
	}

	lintAll, ok := m.Tasks["lint-all"]
	if !ok {
		t.Fatal("manifest task \"lint-all\" is missing")
	}
	if lintAll.Declares == nil {
		t.Error("lint-all must carry a v3 declaration stating it produces nothing and mutates nothing")
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

// The former TestMultiCommandTasksDeclareNoProjectScopedOutputs guard is
// deleted: the plan-time owner check now counts ONE manifest task scheduled by
// several commands as one owner (sameManifestTask exemption in
// tooling/cli/internal/jobs/plan_contract.go, pinned there by
// TestPlanContract_OneTaskScheduledBySeveralCommandsIsOneOwner), which is what
// makes build-generate's project-rooted .gen ownership plannable under
// multi-command invocations.

// commandsByTask maps each task to the sorted set of commands whose pipeline
// references it.
func commandsByTask(m *proto.Manifest) map[string][]string {
	byTask := map[string]map[string]bool{}
	for cmdName, cmd := range m.Commands {
		for _, step := range cmd.Run {
			if step.Task == "" {
				continue
			}
			if byTask[step.Task] == nil {
				byTask[step.Task] = map[string]bool{}
			}
			byTask[step.Task][cmdName] = true
		}
	}
	out := make(map[string][]string, len(byTask))
	for task, set := range byTask {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		out[task] = names
	}
	return out
}

// TestGenerateDeclaresTheClientOutputPort pins the runtime↔manifest link the
// clients half of the ownership decision rests on: the generated client's
// directory is chosen by the project's clientgen config, so it is declarable
// only through a data output port, and the consumer reads it back by name. A
// rename on either side would silently stop the client from being captured.
func TestGenerateDeclaresTheClientOutputPort(t *testing.T) {
	m := loadExtensionManifest(t)
	generate, ok := m.Tasks["build-generate"]
	if !ok {
		t.Fatal("manifest task \"build-generate\" is missing")
	}
	if _, ok := generate.Outputs[GeneratedClientOutputPort]; !ok {
		t.Fatalf("build-generate does not declare the %q output port the runtime reports the generated client directory on",
			GeneratedClientOutputPort)
	}
	// The .gen + clients declarations landed together with the plan-time
	// same-manifest-task exemption (one task scheduled by several commands is
	// one owner, not several — plan_contract.go, found by this slice). Pin the
	// whole ownership decision: .gen owned whole, client via the port.
	if generate.Declares == nil {
		t.Fatal("build-generate lost its v3 declaration")
	}
	gen, ok := generate.Declares.Outputs["gen"]
	if !ok || gen.Kind != proto.OutputKindDirectory || gen.Path != ".gen" || gen.EffectiveRoot() != proto.OutputRootProject {
		t.Fatalf("gen ownership = %+v, want the whole project-rooted .gen directory", gen)
	}
	client, ok := generate.Declares.Outputs["client"]
	if !ok || client.PathFrom != GeneratedClientOutputPort || !client.OptionalEmpty {
		t.Fatalf("client ownership = %+v, want optional-empty pathFrom %q", client, GeneratedClientOutputPort)
	}
	// The client project's own .gen, its dependency install and its project
	// document belong to the workspace (protocol ADR 0005).
	if got, want := proto.DecidablePreserves(client), []string{".gen", "node_modules", "putnami.json"}; !slices.Equal(got, want) {
		t.Errorf("client preserves %v, want %v", got, want)
	}
}

// TestInfraAggregationIsWiredIntoBuild pins the three properties that make
// extension-owned infra aggregation work at all.
//
// It replaced a CLI gate that ran unconditionally on every build, watched every
// generator step in the workload's dependency closure, and decided by tag
// matching that a TypeScript workload runs without HTTP/2. As a pipeline step
// it must therefore: (1) exist under `build` and nowhere else, because the
// aggregated manifest is a build artifact; (2) run AFTER this project's own
// build steps — the dependency closure is covered transitively, because
// build-generate already depends on `^generate`; and (3) never be cached,
// because its inputs are OTHER projects' committed manifests, which no
// per-project cache key covers. A cached aggregation would replay a stale
// deployability manifest from a hit.
func TestInfraAggregationIsWiredIntoBuild(t *testing.T) {
	m := loadExtensionManifest(t)

	task, ok := m.Tasks["build-infra"]
	if !ok {
		t.Fatal("manifest task \"build-infra\" is missing")
	}
	if task.Cache.IsEnabled() {
		t.Error("build-infra is cacheable; a cache hit would replay a stale aggregated manifest, " +
			"because the task's real inputs are other projects' committed infra/requirements.json")
	}
	if task.Declares == nil {
		t.Fatal("build-infra lost its v3 declaration")
	}
	if len(task.Declares.Outputs) != 0 {
		t.Errorf("build-infra declares outputs %+v; it writes into <project>/.gen, which build-generate "+
			"owns whole as its single producer", task.Declares.Outputs)
	}

	var commands []string
	for name, cmd := range m.Commands {
		for _, step := range cmd.Run {
			if step.Task == "build-infra" {
				commands = append(commands, name)
			}
		}
	}
	if len(commands) != 1 || commands[0] != "build" {
		t.Errorf("build-infra commands = %v, want only [build]", commands)
	}

	build, ok := m.Commands["build"]
	if !ok {
		t.Fatal("manifest command \"build\" is missing")
	}
	var step *proto.PipelineStep
	for i := range build.Run {
		if build.Run[i].ID == "infra" {
			step = &build.Run[i]
		}
	}
	if step == nil {
		t.Fatal("build pipeline has no \"infra\" step; nothing emits the aggregated manifest")
	}
	if step.Task != "build-infra" {
		t.Errorf("infra step task = %q, want build-infra", step.Task)
	}
	if !slices.Contains(step.DependsOn, "generate") {
		t.Errorf("infra step dependsOn = %v, want it to include \"generate\" — the committed "+
			"requirements are only current once that step has run", step.DependsOn)
	}
}

// TestTestEnvironmentIsGatedAndKeyedOnTheDependencyClosure pins the manifest
// half of the C9 repairs for the database test environment.
//
// The runtime walks the CLOSURE — dbtestenv.ClosureDatabases reads every
// member's committed infra/requirements.json, exactly as the infra-aggregation
// task does — so both the plan-time gate and the cache key must ask the same
// question:
//
//   - ACTIVATION. A project whose only database requirement is transitive got no
//     `test-env` node at all under a project-local `files` gate, silently losing
//     its provisioning; `closureFiles` gates on what the task actually reads.
//   - CACHE KEY. The test task folds the PRODUCING ACTION's digest, so the
//     producer's declared inputs are the test verdict's identity. A `project`
//     port covered only this project's manifest, leaving a requirements-only
//     change in a dependency invisible to the key and serving a stored verdict
//     against a different provisioned environment.
func TestTestEnvironmentIsGatedAndKeyedOnTheDependencyClosure(t *testing.T) {
	m := loadExtensionManifest(t)

	const manifestPath = "infra/requirements.json"

	test, ok := m.Commands["test"]
	if !ok {
		t.Fatal("manifest command \"test\" is missing")
	}
	gated := 0
	for _, step := range test.Run {
		if step.Task != "test-env-up" && step.Task != "test-env-down" {
			continue
		}
		gated++
		if step.Activation == nil {
			t.Fatalf("step %q lost its activation gate", step.ID)
		}
		if len(step.Activation.Files) != 0 {
			t.Errorf("step %q gates on its OWN files %v; a transitive-only requirement would lose provisioning",
				step.ID, step.Activation.Files)
		}
		if len(step.Activation.ClosureFiles) != 1 || step.Activation.ClosureFiles[0] != manifestPath {
			t.Errorf("step %q closureFiles = %v, want [%s] — the set the runtime reads",
				step.ID, step.Activation.ClosureFiles, manifestPath)
		}
		if step.Task == "test-env-down" {
			if step.Finalizes == nil {
				t.Fatalf("step %q lost its finalizes relation", step.ID)
			}
			if got, want := step.Finalizes.PruneIf, "!params.infra-down"; got != want {
				t.Errorf("step %q finalizes.pruneIf = %q, want %q; an explicit teardown must keep the lifecycle",
					step.ID, got, want)
			}
		}
	}
	if gated != 2 {
		t.Fatalf("gated %d test-environment steps, want the producer and its finalizer", gated)
	}

	up, ok := m.Tasks["test-env-up"]
	if !ok {
		t.Fatal("manifest task \"test-env-up\" is missing")
	}
	port, ok := up.Inputs["requirements"]
	if !ok {
		t.Fatal("test-env-up lost its requirements input port")
	}
	if port.From != proto.TaskInputFromClosure {
		t.Errorf("requirements port from = %q, want %q; a project port leaves the consumer's key blind "+
			"to a dependency's committed requirements", port.From, proto.TaskInputFromClosure)
	}
	if len(port.Files) != 1 || port.Files[0] != manifestPath {
		t.Errorf("requirements port files = %v, want [%s]", port.Files, manifestPath)
	}
	key := proto.DeriveTaskCacheKey(up.Inputs)
	if len(key.ClosureFiles) != 1 || key.ClosureFiles[0] != manifestPath {
		t.Errorf("derived cache key closureFiles = %v, want [%s]", key.ClosureFiles, manifestPath)
	}
	// The manifest must not ALSO declare it as a `project` port: the closure
	// contains the seed, so a second declaration states the same fact twice and
	// invites the two to drift.
	for _, own := range key.Files {
		if own == manifestPath {
			t.Error("the requirements manifest is declared as a project port as well; " +
				"the closure already covers the seed project")
		}
	}
}

// TestPackageDockerBindsTheCompileTarget pins channel scoping on the
// manifest side: the compile step the `package` pipeline schedules exists ONLY
// to feed the image, and the platform set it produces must be decided by keyed,
// plan-time parameters.
//
// The image `platform` is the sharp one. It selects the single bun target the
// compile builds, so if it did not reach the cache key a linux/amd64 entry could
// be restored for a linux/arm64 request and the assembled image would carry an
// executable for the wrong architecture — a failure that only appears when the
// container runs.
func TestPackageDockerBindsTheCompileTarget(t *testing.T) {
	m := loadExtensionManifest(t)

	pkg, ok := m.Commands["package"]
	if !ok {
		t.Fatal("manifest command \"package\" is missing")
	}
	var compileStep *proto.PipelineStep
	for i, step := range pkg.Run {
		if step.Task == "build-compile" {
			compileStep = &pkg.Run[i]
		}
	}
	if compileStep == nil {
		t.Fatal("the package pipeline no longer schedules a compile step")
	}
	// The binding's premise: under `package`, a compile happens for the docker
	// channel and nothing else. If this gate ever widens, the compile serves a
	// second consumer and scoping it to the image's target would starve it.
	// (`!params.image` narrows further: a first-class image project derives its
	// artifact from a published digest and compiles no bun binary at all.)
	if got, want := compileStep.If, "params.docker && !params.image"; got != want {
		t.Errorf("package compile step if = %q, want %q; the docker channel is the only reason this compile "+
			"exists, and that is what lets it build one target", got, want)
	}

	compile, ok := m.Tasks["build-compile"]
	if !ok {
		t.Fatal("manifest task \"build-compile\" is missing")
	}
	for _, name := range []string{"compile-target", "docker", "platform"} {
		input, ok := compile.Inputs[name]
		if !ok || input.From != proto.TaskInputFromParams {
			t.Errorf("build-compile input %q = %+v, present=%t; the resolved bun target set must come from "+
				"plan-time cache-key parameters", name, input, ok)
		}
	}
	derived := proto.DeriveTaskCacheKey(compile.Inputs)
	for _, name := range []string{"compile-target", "docker", "platform"} {
		if !slices.Contains(derived.Params, name) {
			t.Errorf("derived cache key params = %v, missing %q; a target set the key cannot see lets a "+
				"four-target entry answer a one-target request", derived.Params, name)
		}
	}

	// The parameter must be nameable where the image is: `--platform` is a flag
	// of the `package` command, and the compile step reads the same resolved value
	// the docker step assembles with.
	if _, ok := pkg.Flags["platform"]; !ok {
		t.Error("the `package` command declares no `platform` flag; the compile could not be bound to the image")
	}
}

// TestProbeSourceWitnessCoversTheImportScan pins the manifest's workspace
// inputs to the files the import scan reads.
//
// The probe's DependencySources answer is derived from source imports
// (scanPackageImports), and core decides whether to reuse a recorded answer by
// re-hashing exactly the patterns declared here. A source extension the scan
// reads and this list does not witness makes that answer survive the edit that
// changed it: an import added to a package that already links the target keeps
// the edge attributed `declared`, and `putnami deps prune` then removes a
// dependency the sources read.
func TestProbeSourceWitnessCoversTheImportScan(t *testing.T) {
	manifest := loadExtensionManifest(t)
	if manifest.Workspace == nil {
		t.Fatal("manifest declares no workspace adapter")
	}
	declared := make(map[string]bool, len(manifest.Workspace.Inputs))
	for _, pattern := range manifest.Workspace.Inputs {
		declared[pattern] = true
	}
	missing := make([]string, 0, len(packageSourceExtensions))
	for extension := range packageSourceExtensions {
		if !declared["**/*"+extension] {
			missing = append(missing, extension)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("scanned source extensions with no workspace input witness: %v", missing)
	}
}

// The probe attributes every edge it reports (DependencySources), and core
// asks for the attribution only from an adapter that declares it. A probe that
// computes it without the declaration answers nothing: ServeProbe strips it
// from every answer that was not asked, and the declared-edge and visibility
// checks then report the whole ecosystem as unattributed.
func TestWorkspaceAdapterDeclaresEdgeAttribution(t *testing.T) {
	manifest := loadExtensionManifest(t)
	if manifest.Workspace == nil || !manifest.Workspace.DependencySources {
		t.Fatal("the workspace adapter must declare dependencySources: the probe attributes its edges")
	}
}
