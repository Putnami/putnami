package turbo

import (
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
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "monorepo/turbo-adoption"

// demoFile is the Turborepo demo of putnami observe: a pnpm workspace where
// apps/api depends on packages/pricing, with the reports its CI wrote.
const demoFile = "../../tooling/observe/internal/observe/testdata/demos/turbo.txtar"

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

// repository writes the demo's base files, with a turbo.json that also
// declares lint, a dev task and a root task.
func repository(t *testing.T, d demo) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		writeFile(t, root, rel, content)
	}
	return root
}

func readProjectFile(t *testing.T, root, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, dir, wsproto.ConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
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
		Paths: []string{".", "apps/api", "packages/pricing", "tools/other"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTheMembersOfATurborepoWorkspaceBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "packages-become-projects", "the-members-of-a-turborepo-workspace-become-projects-with-their-edges")
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, "tools/other/package.json", `{"name": "other"}`)

	result := probe(t, root)
	got := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		got[p.Path] = p
	}
	if len(got) != 2 {
		t.Fatalf("projects = %+v, want the two members alone", result.Projects)
	}
	api := got["apps/api"]
	if api.SourceName != "@demo/api" || !reflect.DeepEqual(api.Dependencies, []string{"packages/pricing"}) ||
		!reflect.DeepEqual(api.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(api.Tags, []string{"turbo"}) {
		t.Errorf("api = %+v", api)
	}
	var meta stacks.Metadata
	if err := json.Unmarshal(got["packages/pricing"].Metadata, &meta); err != nil || meta.Component != "@demo/pricing" || meta.Root != "." {
		t.Errorf("pricing metadata = %+v, %v", meta, err)
	}

	// A package an import mapped to another extension is not answered.
	writeFile(t, root, "packages/pricing/putnami.json", `{"extensions": ["@putnami/typescript"]}`)
	for _, p := range probe(t, root).Projects {
		if p.Path == "packages/pricing" {
			t.Errorf("an imported TypeScript package is answered: %+v", p)
		}
	}

	// A root without turbo.json is no Turborepo repository.
	if err := os.Remove(filepath.Join(root, Marker)); err != nil {
		t.Fatal(err)
	}
	if result := probe(t, root); len(result.Projects) != 0 {
		t.Errorf("projects without turbo.json = %+v", result.Projects)
	}
}

