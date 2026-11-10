package nx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	detect "go.putnami.dev/tooling/observe/detect/nx"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "monorepo/nx-adoption"

// demoFile is the Nx demo of putnami observe: a Nx 21 workspace of npm
// packages where @demo/api depends on @demo/pricing, with the reports its CI
// wrote.
const demoFile = "../../tooling/observe/internal/observe/testdata/demos/nx.txtar"

// demo holds the demo's sections by prefix: base/ is the repository, head/
// the change on top of it, ci/ what the CI wrote.
type demo map[string]map[string]string

func readDemo(t *testing.T) demo {
	t.Helper()
	data, err := os.ReadFile(demoFile)
	if err != nil {
		t.Fatal(err)
	}
	out := demo{}
	var name string
	var body strings.Builder
	flush := func() {
		if name == "" {
			return
		}
		prefix, rel, _ := strings.Cut(name, "/")
		if out[prefix] == nil {
			out[prefix] = map[string]string{}
		}
		out[prefix][rel] = body.String()
	}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		trimmed := strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(trimmed, "-- ") && strings.HasSuffix(trimmed, " --") {
			flush()
			name, body = strings.TrimSuffix(strings.TrimPrefix(trimmed, "-- "), " --"), strings.Builder{}
			continue
		}
		if name != "" {
			body.WriteString(line)
		}
	}
	flush()
	return out
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repository writes the demo's base files, and a workspace file that
// declares the extensions and records the mode, when mode is not empty.
func repository(t *testing.T, d demo, mode string, extensions ...string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		writeFile(t, root, rel, content)
	}
	if mode != "" || len(extensions) > 0 {
		doc := map[string]any{"name": "shop", "extensions": append([]string{ExtensionName}, extensions...)}
		if mode != "" {
			doc["options"] = map[string]any{ExtensionName: map[string]any{ModeOption: mode}}
		}
		data, _ := json.Marshal(doc)
		writeFile(t, root, wsproto.WorkspaceConfigFilename, string(data))
	}
	return root
}

