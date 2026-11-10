package rush

import (
	"bytes"
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

// repository writes the demo's base files and a workspace config in the
// mode, with command-line.json when commands.
func repository(t *testing.T, d demo, mode string, commands bool) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		writeFile(t, root, rel, content)
	}
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "shop", "extensions": ["@putnami/rush"], "options": {"@putnami/rush": {"mode": "`+mode+`"}}}`)
	if commands {
		writeFile(t, root, CommandLineFile, commandLine)
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
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
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
	writeFile(t, root, "common/autoinstallers/lint/package.json", `{"name": "lint"}`)

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
		if !strings.Contains(string(p.Metadata), `"`+want.name+`"`) {
			t.Errorf("%s metadata names no package %s: %s", dir, want.name, p.Metadata)
		}
	}
	if p, ok := got["common/autoinstallers/lint"]; !ok || len(p.Extensions) != 0 {
		t.Errorf("an autoinstaller is bare: %+v", p)
	}

	// In import mode the TypeScript extension runs the projects.
	root = repository(t, d, ModeImport, false)
	if result := probe(t, root); len(result.Projects) != 0 {
		t.Errorf("import mode answers %+v, want no project", result.Projects)
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
	writeFile(t, root, "apps/web/putnami.json", `{"name": "web", "dependencies": ["@acme/design"], "disable": {"jobs": ["serve"]}}`+"\n")

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
		"options":      map[string]any{ExtensionName: map[string]any{"filePatterns": []any{"../../rush.json", "../../" + CommandLineFile}}},
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
	writeFile(t, root, "legacy/putnami.json", `{"extensions": ["@putnami/rush", "@acme/lint"], "dependencies": ["/libs/pricing"]}`+"\n")
	result, err = Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || !reflect.DeepEqual(result.Retracted, []string{"legacy/putnami.json"}) {
		t.Errorf("retracted = %v, %v", result.Retracted, err)
	}
	if legacy := readProjectFile(t, root, "legacy"); !reflect.DeepEqual(legacy, map[string]any{"extensions": []any{"@acme/lint"}}) {
		t.Errorf("legacy = %v", legacy)
	}

	// A rush.json that is no JSON fails the sync and writes nothing.
	root = repository(t, d, ModeDelegate, true)
	writeFile(t, root, "rush.json", "not json")
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
	writeFile(t, root, "tools/design/putnami.json", `{"extensions": ["@putnami/typescript"]}`+"\n")

	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Retracted) != 0 {
		t.Errorf("an import retracted %v", result.Retracted)
	}
	if api := readProjectFile(t, root, "apps/api"); !reflect.DeepEqual(api, map[string]any{
		"$schema":      stacks.ProjectSchema,
		"extensions":   []any{TypeScriptExtension},
		"dependencies": []any{"/libs/pricing"},
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
	writeFile(t, root, "apps/web/package.json", `{"name": "@demo/web", "version": "1.0.0", "scripts": {"build": "tsc", "start": "node lib", "storybook": "sb"}}`)
	writeFile(t, root, "apps/api/package.json", `{"name": "@demo/api", "version": "1.0.0", "dependencies": {"@demo/pricing": "workspace:*"}, "scripts": {"start": "node lib", "test": "jest"}}`)

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
	if !reflect.DeepEqual(result.Warnings, want) {
		t.Errorf("notes =\n%s\nwant\n%s", strings.Join(result.Warnings, "\n"), strings.Join(want, "\n"))
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
	writeFile(t, root, BuildCacheFile, "// cache\n{ \"buildCacheEnabled\": false }\n")
	if result, _ := Sync(context.Background(), root, &bytes.Buffer{}); len(cacheWarnings(result.Warnings)) != 1 {
		t.Errorf("a disabled cache warns: %v", result.Warnings)
	}
	writeFile(t, root, BuildCacheFile, "// cache\n{ \"buildCacheEnabled\": true, \"cacheProvider\": \"local-only\" }\n")
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
	writeFile(t, root, "apps/web/package.json", `{"name": "@demo/web", "version": "1.0.0", "dependencies": {"@demo/pricing": "workspace:*"}}`)
	before, _ := os.ReadFile(filepath.Join(root, "apps/web/putnami.json"))
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "FAILED" || err == nil || err.Error() != CodeSyncStale {
		t.Errorf("check on a stale graph: %s %v", status, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "apps/web/putnami.json")); !bytes.Equal(before, after) {
		t.Errorf("the check wrote apps/web/putnami.json")
	}

	// In import mode Putnami owns the graph: the check passes.
	root = repository(t, d, ModeImport, true)
	if status, _, err := RunCheck(&pctx.Context{WorkspaceRoot: root}, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("check in import mode: %s %v", status, err)
	}
}

// stubRush puts first on PATH a rush that prints its banner for --help and
// otherwise logs its working directory and arguments to $STUB_LOG, writes
// $STUB_JUNIT to the file $STUB_REPORT names, and exits $STUB_EXIT.
func stubRush(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "rush", `#!/bin/sh
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
	writeFile(t, root, "libs/pricing/putnami.json", string(data)+"\n")
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
	ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{ModeParam: json.RawMessage(`"import"`), stacks.SyncDryRunParam: json.RawMessage("true")}}
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
	if got := Mode(root); got != ModeDelegate {
		t.Errorf("no workspace config = %s, want delegate", got)
	}
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "x", "options": {"@putnami/rush": {"mode": "import"}}}`)
	if got := Mode(root); got != ModeImport {
		t.Errorf("mode = %s, want import", got)
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
	// web keeps its own tsconfig and declares exports; api names no main.
	writeFile(t, root, "apps/web/tsconfig.json", `{"compilerOptions": {"strict": true}}`)
	writeFile(t, root, "apps/web/package.json", `{"name": "@demo/web", "version": "1.0.0", "main": "lib/index.js", "exports": "./lib/index.js"}`)

	dry, err := Stale(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apps/api/putnami.json", "apps/api/tsconfig.json", "apps/web/putnami.json", "libs/pricing/package.json", "libs/pricing/putnami.json", "libs/pricing/tsconfig.json"}
	if !reflect.DeepEqual(dry.Written, want) {
		t.Errorf("dry written = %v, want %v", dry.Written, want)
	}
	var pricingDiff string
	for _, diff := range dry.Diffs {
		if diff.File == "libs/pricing/package.json" {
			pricingDiff = diff.Diff
		}
	}
	if !strings.Contains(pricingDiff, `+   "exports": {`) || !strings.Contains(pricingDiff, `+     ".": "./src/index.ts"`) || strings.Contains(pricingDiff, "- ") {
		t.Errorf("the pricing diff adds exports alone:\n%s", pricingDiff)
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/tsconfig.json")); !os.IsNotExist(err) {
		t.Errorf("a dry sync wrote a tsconfig: %v", err)
	}

	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	pricing, err := os.ReadFile(filepath.Join(root, "libs/pricing/package.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantPricing := strings.Replace(d["base"]["libs/pricing/package.json"], `"main": "lib/index.js",`, `"main": "lib/index.js",
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
	if err != nil || len(again.Written) != 0 {
		t.Errorf("a second import writes %v, %v", again.Written, err)
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

func TestSourceExportsKeepAManifestTheyCannotEdit(t *testing.T) {
	spectest.Proves(t, feature, "import-adopts-typescript-conventions", "an-import-adds-source-exports-and-a-tsconfig")
	dir := t.TempDir()
	writeFile(t, dir, "src/index.ts", "export {}\n")
	for name, manifest := range map[string]string{
		"a main under src":    `{"name": "a", "main": "src/index.ts"}`,
		"no main":             `{"name": "a"}`,
		"exports":             `{"name": "a", "main": "lib/index.js", "exports": "./lib/index.js"}`,
		"a one-line manifest": `{"name": "a", "main": "lib/index.js"}`,
	} {
		if got, err := withSourceExports(dir, []byte(manifest)); err != nil || got != nil {
			t.Errorf("%s: %s, %v", name, got, err)
		}
	}
	got, err := withSourceExports(dir, []byte("{\n\t\"name\": \"a\",\n\t\"types\": \"dist/index.d.ts\"\n}\n"))
	if err != nil || string(got) != "{\n\t\"name\": \"a\",\n\t\"types\": \"dist/index.d.ts\",\n\t\"exports\": {\n\t\t\".\": \"./src/index.ts\"\n\t}\n}\n" {
		t.Errorf("a last types member: %q, %v", got, err)
	}
	if _, err := withSourceExports(dir, []byte("not json")); err == nil {
		t.Error("a manifest that is no JSON passes")
	}
}