func TestTheSyncWritesEachPackageInTheChosenMode(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-package-graph", "the-sync-writes-each-package-putnami-json-in-the-chosen-mode")
	d := readDemo(t)
	root := repository(t, d)

	// Without a recorded mode the sync imports: both packages hold
	// TypeScript sources, so they declare both extensions; their tsc -p build
	// scripts and their vitest scripts move to the TypeScript extension.
	result, err := Sync(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != ModeImport || !reflect.DeepEqual(result.Written, []string{"apps/api/putnami.json", "packages/pricing/putnami.json"}) {
		t.Fatalf("result = %+v", result)
	}
	api := readProjectFile(t, root, "apps/api")
	want := map[string]any{
		"$schema":      ProjectSchema,
		"extensions":   []any{ExtensionName, TypeScriptExtension},
		"dependencies": []any{"/packages/pricing"},
		"tags":         []any{"turbo"},
		"disable":      map[string]any{"jobs": []any{"lint", "@putnami/turbo:test", "@putnami/turbo:build"}},
		// The api starts with node: it declares the node runtime. Its test
		// script type-checked nothing, so the moved test runs no type check.
		"options": map[string]any{TypeScriptExtension: map[string]any{"runtime": "node"}, stacks.TypeScriptRunnerLayer: map[string]any{"types": false}},
	}
	if !reflect.DeepEqual(api, want) {
		t.Errorf("api putnami.json = %v, want %v", api, want)
	}

	// A script that already runs what the TypeScript extension runs moves
	// to it: the Turborepo job of that task is disabled.
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "tsc && cp README.md dist", "test": "bun test --coverage"}}`)
	if _, err := Sync(root, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "packages/pricing")["disable"]; !reflect.DeepEqual(got, map[string]any{"jobs": []any{"lint", "@putnami/turbo:test", "@putnami/typescript:build"}}) {
		t.Errorf("pricing disable = %v", got)
	}
	writeFile(t, root, "packages/pricing/package.json", d["base"]["packages/pricing/package.json"])

	// The recorded delegate mode runs every package through Turborepo, and
	// keeps every member the sync does not own.
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "demo", "options": {"@putnami/turbo": {"mode": "delegate"}}}`)
	writeFile(t, root, "apps/api/putnami.json", `{"name": "api", "extensions": ["@putnami/typescript", "@acme/lint"], "dependencies": ["@acme/design"], "disable": {"jobs": ["serve", "lint"]}}`)
	result, err = Sync(root, Options{})
	if err != nil || result.Mode != ModeDelegate || len(result.Written) != 2 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	api = readProjectFile(t, root, "apps/api")
	want = map[string]any{
		"name":         "api",
		"extensions":   []any{"@acme/lint", ExtensionName},
		"dependencies": []any{"/packages/pricing", "@acme/design"},
		"tags":         []any{"turbo"},
		"disable":      map[string]any{"jobs": []any{"serve", "lint"}},
	}
	if !reflect.DeepEqual(api, want) {
		t.Errorf("api putnami.json = %v, want %v", api, want)
	}

	// A second sync changes nothing, and a package with no test script has
	// test disabled.
	if again, err := Sync(root, Options{}); err != nil || len(again.Written) != 0 || again.Unchanged != 2 {
		t.Errorf("a second sync = %+v, %v", again, err)
	}
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "tsc"}}`)
	if _, err := Sync(root, Options{}); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "packages/pricing")["disable"]; !reflect.DeepEqual(got, map[string]any{"jobs": []any{"lint", "test"}}) {
		t.Errorf("pricing disable = %v", got)
	}

	// A package that leaves the workspace loses what the sync wrote, and an
	// edge to it goes.
	if err := os.Rename(filepath.Join(root, "packages/pricing/package.json"), filepath.Join(root, "packages/pricing/package.json.old")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "apps/api/package.json", `{"name": "@demo/api", "scripts": {"test": "vitest run", "build": "tsc"}}`)
	result, err = Sync(root, Options{})
	if err != nil || !reflect.DeepEqual(result.Retracted, []string{"packages/pricing/putnami.json"}) {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := readProjectFile(t, root, "packages/pricing"); !reflect.DeepEqual(got, map[string]any{"$schema": ProjectSchema}) {
		t.Errorf("a former package keeps %v", got)
	}
	if got := readProjectFile(t, root, "apps/api")["dependencies"]; !reflect.DeepEqual(got, []any{"@acme/design"}) {
		t.Errorf("api dependencies = %v", got)
	}

	// An unknown mode fails, and a root without turbo.json is no repository.
	if _, err := Sync(root, Options{Mode: "copy"}); err == nil || !strings.Contains(err.Error(), "import or delegate") {
		t.Errorf("an unknown mode: %v", err)
	}
	if result, err := Sync(t.TempDir(), Options{}); err != nil || result.Repository {
		t.Errorf("an empty root: %+v %v", result, err)
	}
}

func TestImportMapsVitestAndJestTestScriptsToTheTypeScriptTest(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-package-graph", "import-maps-vitest-and-jest-test-scripts-to-the-typescript-test")
	d := readDemo(t)
	for _, script := range []string{"vitest run --coverage", "vitest", "jest --ci", "npx jest"} {
		t.Run(script, func(t *testing.T) {
			root := repository(t, d)
			writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "tsup src/index.ts", "test": "`+script+`"}}`)
			result, err := Sync(root, Options{})
			if err != nil {
				t.Fatal(err)
			}
			jobs := readProjectFile(t, root, "packages/pricing")["disable"].(map[string]any)["jobs"]
			if !reflect.DeepEqual(jobs, []any{"lint", "@putnami/turbo:test", "@putnami/typescript:build"}) {
				t.Errorf("pricing jobs = %v, want the test on @putnami/typescript", jobs)
			}
			if slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool { return u.Task == "@demo/pricing#test" }) {
				t.Errorf("unmapped = %+v, want the test mapped", result.Unmapped)
			}
			if slices.Contains(result.Unreported, "packages/pricing") {
				t.Errorf("unreported = %v: a test the TypeScript extension runs writes its own reports", result.Unreported)
			}
		})
	}
}

func TestADryRunSyncPrintsTheDiffAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "sync-previews-a-diff", "a-dry-run-sync-prints-the-diff-and-writes-nothing")
	d := readDemo(t)
	root := repository(t, d)
	if _, err := Sync(root, Options{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json"))
	result, err := Sync(root, Options{Mode: ModeDelegate, DryRun: true})
	if err != nil || len(result.Written) != 2 || len(result.Diffs) != 2 {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if !strings.Contains(result.Diffs[0], "--- a/apps/api/putnami.json\n+++ b/apps/api/putnami.json\n") ||
		!strings.Contains(result.Diffs[0], "-    \"@putnami/typescript\"\n+    \"@putnami/turbo\"\n") {
		t.Errorf("diff = %s", result.Diffs[0])
	}
	if after, _ := os.ReadFile(filepath.Join(root, "apps/api/putnami.json")); string(after) != string(before) {
		t.Errorf("a dry run wrote the file")
	}
	lines := strings.Join(Report(result, true), "\n")
	if !strings.Contains(lines, "delegate mode: 2 putnami.json to write") || !strings.Contains(lines, "+    \"@putnami/turbo\"") {
		t.Errorf("report:\n%s", lines)
	}

	// A file the sync creates diffs from /dev/null.
	if got := UnifiedDiff("x/putnami.json", nil, []byte("{}\n")); got != "--- /dev/null\n+++ b/x/putnami.json\n@@ -0,0 +1,1 @@\n+{}\n" {
		t.Errorf("a new file diff = %q", got)
	}
}

func TestTheSyncListsTheTasksItCannotMap(t *testing.T) {
	spectest.Proves(t, feature, "unmapped-tasks-are-listed", "the-sync-lists-the-tasks-it-cannot-map-and-the-packages-without-reports")
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, "turbo.json", `{
  // Turborepo reads comments and trailing commas.
  "tasks": {
    "build": {"dependsOn": ["^build"]},
    "test": {"dependsOn": ["^build"],},
    "dev": {"cache": false, "persistent": true},
    "//#format": {},
    "@demo/api#lint": {},
  },
}`)
	writeFile(t, root, "apps/api/turbo.json", `{"extends": ["//"], "tasks": {"typecheck": {}}}`)
	writeFile(t, root, "apps/api/putnami.json", `{"options": {"@putnami/turbo": {"tasks": {"test": {"reports": {"junit": ["reports/junit.xml"]}}}}}}`)
	result, err := Sync(root, Options{Mode: ModeDelegate})
	if err != nil {
		t.Fatal(err)
	}
	var tasks []string
	for _, u := range result.Unmapped {
		tasks = append(tasks, u.Task)
	}
	if !reflect.DeepEqual(tasks, []string{"//#format", "dev", "typecheck"}) || !strings.Contains(result.Unmapped[1].Reason, "turbo run dev") {
		t.Errorf("unmapped = %+v", result.Unmapped)
	}
	if !reflect.DeepEqual(result.Unreported, []string{"packages/pricing"}) {
		t.Errorf("unreported = %v, want pricing, whose test names no report", result.Unreported)
	}
	// A package-scoped task counts for its package alone: api has a lint
	// script-less task, so its lint stays disabled.
	if got := readProjectFile(t, root, "apps/api")["disable"]; !reflect.DeepEqual(got, map[string]any{"jobs": []any{"lint"}}) {
		t.Errorf("api disable = %v", got)
	}
	lines := strings.Join(Report(result, false), "\n")
	if !strings.Contains(lines, "Not mapped, kept on Turborepo:\n  //#format: a root task") || !strings.Contains(lines, "exit code alone, no JUnit report named: packages/pricing") {
		t.Errorf("report:\n%s", lines)
	}

	// In import mode, a mocha script stays on Turborepo and is listed with
	// the tool @putnami/typescript would run instead; the tsc -p build moves.
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "tsc -p tsconfig.json", "test": "mocha"}}`)
	result, err = Sync(root, Options{Mode: ModeImport})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, u := range result.Unmapped {
		reasons[u.Task] = u.Reason
	}
	if got := reasons["@demo/pricing#test"]; !strings.Contains(got, `runs bun test or vitest or jest or npx vitest or npx jest, not "mocha"`) {
		t.Errorf("pricing test reason = %q, unmapped %+v", got, result.Unmapped)
	}
	if _, listed := reasons["@demo/pricing#build"]; listed || !reflect.DeepEqual(result.Unreported, []string{"packages/pricing"}) {
		t.Errorf("import: unmapped %+v, unreported %v", result.Unmapped, result.Unreported)
	}
}

func TestAnESLintLintScriptMapsToTheTypeScriptLint(t *testing.T) {
	spectest.Proves(t, feature, "import-keeps-eslint", "an-eslint-lint-script-maps-to-the-typescript-lint")
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, "turbo.json", `{"tasks": {"build": {}, "test": {}, "lint": {}}}`)
	for _, script := range []string{"eslint .", "npx eslint", "pnpm exec eslint ."} {
		writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"lint": "`+script+`"}}`)
		result, err := Sync(root, Options{Mode: ModeImport})
		if err != nil {
			t.Fatal(err)
		}
		jobs, _ := readProjectFile(t, root, "packages/pricing")["disable"].(map[string]any)["jobs"].([]any)
		if !slices.Contains(jobs, any(ExtensionName+":lint")) || slices.Contains(jobs, any(TypeScriptExtension+":lint")) {
			t.Errorf("lint script %q: jobs = %v, want the TypeScript lint kept and the Turborepo lint disabled", script, jobs)
		}
		for _, u := range result.Unmapped {
			if u.Task == "@demo/pricing#lint" {
				t.Errorf("lint script %q is listed as unmapped: %s", script, u.Reason)
			}
		}
	}
}

