package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/codegen"
	"go.putnami.dev/go/extension/internal/jobs/configmerge"
	"go.putnami.dev/go/extension/internal/toolchain"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/infraagg"
	"go.putnami.dev/sdk/extension/releaseset"
	"gopkg.in/yaml.v3"

	"go.putnami.dev/protocol/features/spectest"
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
// retired restoreMode opt-in. `restoreMode` is still accepted by the protocol so
// older manifests keep loading, but it decides nothing:
// a task earns declared capture by declaring what it owns, in every command. A
// manifest that sets it again is stating a rule that is not enforced.
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

func TestReleaseSetPlanIsTransportedToGoPackageAndPublishTasks(t *testing.T) {
	m := loadExtensionManifest(t)
	for _, taskName := range []string{"package-go", "publish-go"} {
		port, ok := m.Tasks[taskName].Inputs[releaseset.ContextParamName]
		if !ok || port.From != "params" {
			t.Errorf("task %q release-set input = %+v, present=%v; want from=params", taskName, port, ok)
		}
	}
}

func TestImagePackagePathSkipsCompilerAndOrdersGraphBase(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "image-input-producers", "image-packaging-requires-build-producers-without-go-compilation")
	m := loadExtensionManifest(t)
	steps := map[string]proto.PipelineStep{}
	for _, step := range m.Commands["package"].Run {
		steps[step.ID] = step
	}
	prerequisites := m.Commands["package"].SessionPrerequisites
	if len(prerequisites) != 1 || prerequisites[0].Command != "build" || prerequisites[0].If != "params.image" || len(prerequisites[0].DependsOn) != 0 {
		t.Fatalf("image packaging must materialize its declared build producers without a gate cycle: %+v", prerequisites)
	}
	image := steps["image"]
	if image.Task != "package-docker" || image.If != "params.image" || !slicesContains(image.DependsOn, "^image") {
		t.Fatalf("image package step = %+v, want shared package task depending on upstream image", image)
	}
	docker := steps["docker"]
	if !slicesContains(docker.DependsOn, "^image") || !strings.Contains(docker.If, "!params.image") {
		t.Fatalf("workload docker step = %+v, want upstream image ordering and image-project exclusion", docker)
	}
	for _, id := range []string{"tidy", "generate", "describe", "cross-compile"} {
		if !strings.Contains(steps[id].If, "!params.image") {
			t.Errorf("compiler step %q is not excluded for image projects: %+v", id, steps[id])
		}
	}
	if got := m.Commands["publish"].DependsOn; len(got) != 1 || got[0] != "package" {
		t.Fatalf("publish dependencies = %v, want package only; image packaging owns its conditional producer prerequisite", got)
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestEveryTaskCarriesAV3Declaration pins that the migration is COMPLETE. v3 is
// additive per task, so a task that quietly loses (or never gains) its
// `declares` block keeps v2 inferred capture while the rest of the manifest is
// declared — the mixed state this contract exists to end.
func TestEveryTaskCarriesAV3Declaration(t *testing.T) {
	m := loadExtensionManifest(t)
	for _, name := range sortedTaskNames(m) {
		if m.Tasks[name].Declares == nil {
			t.Errorf("task %q has no v3 declaration; every task in this manifest must state its outputs, effects and source mutation (an empty object states \"none of the three\")", name)
		}
	}
}

// The `stable` input this used to pin is gone with the `--stable` flag: a
// version is derived from git per line (ADR 0021 §8), so a build has no
// stable/prerelease MODE left to key. What still keys the binary bytes —
// version-var and every compile flag — is pinned by
// TestLibraryCompilePruningConditionPreservesExplicitBuildEvidence below.

func TestTestExecDeclaresEveryRuntimeConsumedParam(t *testing.T) {
	m := loadExtensionManifest(t)
	for _, name := range []string{
		"coverage", "enforce-coverage", "race", "timeout", "short", "run",
		"count", "shuffle", "bench", "benchtime", "test-verbose", "test-json",
		"package-parallel", "parallel", "failfast", "coverprofile", "covermode", "outputdir",
		"coverhtml", "coverage-threshold", "coverage-scope",
	} {
		input, ok := m.Tasks["test-exec"].Inputs[name]
		if !ok || input.From != "params" {
			t.Errorf("test-exec input %q = %+v, present=%t; the test runtime consumes this parameter", name, input, ok)
		}
	}
}

// TestCoverageScopeKeysTheTestTask pins the cache half of ADR 0008. The scope
// decides which packages a batched member's profile instruments, so it decides
// the bytes of the declared coverage.out: an entry measured under one scope
// must never be restored for a run under the other. The declared default keeps
// today's batch behavior until a later decision flips it.
func TestCoverageScopeKeysTheTestTask(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "project-scoped-coverage", "the-coverage-scope-keys-the-test-task")
	m := loadExtensionManifest(t)
	flag, ok := m.Commands["test"].Flags["coverage-scope"]
	if !ok {
		t.Fatal("the test command declares no coverage-scope flag")
	}
	if flag.Type != "string" || flag.Default != "batch" || !slices.Equal(flag.Choices, []string{"batch", "project"}) {
		t.Fatalf("coverage-scope flag = %+v, want a string defaulting to batch with choices [batch project]", flag)
	}
	input, ok := m.Tasks["test-exec"].Inputs["coverage-scope"]
	if !ok || input.From != proto.TaskInputFromParams {
		t.Fatalf("test-exec input coverage-scope = %+v, present=%t; want a params input keying the task", input, ok)
	}
}

// A root go.work/go.work.sum change selects every Go project, so every cached
// task that reads module/toolchain state must hash those same workspace files.
// Project-relative "go.work" patterns do not cover the workspace root and would
// let an impacted run restore a verdict from the previous workspace graph.
func TestModuleAwareCachedTasksKeyWorkspaceGraphFiles(t *testing.T) {
	m := loadExtensionManifest(t)
	wantFiles := []string{"go.work", "go.work.sum"}
	for _, taskName := range []string{
		"test-exec", "build-tidy", "build-generate", "build-describe",
		"build-compile", "build-cross-compile", "lint-golangci-fix",
		"lint-golangci-readonly", "lint-staticcheck", "config-extract-exec",
	} {
		task, ok := m.Tasks[taskName]
		if !ok {
			t.Errorf("module-aware task %q is missing", taskName)
			continue
		}
		if !task.Cache.IsEnabled() {
			t.Errorf("module-aware task %q is no longer cached; update this cache-identity contract", taskName)
			continue
		}
		input, ok := task.Inputs["workspaceModules"]
		if !ok || input.From != proto.TaskInputFromWorkspace {
			t.Errorf("%s workspaceModules = %+v, present=%t; want a workspace-scoped input", taskName, input, ok)
			continue
		}
		key := proto.DeriveTaskCacheKey(task.Inputs)
		if len(key.WorkspaceFiles) != len(wantFiles) {
			t.Errorf("%s workspace cache files = %v, want %v", taskName, key.WorkspaceFiles, wantFiles)
			continue
		}
		for _, file := range wantFiles {
			if !containsString(key.WorkspaceFiles, file) {
				t.Errorf("%s workspace cache files = %v, missing %q", taskName, key.WorkspaceFiles, file)
			}
		}
	}
}

// TestTidyKeysOnTheProxyAndCachesNoFailedRun pins the two halves of build-tidy's
// cacheability contract, which decide each other.
//
// GOPROXY decides WHETHER the phase runs a command: `off` forbids every module
// download, and `go mod tidy` resolves a module graph wider than the build list
// a workspace install warms, so the phase reports a no-op instead. That no-op is
// an ordinary cached success, so the variable has to be in the key — otherwise
// the offline entry would answer a run that can resolve modules and silently
// stop tidying it.
//
// The task stays NON-deterministic for the opposite case. A tidy that ran and
// failed reports a skip, and the orchestrator caches a skip only for a task that
// declares `cache.deterministic` (isCacheableResult in
// tooling/cli/internal/jobs/executor.go), so a transient network failure is
// never recorded. Declaring determinism here would cache it.
func TestTidyKeysOnTheProxyAndCachesNoFailedRun(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "offline-tidy-is-a-cacheable-no-op",
		"goproxy-is-a-declared-cache-key-input")
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"build-tidy-keys-on-the-offline-signal")
	m := loadExtensionManifest(t)
	task, ok := m.Tasks["build-tidy"]
	if !ok {
		t.Fatal("build-tidy is missing")
	}
	// The offline signal turns GOPROXY off in every go command
	// (toolchain.OfflineDependencies) whatever the inherited GOPROXY says, so
	// it decides the no-op as much as GOPROXY does.
	key := proto.DeriveTaskCacheKey(task.Inputs)
	for _, name := range []string{"GOPROXY", proto.OfflineDependenciesEnv} {
		if input, found := task.Inputs[name]; !found || input.From != proto.TaskInputFromEnv {
			t.Errorf("build-tidy %s input = %+v, present=%t; want an env-scoped input", name, input, found)
		}
		if !containsString(key.Env, name) {
			t.Errorf("build-tidy env cache inputs = %v, missing %s: an offline no-op would answer an online run", key.Env, name)
		}
	}
	if task.Cache == nil || !task.Cache.IsEnabled() {
		t.Fatalf("build-tidy cache = %+v, want enabled: the offline no-op has nowhere to land", task.Cache)
	}
	if task.Cache.Deterministic {
		t.Error("build-tidy declares cache.deterministic, which would cache the skip a transient tidy failure reports")
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
	// excludes is the declaration's ceded subpaths, joined by "," so the table
	// stays a comparable value. A carve-out moves which task's cache entry
	// carries a subtree, so it belongs in the reviewed table rather than beside
	// it.
	excludes string
	// drift is the declaration's drift policy (protocols/extension ADR 0004).
	// The committed generated client is the ONE output that carries it: this
	// task is the first writer of that directory in a session, so it is the
	// only place the pre-write bytes can be compared with.
	drift string
}

func (r ownerRow) String() string {
	return fmt.Sprintf("%s.%s = %s %s:%s optionalEmpty=%t excludes=[%s] drift=%q",
		r.task, r.id, r.kind, r.root, r.path, r.optionalEmpty, r.excludes, r.drift)
}

