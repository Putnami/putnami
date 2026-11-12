package rush

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

const feature = "monorepo/rush-adoption"

// demoFile is the Rush demo of putnami observe: @demo/api depends on
// @demo/pricing, and @demo/web stands alone.
const demoFile = "../../tooling/observe/internal/observe/testdata/demos/rush.txtar"

// rushVersion is the rushVersion of the demo's rush.json.
const rushVersion = "5.140.0"

// commandLine declares a bulk test command, a phased deploy command and a
// global command; it declares no lint.
const commandLine = `// Rush's custom commands.
{
  "commands": [
    { "commandKind": "bulk", "name": "test", "summary": "Test" },
    { "commandKind": "phased", "name": "deploy", "summary": "Deploy", "phases": ["_phase:deploy"] },
    { "commandKind": "global", "name": "prettier", "summary": "Format", "shellCommand": "prettier ." },
  ]
}
`

// demo holds the demo's sections by prefix: base/ is the repository and
// head/ the change on top of it.
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

// repository writes the demo's base files and a workspace config in the
// mode, with command-line.json when commands.
func repository(t *testing.T, d demo, mode string, commands bool) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		stackstest.WriteFile(t, root, rel, content)
	}
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "extensions": ["@putnami/rush"], "options": {"@putnami/rush": {"mode": "`+mode+`"}}}`)
	if commands {
		stackstest.WriteFile(t, root, CommandLineFile, commandLine)
	}
	return root
}

func readProjectFile(t *testing.T, root, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), wsproto.ConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
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
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, DependencySources: true,
		Paths: []string{".", "apps/api", "apps/web", "libs/pricing", "common/autoinstallers/lint"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTheRushProjectsBecomeProjectsWithTheirEdges(t *testing.T) {
	spectest.Proves(t, feature, "projects-become-projects", "the-rush-projects-become-projects-with-their-edges")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, false)
	stackstest.WriteFile(t, root, "common/autoinstallers/lint/package.json", `{"name": "lint"}`)

	result := probe(t, root)
	got := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		got[p.Path] = p
	}
	for dir, want := range map[string]struct {
		name string
		deps []string
	}{
		"apps/api":     {"@demo/api", []string{"libs/pricing"}},
		"apps/web":     {"@demo/web", nil},
		"libs/pricing": {"@demo/pricing", nil},
	} {
		p, ok := got[dir]
		if !ok {
			t.Fatalf("%s is no project: %+v", dir, result.Projects)
		}
		if p.SourceName != want.name || !slices.Contains(p.Extensions, ExtensionName) {
			t.Errorf("%s = %+v, want %s with %s", dir, p, want.name, ExtensionName)
		}
		var deps []string
		for _, dep := range p.Dependencies {
			deps = append(deps, strings.TrimPrefix(dep, "/"))
		}
		if !reflect.DeepEqual(deps, want.deps) {
			t.Errorf("%s dependencies = %v, want %v", dir, p.Dependencies, want.deps)
		}
		// Each edge is one Rush's own project graph states (ADR 0007 of
		// protocols/workspace).
		for _, dep := range p.Dependencies {
			if p.DependencySources[dep] != wsproto.DependencySourceBuildGraph {
				t.Errorf("%s edge %s source = %q", dir, dep, p.DependencySources[dep])
			}
		}
		if !strings.Contains(string(p.Metadata), `"`+want.name+`"`) {
			t.Errorf("%s metadata names no package %s: %s", dir, want.name, p.Metadata)
		}
	}
	// A directory rush.json does not list, such as an autoinstaller, is no
	// project.
	if p, ok := got["common/autoinstallers/lint"]; ok {
		t.Errorf("an autoinstaller rush.json does not list is a project: %+v", p)
	}

	// In import mode the TypeScript extension runs the projects: each one is
	// a bare member, which names no extension.
	root = repository(t, d, ModeImport, false)
	stackstest.WriteFile(t, root, "common/autoinstallers/lint/package.json", `{"name": "lint"}`)
	var members []string
	for _, p := range probe(t, root).Projects {
		if len(p.Extensions) != 0 || p.SourceName != "" {
			t.Errorf("import mode answers %+v, want a bare member", p)
		}
		members = append(members, p.Path)
	}
	if want := []string{"apps/api", "apps/web", "libs/pricing"}; !slices.Equal(members, want) {
		t.Errorf("import mode answers the members %v, want the ones rush.json lists %v", members, want)
	}
	// A repository without rush.json answers nothing.
	if result := probe(t, t.TempDir()); len(result.Projects) != 0 {
		t.Errorf("no rush.json answers %+v", result.Projects)
	}
}

func TestTheSyncWritesEachProjectPutnamiJSONFromRushJSON(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-project-graph", "the-sync-writes-each-project-putnami-json-from-rush-json")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, true)
	stackstest.WriteFile(t, root, "apps/web/putnami.json", `{"name": "web", "dependencies": ["@acme/design"], "disable": {"jobs": ["serve"]}}`+"\n")

	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"apps/api/putnami.json", "apps/web/putnami.json", "libs/pricing/putnami.json"}; !reflect.DeepEqual(result.Written, want) {
		t.Errorf("written = %v, want %v", result.Written, want)
	}
	api := readProjectFile(t, root, "apps/api")
	if want := map[string]any{
		"$schema":      stacks.ProjectSchema,
		"extensions":   []any{ExtensionName},
		"dependencies": []any{"/libs/pricing"},
		"disable":      map[string]any{"jobs": []any{"lint"}},
		"options":      map[string]any{ExtensionName: map[string]any{"filePatterns": []any{"../../rush.json", "../../" + CommandLineFile, "../../common/config/rush/pnpm-lock.yaml"}}},
	}; !reflect.DeepEqual(api, want) {
		t.Errorf("apps/api = %v\nwant %v", api, want)
	}
	if web := readProjectFile(t, root, "apps/web"); !reflect.DeepEqual(web["dependencies"], []any{"@acme/design"}) || !reflect.DeepEqual(web["disable"], map[string]any{"jobs": []any{"serve", "lint"}}) || web["name"] != "web" {
		t.Errorf("apps/web keeps its own members: %v", web)
	}
	again, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || len(again.Written) != 0 || again.Unchanged != 3 {
		t.Errorf("a second sync rewrites %v, %v", again.Written, err)
	}

	// Without command-line.json neither test nor lint runs.
	root = repository(t, d, ModeDelegate, false)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "libs/pricing")["disable"]; !reflect.DeepEqual(got, map[string]any{"jobs": []any{"test", "lint"}}) {
		t.Errorf("libs/pricing disable = %v, want test and lint", got)
	}

	// A project removed from rush.json is retracted.
	root = repository(t, d, ModeDelegate, true)
	stackstest.WriteFile(t, root, "legacy/putnami.json", `{"extensions": ["@putnami/rush", "@acme/lint"], "dependencies": ["/libs/pricing"]}`+"\n")
	result, err = Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || !reflect.DeepEqual(result.Retracted, []string{"legacy/putnami.json"}) {
		t.Errorf("retracted = %v, %v", result.Retracted, err)
	}
	if legacy := readProjectFile(t, root, "legacy"); !reflect.DeepEqual(legacy, map[string]any{"extensions": []any{"@acme/lint"}}) {
		t.Errorf("legacy = %v", legacy)
	}

	// A rush.json that is no JSON fails the sync and writes nothing.
	root = repository(t, d, ModeDelegate, true)
	stackstest.WriteFile(t, root, "rush.json", "not json")
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "rush.json cannot be parsed") {
		t.Errorf("an unreadable rush.json fails the sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a failed sync writes nothing: %v", err)
	}
	// A root without rush.json gets an empty result.
	if result, err := Sync(context.Background(), t.TempDir(), &bytes.Buffer{}); err != nil || result.Export != "" {
		t.Errorf("no rush.json: %+v %v", result, err)
	}
}

func TestAnImportWritesTypeScriptProjectsAndRetractsNone(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-project-graph", "the-sync-writes-each-project-putnami-json-from-rush-json")
	d := readDemo(t)
	root := repository(t, d, ModeImport, true)
	stackstest.WriteFile(t, root, "tools/design/putnami.json", `{"extensions": ["@putnami/typescript"]}`+"\n")

	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Retracted) != 0 {
		t.Errorf("an import retracted %v", result.Retracted)
	}
	// The demo's projects declare no script: each task is disabled.
	if api := readProjectFile(t, root, "apps/api"); !reflect.DeepEqual(api, map[string]any{
		"$schema":      stacks.ProjectSchema,
		"extensions":   []any{TypeScriptExtension},
		"dependencies": []any{"/libs/pricing"},
		"disable":      map[string]any{"jobs": []any{"test", "lint", "build"}},
	}) {
		t.Errorf("apps/api = %v, want a TypeScript project with its edge", api)
	}
	if got := cacheWarnings(result.Warnings); len(got) != 0 {
		t.Errorf("an import warns about Rush's cache: %v", got)
	}
}

func TestTheSyncListsTheCommandsItDoesNotMap(t *testing.T) {
	spectest.Proves(t, feature, "unmapped-commands-are-listed", "the-sync-lists-the-commands-it-does-not-map")
	d := readDemo(t)
	root := repository(t, d, ModeImport, true)
	stackstest.WriteFile(t, root, "apps/web/package.json", `{"name": "@demo/web", "version": "1.0.0", "scripts": {"build": "tsc", "start": "node lib", "storybook": "sb"}}`)
	stackstest.WriteFile(t, root, "apps/api/package.json", `{"name": "@demo/api", "version": "1.0.0", "dependencies": {"@demo/pricing": "workspace:*"}, "scripts": {"start": "node lib", "test": "jest"}}`)

	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"not mapped: rush deploy, a phased command, has no Putnami job of that name and keeps running through Rush",
		"not mapped: rush prettier, a global command, runs once for the repository and keeps running through Rush",
		"not mapped: the start script of @demo/api, @demo/web has no Putnami job of that name; run it with rushx start",
		"not mapped: the storybook script of @demo/web has no Putnami job of that name; run it with rushx storybook",
	}
	var unmapped []string
	for _, w := range result.Warnings {
		if strings.HasPrefix(w, "not mapped:") {
			unmapped = append(unmapped, w)
		}
	}
	if !reflect.DeepEqual(unmapped, want) {
		t.Errorf("unmapped =\n%s\nwant\n%s", strings.Join(unmapped, "\n"), strings.Join(want, "\n"))
	}

	// The sync job reports them.
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	if status, _, err := RunSync(&pctx.Context{WorkspaceRoot: root}, emit, nil); status != "OK" || err != nil {
		t.Fatalf("sync job: %s %v", status, err)
	}
	if !strings.Contains(events.String(), "not mapped: rush deploy") {
		t.Errorf("the sync job reports no unmapped command:\n%s", events.String())
	}
}

func TestDelegateModeWarnsWithoutTheRushBuildCache(t *testing.T) {
	spectest.Proves(t, feature, "build-cache-warning", "delegate-mode-warns-without-the-rush-build-cache")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, true)
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if got := cacheWarnings(result.Warnings); err != nil || len(got) != 1 || !strings.Contains(got[0], "reruns the projects upstream") {
		t.Fatalf("warnings = %v, %v", result.Warnings, err)
	}
	stackstest.WriteFile(t, root, BuildCacheFile, "// cache\n{ \"buildCacheEnabled\": false }\n")
	if result, _ := Sync(context.Background(), root, &bytes.Buffer{}); len(cacheWarnings(result.Warnings)) != 1 {
		t.Errorf("a disabled cache warns: %v", result.Warnings)
	}
	stackstest.WriteFile(t, root, BuildCacheFile, "// cache\n{ \"buildCacheEnabled\": true, \"cacheProvider\": \"local-only\" }\n")
	if result, _ := Sync(context.Background(), root, &bytes.Buffer{}); len(cacheWarnings(result.Warnings)) != 0 {
		t.Errorf("an enabled cache warns: %v", result.Warnings)
	}
}

func TestAStaleRushGraphFailsValidate(t *testing.T) {
	spectest.Proves(t, feature, "validate-fails-on-a-stale-graph", "a-stale-rush-graph-fails-validate")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, true)
	ctx := &pctx.Context{WorkspaceRoot: root}
	if status, _, err := RunSync(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("sync: %s %v", status, err)
	}
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("check after the sync: %s %v", status, err)
	}
	// @demo/web now depends on @demo/pricing: the edge is stale.
	stackstest.WriteFile(t, root, "apps/web/package.json", `{"name": "@demo/web", "version": "1.0.0", "dependencies": {"@demo/pricing": "workspace:*"}}`)
	before, _ := os.ReadFile(filepath.Join(root, "apps/web/putnami.json"))
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "FAILED" || err == nil || err.Error() != CodeSyncStale {
		t.Errorf("check on a stale graph: %s %v", status, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "apps/web/putnami.json")); !bytes.Equal(before, after) {
		t.Errorf("the check wrote apps/web/putnami.json")
	}

	// In import mode too: a script that changes the tool it runs is stale.
	root = repository(t, d, ModeImport, true)
	ctx = &pctx.Context{WorkspaceRoot: root}
	if status, _, err := RunSync(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("sync in import mode: %s %v", status, err)
	}
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("check after the import: %s %v", status, err)
	}
	stackstest.WriteFile(t, root, "apps/web/package.json", `{"name": "@demo/web", "version": "1.0.0", "scripts": {"test": "mocha"}}`)
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "FAILED" || err == nil || err.Error() != CodeSyncStale {
		t.Errorf("check in import mode on a changed script: %s %v", status, err)
	}
}

// stubRush puts first on PATH a rush that prints its banner for --help and
// otherwise logs its working directory and arguments to $STUB_LOG, writes
// $STUB_JUNIT to the file $STUB_REPORT names, and exits $STUB_EXIT.
func stubRush(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	stackstest.WriteFile(t, bin, "rush", `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "Rush Multi-Project Build Tool ${STUB_VERSION:-`+rushVersion+`} - https://rushjs.io"
  exit 0