func TestAChainedLintScriptStaysOnTurborepo(t *testing.T) {
	spectest.Proves(t, feature, "import-keeps-eslint", "a-chained-lint-script-stays-on-turborepo")
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, "turbo.json", `{"tasks": {"build": {}, "test": {}, "lint": {}}}`)
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"lint": "eslint . && prettier --check ."}}`)
	result, err := Sync(root, Options{Mode: ModeImport})
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := readProjectFile(t, root, "packages/pricing")["disable"].(map[string]any)["jobs"].([]any)
	if !slices.Contains(jobs, any(TypeScriptExtension+":lint")) {
		t.Errorf("jobs = %v, want the TypeScript lint disabled", jobs)
	}
	if !slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool {
		return u.Task == "@demo/pricing#lint" && strings.Contains(u.Reason, "prettier")
	}) {
		t.Errorf("unmapped = %+v, want the chained lint listed", result.Unmapped)
	}
}

func TestESLintWithOtherArgumentsStaysOnTurborepo(t *testing.T) {
	spectest.Proves(t, feature, "import-keeps-eslint", "eslint-with-other-arguments-stays-on-turborepo")
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, "turbo.json", `{"tasks": {"build": {}, "test": {}, "lint": {}}}`)
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"lint": "eslint --ext .ts src"}}`)
	result, err := Sync(root, Options{Mode: ModeImport})
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := readProjectFile(t, root, "packages/pricing")["disable"].(map[string]any)["jobs"].([]any)
	if !slices.Contains(jobs, any(TypeScriptExtension+":lint")) {
		t.Errorf("jobs = %v, want the TypeScript lint disabled", jobs)
	}
	if !slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool {
		return u.Task == "@demo/pricing#lint" && strings.Contains(u.Reason, "would drop")
	}) {
		t.Errorf("unmapped = %+v, want the lint listed with the arguments it would lose", result.Unmapped)
	}
}

func TestAStalePackageGraphFailsValidate(t *testing.T) {
	spectest.Proves(t, feature, "validate-fails-on-a-stale-graph", "a-stale-package-graph-fails-validate")
	d := readDemo(t)
	root := repository(t, d)
	if _, err := Sync(root, Options{}); err != nil {
		t.Fatal(err)
	}
	check := func() (string, string) {
		var events strings.Builder
		emit := jsonl.New()
		emit.Observe(func(message string) { events.WriteString(message + "\n") })
		status, _, err := RunCheck(&pctx.Context{WorkspaceRoot: root}, emit, nil)
		if err != nil {
			events.WriteString(err.Error())
		}
		return status, events.String()
	}
	if status, events := check(); status != "OK" {
		t.Fatalf("a current graph: %s %s", status, events)
	}
	// pricing gains a workspace dependency on api.
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "tsc", "test": "vitest run"}, "dependencies": {"@demo/api": "workspace:*"}}`)
	before, _ := os.ReadFile(filepath.Join(root, "packages/pricing/putnami.json"))
	status, events := check()
	if status != "FAILED" || !strings.Contains(events, CodeSyncStale) || !strings.Contains(events, "packages/pricing/putnami.json") || !strings.Contains(events, `+    "/apps/api"`) {
		t.Errorf("a stale graph: %s %s", status, events)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "packages/pricing/putnami.json")); string(after) != string(before) {
		t.Error("the check wrote the file")
	}
}

// stubTurbo puts first on PATH a turbo that prints its version, logs each
// call with its working directory to $STUB_LOG, and for run test writes the
// demo's JUnit and coverage reports of the filtered package.
func stubTurbo(t *testing.T, d demo) {
	t.Helper()
	bin, reports := t.TempDir(), t.TempDir()
	for rel, content := range d["ci"] {
		writeFile(t, reports, rel, content)
	}
	script := `#!/bin/sh
set -e
echo "$(pwd) turbo $*" >> "$STUB_LOG"
case "$1" in --version) echo "${STUB_VERSION:-2.5.4}"; exit 0;; esac
case "$*" in
  "run test --filter=@demo/pricing") dir=packages/pricing;;
  "run test --filter=@demo/api") dir=apps/api;;
  *) exit 0;;