// TestDeclaredOutputOwnerTable pins the declared output ownership as a
// table. Every filesystem region this extension declares has exactly one owning
// task, and the table is what makes a change to that mapping a reviewed change
// rather than a silent one.
//
// The collisions the Go pipelines actually contain, and how each was resolved:
//
//   - `<project>/.gen` has four writers (build-generate, build-describe, the two
//     config-merge tasks, and config-extract's fallback path). Resolved by SINGLE
//     PRODUCER plus CEDED SUBPATHS: build-generate owns the subtree whole
//     except the subpaths it excludes (cededGenSubpaths), and everything else
//     writes into generate-owned territory instead of claiming a slice of it.
//     What that costs: a write inside .gen survives a cache hit only when
//     generate's own snapshot already contained it, and generate runs BEFORE
//     describe. Anything describe produces that a LATER task consumes must
//     therefore be owned by describe — which is why the bundle is ceded rather
//     than written into a territory whose owner cannot capture it. The
//     carve-out and the claim are two halves of one decision: dropping either
//     one makes the plan illegal or the bundle uncapturable. `.gen/conf` is
//     ceded for the OPPOSITE ordering: config-merge runs BEFORE generate, so
//     generate's snapshot would adopt whatever merged file is on disk and its
//     restore would delete the file whenever the snapshot lacks it, while a
//     config-merge hit would reproduce nothing. Both config-merge tasks
//     declare the merged file: the test variant by its literal path
//     (APP_ENV is pinned there), the build variant through the mergedConfig
//     port, because two pathFrom outputs on ONE port name collide at plan
//     time for two manifest tasks planned for the same project.
//   - build-compile and build-cross-compile both write `bin/` into the
//     per-command output directory. Resolved by COMMAND DISJOINTNESS: they
//     never appear in one command (compile only under `build`, cross-compile only
//     under `package`), so they resolve to different directories.
//   - The three package channels share one per-command output directory.
//     Resolved by EXACT DISJOINT SUBPATHS: `go/`, `archives/`, `docker/`. Each
//     records its channel INSIDE its own subpath, so the records need no
//     row of their own and no packager writes where another can see it.
//   - `<command-output>/metadata.json` has ONE writer, package-archives: it is
//     the archive publication manifest, not the channel index it used to be.
//   - The committed `schema/config.json` pair has three writers (build-generate,
//     build-describe, config-extract-exec). Resolved by SINGLE DECLARED OWNER:
//     config-extract-exec, the one task whose cache hit must reproduce it.
//   - test-env-up's two rows are the only INVOCATION-scoped outputs here (an
//     earlier migration). They collide with nothing by construction: they live in
//     the orchestrator's private per-invocation tree, not in the project or the
//     command-output directory, and they are never captured or restored. The
//     `bindings` row is the extension's only `sensitive` output — the database
//     credential — and its `lease` sibling is deliberately NOT sensitive,
//     because the finalizer's whole job is to read a record that cannot carry
//     one.
//
// TestCommittedSidecarsAreDeliberatelyUnclaimed pins the footprints that are
// deliberately owned by NO task, and
// TestTheArchivePackagerOwnsTheArchivePublicationManifest pins the one that
// stopped being one.
func TestDeclaredOutputOwnerTable(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "committed-client-drift", "the-describe-client-output-declares-fail-on-drift")
	m := loadExtensionManifest(t)

	want := []ownerRow{
		{task: "build-compile", id: "bin", kind: "directory", root: "command-output", path: "bin", optionalEmpty: true},
		{task: "build-cross-compile", id: "bin", kind: "directory", root: "command-output", path: "bin", optionalEmpty: true},
		{task: "build-describe", id: "client", kind: "directory", root: "project", path: "clientOutputs", optionalEmpty: true, drift: "fail"},
		{task: "build-describe", id: "clientgen", kind: "directory", root: "project", path: ".gen/clientgen", optionalEmpty: true},
		{task: "build-describe", id: "design", kind: "directory", root: "project", path: ".gen/design", optionalEmpty: true},
		{task: "build-describe", id: "migrationBundle", kind: "directory", root: "project", path: ".gen/migration-bundle", optionalEmpty: true},
		{task: "build-describe", id: "migrations", kind: "file", root: "project", path: ".gen/migrations.json", optionalEmpty: true},
		{task: "build-describe", id: "schema", kind: "directory", root: "project", path: ".gen/schema", optionalEmpty: true},
		{task: "build-generate", id: "gen", kind: "directory", root: "project", path: ".gen", excludes: strings.Join(cededGenSubpaths, ",")},
		{task: "config-extract-exec", id: "jsonSchema", kind: "file", root: "project", path: "schema/config.jsonschema.json", optionalEmpty: true},
		{task: "config-extract-exec", id: "schema", kind: "file", root: "project", path: "schema/config.json", optionalEmpty: true},
		// The merged config file, inside the ceded .gen/conf. The build
		// variant's path carries the ambient APP_ENV suffix, so it is a port.
		{task: "config-merge-exec", id: "merged", kind: "file", root: "project", path: configmerge.MergedConfigPort, optionalEmpty: true},
		{task: "config-merge-test-exec", id: "merged", kind: "file", root: "project", path: ".gen/conf/.env.test.yaml", optionalEmpty: true},
		{task: "package-archives", id: "archives", kind: "directory", root: "command-output", path: "archives", optionalEmpty: true},
		// The archive publication manifest. One producer — this task — so
		// it is declared and restored with the archives it describes.
		{task: "package-archives", id: "publication-manifest", kind: "file", root: "command-output", path: "metadata.json", optionalEmpty: true},
		// The deployment declaration, inside the ceded .gen/deployment.json.
		// Required, so a run that writes none caches nothing.
		{task: "package-deployment", id: "deployment", kind: "file", root: "project", path: ".gen/deployment.json"},
		{task: "package-docker", id: "docker", kind: "directory", root: "command-output", path: "docker", optionalEmpty: true},
		{task: "package-go", id: "go", kind: "directory", root: "command-output", path: "go", optionalEmpty: true},
		{task: "publish-docker", id: "published-image", kind: "file", root: "command-output", path: "docker/published-image.json", optionalEmpty: true},
		{task: "test-env-up", id: "bindings", kind: "runtime-file", root: "invocation", path: "database/bindings.json", optionalEmpty: true},
		{task: "test-env-up", id: "lease", kind: "file", root: "invocation", path: "database/lease.json", optionalEmpty: true},
		{task: "test-exec", id: "coverage", kind: "file", root: "command-output", path: "coverage.out", optionalEmpty: true},
		{task: "test-exec", id: "coverageHtml", kind: "file", root: "command-output", path: "coverage.html", optionalEmpty: true},
		// The reserved feature-verification report, one per project under
		// batching too; optional because most projects bind no checks.
		{task: "test-exec", id: "featureVerification", kind: "file", root: "command-output", path: "putnami-feature-verification.json", optionalEmpty: true},
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
				excludes:      strings.Join(output.Excludes, ","),
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

// cededGenSubpaths is the reviewed carve-out table: every subpath
// build-generate cedes out of <project>/.gen, in the order the manifest
// normalizes it to.
//
// It exists because a cede is only half a decision. build-generate runs FIRST,
// so its capture predates everything build-describe writes under .gen; ceding a
// subpath stops generate from adopting and resurrecting those bytes, and a
// SECOND statement decides whether anything captures them at all.
// cededGenOwner records that second statement for every row.
var cededGenSubpaths = []string{
	".gen/.describe.lock",
	".gen/clientgen",
	".gen/conf",
	".gen/config-deps.json",
	".gen/deployment.json",
	".gen/design",
	".gen/migration-bundle",
	".gen/migrations.json",
	".gen/schema",
}

// cededClaim names one declared output of one manifest task that claims a
// ceded subpath — the whole subpath, a file strictly inside it, or (for a
// pathFrom output) a path the task reports at run time.
type cededClaim struct {
	task   string
	output string
	// required marks a claim whose output is not optionalEmpty: the claimant
	// writes the path on every success, and a run that writes nothing caches
	// nothing, so a cache hit never restores a file the current inputs do not
	// produce.
	required bool
}

// cededGenOwner maps each ceded subpath to the declared outputs that claim it,
// or to nil for the ones deliberately claimed by NOBODY.
//
// An unclaimed cede is a statement, not an oversight: protocol ADR 0003 says so
// explicitly, and each of the two here is run-scoped scratch that a LATER task
// never reads. Leaving them inside generate's capture is what was wrong — their
// presence depended on whether the tree happened to be clean, which alone makes
// a `cache.deterministic` task disagree with itself between two equivalent runs.
var cededGenOwner = map[string][]cededClaim{
	// A lockedfile mutex, taken and released within one describe run. It stays
	// at this fixed per-project path on purpose: `build` and `test` in one
	// invocation must serialize on the SAME mutex, and a per-command output
	// directory would give them two.
	".gen/.describe.lock": nil,
	".gen/clientgen":      {{task: "build-describe", output: "clientgen"}},
	// The merged config file, claimed by BOTH config-merge tasks: the
	// test variant at the literal .gen/conf/.env.test.yaml, the build variant
	// at the environment-suffixed path its mergedConfig port reports. The two
	// claims are disjoint files inside one ceded directory, never the
	// directory itself; the .manifest.json sidecar beside them is claimed by
	// nobody (a generatedAt timestamp, and no reader).
	".gen/conf": {
		{task: "config-merge-exec", output: "merged"},
		{task: "config-merge-test-exec", output: "merged"},
	},
	// The dependency-config fragment the describe binary emits and the SAME
	// describe job folds into schema/config.json before it finishes.
	".gen/config-deps.json": nil,
	// The workload's deployment declaration, written by a package step that
	// runs after generate. It is REQUIRED rather than optionalEmpty: the task
	// removes the file when it writes none (a library, an aggregate with an
	// error finding), and an empty optional capture would restore nothing over
	// an earlier declaration, leaving it in place.
	".gen/deployment.json":  {{task: "package-deployment", output: "deployment", required: true}},
	".gen/design":           {{task: "build-describe", output: "design"}},
	".gen/migration-bundle": {{task: "build-describe", output: "migrationBundle"}},
	".gen/migrations.json":  {{task: "build-describe", output: "migrations"}},
	".gen/schema":           {{task: "build-describe", output: "schema"}},
}

// cededSubpathContains reports whether a literal declared path is the ceded
// subpath or sits strictly inside it — the containment the CLI's ownership
// check applies (cedesRegion in protocols/extension/task_contract.go).
func cededSubpathContains(ceded, declared string) bool {
	return declared == ceded || strings.HasPrefix(declared, ceded+"/")
}

// TestGenCedesEverySubpathDescribeProducesAfterGenerate pins the statements
// every carve-out out of <project>/.gen rests on. Any one of them alone is
// wrong:
//
//  1. build-generate EXCLUDES the subpath. Without the carve-out a claim on it
//     makes the plan illegal (two owners of one region), and generate's capture
//     keeps adopting whatever an earlier build left there — so a hit resurrects
//     it for a project whose current sources produce nothing.
//  2. The claimant DECLARES a project-rooted, optionalEmpty output at that
//     path (or a file strictly inside it, or a port that resolves to one), or
//     the table states that nobody does and why. Without a claim nobody
//     captures the subtree and a run serving both tasks from cache restores a
//     .gen without it. The path is the documented
//     contract path consumers read, never a private staging copy. A claim the
//     table marks required declares a required output instead, and the
//     deployment step that holds it is pinned by
//     TestDeploymentDeclarationIsAGatedCachedPackageStep.
//  3. Every command that schedules generate also schedules describe AFTER it.
//     Generate's restore leaves the ceded subtrees exactly as it finds them
//     (protocol ADR 0003, 2026-09-14 amendment), so what sits there after a
//     generate hit is whatever the previous describe left; only describe's
//     later restore (or execution) makes it what the current sources produce.
//     The config-merge cede needs no such ordering: generate neither writes
//     nor reads .gen/conf, and the reader (test-exec) is ordered after
//     config-merge by the test pipeline itself.
func TestGenCedesEverySubpathDescribeProducesAfterGenerate(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "merged-config-survives-a-cache-hit", "build-generate-cedes-gen-conf-to-the-config-merge-tasks")
	m := loadExtensionManifest(t)

	gen := m.Tasks["build-generate"].Declares.Outputs["gen"]
	if !slices.Equal(gen.Excludes, cededGenSubpaths) {
		t.Errorf("build-generate gen excludes = %v, want %v", gen.Excludes, cededGenSubpaths)
	}

	for _, ceded := range cededGenSubpaths {
		claims, recorded := cededGenOwner[ceded]
		if !recorded {
			t.Errorf("ceded subpath %q has no reviewed owner decision; add it to cededGenOwner", ceded)
			continue
		}
		if len(claims) == 0 {
			// Claimed by nobody on purpose. The only thing to assert is that no
			// task quietly claimed it after all, which the owner table would have
			// caught, so assert the statement that is easy to break: generate
			// still cedes it (checked above) and no other task claims it.
			for _, name := range sortedTaskNames(m) {
				task := m.Tasks[name]
				if name == "build-generate" || task.Declares == nil {
					continue
				}
				for otherID, output := range task.Declares.Outputs {
					if output.EffectiveRoot() == proto.OutputRootProject && cededSubpathContains(ceded, output.Path) {
						t.Errorf("%s claims %q as output %q, but the table records it as claimed by nobody",
							name, ceded, otherID)
					}
				}
			}
			continue
		}
		for _, claim := range claims {
			task, ok := m.Tasks[claim.task]
			if !ok || task.Declares == nil {
				t.Errorf("manifest task %q (claimant of %q) is missing or has no declaration", claim.task, ceded)
				continue
			}
			output, ok := task.Declares.Outputs[claim.output]
			if !ok {
				t.Errorf("%s declares no %q output; nothing captures the ceded %q", claim.task, claim.output, ceded)
				continue
			}
			if output.EffectiveRoot() != proto.OutputRootProject || output.OptionalEmpty == claim.required {
				want := "an optionalEmpty"
				if claim.required {
					want = "a required"
				}
				t.Errorf("%s.%s = %+v, want %s project-rooted output inside %q", claim.task, claim.output, output, want, ceded)
				continue
			}
			if output.PathFrom != "" {
				// The path is known only at run time; the runtime's own tests pin
				// that the port reports a file inside the ceded subpath
				// (configmerge.TestRun_ReportsMergedConfigPortProjectRelativeOnSuccess).
				continue
			}
			if !cededSubpathContains(ceded, output.Path) {
				t.Errorf("%s.%s declares %q, which is not the ceded %q or a path inside it", claim.task, claim.output, output.Path, ceded)
			}
		}
	}

	for _, command := range sortedKeysOf(m.Commands) {
		if !commandSchedulesTask(m, command, "build-generate") {
			continue
		}
		describeStep := stepForTask(m, command, "build-describe")
		if describeStep.ID == "" {
			t.Errorf("command %q schedules build-generate without build-describe; the ceded subpaths would never be restored", command)
			continue
		}
		generate := stepForTask(m, command, "build-generate")
		if !stepDependsOn(m, command, describeStep, generate.ID) {
			t.Errorf("command %q runs build-describe (%s) without depending on build-generate (%s); "+
				"the ceded subtrees would not be guaranteed to reflect the current sources once generate ran",
				command, describeStep.ID, generate.ID)
		}
		// Control: the reachability helper must discriminate. Generate never
		// depends on describe, so asking the question the other way round has to
		// answer no — otherwise the assertion above would pass for any pipeline.
		if stepDependsOn(m, command, generate, describeStep.ID) {
			t.Errorf("command %q: build-generate reportedly depends on build-describe, so the ordering check is vacuous", command)
		}
	}
}

