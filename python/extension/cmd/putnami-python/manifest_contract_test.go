package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/docslinks"
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

// TestExtensionManifestPassesFullValidation is the `putnami dev extension
// validate` gate as a unit test: the real manifest must produce ZERO
// diagnostics under the comprehensive protocol entry point, which includes the
// v3 task-contract harness (exactness, one owner per output, honest effects).
func TestExtensionManifestPassesFullValidation(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "explicit-opt-in", "the-extension-manifest-passes-the-protocol-validation-the-cli-applies")
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

// Ruff and pytest resolve the shared uv workspace. The root pyproject.toml and
// uv.lock therefore belong in WorkspaceFiles, not in project-relative patterns
// whose leading slash still resolves under each project root.
func TestCachedPythonTasksKeyUvWorkspaceFiles(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "workspace-scope", "cached-python-tasks-are-keyed-on-the-uv-workspace-files")
	m := loadExtensionManifest(t)
	want := map[string]bool{"pyproject.toml": true, "uv.lock": true}
	for _, taskName := range []string{
		"lint-format-fix", "lint-format-readonly", "lint-check-fix",
		"lint-check-readonly", "test-run",
	} {
		task, ok := m.Tasks[taskName]
		if !ok {
			t.Errorf("cached Python task %q is missing", taskName)
			continue
		}
		input, ok := task.Inputs["workspaceConfig"]
		if !ok || input.From != proto.TaskInputFromWorkspace {
			t.Errorf("%s workspaceConfig = %+v, present=%t; want a workspace-scoped input", taskName, input, ok)
			continue
		}
		key := proto.DeriveTaskCacheKey(task.Inputs)
		if len(key.WorkspaceFiles) != len(want) {
			t.Errorf("%s workspace cache files = %v, want pyproject.toml and uv.lock", taskName, key.WorkspaceFiles)
			continue
		}
		for _, file := range key.WorkspaceFiles {
			if !want[file] {
				t.Errorf("%s workspace cache files = %v, unexpected %q", taskName, key.WorkspaceFiles, file)
			}
		}
	}
}