fi
echo "$(pwd -P) rush $*" >> "$STUB_LOG"
if [ -n "$STUB_REPORT" ]; then
  mkdir -p "$(dirname "$STUB_REPORT")"
  printf '%s' "$STUB_JUNIT" > "$STUB_REPORT"
fi
exit "${STUB_EXIT:-0}"
`)
	if err := os.Chmod(filepath.Join(bin, "rush"), 0o755); err != nil {
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

func TestATaskRunsRushOnItsProjectAndRecordsTheDelegation(t *testing.T) {
	spectest.Proves(t, feature, "tasks-delegate-to-rush", "a-task-runs-rush-on-its-project-and-records-the-delegation")
	d := readDemo(t)
	stubRush(t)
	root := repository(t, d, ModeDelegate, true)
	rootReal, _ := filepath.EvalSymlinks(root)

	for task, want := range map[string]string{
		stacks.TaskBuild: "rush build --to @demo/pricing",
		stacks.TaskTest:  "rush test --to @demo/pricing",
		stacks.TaskLint:  "rush lint --to @demo/pricing",
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		status, data, events := runJob(t, root, "libs/pricing", task)
		if status != "OK" || data["delegatedTo"] != "rush" {
			t.Errorf("%s: status %s, data %v, events %s", task, status, data, events)
		}
		if log := stubLog(t); len(log) != 1 || log[0] != rootReal+" "+want {
			t.Errorf("%s ran %q, want %q in %s", task, log, want, rootReal)
		}
	}

	t.Setenv("STUB_VERSION", "5.139.0")
	if status, _, events := runJob(t, root, "libs/pricing", stacks.TaskBuild); status != "FAILED" || !strings.Contains(events, stacks.CodeToolchainMismatch) {
		t.Errorf("a Rush version other than rush.json's fails the task: %s %s", status, events)
	}
	t.Setenv("STUB_VERSION", "")
	t.Setenv("STUB_EXIT", "3")
	if status, _, events := runJob(t, root, "libs/pricing", stacks.TaskBuild); status != "FAILED" || !strings.Contains(events, stacks.CodeStepFailed) {
		t.Errorf("a failing rush build fails the task: %s %s", status, events)
	}
}

func TestTheTestTaskReadsTheReportsItsProjectNames(t *testing.T) {
	spectest.Proves(t, feature, "reports-come-from-the-project-options", "the-test-task-reads-the-reports-its-project-names")
	d := readDemo(t)
	stubRush(t)
	root := repository(t, d, ModeDelegate, true)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	// Without reports the result is the exit code, and the sync names the
	// project.
	if status, data, events := runJob(t, root, "libs/pricing", stacks.TaskTest); status != "OK" || data["testSummary"] != nil {
		t.Errorf("a test without reports: %s %v %s", status, data, events)
	}
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(result.Warnings, func(n string) bool {
		return strings.HasPrefix(n, "no test report: the test task of @demo/api, @demo/pricing, @demo/web reads no JUnit")
	}) {
		t.Errorf("notes = %v, want the projects without reports", result.Warnings)
	}

	// A project that names its JUnit report reads it.
	report := filepath.Join(root, "libs/pricing/temp/junit.xml")
	t.Setenv("STUB_REPORT", report)
	t.Setenv("STUB_JUNIT", `<testsuites><testsuite name="pricing" tests="2"><testcase classname="pricing" name="rounds"/><testcase classname="pricing" name="keeps"/></testsuite></testsuites>`)
	project := readProjectFile(t, root, "libs/pricing")
	layer := project["options"].(map[string]any)[ExtensionName].(map[string]any)
	layer["tasks"] = map[string]any{"test": map[string]any{"reports": map[string]any{"junit": []string{"temp/junit.xml"}}}}
	data, _ := json.MarshalIndent(project, "", "  ")
	stackstest.WriteFile(t, root, "libs/pricing/putnami.json", string(data)+"\n")
	status, out, events := runJob(t, root, "libs/pricing", stacks.TaskTest)
	if status != "OK" {
		t.Fatalf("status %s: %s", status, events)
	}
	if got, _ := out["testSummary"].(map[string]any); got == nil || got["total"] != 2 || got["passed"] != 2 {
		t.Errorf("testSummary = %v, want the 2 cases of the named report", out["testSummary"])
	}
	result, _ = Sync(context.Background(), root, &bytes.Buffer{})
	for _, n := range result.Warnings {
		if strings.Contains(n, "@demo/pricing") {
			t.Errorf("a project that names its reports is still listed: %s", n)
		}
	}

	// A failed case fails the task.
	t.Setenv("STUB_JUNIT", `<testsuites><testsuite name="pricing" tests="1" failures="1"><testcase classname="pricing" name="rounds"><failure message="off by one"/></testcase></testsuite></testsuites>`)
	if status, _, events := runJob(t, root, "libs/pricing", stacks.TaskTest); status != "FAILED" || !strings.Contains(events, "1 of 1 tests failed") {
		t.Errorf("a failed case: %s %s", status, events)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	s := stack(t)
	if !s.Profile.Delegated {
		t.Fatal("the Rush profile is delegated")
	}
	stackstest.CheckManifest(t, "putnami.extension.json", s)
}

func TestTheModeParamOutranksTheRecordedMode(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-project-graph", "the-sync-writes-each-project-putnami-json-from-rush-json")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, false)
	ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{ModeParam: json.RawMessage(`"import"`), "dryRun": json.RawMessage("true")}}
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	status, data, err := RunSync(ctx, emit, nil)
	if status != "OK" || err != nil || data["dryRun"] != true {
		t.Fatalf("preview: %s %v %v", status, data, err)
	}
	if !strings.Contains(events.String(), "apps/api/tsconfig.json would change") || !strings.Contains(events.String(), TypeScriptExtension) {
		t.Errorf("an import preview of a delegated workspace shows no TypeScript project:\n%s", events.String())
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a preview wrote apps/api/putnami.json: %v", err)
	}
}

func TestModeReadsTheWorkspaceOptions(t *testing.T) {
	root := t.TempDir()
	if got, err := Mode(root); err != nil || got != ModeImport {
		t.Errorf("no workspace config = %s %v, want import", got, err)
	}
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "x", "options": {"@putnami/rush": {"mode": "delegate"}}}`)
	if got, err := Mode(root); err != nil || got != ModeDelegate {
		t.Errorf("mode = %s %v, want delegate", got, err)
	}
	// A mode that names none is an error, not an import.
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "x", "options": {"@putnami/rush": {"mode": "delgate"}}}`)
	if _, err := Mode(root); err == nil {
		t.Error("an unknown mode imports")
	}
	stackstest.WriteFile(t, root, "rush.json", `{"rushVersion": "5.140.0", "projects": [{"packageName": "@demo/api", "projectFolder": "apps/api"}]}`)
	stackstest.WriteFile(t, root, "apps/api/package.json", `{"name": "@demo/api", "version": "1.0.0"}`)
	_, diags := Detect(root, wsproto.ProbeRequest{Paths: []string{"apps/api"}})
	if len(diags) != 1 || diags[0].Code != stacks.CodeAdoptionModeInvalid {
		t.Errorf("the probe of an unknown mode: %v", diags)
	}
	if _, err := Sync(context.Background(), root, io.Discard); err == nil || !strings.Contains(err.Error(), "delgate") {
		t.Errorf("the sync of an unknown mode: %v", err)
	}
}

// cacheWarnings keeps the warnings about Rush's build cache.
func cacheWarnings(warnings []string) []string {
	var out []string
	for _, w := range warnings {
		if strings.Contains(w, "build cache") {
			out = append(out, w)
		}
	}
	return out
}

func TestAnImportAddsSourceExportsAndATSConfig(t *testing.T) {
	spectest.Proves(t, feature, "import-adopts-typescript-conventions", "an-import-adds-source-exports-and-a-tsconfig")
	d := readDemo(t)
	root := repository(t, d, ModeImport, false)
	// pricing builds with Bun, so its main names an output nothing writes in
	// the project; web keeps its own tsconfig and declares exports; api
	// names no main.
	pricingManifest := strings.Replace(d["base"]["libs/pricing/package.json"], `"main": "lib/index.js",`, `"main": "lib/index.js",
  "scripts": {
    "build": "bun build src/index.ts --outdir lib"
  },`, 1)
	stackstest.WriteFile(t, root, "libs/pricing/package.json", pricingManifest)
	stackstest.WriteFile(t, root, "apps/web/tsconfig.json", `{"compilerOptions": {"strict": true}}`)
	stackstest.WriteFile(t, root, "apps/web/package.json", `{"name": "@demo/web", "version": "1.0.0", "main": "lib/index.js", "exports": "./lib/index.js"}`)

	dry, err := Stale(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	var diffs []string
	for _, diff := range dry.Changes {
		diffs = append(diffs, diff.File)
	}
	slices.Sort(diffs)
	want := []string{"apps/api/putnami.json", "apps/api/tsconfig.json", "apps/web/putnami.json", "libs/pricing/package.json", "libs/pricing/putnami.json", "libs/pricing/tsconfig.json"}
	if !reflect.DeepEqual(diffs, want) {
		t.Errorf("dry diffs = %v, want %v", diffs, want)
	}
	var pricingDiff string
	for _, c := range dry.Changes {
		if c.File == "libs/pricing/package.json" {
			pricingDiff = stacks.Diff(c.File, c.Before, c.After)
		}
	}
	if !strings.Contains(pricingDiff, "\n+  \"exports\": {") || !strings.Contains(pricingDiff, "\n+    \".\": \"./src/index.ts\"") || strings.Contains(pricingDiff, "\n- ") || strings.Contains(pricingDiff, "\n-  ") {
		t.Errorf("the pricing diff adds exports alone:\n%s", pricingDiff)
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/tsconfig.json")); !os.IsNotExist(err) {
		t.Errorf("a dry sync wrote a tsconfig: %v", err)
	}

	wet, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"libs/pricing/package.json: exports", "apps/api/tsconfig.json: written"} {
		if !slices.ContainsFunc(wet.Warnings, func(w string) bool { return strings.HasPrefix(w, file) }) {
			t.Errorf("the sync lists no %q: %v", file, wet.Warnings)
		}
	}
	pricing, err := os.ReadFile(filepath.Join(root, "libs/pricing/package.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantPricing := strings.Replace(pricingManifest, `"main": "lib/index.js",`, `"main": "lib/index.js",
  "exports": {
    ".": "./src/index.ts"
  },`, 1)
	if string(pricing) != wantPricing {
		t.Errorf("pricing package.json =\n%s\nwant\n%s", pricing, wantPricing)
	}
	if config, _ := os.ReadFile(filepath.Join(root, "apps/api/tsconfig.json")); string(config) != "{\n  \"extends\": \"../../tsconfig.json\"\n}\n" {
		t.Errorf("apps/api tsconfig = %s", config)
	}
	if config, _ := os.ReadFile(filepath.Join(root, "apps/web/tsconfig.json")); string(config) != `{"compilerOptions": {"strict": true}}` {
		t.Errorf("apps/web keeps its tsconfig: %s", config)
	}
	again, err := Stale(context.Background(), root, &bytes.Buffer{})
	if err != nil || len(again.Written) != 0 || len(again.Changes) != 0 {
		t.Errorf("a second import writes %v %v, %v", again.Written, again.Changes, err)
	}

	// A tsc build writes the output its main names: the entry stays. So
	// does a project's own tsconfig*.json, which its tools read.
	root = repository(t, d, ModeImport, false)
	tscManifest := strings.Replace(pricingManifest, "bun build src/index.ts --outdir lib", "tsc -p tsconfig.lib.json", 1)
	stackstest.WriteFile(t, root, "libs/pricing/package.json", tscManifest)
	stackstest.WriteFile(t, root, "libs/pricing/tsconfig.lib.json", `{"compilerOptions": {"outDir": "lib"}}`)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "libs/pricing/package.json")); string(data) != tscManifest {
		t.Errorf("a tsc build's package.json changed:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(root, "libs/pricing/tsconfig.json")); !os.IsNotExist(err) {
		t.Errorf("a project with a tsconfig.lib.json got a tsconfig.json: %v", err)
	}

	// Delegate mode rewrites no package.json and writes no tsconfig.
	root = repository(t, d, ModeDelegate, false)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "libs/pricing/package.json")); string(data) != d["base"]["libs/pricing/package.json"] {
		t.Errorf("delegate mode rewrote pricing's package.json")
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/tsconfig.json")); !os.IsNotExist(err) {
		t.Errorf("delegate mode wrote a tsconfig: %v", err)
	}
}

func TestAnImportKeepsTheEntryOfAProjectRushPublishes(t *testing.T) {
	spectest.Proves(t, feature, "a-moved-task-does-what-its-script-did", "an-imported-task-does-what-its-script-did")
	d := readDemo(t)
	root := repository(t, d, ModeImport, false)
	stackstest.WriteFile(t, root, "rush.json", strings.Replace(d["base"]["rush.json"], `"shouldPublish": false`, `"shouldPublish": true`, 1))
	manifest := strings.Replace(d["base"]["libs/pricing/package.json"], `"main": "lib/index.js",`, `"main": "lib/index.js",
  "scripts": {
    "build": "bun build src/index.ts --outdir lib"
  },`, 1)
	stackstest.WriteFile(t, root, "libs/pricing/package.json", manifest)
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "libs/pricing/package.json")); string(data) != manifest {
		t.Errorf("a published project's package.json changed:\n%s", data)
	}
	// Putnami's Bun build would not write the lib/index.js its consumers
	// read: the build stays on Rush.
	want := `not mapped: the build script of @demo/pricing: its package.json names "lib/index.js", which the Bun build of @putnami/typescript does not write, and rush.json publishes it, so that entry stays: it keeps running through Rush, with rushx build`
	if !slices.Contains(result.Warnings, want) {
		t.Errorf("warnings = %v, want the build kept on Rush", result.Warnings)
	}
	if jobs := readProjectFile(t, root, "libs/pricing")["disable"].(map[string]any)["jobs"]; !slices.Contains(jobs.([]any), any("@putnami/typescript:build")) {
		t.Errorf("libs/pricing jobs = %v, want the build on Rush", jobs)
	}
}

func TestASwitchOfModeDropsTheOtherModesExtension(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-project-graph", "the-sync-writes-each-project-putnami-json-from-rush-json")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, false)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "options": {"@putnami/rush": {"mode": "import"}}}`)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	api := readProjectFile(t, root, "apps/api")
	if !reflect.DeepEqual(api["extensions"], []any{TypeScriptExtension}) || api["options"] != nil {
		t.Errorf("after an import apps/api = %v, want the TypeScript extension alone", api)
	}
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "options": {"@putnami/rush": {"mode": "delegate"}}}`)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if api := readProjectFile(t, root, "apps/api"); !reflect.DeepEqual(api["extensions"], []any{ExtensionName}) {
		t.Errorf("after a delegation apps/api extensions = %v, want the Rush extension alone", api["extensions"])
	}
}

// stubRushx puts first on PATH a rushx that logs its working directory and
// arguments to $STUB_LOG.
func stubRushx(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	stackstest.WriteFile(t, bin, "rushx", "#!/bin/sh\necho \"$(pwd -P) rushx $*\" >> \"$STUB_LOG\"\n")
	if err := os.Chmod(filepath.Join(bin, "rushx"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestAnImportKeepsATargetThatRunsAnotherToolOnRush(t *testing.T) {
	spectest.Proves(t, feature, "import-keeps-the-tool-that-checks-the-code", "a-target-that-runs-another-tool-keeps-running-with-rushx")
	d := readDemo(t)
	stubRush(t)
	stubRushx(t)
	root := repository(t, d, ModeImport, false)
	// api bundles with webpack and tests with Mocha, which @putnami/typescript
	// does not run, and lints with a bare ESLint, which it does; pricing
	// builds with tsc and tests with Jest, and its ESLint takes arguments.
	stackstest.WriteFile(t, root, "apps/api/package.json", `{"name": "@demo/api", "version": "1.0.0", "private": true, "dependencies": {"@demo/pricing": "workspace:*"}, "scripts": {"build": "webpack", "test": "mocha", "lint": "eslint ."}}`)
	stackstest.WriteFile(t, root, "libs/pricing/package.json", `{"name": "@demo/pricing", "version": "1.2.0", "private": true, "scripts": {"build": "tsc", "test": "jest", "lint": "eslint --max-warnings 0 src"}}`)

	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	keptSteps := func(task string) any {
		return map[string]any{"steps": []any{[]any{"rushx", task}}, "dir": "project"}
	}
	if api := readProjectFile(t, root, "apps/api"); !reflect.DeepEqual(api, map[string]any{
		"$schema":      stacks.ProjectSchema,
		"extensions":   []any{ExtensionName, TypeScriptExtension},
		"dependencies": []any{"/libs/pricing"},
		"disable":      map[string]any{"jobs": []any{"@putnami/rush:lint", "@putnami/typescript:test", "@putnami/typescript:build"}},
		// Its eslint . writes no fix: the moved lint checks only.
		"options": map[string]any{
			ExtensionName:              map[string]any{"tasks": map[string]any{"build": keptSteps("build"), "test": keptSteps("test")}},
			stacks.TypeScriptLintLayer: map[string]any{"fix": false},
		},
	}) {
		t.Errorf("apps/api = %v", api)
	}
	pricing := readProjectFile(t, root, "libs/pricing")
	if !reflect.DeepEqual(pricing["disable"], map[string]any{"jobs": []any{"@putnami/rush:test", "@putnami/rush:build", "@putnami/typescript:lint"}}) {
		t.Errorf("libs/pricing disable = %v", pricing["disable"])
	}
	// Its jest test moved, and Jest type-checked nothing.
	if options := pricing["options"].(map[string]any)[stacks.TypeScriptRunnerLayer]; !reflect.DeepEqual(options, map[string]any{"types": false}) {
		t.Errorf("libs/pricing options = %v, want a test with no type check", options)
	}
	if !slices.Contains(result.Warnings, stacks.LintDependentsNote) {
		t.Errorf("warnings = %v, want the lint selection note", result.Warnings)
	}
	// web declares no script: the TypeScript extension alone, every task
	// disabled.
	if web := readProjectFile(t, root, "apps/web"); !reflect.DeepEqual(web["extensions"], []any{TypeScriptExtension}) {
		t.Errorf("apps/web extensions = %v", web["extensions"])
	}
	for _, want := range []string{
		`not mapped: the build script of @demo/api: @putnami/typescript runs bun build or tsc alone, not "webpack": it keeps running through Rush, with rushx build`,
		`not mapped: the test script of @demo/api: @putnami/typescript runs bun test or vitest or jest or npx vitest or npx jest, not "mocha": it keeps running through Rush, with rushx test`,
		`not mapped: the lint script of @demo/pricing: its ESLint takes arguments ("--max-warnings 0 src") that @putnami/typescript would drop: move them to the ESLint configuration; it keeps running through Rush, with rushx lint`,
	} {
		if !slices.Contains(result.Warnings, want) {
			t.Errorf("warnings = %v, want %q", result.Warnings, want)
		}
	}
	// The scripts keep running as the repository defines them.
	if data, _ := os.ReadFile(filepath.Join(root, "apps/api/package.json")); !strings.Contains(string(data), `"test": "mocha"`) {
		t.Errorf("the import rewrote api's scripts: %s", data)
	}

	// The probe claims api, which keeps targets on Rush, and not web, which
	// stays a bare member.
	var claimed []string
	for _, p := range probe(t, root).Projects {
		if slices.Contains(p.Extensions, ExtensionName) {
			claimed = append(claimed, p.Path)
		}
	}
	if !slices.Contains(claimed, "apps/api") || slices.Contains(claimed, "apps/web") {
		t.Errorf("an import claims %v, want apps/api and not apps/web", claimed)
	}
	apiReal, _ := filepath.EvalSymlinks(filepath.Join(root, "apps/api"))
	if status, data, events := runJob(t, root, "apps/api", stacks.TaskTest); status != "OK" || data["delegatedTo"] != "rush" {
		t.Errorf("the kept test: %s %v %s", status, data, events)
	}
	if log := stubLog(t); len(log) != 1 || log[0] != apiReal+" rushx test" {
		t.Errorf("the kept test ran %q, want rushx test in %s", log, apiReal)
	}

	// Back to delegate mode, the rushx steps leave.
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "options": {"@putnami/rush": {"mode": "delegate"}}}`)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	api := readProjectFile(t, root, "apps/api")
	layer, _ := api["options"].(map[string]any)[ExtensionName].(map[string]any)
	if !reflect.DeepEqual(api["extensions"], []any{ExtensionName}) || layer["tasks"] != nil {
		t.Errorf("after a delegation apps/api = %v", api)
	}
}