// stepForTask returns the command's step that runs task, or a zero step.
func stepForTask(m *proto.Manifest, command, task string) proto.PipelineStep {
	for _, step := range m.Commands[command].Run {
		if step.Task == task {
			return step
		}
	}
	return proto.PipelineStep{}
}

// stepDependsOn reports whether step reaches id through this command's
// dependsOn edges. Cross-project dependencies ("^name") are not ordering edges
// inside one command, so they are ignored.
func stepDependsOn(m *proto.Manifest, command string, step proto.PipelineStep, id string) bool {
	byID := map[string]proto.PipelineStep{}
	for _, candidate := range m.Commands[command].Run {
		byID[candidate.ID] = candidate
	}
	seen := map[string]bool{}
	var reaches func(proto.PipelineStep) bool
	reaches = func(from proto.PipelineStep) bool {
		for _, dep := range from.DependsOn {
			if strings.HasPrefix(dep, "^") || seen[dep] {
				continue
			}
			seen[dep] = true
			if dep == id {
				return true
			}
			if next, ok := byID[dep]; ok && reaches(next) {
				return true
			}
		}
		return false
	}
	return reaches(step)
}

// TestCompileAndCrossCompileNeverShareACommand pins the premise the bin/
// ownership rests on. Both tasks declare the same command-output path; that
// is legal only because they are scheduled by disjoint command sets, so the
// per-command output directory each resolves to is a different directory. If
// they ever land in one command, the declarations become a real collision and
// this test says so before the planner does.
func TestCompileAndCrossCompileNeverShareACommand(t *testing.T) {
	m := loadExtensionManifest(t)
	commands := commandsByTask(m)

	if got := commands["build-compile"]; strings.Join(got, ",") != "build" {
		t.Errorf("build-compile commands = %v, want [build]", got)
	}
	if got := commands["build-cross-compile"]; strings.Join(got, ",") != "package" {
		t.Errorf("build-cross-compile commands = %v, want [package]", got)
	}
	if sharesAnyCommand(commands["build-compile"], commands["build-cross-compile"]) {
		t.Errorf("build-compile (%v) and build-cross-compile (%v) now share a command; "+
			"their identical bin/ declarations would resolve to one directory",
			commands["build-compile"], commands["build-cross-compile"])
	}
}

// TestOrdinaryBuildCompilesForTheHostOnly pins the nature+intent contract of
// the nature+intent rule at the level a manifest can state it: WHICH task ordinary
// `build` schedules.
//
// The 4-platform matrix used to be `build`'s unconditional default, and no step
// of `build` consumed the binaries it produced — 240 discard cross-compiles in
// one Putnami Cloud validation gate, 45% of its physical time. The matrix is now
// reachable only through the distribution intent (`package`, which keeps
// build-cross-compile) or through the explicit `platforms`/`--target`
// parameters. A step that moved `build` back onto build-cross-compile would
// restore the matrix silently, so the mapping is pinned rather than described.
func TestOrdinaryBuildCompilesForTheHostOnly(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "declared-compile-matrix", "ordinary-build-never-schedules-the-cross-compile-task")
	m := loadExtensionManifest(t)

	for _, step := range m.Commands["build"].Run {
		if step.Task == "build-cross-compile" {
			t.Fatalf("build step %q schedules build-cross-compile; ordinary `build` compiles for the "+
				"host and reaches the platform matrix only through `platforms`/`--target`", step.ID)
		}
	}
	if !commandSchedulesTask(m, "build", "build-compile") {
		t.Error("the `build` pipeline no longer schedules build-compile; ordinary build must still produce compile evidence")
	}
	if !commandSchedulesTask(m, "package", "build-cross-compile") {
		t.Error("the `package` pipeline no longer schedules build-cross-compile; the release matrix belongs to distribution (distribution scoping owns narrowing it)")
	}
}

// TestLibraryCompilePruningConditionPreservesExplicitBuildEvidence pins the
// declarative half of the compile-omission rule. The planner may omit an ordinary library compile
// only for the canonical top-level validation trio; every parameter consumed
// by the compile task must turn the step back on so tags, target platforms, and
// other explicitly requested evidence are never mistaken for the host default.
// Race is shared with test, so its resolved values must be compared across the
// two commands: either command alone may enable a different Go source set.
func TestLibraryCompilePruningConditionPreservesExplicitBuildEvidence(t *testing.T) {
	m := loadExtensionManifest(t)
	var compile proto.PipelineStep
	for _, step := range m.Commands["build"].Run {
		if step.ID == "compile" {
			compile = step
			break
		}
	}
	if compile.ID == "" {
		t.Fatal("build pipeline has no compile step")
	}

	for _, fragment := range []string{
		"project.type != 'library'",
		"commands.build", "commands.lint", "commands.test",
		"params.race != commandParams.test.race",
	} {
		if !strings.Contains(compile.If, fragment) {
			t.Errorf("build compile condition %q is missing %q", compile.If, fragment)
		}
	}

	// These are the params whose values the ordinary compile task declares as
	// cache-key inputs. If one changes the evidence but disappears from this
	// condition, a combined gate could prune the only task that observes it.
	for _, name := range []string{
		"platforms", "target", "ldflags", "gcflags", "asmflags", "tags",
		"race", "trimpath", "buildmode", "cgo", "buildvcs", "version-var",
	} {
		if !strings.Contains(compile.If, "params."+name) {
			t.Errorf("build compile condition %q does not preserve explicit %q evidence", compile.If, name)
		}
	}

	// False is meaningful for cgo (it forces CGO_ENABLED=0) and for buildvcs (it
	// strips the VCS stamp), and zero is still an explicit -p request. Presence
	// checks keep those falsy values from being confused with absence.
	for _, fragment := range []string{"params.cgo != null", "params.buildvcs != null", "params.p != null"} {
		if !strings.Contains(compile.If, fragment) {
			t.Errorf("build compile condition %q is missing presence check %q", compile.If, fragment)
		}
	}
}

// TestPlatformSelectionParamsAreCacheKeyInputs is the cache-identity half of
// the nature+intent rule, and it is the one that makes the change SAFE rather than merely
// smaller.
//
// The resolved platform set decides the bytes a compile produces. Every input
// that can move it — the `platforms` project option / `--platforms` flag and
// `--target` — must therefore be a plan-time PARAMETER the orchestrator resolved
// and keyed, never something the task discovers for itself by reading
// putnami.json at execution time (the shape `ReadGoEntrypoint` uses, and the
// reason this pin exists). DeriveTaskCacheKey lifts every `from: "params"` port
// into Key.Params, which is exactly what the executor hashes, so asserting the
// derived key is asserting the real cache identity: change the platforms config
// and the key moves.
func TestPlatformSelectionParamsAreCacheKeyInputs(t *testing.T) {
	m := loadExtensionManifest(t)

	for _, name := range []string{"platforms", "target"} {
		input, ok := m.Tasks["build-compile"].Inputs[name]
		if !ok || input.From != "params" {
			t.Errorf("build-compile input %q = %+v, present=%t; the resolved platform set must be a plan-time "+
				"cache-key parameter, not a value the task reads for itself", name, input, ok)
		}
	}

	derived := proto.DeriveTaskCacheKey(m.Tasks["build-compile"].Inputs)
	for _, name := range []string{"platforms", "target"} {
		if !containsString(derived.Params, name) {
			t.Errorf("derived cache key params = %v, missing %q; a platform request that does not reach the "+
				"key lets a stored verdict be served for a platform set it was never built for",
				derived.Params, name)
		}
	}

	// The user-facing half: a parameter only reaches the plan if the command
	// declares it. A task input with no flag behind it can still be set from
	// project options, but `--platforms` would silently do nothing.
	if _, ok := m.Commands["build"].Flags["platforms"]; !ok {
		t.Error("the `build` command declares no `platforms` flag; the opt-in must be reachable from the CLI, not only from putnami.json")
	}

	// The DEFAULT platform set is the machine, and no parameter carries the
	// machine. build-compile therefore declares the `hostPlatform` runtime input,
	// which is what puts GOOS/GOARCH in its key. Other key components differ
	// between platforms only as a side effect of what they identify: the runtime
	// toolchain identity, and the implementation digest of an installed
	// extension that ships platform binaries. A side effect is not a contract,
	// and entries are shared between laptops and CI.
	// The same gap is PRE-EXISTING on test-exec and is left to its own change.
	hostInput, ok := m.Tasks["build-compile"].Inputs["hostPlatform"]
	if !ok || hostInput.From != "runtime" {
		t.Errorf("build-compile input hostPlatform = %+v, present=%t; want a runtime input, or a darwin-built "+
			"entry stays restorable on a linux runner", hostInput, ok)
	}
	if !containsString(derived.Runtime, "hostPlatform") {
		t.Errorf("derived cache key runtime inputs = %v, missing hostPlatform", derived.Runtime)
	}

	// cache.deterministic claims the result is fully determined by the key. That
	// is only true once the host is IN the key, so the two must move together.
	if policy := m.Tasks["build-compile"].Cache; policy == nil || !policy.Deterministic {
		t.Error("build-compile is no longer declared deterministic; with hostPlatform in the key its result " +
			"IS fully determined by the key, and dropping the claim would silently stop caching its skips")
	}

}