esac
mkdir -p "$dir/reports/coverage"
cp "` + reports + `/$dir/reports/junit.xml" "$dir/reports/junit.xml"
cp "` + reports + `/$dir/reports/coverage/lcov.info" "$dir/reports/coverage/lcov.info"
exit "${STUB_TEST_EXIT:-0}"
`
	writeFile(t, bin, "turbo", script)
	if err := os.Chmod(filepath.Join(bin, "turbo"), 0o755); err != nil {
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

// runJob runs a task job of the stack on one package of root, the way the
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

func TestATaskRunsTurboOnItsPackageAndRecordsTheDelegation(t *testing.T) {
	spectest.Proves(t, feature, "tasks-delegate-to-turbo", "a-task-runs-turbo-on-its-package-and-records-the-delegation")
	d := readDemo(t)
	stubTurbo(t, d)
	root := repository(t, d)
	rootReal, _ := filepath.EvalSymlinks(root)

	for _, task := range stacks.TaskNames {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		status, data, events := runJob(t, root, "apps/api", task)
		if status != "OK" || data["delegatedTo"] != "turbo" {
			t.Errorf("%s: status %s, data %v, events %s", task, status, data, events)
		}
		log := stubLog(t)
		want := rootReal + " turbo run " + task + " --filter=@demo/api"
		if len(log) != 2 || !strings.HasSuffix(log[0], " turbo --version") || log[1] != want {
			t.Errorf("%s ran %q, want the version check then %q", task, log, want)
		}
	}
	t.Setenv("STUB_TEST_EXIT", "1")
	if status, _, events := runJob(t, root, "apps/api", stacks.TaskTest); status != "FAILED" || !strings.Contains(events, stacks.CodeStepFailed) {
		t.Errorf("a failing turbo run fails the task: %s %s", status, events)
	}
}

func TestTheReportsThePackageOptionsNameBecomeTheTestSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-from-the-package-options", "the-reports-the-package-options-name-become-the-test-summary")
	d := readDemo(t)
	stubTurbo(t, d)
	root := repository(t, d)

	// Without reports, the result is the exit code alone.
	status, data, events := runJob(t, root, "packages/pricing", stacks.TaskTest)
	if _, summarized := data["testSummary"]; status != "OK" || summarized {
		t.Errorf("without reports: %s %v %s", status, data, events)
	}

	writeFile(t, root, "packages/pricing/putnami.json", `{"extensions": ["@putnami/turbo"], "options": {"@putnami/turbo": {"tasks": {"test": {"reports": {"junit": ["reports/junit.xml"], "coverage": [{"format": "lcov", "paths": ["reports/coverage/lcov.info"]}]}}}}}}`)
	status, data, events = runJob(t, root, "packages/pricing", stacks.TaskTest)
	if status != "OK" {
		t.Fatalf("status %s: %s", status, events)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 6 || got["passed"] != 6 {
		t.Errorf("testSummary = %v, want pricing's 6 cases", got)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 19 || got["total"] != 19 {
		t.Errorf("coverageSummary = %v, want pricing's 19 lines", got)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	s := stack(t)
	if !s.Profile.Delegated {
		t.Fatal("the Turborepo profile is delegated")
	}
	stackstest.CheckManifest(t, "putnami.extension.json", s)
	if !slices.Equal(Modes, []string{ModeImport, ModeDelegate}) {
		t.Errorf("modes = %v, import first", Modes)
	}
}

func TestDelegateModeImportsThePackagesTheImportListSelects(t *testing.T) {
	spectest.Proves(t, feature, "delegate-imports-listed-packages", "delegate-mode-imports-the-packages-the-import-list-selects")
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "bun build src/index.ts --outdir dist", "test": "bun test"}, "dependencies": {"zod": "3.23.8"}}`)
	workspace := func(entries ...string) {
		data, _ := json.Marshal(map[string]any{"name": "demo", "options": map[string]any{ExtensionName: map[string]any{"mode": ModeDelegate, "import": entries}}})
		writeFile(t, root, wsproto.WorkspaceConfigFilename, string(data))
	}
	workspace("/packages/pricing", "libs/*")
	result, err := Sync(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != ModeDelegate || !slices.Equal(result.Imported, []string{"packages/pricing"}) || !slices.Equal(result.Unmatched, []string{"libs/*"}) {
		t.Fatalf("result = %+v", result)
	}
	// pricing is imported: both extensions, and the TypeScript extension
	// runs its build and test.
	pricing := readProjectFile(t, root, "packages/pricing")
	if got, want := pricing["extensions"], []any{ExtensionName, TypeScriptExtension}; !reflect.DeepEqual(got, want) {
		t.Errorf("pricing extensions = %v, want %v", got, want)
	}
	if got, want := pricing["disable"], map[string]any{"jobs": []any{"lint", "@putnami/turbo:test", "@putnami/turbo:build"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("pricing disable = %v, want %v", got, want)
	}
	// api stays delegated: Turborepo alone runs it.
	api := readProjectFile(t, root, "apps/api")
	if got, want := api["extensions"], []any{ExtensionName}; !reflect.DeepEqual(got, want) {
		t.Errorf("api extensions = %v, want %v", got, want)
	}
	if got, want := api["dependencies"], []any{"/packages/pricing"}; !reflect.DeepEqual(got, want) {
		t.Errorf("api dependencies = %v, want %v", got, want)
	}
	if report := strings.Join(Report(result, false), "\n"); !strings.Contains(report, "delegate mode with 1 imported (packages/pricing)") {
		t.Errorf("the report names no import:\n%s", report)
	}

	// The import parameter outranks the config: an empty list delegates
	// pricing again, and import mode reads no list.
	preview, err := Sync(root, Options{DryRun: true, Import: []string{}, ImportGiven: true})
	if err != nil || len(preview.Imported) != 0 || !slices.Equal(preview.Written, []string{"packages/pricing/putnami.json"}) {
		t.Errorf("the empty import parameter: %+v %v", preview, err)
	}
	if r, err := Sync(root, Options{Mode: ModeImport, DryRun: true}); err != nil || r.Imported != nil || r.Unmatched != nil {
		t.Errorf("import mode read the list: %+v %v", r, err)
	}

	check := func() (string, string) {
		var events strings.Builder
		emit := jsonl.New()
		emit.Observe(func(message string) { events.WriteString(message + "\n") })
		status, _, err := RunCheck(&pctx.Context{WorkspaceRoot: root}, emit, nil)
		if err != nil {
			events.WriteString(err.Error())
		}
		return status, events.String()
	}
	if status, events := check(); status != "OK" || !strings.Contains(events, `lists "libs/*", which selects no package`) {
		t.Errorf("a current list: %s %s", status, events)
	}
	// pricing leaves the list with no sync: validate fails.
	workspace()
	if status, events := check(); status != "FAILED" || !strings.Contains(events, CodeSyncStale) || !strings.Contains(events, "packages/pricing/putnami.json") {
		t.Errorf("a list the putnami.json files do not match: %s %s", status, events)
	}

	// The sync job reads the import parameter, and fails on one that is no
	// list.
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{"dryRun": json.RawMessage(`true`), "import": json.RawMessage(`["packages/*"]`)}}
	status, data, err := RunSync(ctx, emit, nil)
	if err != nil || status != "OK" || !reflect.DeepEqual(data["imported"], []string{"packages/pricing"}) {
		t.Errorf("status %s, data %v, err %v\n%s", status, data, err, events.String())
	}
	ctx.Params["import"] = json.RawMessage(`{}`)
	if status, _, _ := RunSync(ctx, emit, nil); status != "FAILED" {
		t.Errorf("an import parameter that is no list: %s", status)
	}
	// A malformed entry fails the sync.
	workspace("packages/[")
	if _, err := Sync(root, Options{}); err == nil {
		t.Error("a malformed entry passed")
	}
}