// TestEveryTaskCarriesAV3Declaration pins that the migration is COMPLETE. v3 is
// additive per task, so a task that quietly loses (or never gains) its
// `declares` block keeps v2 inferred capture while the rest of the manifest is
// declared — the mixed state this slice exists to end.
func TestEveryTaskCarriesAV3Declaration(t *testing.T) {
	m := loadExtensionManifest(t)
	for _, name := range sortedTaskNames(m) {
		if m.Tasks[name].Declares == nil {
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

func (r ownerRow) String() string {
	return fmt.Sprintf("%s.%s = %s %s:%s optionalEmpty=%t", r.task, r.id, r.kind, r.root, r.path, r.optionalEmpty)
}

// TestDeclaredOutputOwnerTable pins the ownership decision for the Python
// extension, amended once by the executable-spec gate: the
// ONLY captured artifact this extension produces is the reserved
// spec-verification report the test task's merge writes.
//
//   - The two ruff phases rewrite sources in place and declare cache.noOutput,
//     which already says they write no output files; source mutation is carried
//     by mutatesSources, not by an output (see
//     TestOnlyTheRuffFixTasksMutateSources).
//   - pytest is still invoked with no coverage or JUnit reporter, so those
//     files do not exist to own (see TestPytestOwnsNoReportsUntilItWritesThem);
//     the one row below is the spec-verification report the adapter's merge
//     writes from the injected plugin's fragments.
//   - workspace-install and deps-upgrade rewrite shared workspace files
//     (uv.lock, the root pyproject.toml), which is the workspace-files EFFECT,
//     not an owned output.
//   - serve and run produce a process, not a file.
//
// A row appearing or vanishing here must be a reviewed change rather than a
// silent one.
func TestDeclaredOutputOwnerTable(t *testing.T) {
	m := loadExtensionManifest(t)

	want := []ownerRow{
		{task: "test-run", id: "featureVerification", kind: "file", root: "command-output", path: "putnami-feature-verification.json", optionalEmpty: true},
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
	if len(rows) == 0 {
		return "\n  <none>"
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.String())
	}
	return "\n  " + strings.Join(out, "\n  ")
}

// TestTaskEffectTable pins HONEST EFFECTS per task: what each task does beyond
// writing its declared outputs, and — for the effects a cache hit cannot
// reproduce — that the task is not cacheable. A task missing from the table
// declares no effects.
//
// Every task in this manifest shells out through `uv`, and `uv run` resolves and
// downloads the tool it is asked for (network) into the machine-global uv cache
// and virtualenv (toolchain-cache). Every task also runs the uv workspace sync
// first, which rewrites the workspace-root pyproject.toml — that is
// workspace-files, and it is why even a per-project lint task declares it.
func TestTaskEffectTable(t *testing.T) {
	m := loadExtensionManifest(t)

	want := map[string][]string{
		"deps-upgrade-exec":      {"network", "toolchain-cache", "workspace-files"},
		"lint-check-fix":         {"network", "toolchain-cache", "workspace-files"},
		"lint-check-readonly":    {"network", "toolchain-cache", "workspace-files"},
		"lint-format-fix":        {"network", "toolchain-cache", "workspace-files"},
		"lint-format-readonly":   {"network", "toolchain-cache", "workspace-files"},
		"run-exec":               {"network", "process", "toolchain-cache", "workspace-files"},
		"serve-run":              {"network", "process", "toolchain-cache", "workspace-files"},
		"test-run":               {"network", "toolchain-cache", "workspace-files"},
		"workspace-install-exec": {"network", "toolchain-cache", "workspace-files"},
		// workspace-sync rewrites the `[project] name` of every selected
		// project's pyproject.toml (workspace-files) and nothing else: it runs
		// no uv command, so neither network nor toolchain-cache applies.
		"workspace-sync-exec": {"workspace-files"},
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

// TestServeAndRunDeclareTheProcessEffect pins the one place this manifest is
// AHEAD of the Go extension's. Go's serve-run genuinely starts a long-lived
// process but cannot say so, because the "process" effect forces
// cache.enabled:false and flipping that would move serve~serve from MISS to
// NO-CACHE — a known cache-policy gap. Both Python long-running tasks
// already set cache:false, so there is no plan to change and the effect is
// declared outright. If either ever becomes cacheable, that combination is a
// validation error and this test says which end broke first.
func TestServeAndRunDeclareTheProcessEffect(t *testing.T) {
	spectest.Proves(t, "python/experimental-workspace-integration", "native-tools", "serve-and-run-declare-the-process-effect-they-have")
	for _, name := range []string{"serve-run", "run-exec"} {
		m := loadExtensionManifest(t)
		task, ok := m.Tasks[name]
		if !ok {
			t.Fatalf("manifest task %q is missing", name)
		}
		if task.Cache.IsEnabled() {
			t.Errorf("task %q now enables caching; a task declaring the external %q effect must not be cacheable",
				name, proto.EffectProcess)
		}
		if task.Declares == nil {
			t.Fatalf("task %q lost its v3 declaration", name)
		}
		found := false
		for _, effect := range task.Declares.Effects {
			if effect == proto.EffectProcess {
				found = true
			}
		}
		if !found {
			t.Errorf("task %q must declare the %q effect: it runs the workload itself", name, proto.EffectProcess)
		}
	}
}

// TestPytestOwnsNoReportsUntilItWritesThem pins the KNOWN under-declaration of
// this manifest so it stays a decision instead of decaying into an oversight.
//
// The Go and TypeScript test tasks own coverage and JUnit files in the
// per-command output directory. The Python one invokes pytest with neither
// reporter — no --cov, no --junitxml — so those files do not exist to be
// captured; the run's counts travel as the testSummary payload parsed from
// stdout. Declaring them "just in case" would be a claim on paths the task
// never writes. The day pytest grows a reporter here, this test fails and the
// declaration has to grow with it.
//
// The exception is deliberate and single: the adapter itself — not a
// pytest reporter — writes the reserved spec-verification report from the
// injected plugin's fragments, and that file IS declared. Exactly that one.
func TestPytestOwnsNoReportsUntilItWritesThem(t *testing.T) {
	m := loadExtensionManifest(t)
	test, ok := m.Tasks["test-run"]
	if !ok {
		t.Fatal("manifest task \"test-run\" is missing")
	}
	if test.Declares == nil {
		t.Fatal("test-run lost its v3 declaration")
	}
	if len(test.Declares.Outputs) != 1 {
		t.Errorf("test-run declares %d output(s); the spec-verification report is the single deliberate exception, "+
			"and any pytest-reporter file must come with the flag that writes it", len(test.Declares.Outputs))
	}
	if _, ok := test.Declares.Outputs["featureVerification"]; !ok {
		t.Error("test-run lost the featureVerification declaration; a cache hit would silently disarm the spec gate")
	}
	for _, arg := range test.Args {
		if strings.Contains(arg, "cov") || strings.Contains(arg, "junit") {
			t.Errorf("test-run passes %q: it now produces a report and must declare the file it writes", arg)
		}
	}
	if !strings.Contains(test.Description, "junitxml") {
		t.Error("test-run must keep documenting why it owns no coverage or JUnit report")
	}
}

// TestOnlyTheRuffFixTasksMutateSources pins the check/fix split this slice
// introduced. ruff has two phases and each has a writing and a reporting mode:
// `ruff format` rewrites while `ruff format --check` only reports, and
// `ruff check --fix` rewrites while `ruff check` only reports. Before B3d one
// manifest task served both modes of each phase and declared the "sources"
// write unconditionally, which over-claimed on every check-only run. Now the
// mutating halves are their own tasks, they are the only ones declaring
// mutatesSources — together with the project-scoped "sources" write resource,
// because that resource, not the flag, is what the planner serializes
// conflicting jobs on — and the lint pipeline is gated so a check-only run can
// never schedule them.
func TestOnlyTheRuffFixTasksMutateSources(t *testing.T) {
	m := loadExtensionManifest(t)
	fixTasks := map[string]bool{"lint-format-fix": true, "lint-check-fix": true}

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

	// The read-only halves, and the documentation link check that runs in both
	// modes, must still READ the sources they report on, so the planner keeps
	// ordering them against a concurrent formatter.
	for _, name := range []string{"lint-format-readonly", "lint-check-readonly", "lint-docs"} {
		task, ok := m.Tasks[name]
		if !ok {
			t.Fatalf("manifest task %q is missing", name)
		}
		if !declaresSourceRead(task) {
			t.Errorf("task %q must declare the project-scoped %q read resource", name, proto.ResourceIDSources)
		}
	}

	// The pipeline is what makes the split real: each ruff step must be gated on
	// the fix param its task is named for, so a check-only run can never
	// schedule a mutating task. The documentation link check never writes, so
	// it runs in both modes, gated only on its own flag.
	guards := map[string]string{
		"format":            "params.fix",
		"check":             "params.fix",
		"format-check-only": "!params.fix",
		"check-check-only":  "!params.fix",
		"docs":              "params.docs-links",
	}
	tasksByStep := map[string]string{
		"format":            "lint-format-fix",
		"check":             "lint-check-fix",
		"format-check-only": "lint-format-readonly",
		"check-check-only":  "lint-check-readonly",
		"docs":              "lint-docs",
	}
	lint, ok := m.Commands["lint"]
	if !ok {
		t.Fatal("manifest command \"lint\" is missing")
	}
	seen := map[string]bool{}
	for _, step := range lint.Run {
		want, tracked := guards[step.ID]
		if !tracked {
			t.Errorf("lint pipeline has an unexpected step %q; every ruff step belongs to one half of the check/fix split", step.ID)
			continue
		}
		seen[step.ID] = true
		if step.If != want {
			t.Errorf("lint step %q condition = %q, want %q", step.ID, step.If, want)
		}
		if step.Task != tasksByStep[step.ID] {
			t.Errorf("lint step %q runs task %q, want %q", step.ID, step.Task, tasksByStep[step.ID])
		}
	}
	for id := range guards {
		if !seen[id] {
			t.Errorf("lint pipeline no longer has a %q step; the check/fix split is what the mutation flags describe", id)
		}
	}
}

// TestHarnessRejectsADishonestDeclaration is the mutation check that keeps the
// conformance tests above from being vacuous: flip one row of the real manifest
// and the SAME predicates that pass on it must fail. Without this, a harness
// that silently validated nothing would look identical to a passing one.
func TestHarnessRejectsADishonestDeclaration(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m *proto.Manifest)
		wantMsg string
	}{
		{
			// A read-only ruff pass that claims to rewrite sources without the
			// write resource the planner serializes on.
			name: "mutatesSources without the sources write resource",
			mutate: func(m *proto.Manifest) {
				task := m.Tasks["lint-check-readonly"]
				task.Declares = &proto.TaskDeclaration{MutatesSources: true}
				m.Tasks["lint-check-readonly"] = task
			},
			wantMsg: "write resource",
		},
		{
			// An external effect on a cacheable task: a cache hit would replay
			// the stored result and never start the process.
			name: "external effect while caching is enabled",
			mutate: func(m *proto.Manifest) {
				task := m.Tasks["test-run"]
				task.Declares = &proto.TaskDeclaration{Effects: []string{proto.EffectProcess}}
				m.Tasks["test-run"] = task
			},
			wantMsg: "cache.enabled",
		},
		{
			// An output on a task that already says it writes none.
			name: "declared output on a noOutput task",
			mutate: func(m *proto.Manifest) {
				task := m.Tasks["lint-format-fix"]
				task.Declares = &proto.TaskDeclaration{
					MutatesSources: true,
					Outputs: map[string]proto.DeclaredOutput{
						"report": {Kind: proto.OutputKindFile, Root: proto.OutputRootCommandOutput, Path: "ruff.json"},
					},
				}
				m.Tasks["lint-format-fix"] = task
			},
			wantMsg: "noOutput",
		},
		{
			// Two tasks of ONE command claiming the same command-output file.
			name: "two tasks of one command claiming one path",
			mutate: func(m *proto.Manifest) {
				for _, name := range []string{"lint-format-readonly", "lint-check-readonly"} {
					task := m.Tasks[name]
					task.Cache = nil
					task.Declares = &proto.TaskDeclaration{
						Outputs: map[string]proto.DeclaredOutput{
							"report": {Kind: proto.OutputKindFile, Root: proto.OutputRootCommandOutput, Path: "ruff.json"},
						},
					}
					m.Tasks[name] = task
				}
			},
			wantMsg: "exactly one owner",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := loadExtensionManifest(t)
			if diags := proto.ValidateTaskContracts(m); len(diags) > 0 {
				t.Fatalf("the real manifest must be clean before it is mutated:\n  %s", formatDiagnostics(diags))
			}
			tc.mutate(m)
			diags := proto.ValidateTaskContracts(m)
			if len(diags) == 0 {
				t.Fatal("the mutated manifest produced no diagnostics; the conformance harness is not checking anything")
			}
			if !strings.Contains(formatDiagnostics(diags), tc.wantMsg) {
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

func declaresSourceRead(task proto.TaskDefinition) bool {
	for _, ref := range task.Reads {
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

// TestLintDocsIsKeyedOnTheCandidateCut holds the lint-docs declaration to the
// one the SDK's docslinks.Job reads, so every language extension caches the
// check on the same key. The check reads the workspace's Git candidate cut,
// which the workspace input `git:**` holds: a deleted or renamed link target
// moves the key, and an ignored file does not. It writes nothing, so it
// declares no output, no effect and no source rewrite. A `git:` input needs
// the CLI contract whose key holds the executable bit, and the manifest
// carries the stamp the packager earns for it.
func TestLintDocsIsKeyedOnTheCandidateCut(t *testing.T) {
	manifest := loadExtensionManifest(t)
	task, ok := manifest.Tasks["lint-docs"]
	if !ok {
		t.Fatal("manifest task \"lint-docs\" is missing")
	}
	if manifest.CLIContract != proto.RequiredCLIContract(manifest) {
		t.Errorf("cliContract = %d, want the contract the manifest's vocabulary requires, %d",
			manifest.CLIContract, proto.RequiredCLIContract(manifest))
	}
	if !reflect.DeepEqual(task.Inputs, docslinks.Inputs()) {
		t.Errorf("lint-docs inputs = %+v, want docslinks.Inputs() = %+v", task.Inputs, docslinks.Inputs())
	}
	if !task.Cache.IsEnabled() || !task.Cache.NoOutput {
		t.Errorf("lint-docs cache = %+v, want enabled with noOutput", task.Cache)
	}
	if task.Declares == nil || len(task.Declares.Outputs) > 0 || len(task.Declares.Effects) > 0 || task.Declares.MutatesSources {
		t.Errorf("lint-docs declares %+v, want no output, no effect and no source rewrite", task.Declares)
	}
}