// TestHostBuildParametersArePinnedOrKeyed is the cache-identity half of the
// shared host build, and it is DERIVED from the resolver rather than from a list written here.
//
// One helper — toolchain.ResolveHostBuild — now decides the program every host
// `go build` of this extension produces, and it reads argv first, then the
// resolved parameter bag. So for each task whose job builds with it, every name
// in toolchain.HostBuildParamNames must be either PINNED in the task's own argv
// (a parameter cannot move an argv value) or DECLARED as a `from: "params"`
// cache-key input. A name that is neither lets two runs that build different
// programs share one entry, which a hand-written list of names misses as soon
// as the resolver reads one more. The bag really
// can carry them: the planner applies the project's options["@putnami/go"]
// layer for whatever command it is planning, without filtering to that
// command's flags.
//
// build-cross-compile is in the table because the distribution build consumes
// the same flags; its argv pins -trimpath and cgo=false, so those two are the
// ones it does not declare.
func TestHostBuildParametersArePinnedOrKeyed(t *testing.T) {
	m := loadExtensionManifest(t)

	for _, name := range []string{"build-compile", "build-cross-compile", "build-describe"} {
		task, ok := m.Tasks[name]
		if !ok {
			t.Errorf("manifest task %q is missing; it builds with the shared host build configuration", name)
			continue
		}
		derived := proto.DeriveTaskCacheKey(task.Inputs)
		pinned := pinnedArgvFlags(task.Args)
		for _, param := range toolchain.HostBuildParamNames {
			if _, isPinned := pinned[param]; isPinned {
				if _, declared := task.Inputs[param]; declared {
					t.Errorf("task %q both pins %q in argv and declares it as an input; the declaration keys a "+
						"value the job can never read", name, param)
				}
				continue
			}
			input, declared := task.Inputs[param]
			if !declared || input.From != "params" {
				t.Errorf("task %q neither pins %q in its argv nor declares it as a `from: params` input, but "+
					"toolchain.ResolveHostBuild reads it; the task would cache bytes its key does not describe",
					name, param)
				continue
			}
			if !containsString(derived.Params, param) {
				t.Errorf("derived %q cache key params = %v, missing %q", name, derived.Params, param)
			}
		}
	}

	// -ldflags is the one flag the shared configuration does NOT carry: it
	// reaches the link action alone, so describe builds without it and still
	// shares every compile action with the stamped compile build. The two tasks
	// that DO link with it must key it, and describe must not have to.
	for _, name := range []string{"build-compile", "build-cross-compile"} {
		if input, ok := m.Tasks[name].Inputs["ldflags"]; !ok || input.From != "params" {
			t.Errorf("task %q input ldflags = %+v, present=%t; the linking phases read it from the bag", name, input, ok)
		}
	}
	if _, ok := m.Tasks["build-describe"].Inputs["ldflags"]; ok {
		t.Error("build-describe declares ldflags; the describe binary is built without linker flags on purpose, " +
			"and keying them would re-link it on every version bump")
	}
}

// pinnedArgvFlags returns the flag names a task's manifest argv fixes, in the
// spelling cli.ParseFlags gives them: `--flag value`, `--flag=value`, a bare
// `--flag`, and `--no-flag`. Matching is exact — `--phase` pins "phase", not
// "p".
func pinnedArgvFlags(args []string) map[string]struct{} {
	pinned := make(map[string]struct{})
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name := strings.TrimLeft(arg, "-")
		if index := strings.IndexByte(name, '='); index >= 0 {
			name = name[:index]
		}
		name = strings.TrimPrefix(name, "no-")
		pinned[name] = struct{}{}
	}
	return pinned
}

// TestPackageChannelPlatformsAreCacheKeyInputs is the cache-identity half of
// the channel-scoping rule, and — like its nature+intent sibling above — it is what makes the
// narrowing SAFE rather than merely cheaper.
//
// build-cross-compile no longer compiles a fixed matrix: it compiles what the
// invocation's package channels consume. Every input to that decision must
// therefore be a plan-time parameter the orchestrator resolved and keyed. The
// image `platform` is the sharpest case — it decides the ONE platform the docker
// channel compiles, so without it in the key a linux/amd64 entry would be
// restored for a linux/arm64 request and the image would carry a binary for the
// wrong architecture.
func TestPackageChannelPlatformsAreCacheKeyInputs(t *testing.T) {
	m := loadExtensionManifest(t)

	crossCompile := m.Tasks["build-cross-compile"]
	for _, name := range []string{"target", "platforms", "platform", "archives", "docker", "go"} {
		input, ok := crossCompile.Inputs[name]
		if !ok || input.From != "params" {
			t.Errorf("build-cross-compile input %q = %+v, present=%t; the channel-scoped platform set must be "+
				"resolved from plan-time cache-key parameters", name, input, ok)
		}
	}

	derived := proto.DeriveTaskCacheKey(crossCompile.Inputs)
	for _, name := range []string{"platform", "platforms", "docker", "archives"} {
		if !containsString(derived.Params, name) {
			t.Errorf("derived cache key params = %v, missing %q; a platform set that does not reach the key "+
				"lets a four-platform entry answer a one-platform request", derived.Params, name)
		}
	}

	// The channel set falls back to the project's `publish` declaration when the
	// invocation names no channel parameter (the orchestrator injects declared
	// channels into the PLAN's params but not into a task's own map). That
	// declaration lives in putnami.json, so the file must stay a declared input
	// or the fallback would move the output without moving the key.
	config, ok := crossCompile.Inputs["config"]
	if !ok || config.From != "project" || !containsString(config.Files, "putnami.json") {
		t.Errorf("build-cross-compile config input = %+v, present=%t; the publish declaration decides the channel "+
			"set, so putnami.json must be a keyed project file", config, ok)
	}

	// The user-facing half: a parameter only reaches the plan if the command
	// declares it. Without the flag, `--platforms` on `package` would silently
	// do nothing while the identical flag on `build` worked.
	if _, ok := m.Commands["package"].Flags["platforms"]; !ok {
		t.Error("the `package` command declares no `platforms` flag; the release matrix must be nameable from " +
			"the CLI, not only from putnami.json")
	}
}

// TestServeIsBuildAndExec pins that `serve` compiles nothing ahead of itself.
//
// The pipeline used to schedule build-compile and then ignore its output:
// serve-run ran `go run`, which compiled the program a SECOND time. That was one
// whole discarded compile per served project, and it also coupled `serve` to the
// bin/ layout of another command's output tree.
func TestServeIsBuildAndExec(t *testing.T) {
	m := loadExtensionManifest(t)

	for _, step := range m.Commands["serve"].Run {
		if step.Task == "build-compile" || step.Task == "build-cross-compile" {
			t.Errorf("serve step %q schedules %q; serve is build-and-exec and emits no binary", step.ID, step.Task)
		}
	}
	if serve, ok := m.Tasks["serve-run"]; !ok {
		t.Fatal("manifest task \"serve-run\" is missing")
	} else if serve.Declares != nil && len(serve.Declares.Outputs) > 0 {
		t.Errorf("serve-run declares outputs %v; a served process is not an artifact", sortedKeysOf(serve.Declares.Outputs))
	}
}

func commandSchedulesTask(m *proto.Manifest, command, task string) bool {
	for _, step := range m.Commands[command].Run {
		if step.Task == task {
			return true
		}
	}
	return false
}