func TestImportMapsABuildScriptThatRunsTscAlone(t *testing.T) {
	spectest.Proves(t, feature, "import-maps-a-tsc-build-script", "a-tsc-alone-build-script-is-imported")
	spectest.Proves(t, feature, "import-maps-a-tsc-build-script", "a-tsc-script-with-another-argument-stays-on-turborepo")
	d := readDemo(t)
	root := repository(t, d)
	for script, mapped := range map[string]bool{
		"tsc":                      true,
		"tsc -b":                   true,
		"tsc --build --verbose":    true,
		"tsc -p tsconfig.lib.json": true,
		"tsc --watch":              false,
		"tsc -b --force":           false,
		"tsc && cp README.md dist": false,
		"NODE_ENV=production tsc":  false,
		"tsup src/index.ts":        false,
	} {
		writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "`+script+`", "test": "bun test"}}`)
		result, err := Sync(root, Options{Mode: ModeImport})
		if err != nil {
			t.Fatal(err)
		}
		reasons := map[string]string{}
		for _, u := range result.Unmapped {
			reasons[u.Task] = u.Reason
		}
		disabled := readProjectFile(t, root, "packages/pricing")["disable"].(map[string]any)["jobs"].([]any)
		if mapped {
			if _, listed := reasons["@demo/pricing#build"]; listed || !slices.Contains(disabled, any(ExtensionName+":build")) {
				t.Errorf("%q: build listed %v, disabled %v; want it moved to %s", script, listed, disabled, TypeScriptExtension)
			}
			continue
		}
		// A chained script gets the reason that names the chain.
		want := "tsc alone"
		if strings.ContainsAny(script, "&|;") {
			want = "chains another command"
		}
		reason := reasons["@demo/pricing#build"]
		if !strings.Contains(reason, want) || !strings.Contains(reason, script) || !slices.Contains(disabled, any(TypeScriptExtension+":build")) {
			t.Errorf("%q: reason %q, disabled %v; want it kept on Turborepo with its reason", script, reason, disabled)
		}
	}
}

func TestImportMarksTheNodeRuntime(t *testing.T) {
	spectest.Proves(t, feature, "import-marks-the-node-runtime", "a-node-start-script-marks-the-node-runtime")
	d := readDemo(t)
	root := repository(t, d)
	// The demo's api starts with node dist/server.js; pricing has no start
	// script.
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"build": "bun build src/index.ts --outdir dist"}}`)
	if _, err := Sync(root, Options{Mode: ModeImport}); err != nil {
		t.Fatal(err)
	}
	runtime := func(dir string) any {
		options, _ := readProjectFile(t, root, dir)["options"].(map[string]any)
		layer, _ := options[TypeScriptExtension].(map[string]any)
		return layer["runtime"]
	}
	if got := runtime("apps/api"); got != "node" {
		t.Errorf("a node start script: runtime = %v, want node", got)
	}
	if got := runtime("packages/pricing"); got != nil {
		t.Errorf("a package without a node start script: runtime = %v, want none", got)
	}

	// A delegated package keeps no runtime: Turborepo alone runs it.
	root = repository(t, d)
	writeFile(t, root, "packages/pricing/package.json", `{"name": "@demo/pricing", "scripts": {"start": "node dist/main.js"}}`)
	if _, err := Sync(root, Options{Mode: ModeDelegate}); err != nil {
		t.Fatal(err)
	}
	if got := runtime("packages/pricing"); got != nil {
		t.Errorf("a delegated package: runtime = %v, want none", got)
	}
}