// stubNx puts first on PATH an npx that answers npx --no nx --version with
// the demo's Nx version and, for npx --no nx run <project>:test, writes the
// demo's reports of that project. It logs each call, with its working
// directory, to $STUB_LOG.
func stubNx(t *testing.T, d demo) {
	t.Helper()
	bin, reports := t.TempDir(), t.TempDir()
	for rel, content := range d["ci"] {
		writeFile(t, reports, rel, content)
	}
	script := `#!/bin/sh
set -e
echo "$(pwd) npx $*" >> "$STUB_LOG"
[ "$1" = "--no" ] && [ "$2" = "nx" ] || { echo "unexpected npx $*" >&2; exit 2; }
case "$3" in
  --version) printf 'Nx Version:\n- Local: v%s\n- Global: Not found\n' "${STUB_VERSION:-21.0.0}"; exit 0;;
  run)
    case "$4" in
      @demo/api:test) dir=apps/api;;
      @demo/pricing:test) dir=packages/pricing;;
      *) exit "${STUB_EXIT:-0}";;
    esac
    [ -n "$STUB_SKIP_REPORTS" ] && exit 0
    mkdir -p "$dir/reports/coverage"
    cp "` + reports + `/$dir/reports/junit.xml" "$dir/reports/junit.xml"
    cp "` + reports + `/$dir/reports/coverage/cobertura-coverage.xml" "$dir/reports/coverage/cobertura-coverage.xml"
    exit "${STUB_EXIT:-0}";;
esac
`
	writeFile(t, bin, "npx", script)
	if err := os.Chmod(filepath.Join(bin, "npx"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
}

func stubLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("STUB_LOG"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func stack(t *testing.T) stacks.Stack {
	t.Helper()
	s, err := Stack()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func probe(t *testing.T, root string) wsproto.ProbeResult {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
		Paths: []string{".", "apps/api", "packages/pricing"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func readProjectFile(t *testing.T, root, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, dir, "putnami.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func sync(t *testing.T, root string, opts SyncOptions) SyncResult {
	t.Helper()
	result, err := Sync(context.Background(), root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTheProjectsOfANxWorkspaceBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "projects-become-projects", "the-projects-of-a-nx-workspace-become-projects-with-their-edges")
	result := probe(t, repository(t, readDemo(t), ""))
	if len(result.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v", result.Diagnostics)
	}
	want := map[string]struct {
		name string
		deps []string
	}{
		"apps/api":         {"@demo/api", []string{"packages/pricing"}},
		"packages/pricing": {"@demo/pricing", nil},
	}
	if len(result.Projects) != len(want) {
		t.Fatalf("projects = %+v, want %v; the root package.json is no Nx project", result.Projects, want)
	}
	for _, p := range result.Projects {
		w := want[p.Path]
		if p.SourceName != w.name || p.SourceFile != p.Path+"/package.json" || !reflect.DeepEqual(p.Dependencies, w.deps) ||
			!reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"nx"}) {
			t.Errorf("%s = %+v", p.Path, p)
		}
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil ||
			!reflect.DeepEqual(meta, stacks.Metadata{Component: w.name, Root: ".", Toolchain: stacks.Expected{Version: "^21.0.0", Source: "package.json"}, Manifest: p.Path + "/package.json"}) {
			t.Errorf("%s metadata = %s (%v)", p.Path, p.Metadata, err)
		}
	}
}

func TestTheSyncWritesEachProjectPutnamiJSONInBothModes(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-project-graph", "the-sync-writes-each-project-putnami-json-in-both-modes")
	d := readDemo(t)

	// A root with no nx.json is no Nx workspace.
	if result := sync(t, t.TempDir(), SyncOptions{}); result.Source != "" || len(result.Written) != 0 {
		t.Errorf("no Nx workspace: %+v", result)
	}

	// Delegate: each project gets the extension, its edges, lint disabled
	// since it has no lint target, and the reports its jest configuration
	// names.
	root := repository(t, d, ModeDelegate)
	writeFile(t, root, "apps/api/putnami.json", `{"name": "storefront", "dependencies": ["/gone", "@acme/design"], "disable": {"jobs": ["publish", "test"]}}`+"\n")
	result := sync(t, root, SyncOptions{})
	if want := []string{"apps/api/putnami.json", "packages/pricing/putnami.json"}; result.Mode != ModeDelegate || result.Source != "nx.json" || !reflect.DeepEqual(result.Written, want) {
		t.Errorf("result %+v, want %v written in delegate mode", result, want)
	}
	reports := func(dir string) map[string]any {
		return map[string]any{ExtensionName: map[string]any{"tasks": map[string]any{"test": map[string]any{"reports": map[string]any{
			"junit":    []any{"reports/junit.xml"},
			"coverage": []any{map[string]any{"format": "cobertura", "paths": []any{"reports/coverage/cobertura-coverage.xml"}}},
		}}}}}
	}
	pricing := readProjectFile(t, root, "packages/pricing")
	if want := map[string]any{"$schema": ProjectSchema, "extensions": []any{ExtensionName}, "disable": map[string]any{"jobs": []any{"lint"}}, "options": reports("packages/pricing")}; !reflect.DeepEqual(pricing, want) {
		t.Errorf("pricing = %v, want %v", pricing, want)
	}
	// An existing file keeps what the sync does not own; a dependency on a
	// directory that is gone goes, and a disabled test of a tested project
	// too.
	api := readProjectFile(t, root, "apps/api")
	if want := map[string]any{"name": "storefront", "extensions": []any{ExtensionName}, "dependencies": []any{"/packages/pricing", "@acme/design"}, "disable": map[string]any{"jobs": []any{"publish", "lint"}}, "options": reports("apps/api")}; !reflect.DeepEqual(api, want) {
		t.Errorf("api = %v, want %v", api, want)
	}

	// A second sync changes nothing.
	if again := sync(t, root, SyncOptions{}); len(again.Written) != 0 || again.Unchanged != 2 {
		t.Errorf("a second sync = %+v", again)
	}

	// A dry run in the other mode writes nothing and returns the diff.
	before, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json"))
	writeFile(t, root, "putnami.workspace.json", `{"name": "shop", "extensions": ["@putnami/nx", "@putnami/typescript"], "options": {"@putnami/nx": {"mode": "delegate"}}}`)
	dry := sync(t, root, SyncOptions{Mode: ModeImport, DryRun: true})
	if dry.Mode != ModeImport || len(dry.Changes) != 2 {
		t.Fatalf("dry run = %+v", dry)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json")); string(after) != string(before) {
		t.Errorf("a dry run wrote %s", after)
	}
	if diff := Diff(dry.Changes[0].File, dry.Changes[0].Before, dry.Changes[0].After); !strings.Contains(diff, "+++ b/apps/api/putnami.json") || !strings.Contains(diff, `+      "@putnami/typescript:build",`) {
		t.Errorf("diff = %s", diff)
	}

	// A former project loses the extension and what the sync owns.
	writeFile(t, root, "libs/old/putnami.json", `{"extensions": ["@putnami/nx", "/x"], "dependencies": ["/apps/api"], "disable": {"jobs": ["lint", "serve"]}}`)
	result = sync(t, root, SyncOptions{})
	if !reflect.DeepEqual(result.Retracted, []string{"libs/old/putnami.json"}) {
		t.Errorf("retracted = %v", result.Retracted)
	}
	if old := readProjectFile(t, root, "libs/old"); !reflect.DeepEqual(old, map[string]any{"extensions": []any{"/x"}, "disable": map[string]any{"jobs": []any{"serve"}}}) {
		t.Errorf("old = %v", old)
	}
}

func TestImportMapsEachTypeScriptTargetToTheTypeScriptTask(t *testing.T) {
	spectest.Proves(t, feature, "import-maps-typescript-targets", "import-maps-each-typescript-target-to-the-typescript-task")
	d := readDemo(t)
	root := repository(t, d, ModeImport, TypeScriptExtension)
	// pricing's scripts already run what @putnami/typescript runs: they move
	// to it. The api keeps jest, a webpack build and an eslint lint, which
	// stay on Nx.
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "bun build src/index.ts --outdir dist", "test": "bun test", "lint": "biome check ."}}`)
	writeFile(t, root, "apps/api/project.json", `{"name": "@demo/api", "targets": {"build": {"executor": "@nx/webpack:webpack"}, "lint": {"executor": "@nx/eslint:lint"}}}`)
	result := sync(t, root, SyncOptions{})
	if result.Mode != ModeImport {
		t.Fatalf("mode = %s", result.Mode)
	}
	jobs := func(dir string) any { return readProjectFile(t, root, dir)["disable"].(map[string]any)["jobs"] }
	if got, want := jobs("packages/pricing"), []any{"@putnami/nx:build", "@putnami/nx:lint", "@putnami/nx:test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pricing jobs = %v, want %v", got, want)
	}
	if got, want := jobs("apps/api"), []any{"@putnami/typescript:build", "@putnami/typescript:lint", "@putnami/typescript:test"}; !reflect.DeepEqual(got, want) {
		t.Errorf("api jobs = %v, want %v", got, want)
	}
	// A putnami.json that names extensions is claimed by those alone: each
	// TypeScript project names @putnami/typescript beside the extension.
	for _, dir := range []string{"packages/pricing", "apps/api"} {
		if got, want := readProjectFile(t, root, dir)["extensions"], []any{ExtensionName, TypeScriptExtension}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s extensions = %v, want %v", dir, got, want)
		}
	}
	// A test the TypeScript extension runs carries no Nx reports; one that
	// stays on Nx does.
	if _, ok := readProjectFile(t, root, "packages/pricing")["options"]; ok {
		t.Errorf("an imported test carries no Nx reports")
	}
	if _, ok := readProjectFile(t, root, "apps/api")["options"]; !ok {
		t.Errorf("a test that stays on Nx carries its reports")
	}

	// Without @putnami/typescript in the workspace, import runs every target
	// through Nx, and says why.
	root = repository(t, d, ModeImport)
	result = sync(t, root, SyncOptions{})
	if got := jobs("packages/pricing"); !reflect.DeepEqual(got, []any{"lint"}) {
		t.Errorf("jobs without the TypeScript extension = %v", got)
	}
	if got := readProjectFile(t, root, "packages/pricing")["extensions"]; !reflect.DeepEqual(got, []any{ExtensionName}) {
		t.Errorf("extensions without the TypeScript extension = %v", got)
	}
	if !slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool { return strings.Contains(u.Reason, "does not declare @putnami/typescript") }) {
		t.Errorf("unmapped = %+v, want the undeclared extension named", result.Unmapped)
	}
}

func TestAnUnmappedTargetIsListedWithItsReason(t *testing.T) {
	spectest.Proves(t, feature, "unmapped-targets-are-listed", "an-unmapped-target-is-listed-with-its-reason")
	d := readDemo(t)
	root := repository(t, d, ModeImport, TypeScriptExtension)
	writeFile(t, root, "apps/api/project.json", `{"name": "@demo/api", "targets": {"build": {"executor": "@nx/webpack:webpack"}}}`)
	result := sync(t, root, SyncOptions{})
	reasons := map[string]string{}
	for _, u := range result.Unmapped {
		reasons[u.Project+":"+u.Target] = u.Reason
	}
	for key, want := range map[string]string{
		"@demo/api:start":     "nx run @demo/api:start",
		"@demo/api:build":     "@nx/webpack:webpack",
		"@demo/api:test":      `not "jest"`,
		"@demo/pricing:build": `not "tsc -p tsconfig.json"`,
		"@demo/pricing:test":  `not "jest"`,
	} {
		if !strings.Contains(reasons[key], want) {
			t.Errorf("%s reason = %q, want %q (all: %v)", key, reasons[key], want, reasons)
		}
	}

	// A delegated test whose configuration names no report is listed.
	root = repository(t, d, ModeDelegate)
	writeFile(t, root, "apps/api/jest.config.js", "module.exports = {};\n")
	result = sync(t, root, SyncOptions{})
	if !slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool {
		return u.Project == "@demo/api" && u.Target == "test" && strings.Contains(u.Reason, "exit code alone")
	}) {
		t.Errorf("unmapped = %+v, want the api test without reports", result.Unmapped)
	}
	if _, ok := readProjectFile(t, root, "apps/api")["options"]; ok {
		t.Errorf("a test without reports carries no reports override")
	}
}

func TestTheReportsResolveRootDirAndTheNxJestWorkingDirectory(t *testing.T) {
	spectest.Proves(t, feature, "reports-from-project-options", "the-junit-and-coverage-files-the-project-names-become-the-summary")
	root := t.TempDir()
	config := func(text string) {
		writeFile(t, root, "libs/a/jest.config.ts", text)
	}
	reports := func(executor string) map[string]any {
		return testReports(root, "libs/a", detect.Target{Name: "test", Executor: executor})
	}
	config(`export default { reporters: ['default', ['jest-junit', { outputDirectory: '<rootDir>/reports' }]], coverageDirectory: '<rootDir>/coverage', coverageReporters: ['lcov'] };`)
	want := map[string]any{"junit": []any{"reports/junit.xml"}, "coverage": []any{map[string]any{"format": "lcov", "paths": []any{"coverage/lcov.info"}}}}
	if got := reports("@nx/jest:jest"); !reflect.DeepEqual(got, want) {
		t.Errorf("<rootDir> reports = %v, want %v", got, want)
	}
	// Under @nx/jest, jest-junit writes in the workspace root.
	config(`export default { reporters: [['jest-junit', { outputDirectory: 'reports/libs/a' }]] };`)
	if got, want := reports("@nx/jest:jest"), map[string]any{"junit": []any{"{root}/reports/libs/a/junit.xml"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("@nx/jest reports = %v, want %v", got, want)
	}
	if got, want := reports("nx:run-script"), map[string]any{"junit": []any{"reports/libs/a/junit.xml"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("run-script reports = %v, want %v", got, want)
	}
	// The demo's configuration names <rootDir> elsewhere: under @nx/jest its
	// jest-junit report still lands in the workspace root.
	config(readDemo(t)["base"]["packages/pricing/jest.config.js"])
	want = map[string]any{"junit": []any{"{root}/reports/junit.xml"}, "coverage": []any{map[string]any{"format": "cobertura", "paths": []any{"reports/coverage/cobertura-coverage.xml"}}}}
	if got := reports("@nx/jest:jest"); !reflect.DeepEqual(got, want) {
		t.Errorf("demo under @nx/jest = %v, want %v", got, want)
	}
	// An absolute outputDirectory names no report.
	config(`export default { reporters: [['jest-junit', { outputDirectory: '/tmp/reports' }]] };`)
	if got := reports("@nx/jest:jest"); got != nil {
		t.Errorf("an absolute outputDirectory = %v, want no report", got)
	}
}

func TestAStaleProjectGraphFailsValidate(t *testing.T) {
	spectest.Proves(t, feature, "validate-fails-on-a-stale-graph", "a-stale-project-graph-fails-validate")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate)
	sync(t, root, SyncOptions{})
	run := func() (string, string) {
		var events strings.Builder
		emit := jsonl.New()
		emit.Observe(func(message string) { events.WriteString(message + "\n") })
		status, _, _ := RunCheck(&pctx.Context{WorkspaceRoot: root}, emit, nil)
		return status, events.String()
	}
	if status, events := run(); status != "OK" {
		t.Errorf("a current graph: %s %s", status, events)
	}
	// The api stops importing pricing: Nx's graph loses the edge.
	writeFile(t, root, "apps/api/package.json", `{"name": "@demo/api", "scripts": {"build": "tsc", "test": "jest"}}`)
	before, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json"))
	status, events := run()
	if status != "FAILED" || !strings.Contains(events, "no longer matches the Nx project graph") || !strings.Contains(events, "apps/api/putnami.json") {
		t.Errorf("a stale graph: %s %s", status, events)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json")); string(after) != string(before) {
		t.Errorf("the check wrote %s", after)
	}
}

// runJob runs a task job of the stack on one project of root, the way the
// CLI does, and returns its status, its data and what it emitted.
func runJob(t *testing.T, root, dir, task string) (string, map[string]any, string) {
	t.Helper()
	s := stack(t)
	result := probe(t, root)
	var meta json.RawMessage
	for _, p := range result.Projects {
		if p.Path == dir {
			meta = p.Metadata
		}
	}
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	ctx := &pctx.Context{WorkspaceRoot: root}
	ctx.Project.FullPath = filepath.Join(root, filepath.FromSlash(dir))
	ctx.Project.Metadata = map[string]json.RawMessage{ExtensionName: meta}
	status, data, err := s.Jobs()[task](ctx, emit, []string{"--report-dir", filepath.Join(root, ".putnami/out", dir)})
	if err != nil {
		events.WriteString(err.Error() + "\n")
	}
	return status, data, events.String()
}

func TestATaskRunsNxRunOnItsProjectAndRecordsTheDelegation(t *testing.T) {
	spectest.Proves(t, feature, "tasks-delegate-to-nx", "a-task-runs-nx-run-on-its-project-and-records-the-delegation")
	d := readDemo(t)
	stubNx(t, d)
	root := repository(t, d, ModeDelegate)
	rootReal, _ := filepath.EvalSymlinks(root)

	for task, want := range map[string]string{
		stacks.TaskLint:  "npx --no nx run @demo/pricing:lint --outputStyle=static",
		stacks.TaskBuild: "npx --no nx run @demo/pricing:build --outputStyle=static",
		stacks.TaskTest:  "npx --no nx run @demo/pricing:test --outputStyle=static",
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		status, data, events := runJob(t, root, "packages/pricing", task)
		if status != "OK" || data["delegatedTo"] != "nx" {
			t.Errorf("%s: status %s, data %v, events %s", task, status, data, events)
		}
		log := stubLog(t)
		if len(log) != 2 || !strings.HasSuffix(log[0], " npx --no nx --version") || log[1] != rootReal+" "+want {
			t.Errorf("%s ran %q, want the version check then %q in %s", task, log, want, rootReal)
		}
	}

	t.Setenv("STUB_VERSION", "20.8.0")
	if status, _, events := runJob(t, root, "packages/pricing", stacks.TaskLint); status != "FAILED" || !strings.Contains(events, stacks.CodeToolchainMismatch) {
		t.Errorf("a Nx version outside package.json's range fails the task: %s %s", status, events)
	}
	t.Setenv("STUB_VERSION", "")
	t.Setenv("STUB_EXIT", "1")
	if status, _, events := runJob(t, root, "packages/pricing", stacks.TaskBuild); status != "FAILED" || !strings.Contains(events, stacks.CodeStepFailed) {
		t.Errorf("a failing nx run fails the task: %s %s", status, events)
	}
}

func TestTheReportsTheTestConfigurationNamesBecomeTheSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-from-project-options", "the-junit-and-coverage-files-the-project-names-become-the-summary")
	d := readDemo(t)
	stubNx(t, d)
	root := repository(t, d, ModeDelegate)
	sync(t, root, SyncOptions{})
	// A report an earlier run left is removed before the run: the api's
	// report in pricing's place is never read.
	writeFile(t, root, "packages/pricing/reports/junit.xml", d["ci"]["apps/api/reports/junit.xml"])
	t.Setenv("STUB_SKIP_REPORTS", "1")
	if status, _, events := runJob(t, root, "packages/pricing", stacks.TaskTest); status != "FAILED" || !strings.Contains(events, "wrote no") {
		t.Errorf("a run that writes no report fails, whatever an earlier run left: %s %s", status, events)
	}
	t.Setenv("STUB_SKIP_REPORTS", "")

	status, data, events := runJob(t, root, "packages/pricing", stacks.TaskTest)
	if status != "OK" {
		t.Fatalf("status %s: %s", status, events)
	}
	summary, _ := data["testSummary"].(map[string]any)
	coverage, _ := data["coverageSummary"].(map[string]any)
	if summary == nil || coverage == nil || summary["failed"] != 0 || summary["total"] == 0 {
		t.Errorf("summaries = %v %v", summary, coverage)
	}

	// Without reports, the result is the exit code alone.
	writeFile(t, root, "packages/pricing/putnami.json", `{"extensions": ["@putnami/nx"]}`)
	status, data, events = runJob(t, root, "packages/pricing", stacks.TaskTest)
	if _, ok := data["testSummary"]; status != "OK" || ok {
		t.Errorf("without reports: %s %v %s", status, data, events)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	s := stack(t)
	if !s.Profile.Delegated {
		t.Fatal("the Nx profile is delegated")
	}
	stackstest.CheckManifest(t, "putnami.extension.json", s)
}

func TestTheSyncJobPreviewsAnotherModeAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-project-graph", "the-sync-writes-each-project-putnami-json-in-both-modes")
	root := repository(t, readDemo(t), ModeImport)
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{"dryRun": json.RawMessage(`true`), ModeOption: json.RawMessage(`"delegate"`)}}
	status, data, err := RunSync(ctx, emit, nil)
	if err != nil || status != "OK" || data["mode"] != ModeDelegate || data["dryRun"] != true {
		t.Fatalf("status %s, data %v, err %v", status, data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a dry run writes no putnami.json: %v", err)
	}
	for _, want := range []string{"apps/api/putnami.json would change (delegate mode)", "+++ b/apps/api/putnami.json", "@demo/api:start no Putnami task runs it"} {
		if !strings.Contains(events.String(), want) {
			t.Errorf("the dry run's output lacks %q:\n%s", want, events.String())
		}
	}
	// An unknown mode fails the job.
	ctx.Params[ModeOption] = json.RawMessage(`"mirror"`)
	if status, _, _ := RunSync(ctx, emit, nil); status != "FAILED" {
		t.Errorf("an unknown mode: %s", status)
	}
}

func TestTheDryRunOfASwitchToImportPreviewsWhatTheApplyWrites(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-project-graph", "the-sync-writes-each-project-putnami-json-in-both-modes")
	d := readDemo(t)
	for name, adopted := range map[string]func(root string){
		// The workspace was adopted in delegate mode and synchronized.
		"delegate to import": func(root string) {
			writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "extensions": ["@putnami/nx"], "options": {"@putnami/nx": {"mode": "delegate"}}}`)
			sync(t, root, SyncOptions{})
		},
		// The workspace declares the extension alone, records no mode and
		// was never synchronized.
		"a first import": func(root string) {
			writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "extensions": ["@putnami/nx"]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := repository(t, d, "")
			// pricing's scripts run what @putnami/typescript runs, so import
			// moves its targets; the api's stay on Nx.
			writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "bun build src/index.ts --outdir dist", "test": "bun test", "lint": "biome check ."}}`)
			adopted(root)
			workspace, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))

			// The preview of putnami init --from nx --mode import: the mode,
			// and @putnami/typescript, which --apply declares before its sync.
			preview := sync(t, root, SyncOptions{Mode: ModeImport, DryRun: true, Declared: []string{TypeScriptExtension}})
			var events strings.Builder
			emit := jsonl.New()
			emit.Observe(func(message string) { events.WriteString(message + "\n") })
			ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{
				"dryRun":      json.RawMessage(`true`),
				ModeOption:    json.RawMessage(`"import"`),
				DeclaredParam: json.RawMessage(`["@putnami/typescript"]`),
			}}
			status, data, err := RunSync(ctx, emit, nil)
			if err != nil || status != "OK" || !reflect.DeepEqual(data["written"], preview.Written) {
				t.Fatalf("the job's dry run: status %s, data %v, err %v, want %v written\n%s", status, data, err, preview.Written, events.String())
			}
			if !strings.Contains(events.String(), `+      "@putnami/nx:build",`) {
				t.Errorf("the preview moves pricing's build to @putnami/typescript:\n%s", events.String())
			}
			if after, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename)); string(after) != string(workspace) {
				t.Errorf("a dry run wrote %s", after)
			}
			// Without the extension --apply declares, the dry run moves no
			// target: it previewed something else than --apply writes.
			if without := sync(t, root, SyncOptions{Mode: ModeImport, DryRun: true}); reflect.DeepEqual(changes(without), changes(preview)) {
				t.Errorf("the dry run without the declared extension previews the apply: %v", changes(without))
			}

			// --apply records the mode and declares @putnami/typescript, then
			// synchronizes in the recorded mode.
			writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "extensions": ["@putnami/nx", "@putnami/typescript"], "options": {"@putnami/nx": {"mode": "import"}}}`)
			applied := sync(t, root, SyncOptions{})
			if len(applied.Written) == 0 || !reflect.DeepEqual(preview.Written, applied.Written) || !reflect.DeepEqual(preview.Retracted, applied.Retracted) {
				t.Errorf("preview wrote %v and retracted %v, apply %v and %v", preview.Written, preview.Retracted, applied.Written, applied.Retracted)
			}
			if got, want := changes(preview), changes(applied); !reflect.DeepEqual(got, want) {
				t.Errorf("the preview's changes:\n%v\nthe apply's:\n%v", got, want)
			}
			if !reflect.DeepEqual(preview.Unmapped, applied.Unmapped) {
				t.Errorf("the preview lists %+v unmapped, the apply %+v", preview.Unmapped, applied.Unmapped)
			}
		})
	}

	// A declaredExtensions parameter that names no list fails the job.
	root := repository(t, d, ModeDelegate)
	ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{"dryRun": json.RawMessage(`true`), DeclaredParam: json.RawMessage(`{"a": 1}`)}}
	if status, _, _ := RunSync(ctx, jsonl.New(), nil); status != "FAILED" {
		t.Errorf("a malformed %s: %s", DeclaredParam, status)
	}
	// A comma-separated one, as a flag gives it, reads as a list.
	if got, err := stringsParam(pctx.Params{DeclaredParam: json.RawMessage(`"@putnami/typescript, @putnami/go"`)}, DeclaredParam); err != nil || !reflect.DeepEqual(got, []string{TypeScriptExtension, "@putnami/go"}) {
		t.Errorf("a comma-separated %s = %v, %v", DeclaredParam, got, err)
	}
}