func sortedKeysOf[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestTheArchivePackagerOwnsTheArchivePublicationManifest pins who writes the
// one command-output file at the root of the package directory.
//
// `<command-output>/metadata.json` used to be a channel INDEX that package-go,
// package-archives and package-docker each read-merge-wrote — three producers of
// one file, which is exactly what ONE OWNER PER OUTPUT forbids, so it stayed
// unclaimed by all three and the whole command lost declared capture with it.
//
// The channel index is DERIVED
// from a record each packager writes inside the directory it owns, so it is not
// a file at all. What remains at that path is the archive publication manifest,
// which describes the archives in the sibling archives/ directory and is read by
// the archive uploader out of tree (@putnami/cloud publish-archives). It has one
// producer — the archive packager — so it is DECLARED, and a restore of that
// task reproduces it.
func TestTheArchivePackagerOwnsTheArchivePublicationManifest(t *testing.T) {
	m := loadExtensionManifest(t)

	owners := []string{}
	for _, name := range []string{"package-archives", "package-docker", "package-go"} {
		task, ok := m.Tasks[name]
		if !ok {
			t.Fatalf("manifest task %q is missing", name)
		}
		if task.Declares == nil {
			t.Fatalf("task %q lost its v3 declaration", name)
		}
		for _, output := range task.Declares.Outputs {
			if output.Path == "metadata.json" && output.Root == proto.OutputRootCommandOutput {
				owners = append(owners, name)
			}
		}
	}
	if len(owners) != 1 || owners[0] != "package-archives" {
		t.Errorf("owners of <command-output>/metadata.json = %v, want exactly [package-archives]: "+
			"the archive publication manifest describes the archives, and a second writer would be "+
			"a merge point", owners)
	}

	// And no packager claims another's channel record: each record lives inside
	// the directory its own task already owns, so it needs no declaration of its
	// own and must not get one.
	for name, task := range m.Tasks {
		if task.Declares == nil {
			continue
		}
		for id, output := range task.Declares.Outputs {
			if output.Kind == proto.OutputKindFile && strings.HasSuffix(output.Path, "channel.json") {
				t.Errorf("task %q declares the channel record %q as output %q; the record is captured by "+
					"the directory output that already owns it", name, output.Path, id)
			}
		}
	}
}

// TestCommittedSidecarsAreDeliberatelyUnclaimed pins the second footprint no
// task owns: the tracked schema/infra sidecars the build pipeline reconciles.
//
// `schema/openapi.json`, `schema/capabilities.json` and `infra/requirements.json`
// are committed source files whose WRITER is chosen at runtime by the
// describe-sole-committer rule — build-describe commits them for
// app projects, build-generate for library/CLI projects. A static declaration
// cannot express "whichever of these two runs", and naming either one would be a
// false exclusivity claim, so both stay unclaimed; the canonical build-time copy
// of each lives under .gen, which build-generate does own whole.
//
// `schema/config.json` is the exception and is asserted the other way: it has a
// declared owner (config-extract-exec), and the two pipeline writers must keep
// their hands off the declaration.
func TestCommittedSidecarsAreDeliberatelyUnclaimed(t *testing.T) {
	m := loadExtensionManifest(t)

	unclaimed := []string{"schema/openapi.json", "schema/capabilities.json", "infra/requirements.json"}
	for _, name := range sortedTaskNames(m) {
		task := m.Tasks[name]
		if task.Declares == nil {
			continue
		}
		for id, output := range task.Declares.Outputs {
			for _, path := range unclaimed {
				if output.Path == path {
					t.Errorf("task %q claims the committed sidecar %q as output %q; its writer is chosen at runtime "+
						"(describe for app projects, generate for library projects), so a static owner is a false claim",
						name, path, id)
				}
			}
		}
	}

	for _, name := range []string{"build-generate", "build-describe"} {
		task, ok := m.Tasks[name]
		if !ok {
			t.Fatalf("manifest task %q is missing", name)
		}
		for id, output := range task.Declares.Outputs {
			if strings.HasPrefix(output.Path, "schema/") {
				t.Errorf("task %q claims %q as output %q; the committed config schema is owned by config-extract-exec",
					name, output.Path, id)
			}
		}
	}
}

// TestGenerateIsTheSingleProducerOfGen pins the .gen half of the ownership
// decision: build-generate owns the project-rooted subtree apart from the
// subpaths it explicitly CEDES, and every other task that writes inside it
// declares nothing there unless generate ceded exactly that path. Declaring a
// project-rooted path on a task five commands schedule is plannable only
// because the plan-time owner check counts one manifest task scheduled by
// several commands as ONE owner (sameManifestTask in
// tooling/cli/internal/jobs/plan_contract.go, pinned there by
// TestPlanContract_OneTaskScheduledBySeveralCommandsIsOneOwner).
//
// A cede is a two-sided statement, so this test checks both sides: a claim
// inside .gen is legal only when generate cedes that exact path or a subpath
// containing it (the containment the CLI's cedesRegion applies), and a ceded
// path with no claimant is captured by nobody — which for
// .gen/migration-bundle or .gen/conf means a run serving both tasks from
// cache restores a .gen without it.
func TestGenerateIsTheSingleProducerOfGen(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "merged-config-survives-a-cache-hit", "both-config-merge-tasks-declare-the-merged-file-inside-the-ceded-gen-conf")
	m := loadExtensionManifest(t)

	generate, ok := m.Tasks["build-generate"]
	if !ok {
		t.Fatal("manifest task \"build-generate\" is missing")
	}
	if generate.Declares == nil {
		t.Fatal("build-generate lost its v3 declaration")
	}
	gen, ok := generate.Declares.Outputs["gen"]
	if !ok || gen.Kind != proto.OutputKindDirectory || gen.Path != ".gen" || gen.EffectiveRoot() != proto.OutputRootProject {
		t.Fatalf("gen ownership = %+v, want the project-rooted .gen directory", gen)
	}

	// Everyone else writes into generate-owned territory and claims none of it,
	// except at (or inside) a path generate ceded.
	claimed := map[string][]string{}
	for _, name := range sortedTaskNames(m) {
		if name == "build-generate" {
			continue
		}
		task := m.Tasks[name]
		if task.Declares == nil {
			continue
		}
		for id, output := range task.Declares.Outputs {
			if output.EffectiveRoot() != proto.OutputRootProject {
				continue
			}
			if output.Path != ".gen" && !strings.HasPrefix(output.Path, ".gen/") {
				continue
			}
			ceded := ""
			for _, exclude := range gen.Excludes {
				if cededSubpathContains(exclude, output.Path) {
					ceded = exclude
					break
				}
			}
			if ceded != "" {
				claimed[ceded] = append(claimed[ceded], name)
				continue
			}
			t.Errorf("task %q claims %q as output %q; build-generate produces all of .gen but the "+
				"subpaths it cedes through declares.outputs.gen.excludes", name, output.Path, id)
		}
	}

	// A ceded path nobody claims is captured by nobody. That is legal — protocol
	// ADR 0003 says the declaration states it — but it is never an accident, so
	// every one of them has to be a row in the reviewed table with no claimant
	// and a stated reason. A pathFrom claimant
	// (config-merge-exec) is invisible to the literal scan above, so the table's
	// claims are also checked against what the manifest declares.
	for _, ceded := range gen.Excludes {
		claims, recorded := cededGenOwner[ceded]
		if !recorded {
			t.Errorf("build-generate cedes %q but the reviewed table does not record who claims it, "+
				"or that nobody does; the subtree would be captured by nobody", ceded)
			continue
		}
		wantTasks := make([]string, 0, len(claims))
		for _, claim := range claims {
			task, ok := m.Tasks[claim.task]
			if !ok || task.Declares == nil {
				t.Errorf("the reviewed table gives %q to task %q, which is missing or declares nothing", ceded, claim.task)
				continue
			}
			output, ok := task.Declares.Outputs[claim.output]
			if !ok {
				t.Errorf("the reviewed table gives %q to %s output %q, but no such output is declared",
					ceded, claim.task, claim.output)
				continue
			}
			if output.PathFrom != "" {
				continue // resolved at run time; not part of the literal scan
			}
			wantTasks = append(wantTasks, claim.task)
		}
		if got := claimed[ceded]; !slices.Equal(got, wantTasks) {
			t.Errorf("literal claimants of the ceded %q = %v, want %v per the reviewed table", ceded, got, wantTasks)
		}
	}

	// The two config-merge variants are two manifest tasks planned for the
	// same project under `build,test`, so the plan-time owner check compares
	// their declarations. It compares two pathFrom outputs by PORT under one
	// root and never compares a port with a literal path, so exactly one of
	// them may name the file through a port.
	buildMerged := m.Tasks["config-merge-exec"].Declares.Outputs["merged"]
	testMerged := m.Tasks["config-merge-test-exec"].Declares.Outputs["merged"]
	if buildMerged.PathFrom != configmerge.MergedConfigPort {
		t.Errorf("config-merge-exec merged output pathFrom = %q, want the runtime's %q port", buildMerged.PathFrom, configmerge.MergedConfigPort)
	}
	if _, ok := m.Tasks["config-merge-exec"].Outputs[configmerge.MergedConfigPort]; !ok {
		t.Errorf("config-merge-exec does not declare the %q data port its merged output resolves from", configmerge.MergedConfigPort)
	}
	if testMerged.Path != ".gen/conf/.env.test.yaml" || testMerged.PathFrom != "" {
		t.Errorf("config-merge-test-exec merged output = %+v, want the literal .gen/conf/.env.test.yaml "+
			"(APP_ENV is pinned to test; a second pathFrom on the same port would overlap at plan time)", testMerged)
	}
	if env := m.Tasks["config-merge-test-exec"].Env["APP_ENV"]; env != "test" {
		t.Errorf("config-merge-test-exec APP_ENV = %q, want test: the literal output path depends on it", env)
	}

	// The multi-command scheduling the exemption exists for must stay real:
	// if build-generate ever ran under a single command, the note above would
	// silently become stale.
	if cmds := commandsByTask(m)["build-generate"]; len(cmds) < 2 {
		t.Errorf("build-generate commands = %v; the sameManifestTask exemption is what makes this "+
			"project-rooted declaration plannable, so losing multi-command scheduling should be a reviewed change", cmds)
	}
}

// TestConfigMergeKeysOnTheClosureConfigFiles pins the input half of the
// merged-config ownership.
// config-merge reads its DIRECT dependencies' merged .gen/conf/.env.<env>.yaml
// (or their source conf/.env*.yaml), and a dependency's merged file is itself a
// function of ITS closure's source files — so the merged file is a function of
// the transitive closure's conf/.env*.yaml, and the cache key must fold them.
// Before this input the key covered the project's own config files only: a
// dependency's config change never moved it, which merely left the on-disk file
// stale while the task declared no output, and would actively RESTORE a stale
// merged file now that a hit reproduces one.
//
// The closure includes the seed project (projectDependencyClosure in
// tooling/cli/internal/jobs/context.go, folded by closureInputsDigest and
// keyFilePatterns), so the project's own files are not declared a second time
// through a `project` input.
func TestConfigMergeKeysOnTheClosureConfigFiles(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "merged-config-survives-a-cache-hit", "a-dependency-config-change-is-a-cache-key-input-of-config-merge")
	m := loadExtensionManifest(t)

	wantFiles := []string{"conf/.env.yaml", "conf/.env.*.yaml"}
	for _, name := range []string{"config-merge-exec", "config-merge-test-exec"} {
		task, ok := m.Tasks[name]
		if !ok {
			t.Fatalf("manifest task %q is missing", name)
		}
		input, ok := task.Inputs["config-files"]
		if !ok {
			t.Errorf("%s declares no config-files input", name)
			continue
		}
		if input.From != proto.TaskInputFromClosure || !slices.Equal(input.Files, wantFiles) {
			t.Errorf("%s config-files input = %+v, want from=closure files=%v", name, input, wantFiles)
		}
		for id, other := range task.Inputs {
			if id == "config-files" || other.From != proto.TaskInputFromProject {
				continue
			}
			for _, pattern := range other.Files {
				if slices.Contains(wantFiles, pattern) {
					t.Errorf("%s input %q declares %q as a project input too; the closure already covers the project itself",
						name, id, pattern)
				}
			}
		}
		key := proto.DeriveTaskCacheKey(task.Inputs)
		if !slices.Equal(key.ClosureFiles, []string{"conf/.env.*.yaml", "conf/.env.yaml"}) {
			t.Errorf("%s derived closureFiles = %v, want the two config patterns", name, key.ClosureFiles)
		}
	}
}

// TestDescribeDeclaresTheClientOutputPort pins the runtime↔manifest link the
// client half of the ownership decision rests on: the generated client's
// directory is chosen by the project's clientgen config, so it is declarable
// only through a data output port, and the consumer reads it back by name. A
// rename on either side would silently stop the client from being captured.
func TestDescribeDeclaresTheClientOutputPort(t *testing.T) {
	m := loadExtensionManifest(t)
	describe, ok := m.Tasks["build-describe"]
	if !ok {
		t.Fatal("manifest task \"build-describe\" is missing")
	}
	if _, ok := describe.Outputs[codegen.GeneratedClientOutputPort]; !ok {
		t.Fatalf("build-describe does not declare the %q output port the runtime reports the generated client directory on",
			codegen.GeneratedClientOutputPort)
	}
	if describe.Declares == nil {
		t.Fatal("build-describe lost its v3 declaration")
	}
	client, ok := describe.Declares.Outputs["client"]
	if !ok || client.PathFrom != codegen.GeneratedClientOutputPort || !client.OptionalEmpty {
		t.Fatalf("client ownership = %+v, want optional-empty pathFrom %q", client, codegen.GeneratedClientOutputPort)
	}
	// The module files, the project document and the client project's own .gen
	// belong to the workspace (protocol ADR 0005).
	if got, want := proto.DecidablePreserves(client), []string{".gen", "go.mod", "go.sum", "putnami.json"}; !slices.Equal(got, want) {
		t.Errorf("client preserves %v, want %v", got, want)
	}
}