func TestAnImportAddsSourceExportsAndATSConfig(t *testing.T) {
	spectest.Proves(t, feature, "import-adopts-typescript-conventions", "an-import-adds-source-exports-and-a-tsconfig")
	d := readDemo(t)
	root := repository(t, d)
	// pricing builds with Bun, so its main names an output nothing writes in
	// the package; api builds with tsc, which writes the output its main
	// names, and holds its own tsconfig.build.json.
	pricing := "{\n  \"name\": \"@demo/pricing\",\n  \"private\": true,\n  \"main\": \"dist/index.js\",\n  \"scripts\": {\"build\": \"bun build src/index.ts --outdir dist\", \"test\": \"bun test\"}\n}\n"
	api := "{\n  \"name\": \"@demo/api\",\n  \"private\": true,\n  \"main\": \"dist/index.js\",\n  \"scripts\": {\"build\": \"tsc -p tsconfig.build.json\"},\n  \"dependencies\": {\"@demo/pricing\": \"workspace:*\"}\n}\n"
	writeFile(t, root, "packages/pricing/package.json", pricing)
	writeFile(t, root, "apps/api/package.json", api)
	writeFile(t, root, "apps/api/tsconfig.build.json", "{}\n")
	writeFile(t, root, "apps/api/src/index.ts", "export {}\n")

	dry, err := Sync(root, Options{Mode: ModeImport, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "packages/pricing/package.json")); string(data) != pricing {
		t.Errorf("a dry sync wrote pricing's package.json")
	}
	joined := strings.Join(Report(dry, true), "\n")
	if !strings.Contains(joined, "+++ b/packages/pricing/package.json") || !strings.Contains(joined, "+++ b/packages/pricing/tsconfig.json") {
		t.Errorf("the preview shows no convention diff:\n%s", joined)
	}
	result, err := Sync(root, Options{Mode: ModeImport})
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "packages/pricing/package.json")); !strings.Contains(string(data), `"exports": {`) {
		t.Errorf("pricing package.json = %s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "apps/api/package.json")); string(data) != api {
		t.Errorf("a tsc build's package.json changed: %s", data)
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/tsconfig.json")); !os.IsNotExist(err) {
		t.Errorf("api, which holds a tsconfig.build.json, got a tsconfig.json: %v", err)
	}
	if !slices.ContainsFunc(result.Conventions, func(n string) bool { return strings.HasPrefix(n, "packages/pricing/package.json: exports") }) {
		t.Errorf("conventions = %v", result.Conventions)
	}
	if again, err := Sync(root, Options{Mode: ModeImport, DryRun: true}); err != nil || len(again.Diffs) != 0 {
		t.Errorf("a second sync shows %v, %v", again.Diffs, err)
	}
	// Delegate mode adopts no convention.
	root = repository(t, d)
	writeFile(t, root, "packages/pricing/package.json", pricing)
	if result, err := Sync(root, Options{Mode: ModeDelegate}); err != nil || len(result.Conventions) != 0 {
		t.Errorf("delegate mode: %v %v", result.Conventions, err)
	}
}