// changes renders the changes of a sync as file, content before and content
// after.
func changes(r SyncResult) []string {
	var out []string
	for _, c := range r.Changes {
		out = append(out, c.File+"\n"+string(c.Before)+"\n→\n"+string(c.After))
	}
	return out
}

// workspaceWithImport writes the workspace file of a delegated workspace
// that declares @putnami/typescript and lists the import entries.
func workspaceWithImport(t *testing.T, root string, entries ...string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"name":       "shop",
		"extensions": []string{ExtensionName, TypeScriptExtension},
		"options":    map[string]any{ExtensionName: map[string]any{ModeOption: ModeDelegate, "import": entries}},
	})
	writeFile(t, root, wsproto.WorkspaceConfigFilename, string(data))
}

func TestDelegateModeImportsTheProjectsTheImportListSelects(t *testing.T) {
	spectest.Proves(t, feature, "delegate-imports-listed-projects", "delegate-mode-imports-the-projects-the-import-list-selects")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate)
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "bun build src/index.ts --outdir dist", "test": "bun test"}}`)
	workspaceWithImport(t, root, "/packages/pricing", "/tools/*")
	result := sync(t, root, SyncOptions{})
	if result.Mode != ModeDelegate || !slices.Equal(result.Imported, []string{"packages/pricing"}) {
		t.Fatalf("mode %s, imported %v", result.Mode, result.Imported)
	}
	if !slices.Equal(result.Unmatched, []string{"/tools/*"}) {
		t.Errorf("unmatched = %v, want the entry that selects no project", result.Unmatched)
	}
	jobs := func(dir string) any { return readProjectFile(t, root, dir)["disable"].(map[string]any)["jobs"] }
	// pricing is imported: @putnami/typescript runs its build and test.
	if got, want := jobs("packages/pricing"), []any{"@putnami/nx:build", "@putnami/nx:test", "lint"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pricing jobs = %v, want %v", got, want)
	}
	if got, want := readProjectFile(t, root, "packages/pricing")["extensions"], []any{ExtensionName, TypeScriptExtension}; !reflect.DeepEqual(got, want) {
		t.Errorf("pricing extensions = %v, want %v", got, want)
	}
	if got, want := readProjectFile(t, root, "apps/api")["extensions"], []any{ExtensionName}; !reflect.DeepEqual(got, want) {
		t.Errorf("api extensions = %v, want %v", got, want)
	}
	// api stays delegated: every target runs through nx run, and the
	// TypeScript extension runs none.
	if got, want := jobs("apps/api"), []any{"@putnami/typescript:build", "@putnami/typescript:test", "lint"}; !reflect.DeepEqual(got, want) {
		t.Errorf("api jobs = %v, want %v", got, want)
	}
	// A delegated project's targets are not listed as staying on Nx: that is
	// what delegate mode does.
	for _, u := range result.Unmapped {
		if u.Project == "@demo/api" && u.Target != "start" {
			t.Errorf("a delegated target is listed: %+v", u)
		}
	}

	// The import parameter outranks the option: an empty list delegates
	// pricing again, and validate then reports pricing as stale.
	preview := sync(t, root, SyncOptions{DryRun: true, Import: []string{}, ImportGiven: true})
	if len(preview.Imported) != 0 || !slices.Contains(preview.Written, "packages/pricing/putnami.json") {
		t.Errorf("the empty import parameter: imported %v, written %v", preview.Imported, preview.Written)
	}
	// Import mode reads no list.
	if r := sync(t, root, SyncOptions{Mode: ModeImport, DryRun: true}); r.Imported != nil || r.Unmatched != nil {
		t.Errorf("import mode read the list: %v %v", r.Imported, r.Unmatched)
	}

	run := func() (string, string) {
		var events strings.Builder
		emit := jsonl.New()
		emit.Observe(func(message string) { events.WriteString(message + "\n") })
		status, _, _ := RunCheck(&pctx.Context{WorkspaceRoot: root}, emit, nil)
		return status, events.String()
	}
	status, events := run()
	if status != "OK" || !strings.Contains(events, `lists "/tools/*", which selects no Nx project`) {
		t.Errorf("a current list: %s %s", status, events)
	}
	// pricing leaves the list with no sync: validate fails until the sync
	// runs.
	workspaceWithImport(t, root, "apps/*")
	if status, events := run(); status != "FAILED" || !strings.Contains(events, "packages/pricing/putnami.json no longer matches") {
		t.Errorf("a list the putnami.json files do not match: %s %s", status, events)
	}
	sync(t, root, SyncOptions{})
	if status, events := run(); status != "OK" {
		t.Errorf("after the sync: %s %s", status, events)
	}
	// Delegated again, pricing no longer names @putnami/typescript.
	if got, want := readProjectFile(t, root, "packages/pricing")["extensions"], []any{ExtensionName}; !reflect.DeepEqual(got, want) {
		t.Errorf("delegated pricing extensions = %v, want %v", got, want)
	}

	// A malformed entry fails the sync and writes nothing.
	workspaceWithImport(t, root, "packages/[")
	if _, err := Sync(context.Background(), root, SyncOptions{}); err == nil || !strings.Contains(err.Error(), "packages/[") {
		t.Errorf("a malformed entry: %v", err)
	}
}

func TestTheSyncJobReadsTheImportParameter(t *testing.T) {
	spectest.Proves(t, feature, "delegate-imports-listed-projects", "delegate-mode-imports-the-projects-the-import-list-selects")
	root := repository(t, readDemo(t), ModeDelegate, TypeScriptExtension)
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{"dryRun": json.RawMessage(`true`), "import": json.RawMessage(`"/packages/pricing,/nowhere"`)}}
	status, data, err := RunSync(ctx, emit, nil)
	if err != nil || status != "OK" || !reflect.DeepEqual(data["imported"], []string{"packages/pricing"}) {
		t.Fatalf("status %s, data %v, err %v", status, data, err)
	}
	for _, want := range []string{"delegate mode with 1 imported (packages/pricing)", `lists "/nowhere", which selects no Nx project`} {
		if !strings.Contains(events.String(), want) {
			t.Errorf("the dry run's output lacks %q:\n%s", want, events.String())
		}
	}
	ctx.Params["import"] = json.RawMessage(`3`)
	if status, _, _ := RunSync(ctx, emit, nil); status != "FAILED" {
		t.Errorf("an import parameter that is no list: %s", status)
	}
}