// TestTaskEffectTable pins HONEST EFFECTS per task: what each task does beyond
// writing its declared outputs, and — for the effects a cache hit cannot
// reproduce — that the task is not cacheable. A task missing from the table
// declares no effects.
//
// Every task that shells out to the Go toolchain declares `toolchain-cache` (the
// shared machine-global GOCACHE / module cache is exactly that effect) and
// `network` (Go resolves and downloads modules as part of building, unlike a
// pre-installed node_modules tree). build-generate declares neither: it parses
// the project's AST in process and never runs `go`.
func TestTaskEffectTable(t *testing.T) {
	m := loadExtensionManifest(t)

	want := map[string][]string{
		// The cache commands mutate the machine-global Go cache every worktree
		// shares — the toolchain-cache effect exactly — and no cache hit could
		// reproduce that, so both are uncacheable.
		"cache-clean-exec":       {"toolchain-cache"},
		"cache-gc-exec":          {"toolchain-cache"},
		"build-compile":          {"network", "toolchain-cache", "workspace-files"},
		"build-cross-compile":    {"network", "toolchain-cache"},
		"build-describe":         {"network", "toolchain-cache"},
		"build-tidy":             {"network", "toolchain-cache"},
		"deps-upgrade-exec":      {"network", "toolchain-cache", "workspace-files"},
		"lint-golangci-fix":      {"network", "toolchain-cache"},
		"lint-golangci-readonly": {"network", "toolchain-cache"},
		"lint-staticcheck":       {"network", "toolchain-cache"},
		// The Go extension's own archives carry the pinned development tools,
		// built per release platform at package time (internal/jobs/pkg/tools.go).
		// That resolves the tool modules over the network and writes the shared
		// Go build cache; neither is an external effect, so the task stays
		// cacheable and a cache hit reproduces the same archives.
		"package-archives": {"network", "toolchain-cache"},
		"package-docker":   {"network", "toolchain-cache"},
		"publish-docker":   {"network", "registry"},
		"publish-go":       {"network", "registry"},
		"run-exec":         {"network", "process", "toolchain-cache"},
		"serve-run":        {"network", "process", "toolchain-cache"},
		"test-exec":        {"network", "toolchain-cache"},
		// The database test environment runs a container (process) and may pull
		// its pinned image (network). Both are external effects a cache hit
		// could not reproduce, which is also why the pair is uncacheable — and
		// the producer must be, since a task with an invocation-scoped output
		// is never captured. The finalizer only removes containers, so it
		// declares `process` alone.
		"test-env-up":            {"network", "process"},
		"test-env-down":          {"process"},
		"workspace-install-exec": {"network", "toolchain-cache", "workspace-files"},
		// workspace-fetch downloads the modules and the pinned tools with the
		// read credential (network, toolchain-cache). It installs no Go and
		// writes no committed file, so it declares no workspace-files.
		"workspace-fetch-exec": {"network", "toolchain-cache"},
		// workspace-sync rewrites the go.mod of every go.work member whose
		// replace closure is incomplete (workspace-files), then settles those
		// modules with `go mod tidy`, which resolves through the proxy
		// (network) and the shared module cache (toolchain-cache).
		"workspace-sync-exec": {"network", "toolchain-cache", "workspace-files"},
	}

	for _, name := range sortedTaskNames(m) {
		task := m.Tasks[name]
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

// TestServeDeclaresTheProcessEffect is the positive form of the B3b gap test:
// Making serve-run uncacheable, which is what the gap was waiting
// on, so the external `process` effect is now declared outright — an
// uncacheable long-lived process, honestly stated. The old gap test deleted
// itself by design the moment the policy was fixed.
func TestServeDeclaresTheProcessEffect(t *testing.T) {
	m := loadExtensionManifest(t)
	serve, ok := m.Tasks["serve-run"]
	if !ok {
		t.Fatal("manifest task \"serve-run\" is missing")
	}
	if serve.Cache.IsEnabled() {
		t.Fatal("serve-run is cache-eligible again; a cache hit on a long-lived process is meaningless")
	}
	declared := false
	if serve.Declares != nil {
		for _, effect := range serve.Declares.Effects {
			if effect == "process" {
				declared = true
			}
		}
	}
	if !declared {
		t.Fatal("serve-run does not declare the external \"process\" effect despite being uncacheable")
	}
}

// TestOnlyTheGolangciFixTaskMutatesSources pins the check/fix split. The lint
// pipeline runs `lint-golangci-fix` when params.fix is true and
// `lint-golangci-readonly` when it is false; only the first passes --fix to
// golangci-lint, so only it declares source mutation — together with the
// project-scoped "sources" write resource, because that resource, not the flag,
// is what the planner serializes conflicting jobs on. staticcheck has no fix
// mode at all, so its single task serves both pipelines read-only.
func TestOnlyTheGolangciFixTaskMutatesSources(t *testing.T) {
	m := loadExtensionManifest(t)
	fixTasks := map[string]bool{"lint-golangci-fix": true}

	for _, name := range sortedTaskNames(m) {
		task := m.Tasks[name]
		mutates := task.Declares != nil && task.Declares.MutatesSources
		if mutates != fixTasks[name] {
			t.Errorf("task %q mutatesSources = %t, want %t", name, mutates, fixTasks[name])
		}
		if mutates != declaresSourceWrite(task) {
			t.Errorf("task %q mutatesSources = %t but sources write resource = %t; the two must agree",
				name, mutates, declaresSourceWrite(task))
		}
	}

	// The pipeline is what makes the split real: each lint task must be gated on
	// the fix param it is named for, so a check-only run can never schedule the
	// mutating task.
	guards := map[string]string{
		"golangci-lint":            "params.fix &&",
		"golangci-lint-check-only": "!params.fix &&",
	}
	lint, ok := m.Commands["lint"]
	if !ok {
		t.Fatal("manifest command \"lint\" is missing")
	}
	seen := map[string]bool{}
	for _, step := range lint.Run {
		want, tracked := guards[step.ID]
		if !tracked {
			continue
		}
		seen[step.ID] = true
		if !strings.HasPrefix(step.If, want) {
			t.Errorf("lint step %q condition = %q, want it to start with %q", step.ID, step.If, want)
		}
	}
	for id := range guards {
		if !seen[id] {
			t.Errorf("lint pipeline no longer has a %q step; the check/fix split is what the mutation flags describe", id)
		}
	}
}

// TestInfraAggregationIsWiredIntoBuild pins the three properties that make
// extension-owned infra aggregation work at all.
//
// It replaced a CLI gate that ran unconditionally on every build and watched
// every generator step in the workload's dependency closure. As a pipeline step
// it must therefore: (1) exist under `build` and nowhere else, because the
// aggregated manifest is a build artifact; (2) run AFTER the steps that make
// this project's committed requirements current — the closure is covered
// transitively, because generate already depends on `^generate` and describe on
// `^describe`; and (3) never be cached, because its inputs are OTHER projects'
// committed manifests, which no per-project cache key covers. A cached
// aggregation would replay a stale deployability manifest from a hit.
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

	if cmds := commandsByTask(m)["build-infra"]; len(cmds) != 1 || cmds[0] != "build" {
		t.Errorf("build-infra commands = %v, want only [build]", cmds)
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
	for _, want := range []string{"generate", "describe"} {
		if !containsString(step.DependsOn, want) {
			t.Errorf("infra step dependsOn = %v, want it to include %q — the committed requirements "+
				"are only current once that step has run", step.DependsOn, want)
		}
	}
}

// TestDeploymentDeclarationIsAGatedCachedPackageStep pins the manifest half of
// the workload's deployment declaration, the payload of a release-set member of
// kind deployment whose selection fingerprint is this step's task key.
//
//   - GATED. The step runs only under the deployment channel and never for an
//     image project, so every package plan that does not ask for it is
//     unchanged.
//   - ORDERED. It reads the closure's committed infra/requirements.json, which
//     generate (libraries) and describe (applications) write, so it depends on
//     both, exactly as build-infra does.
//   - KEYED. Its key folds the closure's infra/requirements.json and the
//     workload's runtime and overrides files. A requirements-only change in a
//     dependency moves the key, so the member is republished.
//   - REQUIRED. The output is the declaration path itself and is not
//     optionalEmpty, so a run that writes no declaration caches nothing.
func TestDeploymentDeclarationIsAGatedCachedPackageStep(t *testing.T) {
	m := loadExtensionManifest(t)

	task, ok := m.Tasks["package-deployment"]
	if !ok {
		t.Fatal("manifest task \"package-deployment\" is missing")
	}
	if task.Cache == nil || !task.Cache.IsEnabled() || !task.Cache.Deterministic {
		t.Errorf("package-deployment cache = %+v, want an enabled, deterministic cache: its key is the member's selection fingerprint", task.Cache)
	}
	key := proto.DeriveTaskCacheKey(task.Inputs)
	if !slices.Equal(key.ClosureFiles, []string{"infra/requirements.json"}) {
		t.Errorf("package-deployment closure files = %v, want [infra/requirements.json]", key.ClosureFiles)
	}
	if !slices.Equal(key.Files, []string{"infra/overrides.json", "infra/runtime.json"}) {
		t.Errorf("package-deployment project files = %v, want the workload's runtime and overrides files", key.Files)
	}
	if task.Declares == nil || len(task.Declares.Outputs) != 1 {
		t.Fatalf("package-deployment declarations = %+v, want exactly one output", task.Declares)
	}
	if len(task.Declares.Effects) != 0 || task.Declares.MutatesSources {
		t.Errorf("package-deployment declares effects %v (mutatesSources %t); it reads committed files and writes one",
			task.Declares.Effects, task.Declares.MutatesSources)
	}
	output, ok := task.Declares.Outputs["deployment"]
	if !ok || output.Kind != proto.OutputKindFile || output.EffectiveRoot() != proto.OutputRootProject ||
		output.Path != infraagg.DeploymentFile || output.OptionalEmpty {
		t.Errorf("package-deployment deployment output = %+v, want the required project-rooted file %s", output, infraagg.DeploymentFile)
	}

	if cmds := commandsByTask(m)["package-deployment"]; !slices.Equal(cmds, []string{"package"}) {
		t.Errorf("package-deployment commands = %v, want only [package]", cmds)
	}
	step := stepForTask(m, "package", "package-deployment")
	if step.ID != "deployment" {
		t.Fatalf("package step for package-deployment = %+v, want id deployment: the id is the member's package step", step)
	}
	if step.If != "params.deployment && !params.image" {
		t.Errorf("deployment step if = %q, want it gated on the deployment channel and excluded for image projects", step.If)
	}
	for _, want := range []string{"generate", "describe"} {
		if !containsString(step.DependsOn, want) {
			t.Errorf("deployment step dependsOn = %v, want it to include %q: the committed requirements are only current once that step has run",
				step.DependsOn, want)
		}
	}
	flag, ok := m.Commands["package"].Flags["deployment"]
	if !ok || flag.Type != "boolean" || flag.Default != false {
		t.Errorf("package flag deployment = %+v, present=%t; want a boolean channel that defaults to off", flag, ok)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func declaresSourceWrite(task proto.TaskDefinition) bool {
	for _, ref := range task.Writes {
		if ref.ID == proto.ResourceIDSources && ref.EffectiveScope() == proto.ResourceScopeProject {
			return true
		}
	}
	return false
}

func sortedTaskNames(m *proto.Manifest) []string {
	names := make([]string, 0, len(m.Tasks))
	for name := range m.Tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

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

// ---------------------------------------------------------------------------
// Scheduler and tool deadlines
// ---------------------------------------------------------------------------

// longLivedTaskSentinels are the tasks whose timeoutMs is deliberately the -1
// "no deadline" sentinel. Both front a process that is SUPPOSED to outlive the
// job — serve's hot-reload server and run's forwarded workload — so for them a
// finite deadline would be a wrong answer rather than a large one.
//
// TestEveryTaskDeadlineIsFiniteAndPositive is the MANIFEST half of "a
// deliberately hung task still terminates". The executing half belongs to the
// orchestrator, not to this extension: tooling/cli/internal/jobs/runner.go arms
// a context.WithTimeout for every positive deadline (resolving 0 to
// jobs.DefaultTimeoutMs and treating a negative as "no timeout"), hands that
// context to exec.CommandContext, and on expiry SIGTERMs the job's whole
// process group and force-kills whatever survives. No test here can exercise
// that — the deadline is imposed on the runtime from outside, and watching a
// real 600s deadline expire would be a ten-minute test of somebody else's code.
// What this test does guarantee is that the mechanism is always ARMED: no task
// in this manifest reaches the runner carrying the one value that disarms it.
var longLivedTaskSentinels = map[string]bool{
	"serve-run": true,
	"run-exec":  true,
}

// TestAuditedHeavyTaskDeadlines pins the scheduler deadline of the heavy Go
// tasks as a TABLE, for the same reason TestDeclaredOutputOwnerTable pins
// output ownership: these numbers decide whether ordinary work is reported as a
// repository verdict or as an infrastructure failure, so changing one must be a
// reviewed change rather than a silent one.
//
// WHY THESE VALUES (the reviewed platform deadline decision —
// "retry is recovery, not the normal execution path"):
//
//   - lint-golangci-readonly 300000 -> 600000. Production terminated
//     `/intelligence/workloads/intelligence-server:lint~golangci-lint-check-only`
//     at 300002ms with `job timed out` after 646 cache hits. Ending within 2ms
//     of the ceiling is the signature of an undersized deadline, not of a slow
//     repository.
//   - lint-golangci-fix 300000 -> 600000, held EQUAL to the readonly task. It
//     is the same tool over the same sources doing strictly MORE work (--fix
//     plus the `fmt --diff` gate), and both read ONE shared config that exposes
//     ONE tool timeout. A split ceiling would force run.timeout below the
//     smaller of the two and cancel out the readonly raise.
//   - build-cross-compile 600000 -> 900000. It reached its exact 600s ceiling
//     and the same commit then passed on retry in 213s. It compiles every
//     entrypoint once per archive platform, sequentially.
//   - build-compile 300000 -> 900000, held EQUAL to build-cross-compile.
//     Its ordinary work SHRANK — nature+intent scoping made `build` compile for
//     the host alone — but an explicit `platforms` request can still ask this
//     task for the full matrix, and a ceiling sized for the default would then
//     terminate exactly the invocation that asked for the most work. The
//     deadline is sized by what a task can be asked to do, not by its median.
//   - build-describe 300000 -> 900000, held EQUAL to build-compile. This one is
//     argued from CONTAINMENT rather than from a measurement: describe runs the
//     same `go build` of the same entrypoint as compile, with the same
//     configuration, and then
//     runs the resulting binary under its own 60s cap (describeMaxRuntime). Its
//     work therefore CONTAINS compile's, so a ceiling below compile's audited
//     one would terminate describe on a tree where compile is allowed to
//     succeed. The direction matters on Linux, where describe now builds the
//     cgo variant that compile used to build second: the pair costs less
//     overall, and describe's own share is the larger of the two.
//   - lint-staticcheck stays at 300000. Deliberate: it is not in the audited
//     set, it has no tool-owned deadline of its own, and no observation shows
//     it under deadline pressure. It is in the table so that raising it later
//     is also a reviewed change.
//
// MEASUREMENT SET behind "at least 2x ordinary uncached execution" (recorded in
// full in doc/lint.md and doc/build.md; n is too small to call a p95, so these
// are observed MAXIMA):
//
//   - lint, uncached at BOTH levels (scratch GOCACHE + GOLANGCI_LINT_CACHE),
//     tooling/cli at 167,897 Go LOC: 26.6s on 10 cores, 86.6s pinned to 2.
//     n=2 cold + 6 putnami-cache-miss runs (1.9s-19.2s). 600000ms is ~6.9x the
//     86.6s maximum; the binding limit is the 480000ms tool deadline, ~5.5x.
//   - cross-compile, uncached (scratch GOCACHE), four archive platforms of the
//     same project: 75.3s on 10 cores, 54.7s pinned to 2. n=2 cold + 3
//     putnami-cache-miss runs (whole-command wall time 4.6s-14.4s), plus the
//     213s production pass. 900000ms is ~4.2x the 213s maximum.
//
// The local set does NOT contain the workload that actually failed, whose true
// duration is censored at >=300s by the termination itself. So the readonly
// raise is justified twice over: >=2x every ordinary uncached duration that
// could be observed, and exactly 2x the censored lower bound of the failure.
func TestAuditedHeavyTaskDeadlines(t *testing.T) {
	m := loadExtensionManifest(t)

	want := map[string]int{
		"lint-golangci-readonly": 600000,
		"lint-golangci-fix":      600000,
		"lint-staticcheck":       300000,
		"build-cross-compile":    900000,
		"build-compile":          900000,
		"build-describe":         900000,
	}

	for _, name := range sortedNames(want) {
		task, ok := m.Tasks[name]
		if !ok {
			t.Errorf("manifest task %q is missing; the audited deadline table names it", name)
			continue
		}
		if task.TimeoutMs != want[name] {
			t.Errorf("task %q timeoutMs = %d, want %d — see this test's doc comment for the evidence "+
				"behind the value and update it there, not just here", name, task.TimeoutMs, want[name])
		}
	}

	// describe must never be given less than compile: it runs compile's build
	// and then the binary it produced, so a lower ceiling would kill it on a
	// tree where compile is allowed to finish.
	if describe, compile := m.Tasks["build-describe"], m.Tasks["build-compile"]; describe.TimeoutMs < compile.TimeoutMs {
		t.Errorf("build-describe (%d) has a lower ceiling than build-compile (%d); describe's work contains "+
			"compile's", describe.TimeoutMs, compile.TimeoutMs)
	}

	// The fix/readonly pair must stay equal for the reason above: one shared
	// config, one tool timeout.
	if fix, readonly := m.Tasks["lint-golangci-fix"], m.Tasks["lint-golangci-readonly"]; fix.TimeoutMs != readonly.TimeoutMs {
		t.Errorf("lint-golangci-fix (%d) and lint-golangci-readonly (%d) have diverged; they share one "+
			"config and therefore one tool timeout, so the lower ceiling would silently bind both",
			fix.TimeoutMs, readonly.TimeoutMs)
	}
}

// TestEveryTaskDeadlineIsFiniteAndPositive is the manifest half of "a
// deliberately hung task still terminates". A task that loses its deadline
// resolves to the CLI default (300000ms, jobs.DefaultTimeoutMs) rather than to
// "forever", so the dangerous regression is not a MISSING value but a NEGATIVE
// one: tooling/cli/internal/jobs/runner.go reads a negative timeout as "no
// timeout" and never arms a context deadline. Only the two documented
// long-lived tasks may say that.
func TestEveryTaskDeadlineIsFiniteAndPositive(t *testing.T) {
	m := loadExtensionManifest(t)

	for _, name := range sortedTaskNames(m) {
		timeout := m.Tasks[name].TimeoutMs
		if longLivedTaskSentinels[name] {
			if timeout != -1 {
				t.Errorf("task %q timeoutMs = %d, want the -1 no-deadline sentinel; it fronts a process "+
					"meant to outlive the job", name, timeout)
			}
			continue
		}
		if timeout < 0 {
			t.Errorf("task %q timeoutMs = %d — a negative timeout disarms the runner's context deadline "+
				"entirely, so a hung run of this task would never terminate. Add it to "+
				"longLivedTaskSentinels with a reason, or give it a finite budget.", name, timeout)
		}
		if timeout == 0 {
			t.Errorf("task %q declares no timeoutMs and silently inherits the CLI's 300000ms default; "+
				"state the budget the task actually needs", name)
		}
	}
}

// TestGolangciToolDeadlineStaysBelowItsSchedulerDeadline pins the invariant
// that actually failed in production, as a permanent guard rather than a
// one-off constant.
//
// Two deadlines govern a golangci-lint job: the scheduler's, armed at process
// spawn by the CLI, and golangci's own `run.timeout`, armed only once `run`
// begins. When they are EQUAL the scheduler necessarily wins — it started
// first — and the job dies as an opaque `job timed out` with no cause. That is
// exactly what the shipped config's 5m against a 300000ms task produced. The
// tool deadline must therefore stay STRICTLY below the resolved deadline of
// every step that runs golangci-lint, so the tool reports a structured
// "Timeout exceeded" first.
//
// This walks the NORMALIZED plan (step override, else task) rather than the
// task table, because a step-level timeoutMs override would re-open the
// collision without touching a single task.
func TestGolangciToolDeadlineStaysBelowItsSchedulerDeadline(t *testing.T) {
	m := loadExtensionManifest(t)

	toolDeadline := golangciToolDeadline(t)
	if toolDeadline <= 0 {
		t.Fatal("the shipped config declares no run.timeout; without a tool deadline the scheduler is the " +
			"only one that can fire and a slow lint can never report its own cause")
	}

	checked := 0
	for _, row := range normalizedStepDeadlines(m) {
		if !taskRunsGolangci(m.Tasks[row.task]) {
			continue
		}
		checked++
		if toolDeadline >= time.Duration(row.timeoutMs)*time.Millisecond {
			t.Errorf("golangci run.timeout (%s) is not strictly below the scheduler deadline of %s (%dms); "+
				"equal or larger means the scheduler always wins and the job reports `job timed out` "+
				"instead of golangci's own \"Timeout exceeded\"", toolDeadline, row, row.timeoutMs)
		}
	}
	if checked == 0 {
		t.Fatal("no pipeline step was identified as running golangci-lint; the detection in taskRunsGolangci " +
			"has drifted from the manifest and this guard is now vacuous")
	}
}

// TestShippedGolangciExclusionsMatchDirectoryComponentsOnly pins the default
// config's exclusion boundary. exclusions.paths entries are regular
// expressions, so bare names such as "bin" also match internal/binding and a
// bare ".putnami" matches cmd/putnami. That silently removes ordinary source
// files from every consumer that inherits this config.
//
// The patterns match paths relative to the directory golangci-lint runs in
// (run.relative-path-mode: wd), not to the config file, which sits in the
// extension's install directory outside the workspace. They never match a
// path that climbs out of that directory.
func TestShippedGolangciExclusionsMatchDirectoryComponentsOnly(t *testing.T) {
	cfg := readGolangciConfig(t, filepath.Join("..", "..", "config", ".golangci.yml"))
	if cfg.Run.RelativePathMode != "wd" {
		t.Fatalf("run.relative-path-mode = %q, want wd: any other base puts the directories above the workspace, or none of the project, into the paths exclusions.paths matches", cfg.Run.RelativePathMode)
	}
	want := []string{
		`^([^/]*[^./][^/]*/)*vendor(/|$)`,
		`^([^/]*[^./][^/]*/)*node_modules(/|$)`,
		`^([^/]*[^./][^/]*/)*dist(/|$)`,
		`^([^/]*[^./][^/]*/)*\.putnami(/|$)`,
	}
	assertGolangciExclusionBoundaries(t, cfg.Linters.Exclusions.Paths, want)
}

type golangciConfig struct {
	Run struct {
		RelativePathMode string `yaml:"relative-path-mode"`
	} `yaml:"run"`
	Linters struct {
		Exclusions struct {
			Paths []string `yaml:"paths"`
		} `yaml:"exclusions"`
	} `yaml:"linters"`
}

func readGolangciConfig(t *testing.T, path string) golangciConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golangci config: %v", err)
	}
	var cfg golangciConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse golangci config: %v", err)
	}
	return cfg
}

func assertGolangciExclusionBoundaries(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("exclusions.paths = %q, want anchored directory patterns %q", got, want)
	}
	for _, pattern := range got {
		if _, err := regexp.Compile(pattern); err != nil {
			t.Fatalf("compile exclusions.paths pattern %q: %v", pattern, err)
		}
	}
	for _, path := range []string{
		"vendor/dependency.go",
		"nested/node_modules/dependency.go",
		"dist/output.go",
		"nested/.putnami/out/cache.json",
	} {
		if !matchesAnyGolangciExclusion(got, path) {
			t.Errorf("intended directory path %q is not excluded", path)
		}
	}
	for _, path := range []string{
		"internal/binding/binding.go",
		"pkg/distance/distance.go",
		"pkg/builders/builders.go",
		"pkg/node_moduleship/module.go",
		"cmd/putnami/main.go",
		"pkg/vendorized/vendorized.go",
		// Go packages named build or bin hold source.
		"internal/jobs/build/build.go",
		"bin/tool.go",
		// A path that climbs out of the base directory: through a symlinked
		// working directory, every file of the workspace is reported so.
		"../../home/vendor/workspace/main.go",
		"../../.putnami/dist/node_modules/workspace/main.go",
	} {
		if matchesAnyGolangciExclusion(got, path) {
			t.Errorf("ordinary source path %q is unexpectedly excluded", path)
		}
	}
}

func matchesAnyGolangciExclusion(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(path) {
			return true
		}
	}
	return false
}