// TestTheProbeAttributesEachEdgeToTurborepo checks that every edge the probe
// answers is Turborepo's own (build-graph), in delegate mode and for a package
// an import keeps on Turborepo, so the declared-edge check and `putnami deps
// prune` leave alone an edge the sync writes and would write back.
func TestTheProbeAttributesEachEdgeToTurborepo(t *testing.T) {
	d := readDemo(t)
	root := repository(t, d)
	s := stack(t)
	check := func(mode string) {
		t.Helper()
		result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
			DependencySources: true, Paths: []string{".", "apps/api", "packages/pricing"}})
		if err != nil {
			t.Fatal(err)
		}
		edges := 0
		for _, p := range result.Projects {
			for _, dep := range p.Dependencies {
				edges++
				if p.DependencySources[dep] != wsproto.DependencySourceBuildGraph {
					t.Errorf("%s: %s edge %s source = %q, want %q", mode, p.Path, dep, p.DependencySources[dep], wsproto.DependencySourceBuildGraph)
				}
			}
		}
		if edges == 0 {
			t.Fatalf("%s: projects = %+v, want the api's edge to pricing", mode, result.Projects)
		}
	}
	check("delegate")
	writeFile(t, root, "apps/api/putnami.json", `{"extensions": ["@putnami/turbo", "@putnami/typescript"]}`)
	check("import")
}

func TestAnImportedTaskDoesWhatItsScriptDid(t *testing.T) {
	spectest.Proves(t, feature, "a-moved-task-does-what-its-script-did", "an-imported-task-does-what-its-script-did")
	d := readDemo(t)
	root := repository(t, d)
	writeFile(t, root, "turbo.json", `{"tasks": {"build": {}, "test": {}, "lint": {}}}`)
	// pricing is published and its main names a build output: Putnami's Bun
	// build would not write it, so the build stays on Turborepo.
	writeFile(t, root, "packages/pricing/package.json", "{\n  \"name\": \"@demo/pricing\",\n  \"main\": \"dist/index.js\",\n  \"scripts\": {\"build\": \"bun build src/index.ts --outdir dist\", \"test\": \"bun test\", \"lint\": \"biome check .\"}\n}\n")
	writeFile(t, root, "packages/pricing/src/index.ts", "export {}\n")
	writeFile(t, root, "apps/api/package.json", `{"name": "@demo/api", "private": true, "scripts": {"lint": "biome check --write ."}, "dependencies": {"@demo/pricing": "workspace:*"}}`)
	result, err := Sync(root, Options{Mode: ModeImport})
	if err != nil {
		t.Fatal(err)
	}
	pricing := readProjectFile(t, root, "packages/pricing")
	if jobs := pricing["disable"].(map[string]any)["jobs"]; !reflect.DeepEqual(jobs, []any{"@putnami/turbo:lint", "@putnami/turbo:test", "@putnami/typescript:build"}) {
		t.Errorf("pricing jobs = %v, want the build on Turborepo", jobs)
	}
	if !slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool {
		return u.Task == "@demo/pricing#build" && strings.Contains(u.Reason, `names "dist/index.js", which the Bun build`)
	}) {
		t.Errorf("unmapped = %+v, want the build kept for its entry", result.Unmapped)
	}
	want := map[string]any{
		"@putnami/typescript:test": map[string]any{"types": false},
	}
	if !reflect.DeepEqual(pricing["options"], want) {
		t.Errorf("pricing options = %v, want a test with no type check", pricing["options"])
	}
	// A package whose test did not move gets no option.
	if options := readProjectFile(t, root, "apps/api")["options"]; options != nil {
		t.Errorf("api options = %v, want none", options)
	}
	if lines := strings.Join(Report(result, false), "\n"); !strings.Contains(lines, stacks.LintDependentsNote) {
		t.Errorf("report:\n%s", lines)
	}
	// Delegate mode retracts the options.
	if _, err := Sync(root, Options{Mode: ModeDelegate}); err != nil {
		t.Fatal(err)
	}
	if options := readProjectFile(t, root, "packages/pricing")["options"]; options != nil {
		t.Errorf("delegated pricing options = %v", options)
	}
}