// TestHeavyStepsSurfaceTheAuditedDeadlines pins the deadlines as the PLAN sees
// them. `putnami` never schedules a task directly: pipeline.go normalizes each
// command's steps into jobs and resolves the deadline as "step override, else
// task". A step-level timeoutMs on the cross-compile or golangci steps would
// leave the audited task table green while the scheduled job kept the old
// ceiling, so the heavy steps — the ones worker tuning gives CPU headroom to,
// and the ones the audit is about — are locked as a table of their own.
func TestHeavyStepsSurfaceTheAuditedDeadlines(t *testing.T) {
	m := loadExtensionManifest(t)

	want := []string{
		"build/compile -> build-compile = 900000ms",
		// describe runs the same `go build` of the same entrypoint as compile,
		// with the same configuration, so it is declared heavy
		// where a second host build follows it: `build` and `test`, and it
		// carries compile's ceiling for the containment reason in the audited
		// table below.
		"build/describe -> build-describe = 900000ms",
		"lint/golangci-lint -> lint-golangci-fix = 600000ms",
		"lint/golangci-lint-check-only -> lint-golangci-readonly = 600000ms",
		"lint/staticcheck -> lint-staticcheck = 300000ms",
		"lint/staticcheck-check-only -> lint-staticcheck = 300000ms",
		"package/cross-compile -> build-cross-compile = 900000ms",
		"test/describe -> build-describe = 900000ms",
		"test/test -> test-exec = 600000ms",
	}

	var got []string
	for _, row := range normalizedStepDeadlines(m) {
		if !row.heavy {
			continue
		}
		got = append(got, fmt.Sprintf("%s = %dms", row, row.timeoutMs))
	}

	if strings.Join(got, "\n  ") != strings.Join(want, "\n  ") {
		t.Errorf("normalized heavy-step deadlines =\n  %s\nwant\n  %s",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// stepDeadlineRow is one normalized pipeline step: which command scheduled it,
// which task it runs, whether it is heavy, and the deadline the job gets.
type stepDeadlineRow struct {
	command   string
	step      string
	task      string
	heavy     bool
	timeoutMs int
}

func (r stepDeadlineRow) String() string {
	return fmt.Sprintf("%s/%s -> %s", r.command, r.step, r.task)
}

// normalizedStepDeadlines resolves every pipeline step's effective deadline the
// same way the CLI does when it turns a manifest into a job plan: the step's own
// timeoutMs override wins, otherwise the task's
// (tooling/cli/internal/extension/pipeline.go, buildJobDefinition). Rows are
// sorted by command then step so the table is stable.
func normalizedStepDeadlines(m *proto.Manifest) []stepDeadlineRow {
	var rows []stepDeadlineRow
	for name, cmd := range m.Commands {
		for _, step := range cmd.Run {
			task, ok := m.Tasks[step.Task]
			if !ok {
				continue
			}
			timeoutMs := task.TimeoutMs
			if step.TimeoutMs != nil {
				timeoutMs = *step.TimeoutMs
			}
			rows = append(rows, stepDeadlineRow{
				command:   name,
				step:      step.ID,
				task:      step.Task,
				heavy:     step.Heavy != nil && *step.Heavy,
				timeoutMs: timeoutMs,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].command != rows[j].command {
			return rows[i].command < rows[j].command
		}
		return rows[i].step < rows[j].step
	})
	return rows
}

// taskRunsGolangci reports whether a task actually shells out to golangci-lint,
// read off the argv the runtime dispatches on rather than off the task name, so
// a renamed task stays covered.
func taskRunsGolangci(task proto.TaskDefinition) bool {
	for i, arg := range task.Args {
		if arg == "--tool" && i+1 < len(task.Args) && task.Args[i+1] == "golangci-lint" {
			return true
		}
	}
	return false
}

// golangciToolDeadline reads run.timeout out of the config this extension SHIPS
// to consumers. That file — not any project-local override — is the effective
// tool deadline for a consumer that has no .golangci.yml of its own, which is
// the case the production failure came from.
func golangciToolDeadline(t *testing.T) time.Duration {
	t.Helper()
	path := filepath.Join("..", "..", "config", ".golangci.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read shipped golangci config: %v", err)
	}
	var cfg struct {
		Run struct {
			Timeout string `yaml:"timeout"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse shipped golangci config: %v", err)
	}
	if cfg.Run.Timeout == "" {
		return 0
	}
	d, err := time.ParseDuration(cfg.Run.Timeout)
	if err != nil {
		t.Fatalf("run.timeout = %q is not a Go duration golangci-lint can parse: %v", cfg.Run.Timeout, err)
	}
	return d
}

func sortedNames[V any](byName map[string]V) []string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestRemoteBuildCacheIsDeclaredAndNeverKeysATask pins both halves of the
// `remote-build-cache` option.
//
// Declared: every command that spawns `go` through the shared environment
// builder exposes it, defaulting to on, so a user with a cache provider can
// fall back to the plain local Go cache on any of them.
//
// Never an input: the option selects where already identical bytes come from —
// a local directory, or that same directory warmed from the object cache — so
// folding it into a task's identity would split the task cache in two for a
// choice that cannot change a result. It is the same rule the provider socket
// itself follows in core.
func TestRemoteBuildCacheIsDeclaredAndNeverKeysATask(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "shared-build-cache", "the-remote-build-cache-option-never-enters-a-task-cache-key")
	m := loadExtensionManifest(t)
	for _, name := range []string{"build", "test", "lint", "serve", "run", "package"} {
		command, ok := m.Commands[name]
		if !ok {
			t.Errorf("command %q is missing", name)
			continue
		}
		flag, ok := command.Flags[remoteBuildCacheParam]
		if !ok {
			t.Errorf("command %q does not declare %q", name, remoteBuildCacheParam)
			continue
		}
		if flag.Type != "boolean" {
			t.Errorf("command %q flag %q type = %q, want boolean", name, remoteBuildCacheParam, flag.Type)
		}
		if enabled, ok := flag.Default.(bool); !ok || !enabled {
			t.Errorf("command %q flag %q default = %v, want true", name, remoteBuildCacheParam, flag.Default)
		}
		if strings.TrimSpace(flag.Description) == "" {
			t.Errorf("command %q flag %q has no description", name, remoteBuildCacheParam)
		}
	}
	for _, name := range sortedTaskNames(m) {
		if _, ok := m.Tasks[name].Inputs[remoteBuildCacheParam]; ok {
			t.Errorf("task %q keys its cache on %q; the option changes where bytes come from, not what the task computes",
				name, remoteBuildCacheParam)
		}
	}
}

// batchMaxProjectsParam is the option a workspace sets to cap how many Go
// projects share one `go test` invocation.
const batchMaxProjectsParam = "batch-max-projects"

// TestGoTestBatchCapIsAWorkspaceOptionThatNeverKeysATask pins the go-test
// batch policy's side of the cap. The policy names the option and carries no
// static cap, so a workspace that does not set the option keeps unbounded
// batches. The option only decides how ready projects share one invocation,
// so no task input, command flag, or step binding may carry it: the CLI keys a
// task on each of those, and keying on the cap would split the test cache for
// a value that cannot change a verdict.
func TestGoTestBatchCapIsAWorkspaceOptionThatNeverKeysATask(t *testing.T) {
	m := loadExtensionManifest(t)

	spectest.Proves(t, "go/go-project-toolchain", "test-batch-size",
		"the-go-test-batch-policy-reads-its-cap-from-the-batch-max-projects-option")
	spectest.Proves(t, "go/go-project-toolchain", "test-batch-size",
		"an-unset-batch-max-projects-leaves-go-test-batches-unbounded")
	policy := m.Tasks["test-exec"].Batchable
	if policy == nil || policy.Tool != "go-test" {
		t.Fatalf("test-exec batch policy = %+v, want the go-test tool", policy)
	}
	if policy.MaxProjectsParam != batchMaxProjectsParam {
		t.Errorf("test-exec maxProjectsParam = %q, want %q", policy.MaxProjectsParam, batchMaxProjectsParam)
	}
	if policy.MaxProjects != 0 {
		t.Errorf("test-exec maxProjects = %d, want 0: a workspace that does not set %q keeps unbounded batches",
			policy.MaxProjects, batchMaxProjectsParam)
	}

	spectest.Proves(t, "go/go-project-toolchain", "test-batch-size",
		"the-batch-max-projects-option-never-enters-a-task-cache-key")
	same := func(name string) bool {
		fold := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, "-", "")) }
		return fold(name) == fold(batchMaxProjectsParam)
	}
	for _, name := range sortedTaskNames(m) {
		for input := range m.Tasks[name].Inputs {
			if same(input) {
				t.Errorf("task %q declares input %q; the batch cap must never key a task", name, input)
			}
		}
	}
	for name, command := range m.Commands {
		for flag := range command.Flags {
			if same(flag) {
				t.Errorf("command %q declares flag %q; the CLI keys every flag of a command, and the batch cap must never key a task",
					name, flag)
			}
		}
		for _, step := range command.Run {
			for binding := range step.With {
				if same(binding) {
					t.Errorf("command %q step %q binds %q; the batch cap must never key a task", name, step.ID, binding)
				}
			}
		}
	}
}

// TestBuildTasksDoNotKeyOnTestFiles pins which tasks read a `_test.go` file.
//
// `go build` never compiles a test file, and build-describe's host binary is a
// `go build` of the project's main package, so neither task's output is a
// function of one. The declaration matters beyond the project that owns the
// file: `^describe` is the cross-project edge every dependent reaches its
// dependencies through, and a dependent's compile and test sit behind its own
// describe — so a test file in describe's key moves the identity of the whole
// dependent closure for an edit none of it reads.
//
// The exclusion is asserted on the DERIVED cache key, which is what the
// executor hashes, and the converse is asserted too: test-exec runs the tests,
// so dropping them from its key would serve a stored verdict for a test that
// never ran.
func TestBuildTasksDoNotKeyOnTestFiles(t *testing.T) {
	m := loadExtensionManifest(t)

	const goSources = "**/*.go"
	const goTests = "!**/*_test.go"
	for _, name := range []string{"build-generate", "build-describe", "build-compile", "build-cross-compile"} {
		derived := proto.DeriveTaskCacheKey(m.Tasks[name].Inputs)
		if !containsString(derived.Files, goSources) {
			t.Errorf("%s derived cache key files = %v, missing %q", name, derived.Files, goSources)
		}
		if !containsString(derived.Files, goTests) {
			t.Errorf("%s derived cache key files = %v, missing %q; a test-only edit would move this key, "+
				"and through ^describe the key of every dependent's describe, compile and test",
				name, derived.Files, goTests)
		}
	}

	derived := proto.DeriveTaskCacheKey(m.Tasks["test-exec"].Inputs)
	if !containsString(derived.Files, "**/*_test.go") {
		t.Errorf("test-exec derived cache key files = %v, missing the test sources it runs", derived.Files)
	}
	if containsString(derived.Files, goTests) {
		t.Errorf("test-exec derived cache key files = %v excludes test sources; the stored verdict would "+
			"outlive the tests it was produced from", derived.Files)
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
