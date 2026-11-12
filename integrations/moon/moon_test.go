package moon

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
	detect "go.putnami.dev/tooling/observe/detect/moon"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "monorepo/moon-adoption"

// demoFile is the moon demo of putnami observe: api, in TypeScript, depends
// on pricing, in Rust, and web, in JavaScript, stands alone.
const demoFile = "../../tooling/observe/internal/observe/testdata/demos/moon.txtar"

// version is the moon version the tests state in .prototools.
const version = "1.30.4"

// demo holds the demo's sections by prefix: base/ is the repository, head/
// the change on top of it, ci/ the export.
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

// repository writes the demo's base files, .prototools and a workspace
// config in the mode, and the export when withExport.
func repository(t *testing.T, d demo, mode string, withExport bool) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		stackstest.WriteFile(t, root, rel, content)
	}
	stackstest.WriteFile(t, root, ".prototools", "moon = \""+version+"\"\n")
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "demo", "extensions": ["@putnami/moon"], "options": {"@putnami/moon": {"mode": "`+mode+`"}}}`+"\n")
	if withExport {
		stackstest.WriteFile(t, root, detect.DefaultGraphFile, d["ci"][detect.DefaultGraphFile])
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

func probe(t *testing.T, root string) map[string]wsproto.ProbeProject {
	t.Helper()
	s := stack(t)
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, DependencySources: true,
		Paths: []string{".", "apps/api", "apps/web", "packages/pricing", "docs"}})
	if err != nil {
		t.Fatal(err)
	}
	// Each edge is moon's own: its project graph states it.
	for _, p := range result.Projects {
		for _, dep := range p.Dependencies {
			if p.DependencySources[dep] != wsproto.DependencySourceBuildGraph {
				t.Errorf("%s edge %s source = %q", p.Path, dep, p.DependencySources[dep])
			}
		}
	}
	if len(result.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v", result.Diagnostics)
	}
	got := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		got[p.Path] = p
	}
	return got
}

func TestTheMoonProjectsAndTheirInheritedTasksAreRead(t *testing.T) {
	spectest.Proves(t, feature, "projects-and-tasks-are-read", "the-moon-projects-and-their-inherited-tasks-are-read")
	d := readDemo(t)
	for _, withExport := range []bool{false, true} {
		root := repository(t, d, ModeDelegate, withExport)
		detection := detect.Detect(root, detect.Options{})
		if detection.FromExport != withExport {
			t.Fatalf("FromExport = %v", detection.FromExport)
		}
		tasks := map[string][]string{}
		for _, p := range detection.Projects {
			for _, task := range p.Tasks {
				tasks[p.ID] = append(tasks[p.ID], task.Name)
			}
		}
		want := map[string][]string{"api": {"build", "dev", "lint", "test"}, "pricing": {"build", "unit"}, "web": {"lint"}}
		if !reflect.DeepEqual(tasks, want) {
			t.Errorf("export %v: tasks %v, want %v", withExport, tasks, want)
		}

		got := probe(t, root)
		if len(got) != 3 {
			t.Fatalf("projects %+v", got)
		}
		for dir, c := range map[string]struct {
			id   string
			deps []string
		}{"apps/api": {"api", []string{"packages/pricing"}}, "apps/web": {"web", nil}, "packages/pricing": {"pricing", nil}} {
			p := got[dir]
			if p.SourceName != c.id || !reflect.DeepEqual(p.Dependencies, c.deps) || !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"moon"}) {
				t.Errorf("%s = %+v", dir, p)
			}
			var meta stacks.Metadata
			if err := json.Unmarshal(p.Metadata, &meta); err != nil || meta.Component != c.id || meta.Root != "." || meta.Toolchain.Version != version {
				t.Errorf("%s metadata = %s (%v)", dir, p.Metadata, err)
			}
		}
	}
	// A directory that is no moon workspace answers nothing.
	if got := probe(t, t.TempDir()); len(got) != 0 {
		t.Errorf("no workspace: %+v", got)
	}
}

func TestTheSyncWritesEachMoonProjectPutnamiJSON(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-each-project", "the-sync-writes-each-moon-project-putnami-json")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, false)
	// A former project keeps its own members; api keeps its reports.
	stackstest.WriteFile(t, root, "tools/legacy/putnami.json", `{"name": "legacy", "tags": ["moon", "team"], "extensions": ["@putnami/moon", "@acme/lint"], "dependencies": ["/packages/pricing", "@acme/design"], "options": {"@putnami/moon": {"tasks": {"test": {"steps": [["moon", "run", "{component}:check"]]}}}}}`+"\n")
	stackstest.WriteFile(t, root, "apps/api/putnami.json", `{"name": "api", "options": {"@putnami/moon": {"tasks": {"test": {"reports": {"junit": ["reports/junit.xml"]}}}}}}`+"\n")

	result, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"apps/api/putnami.json", "apps/web/putnami.json", "packages/pricing/putnami.json"}; !reflect.DeepEqual(result.Written, want) || !reflect.DeepEqual(result.Retracted, []string{"tools/legacy/putnami.json"}) {
		t.Errorf("written %v, retracted %v", result.Written, result.Retracted)
	}
	if want := []Unmapped{{Project: "api", Task: "dev"}}; len(result.Unmapped) != 1 || result.Unmapped[0].Project != want[0].Project || result.Unmapped[0].Task != want[0].Task || !strings.Contains(result.Unmapped[0].Reason, "moon run api:dev") {
		t.Errorf("unmapped %+v", result.Unmapped)
	}
	if !reflect.DeepEqual(result.Unreported, []string{"pricing"}) || result.Mode != ModeDelegate || len(result.Extensions) != 0 {
		t.Errorf("unreported %v, mode %s, extensions %v", result.Unreported, result.Mode, result.Extensions)
	}

	// A change to the workspace's moon configuration selects every project moon
	// runs.
	inputs := []any{"../../.moon/workspace.yml", "../../.prototools", "../../.moon/tasks/**"}
	marked := map[string]any{ExtensionName: map[string]any{"graphSync": true, "filePatterns": inputs}}
	want := map[string]map[string]any{
		"apps/api": {"$schema": nil, "name": "api", "tags": []any{"moon"}, "extensions": []any{ExtensionName}, "dependencies": []any{"/packages/pricing"},
			"options": map[string]any{ExtensionName: map[string]any{"graphSync": true, "filePatterns": inputs, "tasks": map[string]any{"test": map[string]any{"reports": map[string]any{"junit": []any{"reports/junit.xml"}}}}}}},
		"apps/web": {"$schema": stacks.ProjectSchema, "tags": []any{"moon"}, "extensions": []any{ExtensionName}, "disable": map[string]any{"jobs": []any{"build", "test"}}, "options": marked},
		"packages/pricing": {"$schema": stacks.ProjectSchema, "tags": []any{"moon"}, "extensions": []any{ExtensionName}, "disable": map[string]any{"jobs": []any{"lint"}},
			"options": map[string]any{ExtensionName: map[string]any{"graphSync": true, "filePatterns": inputs, "tasks": map[string]any{"test": map[string]any{"steps": []any{[]any{"moon", "run", "{component}:unit"}}}}}}},
		"tools/legacy": {"name": "legacy", "tags": []any{"team"}, "extensions": []any{"@acme/lint"}, "dependencies": []any{"@acme/design"}},
	}
	for dir, doc := range want {
		got := readProjectFile(t, root, dir)
		if doc["$schema"] == nil {
			delete(doc, "$schema")
		}
		if !reflect.DeepEqual(got, doc) {
			t.Errorf("%s = %v\nwant %v", dir, got, doc)
		}
	}

	// A second sync changes nothing.
	again, err := Sync(context.Background(), root)
	if err != nil || len(again.Written) != 0 || len(again.Retracted) != 0 || again.Unchanged != 3 {
		t.Errorf("a second sync: %+v %v", again, err)
	}

	// The sync job reports the unmapped task and the project whose test reads
	// no report.
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	status, data, err := RunSync(&pctx.Context{WorkspaceRoot: root}, emit, nil)
	if status != "OK" || err != nil || !reflect.DeepEqual(data["unmapped"], []string{"api:dev"}) ||
		!strings.Contains(events.String(), "moon task api:dev") || !strings.Contains(events.String(), "the test task of pricing names no report") {
		t.Errorf("the sync job: %s %v %v\n%s", status, data, err, events.String())
	}

	// Outside a moon workspace the sync writes nothing.
	if result, err := Sync(context.Background(), t.TempDir()); err != nil || result.Export != "" {
		t.Errorf("no workspace: %+v %v", result, err)
	}
}

func TestImportModeMapsAProjectToItsLanguageExtension(t *testing.T) {
	spectest.Proves(t, feature, "import-maps-to-language-extensions", "import-mode-maps-a-project-to-its-language-extension")
	d := readDemo(t)
	root := repository(t, d, ModeImport, true)
	// Before the sync, the projects the import moves to a language extension
	// are bare members: moon lists them, and their extension runs them. docs,
	// which moon does not list, is no project.
	before := probe(t, root)
	if len(before) != 3 || before["apps/web"].SourceName != "web" {
		t.Errorf("before the sync, probe %+v", before)
	}
	for _, dir := range []string{"apps/api", "packages/pricing"} {
		if p, ok := before[dir]; !ok || p.SourceName != "" || len(p.Extensions) != 0 {
			t.Errorf("before the sync %s = %+v (listed %v), want a bare member", dir, p, ok)
		}
	}
	result, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Extensions, []string{"@putnami/rust", "@putnami/typescript"}) || !slices.Contains(result.Written, wsproto.WorkspaceConfigFilename) {
		t.Errorf("extensions %v, written %v", result.Extensions, result.Written)
	}
	inputs := []any{"../../.moon/workspace.yml", "../../.prototools", "../../.moon/tasks/**"}
	for dir, ext := range map[string]string{"apps/web": ExtensionName} {
		doc := readProjectFile(t, root, dir)
		if !reflect.DeepEqual(doc["extensions"], []any{ext}) || !reflect.DeepEqual(doc["tags"], []any{"moon"}) {
			t.Errorf("%s = %v, want %s", dir, doc, ext)
		}
		want := map[string]any{"graphSync": true}
		if ext == ExtensionName {
			want["filePatterns"] = inputs
		}
		if !reflect.DeepEqual(doc["options"], map[string]any{ExtensionName: want}) {
			t.Errorf("%s holds a moon step: %v", dir, doc["options"])
		}
	}
	// api's Vitest test and bare ESLint lint move to @putnami/typescript,
	// which records the runner and the linter, with no type check the test
	// did not run and no fix the lint did not write; its tsc -b is no package.json
	// build script, so its build keeps running through moon (ADR 0017 of
	// tooling/stacks).
	api := readProjectFile(t, root, "apps/api")
	if !reflect.DeepEqual(api["extensions"], []any{ExtensionName, "@putnami/typescript"}) ||
		!reflect.DeepEqual(api["disable"], map[string]any{"jobs": []any{"@putnami/moon:lint", "@putnami/moon:test", "@putnami/typescript:build"}}) ||
		!reflect.DeepEqual(api["options"], map[string]any{
			ExtensionName:              map[string]any{"graphSync": true, "filePatterns": inputs},
			"@putnami/typescript":      map[string]any{"lint": map[string]any{"linter": "eslint"}},
			"@putnami/typescript:test": map[string]any{"runner": "vitest", "types": false},
		}) {
		t.Errorf("apps/api = %v", api)
	}
	if !slices.Contains(result.Warnings, stacks.LintDependentsNote) {
		t.Errorf("warnings = %v, want the lint selection note", result.Warnings)
	}
	if !slices.ContainsFunc(result.Unmapped, func(u Unmapped) bool {
		return u.Project == "api" && u.Task == "build" && strings.Contains(u.Reason, "compiles the tsc of the package.json build script") && strings.HasSuffix(u.Reason, "moon run api:build")
	}) {
		t.Errorf("unmapped = %v, want api:build with its reason", result.Unmapped)
	}
	// pricing's cargo build moves to @putnami/rust; its cargo test stays on
	// moon, since the Rust extension tests with nextest, which runs no
	// doctest.
	pricing := readProjectFile(t, root, "packages/pricing")
	if !reflect.DeepEqual(pricing["extensions"], []any{ExtensionName, "@putnami/rust"}) ||
		!reflect.DeepEqual(pricing["disable"], map[string]any{"jobs": []any{"lint", "@putnami/moon:build", "@putnami/rust:test"}}) ||
		!reflect.DeepEqual(pricing["options"], map[string]any{ExtensionName: map[string]any{"graphSync": true, "filePatterns": inputs,
			"tasks": map[string]any{"test": map[string]any{"steps": []any{[]any{"moon", "run", "{component}:unit"}}}}}}) {
		t.Errorf("packages/pricing = %v", pricing)
	}
	// D-066: api has no tsconfig*.json, so the import writes one.
	if config, _ := os.ReadFile(filepath.Join(root, "apps/api/tsconfig.json")); string(config) != "{\n  \"extends\": \"../../tsconfig.json\"\n}\n" {
		t.Errorf("apps/api tsconfig = %q", config)
	}
	if result.Unreported != nil {
		t.Errorf("an imported project reads its extension's reports: %v", result.Unreported)
	}
	var cfg struct {
		Extensions []string `json:"extensions"`
	}
	data, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	if err := json.Unmarshal(data, &cfg); err != nil || !reflect.DeepEqual(cfg.Extensions, []string{ExtensionName, "@putnami/rust", "@putnami/typescript"}) {
		t.Errorf("workspace extensions %v (%v)", cfg.Extensions, err)
	}
	// The probe answers the project that stays delegated, and those that
	// keep a task on moon.
	got := probe(t, root)
	if len(got) != 3 || got["apps/web"].SourceName != "web" || got["apps/api"].SourceName != "api" || got["packages/pricing"].SourceName != "pricing" {
		t.Errorf("probe %+v", got)
	}
	// Back to delegate mode, every project runs moon again; the language
	// extensions stay declared in the workspace.
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"extensions": ["@putnami/moon", "@putnami/rust", "@putnami/typescript"], "options": {"@putnami/moon": {"mode": "delegate"}}}`)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"apps/api", "packages/pricing"} {
		if doc := readProjectFile(t, root, dir); !reflect.DeepEqual(doc["extensions"], []any{ExtensionName}) {
			t.Errorf("%s = %v", dir, doc)
		}
	}
	if options := readProjectFile(t, root, "apps/api")["options"].(map[string]any); options["@putnami/typescript"] != nil || options["@putnami/typescript:test"] != nil {
		t.Errorf("a delegated api keeps the TypeScript options: %v", options)
	}
	// A workspace that records no mode imports, as putnami init does without
	// a terminal (D-064).
	if mode, err := Mode(t.TempDir()); err != nil || mode != ModeImport {
		t.Errorf("a workspace without a mode: %s %v", mode, err)
	}
}

// stubMoon puts first on PATH a moon that prints its version and, for run,
// writes the JUnit report of its project when STUB_JUNIT names one, then
// exits with STUB_EXIT. It logs each call, with its working directory, to
// $STUB_LOG.
func stubMoon(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
echo "$(pwd) moon $*" >> "$STUB_LOG"
case "$1" in --version) echo "moon ${STUB_VERSION:-` + version + `}"; exit 0;; esac
if [ -n "$STUB_JUNIT" ]; then mkdir -p "$(dirname "$STUB_JUNIT")"; printf '%s' '<testsuites><testsuite name="api" tests="2" failures="0"><testcase classname="api" name="a"/><testcase classname="api" name="b"/></testsuite></testsuites>' > "$STUB_JUNIT"; fi
exit "${STUB_EXIT:-0}"
`
	stackstest.WriteFile(t, bin, "moon", script)
	if err := os.Chmod(filepath.Join(bin, "moon"), 0o755); err != nil {
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
	meta := probe(t, root)[dir].Metadata
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

func TestATaskRunsMoonOnItsProjectAndRecordsTheDelegation(t *testing.T) {
	spectest.Proves(t, feature, "tasks-delegate-to-moon", "a-task-runs-moon-on-its-project-and-records-the-delegation")
	d := readDemo(t)
	stubMoon(t)
	root := repository(t, d, ModeDelegate, false)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	for _, c := range []struct{ dir, task, want string }{
		{"apps/api", stacks.TaskBuild, "moon run api:build"},
		{"apps/api", stacks.TaskLint, "moon run api:lint"},
		{"apps/api", stacks.TaskTest, "moon run api:test"},
		// pricing tags its unit task test: the sync wrote the step.
		{"packages/pricing", stacks.TaskTest, "moon run pricing:unit"},
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		status, data, events := runJob(t, root, c.dir, c.task)
		if status != "OK" || data["delegatedTo"] != "moon" {
			t.Errorf("%s %s: status %s, data %v, events %s", c.dir, c.task, status, data, events)
		}
		log := stubLog(t)
		if len(log) != 2 || !strings.HasSuffix(log[0], " moon --version") || log[1] != rootReal+" "+c.want {
			t.Errorf("%s %s ran %q, want the version check then %q in %s", c.dir, c.task, log, c.want, rootReal)
		}
	}

	t.Setenv("STUB_VERSION", "1.29.0")
	if status, _, events := runJob(t, root, "apps/api", stacks.TaskBuild); status != "FAILED" || !strings.Contains(events, stacks.CodeToolchainMismatch) {
		t.Errorf("a moon version other than .prototools' fails the task: %s %s", status, events)
	}
	t.Setenv("STUB_VERSION", "")
	t.Setenv("STUB_EXIT", "3")
	if status, data, events := runJob(t, root, "apps/api", stacks.TaskBuild); status != "FAILED" || data["delegatedTo"] != "moon" || !strings.Contains(events, stacks.CodeStepFailed) {
		t.Errorf("a failing moon run fails the task: %s %v %s", status, data, events)
	}
}

func TestTheTestTaskReadsTheReportsTheProjectOptionsName(t *testing.T) {
	spectest.Proves(t, feature, "reports-from-options", "the-test-task-reads-the-reports-the-project-options-name")
	d := readDemo(t)
	stubMoon(t)
	root := repository(t, d, ModeDelegate, false)
	stackstest.WriteFile(t, root, "apps/api/putnami.json", `{"options": {"@putnami/moon": {"tasks": {"test": {"reports": {"junit": ["{root}/apps/api/reports/junit.xml"]}}}}}}`+"\n")
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUB_JUNIT", filepath.Join(root, "apps/api/reports/junit.xml"))
	status, data, events := runJob(t, root, "apps/api", stacks.TaskTest)
	if status != "OK" {
		t.Fatalf("status %s: %s", status, events)
	}
	if got, _ := data["testSummary"].(map[string]any); got["total"] != 2 || got["passed"] != 2 || data["delegatedTo"] != "moon" {
		t.Errorf("data = %v, want api's 2 cases", data)
	}
	// pricing names no report: its result is moon's exit code alone.
	t.Setenv("STUB_JUNIT", "")
	status, data, events = runJob(t, root, "packages/pricing", stacks.TaskTest)
	if status != "OK" || data["testSummary"] != nil {
		t.Errorf("pricing: %s %v %s", status, data, events)
	}
}

func TestAStaleMoonGraphFailsValidate(t *testing.T) {
	spectest.Proves(t, feature, "validate-fails-on-a-stale-graph", "a-stale-moon-graph-fails-validate")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, false)
	ctx := &pctx.Context{WorkspaceRoot: root}
	if status, _, err := RunSync(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Fatalf("the sync job: %s %v", status, err)
	}
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("the validate job after the sync: %s %v", status, err)
	}
	// web now depends on api: moon's edges changed, and putnami.json did not.
	stackstest.WriteFile(t, root, "apps/web/moon.yml", "dependsOn: ['api']\n")
	before, _ := os.ReadFile(filepath.Join(root, "apps/web/putnami.json"))
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "FAILED" || err == nil || err.Error() != CodeSyncStale {
		t.Errorf("the validate job on a stale graph: %s %v", status, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "apps/web/putnami.json")); string(after) != string(before) {
		t.Errorf("the check wrote %s", after)
	}
	// Outside a moon workspace the check warns and passes.
	if status, _, err := RunCheck(&pctx.Context{WorkspaceRoot: t.TempDir()}, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("no workspace: %s %v", status, err)
	}
}

func TestTheMoonManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-moon-manifest-conforms-to-the-stack-core")
	s := stack(t)
	if !s.Profile.Delegated {
		t.Fatal("the moon profile is delegated")
	}
	for _, task := range Commands {
		if _, ok := s.Profile.Tasks[task]; !ok {
			t.Errorf("the moon profile declares no %s task", task)
		}
	}
	stackstest.CheckManifest(t, "putnami.extension.json", s)
}

func TestTheSyncKeepsTheWorkspaceConfigOrder(t *testing.T) {
	spectest.Proves(t, feature, "import-maps-to-language-extensions", "import-mode-maps-a-project-to-its-language-extension")
	root := t.TempDir()
	file := filepath.Join(root, wsproto.WorkspaceConfigFilename)
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, "{\n  \"name\": \"demo\",\n  \"options\": {\"@putnami/moon\": {\"mode\": \"import\"}},\n  \"extensions\": [\"@putnami/moon\"]\n}\n")
	if changed, _, err := declareExtensions(root, []string{"@putnami/go"}, false); err != nil || !changed {
		t.Fatalf("declare: %v %v", changed, err)
	}
	data, _ := os.ReadFile(file)
	want := "{\n  \"name\": \"demo\",\n  \"options\": {\n    \"@putnami/moon\": {\n      \"mode\": \"import\"\n    }\n  },\n  \"extensions\": [\n    \"@putnami/moon\",\n    \"@putnami/go\"\n  ]\n}\n"
	if string(data) != want {
		t.Errorf("config:\n%s\nwant\n%s", data, want)
	}
	if changed, _, err := declareExtensions(root, []string{"@putnami/go"}, false); err != nil || changed {
		t.Errorf("a declared extension changes nothing: %v %v", changed, err)
	}
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, "[]")
	if _, _, err := declareExtensions(root, []string{"@putnami/go"}, false); err == nil {
		t.Error("a config that is no object fails")
	}
	if changed, _, err := declareExtensions(t.TempDir(), []string{"@putnami/go"}, false); err != nil || changed {
		t.Errorf("no workspace config: %v %v", changed, err)
	}
}

func TestAnExtensionLoadedFromTheWorkspaceIsDeclared(t *testing.T) {
	spectest.Proves(t, feature, "import-maps-to-language-extensions", "import-mode-maps-a-project-to-its-language-extension")
	root := t.TempDir()
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"extensions": ["/moon-extension", "./tools/go", "/missing"]}`)
	stackstest.WriteFile(t, root, "tools/go/putnami.extension.json", `{"name": "@putnami/go"}`)
	if changed, _, err := declareExtensions(root, []string{"@putnami/go"}, false); err != nil || changed {
		t.Errorf("an extension the workspace loads by path is declared: %v %v", changed, err)
	}
	if changed, _, err := declareExtensions(root, []string{"@putnami/rust"}, true); err != nil || !changed {
		t.Errorf("a dry run reports an undeclared extension: %v %v", changed, err)
	}
}

func TestADryRunOfTheSyncJobPrintsEachDiffAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-each-project", "the-sync-writes-each-moon-project-putnami-json")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, false)
	config, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	// The mode parameter wins over the recorded one: init previews the mode
	// it is about to record.
	ctx := &pctx.Context{WorkspaceRoot: root}
	ctx.Params = pctx.Params{"dryRun": json.RawMessage(`true`), "mode": json.RawMessage(`"import"`)}
	status, data, err := RunSync(ctx, emit, nil)
	if status != "OK" || err != nil || data["mode"] != ModeImport || data["dryRun"] != true {
		t.Fatalf("the dry sync job: %s %v %v", status, data, err)
	}
	out := events.String()
	for _, want := range []string{
		"apps/api/putnami.json would change:\n--- /dev/null\n+++ b/apps/api/putnami.json\n",
		"+    \"@putnami/typescript\"",
		"putnami.workspace.json would change:\n--- a/putnami.workspace.json",
		"+    \"@putnami/rust\",",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the preview prints %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "apps/api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a dry run wrote api: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename)); string(after) != string(config) {
		t.Errorf("a dry run wrote the workspace config:\n%s", after)
	}
	ctx.Params = pctx.Params{"mode": json.RawMessage(`"mirror"`)}
	if status, _, err := RunSync(ctx, jsonl.New(), nil); status != "FAILED" || err == nil {
		t.Errorf("an unknown mode fails the sync: %s %v", status, err)
	}
}

func TestTheSyncKeepsTheProjectOwnSteps(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-each-project", "the-sync-writes-each-moon-project-putnami-json")
	d := readDemo(t)
	root := repository(t, d, ModeDelegate, false)
	own := `{"options": {"@putnami/moon": {"tasks": {"test": {"steps": [["moon", "ci", "pricing:unit"]]}}}}}`
	stackstest.WriteFile(t, root, "packages/pricing/putnami.json", own)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	doc := readProjectFile(t, root, "packages/pricing")
	steps := doc["options"].(map[string]any)[ExtensionName].(map[string]any)["tasks"].(map[string]any)["test"].(map[string]any)["steps"]
	if !reflect.DeepEqual(steps, []any{[]any{"moon", "ci", "pricing:unit"}}) {
		t.Errorf("the project's own steps = %v", steps)
	}
	if stale, err := Stale(context.Background(), root); err != nil || len(stale.Written) != 0 {
		t.Errorf("a project's own steps are no drift: %v %v", stale.Written, err)
	}
	// A step the sync wrote is the sync's: it goes once the task is named.
	stackstest.WriteFile(t, root, "packages/pricing/putnami.json", `{"options": {"@putnami/moon": {"tasks": {"test": {"steps": [["moon", "run", "{component}:unit"]]}}}}}`)
	stackstest.WriteFile(t, root, "packages/pricing/moon.yml", "language: 'rust'\ntasks:\n  test:\n    command: 'cargo test'\n")
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if doc := readProjectFile(t, root, "packages/pricing"); !reflect.DeepEqual(doc["options"], map[string]any{ExtensionName: map[string]any{"graphSync": true, "filePatterns": []any{"../../.moon/workspace.yml", "../../.prototools", "../../.moon/tasks/**"}}}) {
		t.Errorf("pricing options = %v", doc["options"])
	}
}

func TestImportDeclaresIntoTheObjectFormOfExtensions(t *testing.T) {
	spectest.Proves(t, feature, "import-maps-to-language-extensions", "import-mode-maps-a-project-to-its-language-extension")
	d := readDemo(t)
	root := repository(t, d, ModeImport, false)
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, "{\n  \"extensions\": {\"@putnami/moon\": \"^0.1.0\"},\n  \"options\": {\"@putnami/moon\": {\"mode\": \"import\"}}\n}\n")
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	want := "{\n  \"extensions\": {\n    \"@putnami/moon\": \"^0.1.0\",\n    \"@putnami/rust\": \"\",\n    \"@putnami/typescript\": \"\"\n  },\n  \"options\": {\n    \"@putnami/moon\": {\n      \"mode\": \"import\"\n    }\n  }\n}\n"
	if string(data) != want {
		t.Errorf("config:\n%s\nwant\n%s", data, want)
	}
	if stale, err := Stale(context.Background(), root); err != nil || len(stale.Written) != 0 {
		t.Errorf("after the sync nothing is stale: %v %v", stale.Written, err)
	}
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"extensions": 3, "options": {"@putnami/moon": {"mode": "import"}}}`)
	if _, err := Sync(context.Background(), root); err == nil || !strings.Contains(err.Error(), "neither a list") {
		t.Errorf("extensions of another form fails the sync: %v", err)
	}
}

func TestAnUnknownRecordedModeFailsTheSync(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-each-project", "the-sync-writes-each-moon-project-putnami-json")
	d := readDemo(t)
	root := repository(t, d, "mirror", false)
	if _, err := Sync(context.Background(), root); err == nil || !strings.Contains(err.Error(), "unknown adoption mode mirror") {
		t.Errorf("err = %v", err)
	}
	if _, err := Mode(root); err == nil {
		t.Error("an unknown mode delegates")
	}
	_, diags := Detect(root, wsproto.ProbeRequest{Paths: []string{"."}})
	if len(diags) != 1 || diags[0].Code != stacks.CodeAdoptionModeInvalid {
		t.Errorf("the probe of an unknown mode: %v", diags)
	}
}

func TestTheDryRunOfASwitchToImportPreviewsWhatTheApplyWrites(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-each-project", "the-sync-writes-each-moon-project-putnami-json")
	d := readDemo(t)
	// The workspace was adopted in delegate mode and synchronized.
	root := repository(t, d, ModeDelegate, true)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// The preview of putnami init --from moon --mode import: the sync maps
	// each project to its language extension without reading what the
	// workspace declares, and declares those extensions itself.
	preview, err := synchronize(context.Background(), root, true, ModeImport)
	if err != nil {
		t.Fatal(err)
	}
	previewed := map[string][]byte{}
	for _, c := range preview.Changes {
		if c.File != wsproto.WorkspaceConfigFilename {
			previewed[c.File] = c.After
		}
	}
	if len(previewed) == 0 || !slices.Contains(preview.Written, wsproto.WorkspaceConfigFilename) {
		t.Fatalf("the preview changes %v", preview.Written)
	}

	// --apply records the mode, then synchronizes in the recorded mode.
	stackstest.WriteFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "demo", "extensions": ["@putnami/moon"], "options": {"@putnami/moon": {"mode": "import"}}}`+"\n")
	applied, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied.Written, preview.Written) || !reflect.DeepEqual(applied.Retracted, preview.Retracted) || !reflect.DeepEqual(applied.Extensions, preview.Extensions) {
		t.Errorf("the preview wrote %v, retracted %v, declared %v; the apply %v, %v, %v", preview.Written, preview.Retracted, preview.Extensions, applied.Written, applied.Retracted, applied.Extensions)
	}
	for file, want := range previewed {
		if got, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(file))); string(got) != string(want) {
			t.Errorf("%s: the apply wrote\n%s\nthe preview showed\n%s", file, got, want)
		}
	}
}

func TestAnImportKeepsOnMoonATaskThatRunsAnotherTool(t *testing.T) {
	spectest.Proves(t, feature, "import-keeps-the-tool-that-checks-the-code", "a-task-that-runs-another-tool-keeps-running-through-moon")
	d := readDemo(t)
	root := repository(t, d, ModeImport, false)
	// api tests with Mocha and lints with the ESLint it inherits; pricing lints with rustfmt and tests with
	// cargo nextest; a Go project tests with go test and lints with go vet.
	stackstest.WriteFile(t, root, "apps/api/moon.yml", "language: 'typescript'\ndependsOn: ['pricing']\ntasks:\n  build:\n    command: 'bun build src/index.ts --outdir dist'\n  test:\n    command: 'mocha'\n")
	stackstest.WriteFile(t, root, "packages/pricing/moon.yml", "language: 'rust'\ntasks:\n  unit:\n    command: 'cargo nextest run'\n    tags: ['test']\n  lint:\n    command: 'cargo fmt --check'\n")
	stackstest.WriteFile(t, root, "apps/svc/moon.yml", "language: 'go'\ntasks:\n  test:\n    script: 'go test ./...'\n  lint:\n    command: 'go'\n    args: ['vet', './...']\n")
	stackstest.WriteFile(t, root, "apps/svc/go.mod", "module svc\n")
	result, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, u := range result.Unmapped {
		reasons[u.Project+":"+u.Task] = u.Reason
	}
	for task, want := range map[string]string{
		"api:test":     `@putnami/typescript runs bun test or vitest or jest or npx vitest or npx jest, not "mocha": it keeps running through moon run api:test`,
		"pricing:lint": `@putnami/rust has no lint that runs only what a moon task runs: it keeps running through moon run pricing:lint`,
		"svc:lint":     `@putnami/go has no lint that runs only what a moon task runs: it keeps running through moon run svc:lint`,
	} {
		if reasons[task] != want {
			t.Errorf("%s reason = %q, want %q", task, reasons[task], want)
		}
	}
	for dir, jobs := range map[string][]any{
		"apps/api":         {"@putnami/moon:build", "@putnami/moon:lint", "@putnami/typescript:test"},
		"packages/pricing": {"build", "@putnami/moon:test", "@putnami/rust:lint"},
		"apps/svc":         {"build", "@putnami/moon:test", "@putnami/go:lint"},
	} {
		doc := readProjectFile(t, root, dir)
		if !reflect.DeepEqual(doc["disable"], map[string]any{"jobs": jobs}) || !slices.Contains(stringsOf(doc["extensions"]), ExtensionName) {
			t.Errorf("%s = %v, want the jobs %v", dir, doc, jobs)
		}
	}
	// A kept task moon tags, not names, runs its own task.
	pricing := readProjectFile(t, root, "packages/pricing")
	if tasks := pricing["options"].(map[string]any)[ExtensionName].(map[string]any)["tasks"]; tasks != nil {
		t.Errorf("pricing writes steps for a moved task: %v", tasks)
	}
	stackstest.WriteFile(t, root, "packages/pricing/moon.yml", "language: 'rust'\ntasks:\n  unit:\n    command: 'cargo test-all'\n    tags: ['test']\n")
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	pricing = readProjectFile(t, root, "packages/pricing")
	if steps := pricing["options"].(map[string]any)[ExtensionName].(map[string]any)["tasks"]; !reflect.DeepEqual(steps, map[string]any{"test": map[string]any{"steps": []any{[]any{"moon", "run", "{component}:unit"}}}}) {
		t.Errorf("pricing's kept unit task steps = %v", steps)
	}
	// The check sees a task that changed its tool.
	if status, _, err := RunCheck(&pctx.Context{WorkspaceRoot: root}, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("check after the sync: %s %v", status, err)
	}
	stackstest.WriteFile(t, root, "packages/pricing/moon.yml", "language: 'rust'\ntasks:\n  unit:\n    command: 'cargo nextest run'\n    tags: ['test']\n")
	if status, _, err := RunCheck(&pctx.Context{WorkspaceRoot: root}, jsonl.New(), nil); status != "FAILED" || err == nil || err.Error() != CodeSyncStale {
		t.Errorf("check on a changed tool: %s %v", status, err)
	}
}

func TestATypeScriptTaskMovesOnlyWhereItsScriptsLeadTheExtension(t *testing.T) {
	spectest.Proves(t, feature, "import-keeps-the-tool-that-checks-the-code", "a-task-that-runs-another-tool-keeps-running-through-moon")
	root := t.TempDir()
	stackstest.WriteFile(t, root, "p/package.json", `{"name": "p", "scripts": {"build": "tsc -p tsconfig.lib.json", "test": "jest", "lint": "eslint ."}}`)
	p := detect.Project{ID: "p", Dir: "p", Language: "typescript", Tasks: []detect.Task{{Name: "build"}, {Name: "test"}, {Name: "lint"}}}
	for _, c := range []struct {
		command, line string
		moves         bool
	}{
		{"build", "tsc -p tsconfig.lib.json", true},
		{"build", "tsc -b", false},
		{"build", "bun build src/index.ts", false},
		{"test", "vitest run", true},
		{"test", "bun test", true},
		{"test", "jest --config jest.ci.js", false},
		{"lint", "biome check .", false},
		{"lint", "eslint", true},
		{"test", "", false},
	} {
		if got, reason := moves(root, p, c.command, c.line); got != c.moves || (!got && reason == "") {
			t.Errorf("%s %q moves = %v (%q), want %v", c.command, c.line, got, reason, c.moves)
		}
	}
	// A moved bun test records its runner: the extension would otherwise run
	// the Jest the package.json test script launches.
	stackstest.WriteFile(t, root, "b/package.json", `{"name": "b", "scripts": {"test": "cross-env TZ=UTC jest"}}`)
	bun := detect.Project{ID: "b", Dir: "b", Language: "typescript", Tasks: []detect.Task{{Name: "test", Command: "bun test"}}}
	if plan := planImport(root, bun, map[string]string{"test": "test"}); plan.runner != "bun" || len(plan.kept) != 0 {
		t.Errorf("a bun test plans %+v, want it moved with the bun runner", plan)
	}
	for line, want := range map[string]string{"bundle exec rspec": "rspec", "vendor/bin/phpunit --testdox": "phpunit --testdox", "uv run pytest -q": "pytest -q"} {
		if got := bareCommand(line); got != want {
			t.Errorf("bareCommand(%q) = %q, want %q", line, got, want)
		}
	}
	// A task moves only with the arguments its extension runs the same
	// without: the extension drops the others, such as build tags, a release
	// profile or a subset of the suite.
	for _, c := range []struct {
		language, command, line string
		moves                   bool
		dropped                 string
	}{
		{"go", "test", "go test ./...", true, ""},
		{"go", "test", "go test -tags integration ./...", false, "-tags integration"},
		{"go", "build", "go build ./...", true, ""},
		{"rust", "build", "cargo build --release", false, "--release"},
		{"rust", "build", "cargo build", true, ""},
		{"rust", "test", "cargo nextest run", true, ""},
		{"rust", "test", "cargo nextest run -p pricing", false, "-p pricing"},
		{"python", "test", "uv run pytest tests/unit", false, "tests/unit"},
		{"python", "test", "pytest -q", true, ""},
		{"go", "test", "go test .", false, "."},
		{"go", "build", "go build .", false, "."},
		{"python", "lint", "ruff check .", false, ""},
		{"php", "test", "vendor/bin/phpunit --testsuite unit", false, "--testsuite unit"},
		{"php", "lint", "vendor/bin/phpstan " + strings.Replace("analyze", "z", "s", 1), true, ""},
		{"php", "lint", "vendor/bin/phpstan analyze", true, ""},
		{"ruby", "test", "bundle exec rspec", true, ""},
		{"ruby", "test", "bundle exec rspec spec/models", false, "spec/models"},
		{"go", "test", "gotestsum", false, ""},
	} {
		ok, reason := moves(root, detect.Project{ID: "x", Language: c.language}, c.command, c.line)
		if ok != c.moves || (c.dropped != "" && !strings.Contains(reason, "would drop \""+c.dropped+"\"")) {
			t.Errorf("%s %q moves = %v (%q), want %v dropping %q", c.language, c.line, ok, reason, c.moves, c.dropped)
		}
	}
	if ok, _ := moves(root, detect.Project{ID: "r", Language: "ruby"}, "test", "bundle exec rspec spec && echo done"); ok {
		t.Error("a chained command moves")
	}
	if ok, reason := moves(root, detect.Project{ID: "py", Language: "python"}, "build", "uv build"); ok || !strings.Contains(reason, "has no build") {
		t.Errorf("a Python build moves: %v %q", ok, reason)
	}
}

func TestAMovedTypeScriptTaskDoesWhatItsTaskDid(t *testing.T) {
	spectest.Proves(t, feature, "a-moved-task-does-what-its-script-did", "an-imported-task-does-what-its-task-did")
	root := t.TempDir()
	// lib is published and names a build output: the Bun build would not
	// write it. app names its sources.
	stackstest.WriteFile(t, root, "lib/package.json", `{"name": "lib", "main": "dist/index.js"}`)
	stackstest.WriteFile(t, root, "app/package.json", `{"name": "app", "private": true, "main": "src/index.ts"}`)
	lib := detect.Project{ID: "lib", Dir: "lib", Language: "typescript", Tasks: []detect.Task{{Name: "build", Command: "bun build src/index.ts"}}}
	if ok, reason := moves(root, lib, "build", "bun build src/index.ts"); ok || !strings.Contains(reason, `names "dist/index.js", which the Bun build`) || !strings.HasSuffix(reason, "moon run lib:build") {
		t.Errorf("lib build moves = %v (%q), want it kept on moon", ok, reason)
	}
	app := detect.Project{ID: "app", Dir: "app", Language: "typescript", Tasks: []detect.Task{
		{Name: "build", Command: "bun build src/index.ts"}, {Name: "lint", Command: "biome check --write ."}, {Name: "test", Command: "bun test"},
	}}
	plan := planImport(root, app, map[string]string{"build": "build", "lint": "lint", "test": "test"})
	if len(plan.kept) != 0 || !plan.bunBuild || !plan.lintMoved || !plan.testMoved {
		t.Errorf("app plans %+v, want every task moved, a lint that writes and a test", plan)
	}
}

func stringsOf(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestAnImportAddsSourceExportsAndATSConfig(t *testing.T) {
	spectest.Proves(t, feature, "import-adopts-typescript-conventions", "an-import-adds-source-exports-and-a-tsconfig")
	d := readDemo(t)
	root := repository(t, d, ModeImport, true)
	// api builds with Bun, so its main names an output nothing writes in the
	// project.
	stackstest.WriteFile(t, root, "apps/api/moon.yml", "language: 'typescript'\ndependsOn: ['pricing']\ntasks:\n  build:\n    command: 'bun build src/index.ts --outdir lib'\n")
	manifest := "{\n  \"name\": \"@demo/api\",\n  \"private\": true,\n  \"main\": \"lib/index.js\"\n}\n"
	stackstest.WriteFile(t, root, "apps/api/package.json", manifest)
	dry, err := Stale(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, c := range dry.Changes {
		files = append(files, c.File)
	}
	if !slices.Contains(files, "apps/api/package.json") || !slices.Contains(files, "apps/api/tsconfig.json") {
		t.Errorf("a dry import shows %v, want the exports and the tsconfig", files)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "apps/api/package.json")); string(data) != manifest {
		t.Errorf("a dry import wrote package.json")
	}
	result, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "apps/api/package.json")); !strings.Contains(string(data), `"exports": {`) {
		t.Errorf("api package.json = %s", data)
	}
	if !slices.ContainsFunc(result.Warnings, func(w string) bool {
		return strings.HasPrefix(w, "apps/api/package.json: exports ./src/index.ts added")
	}) {
		t.Errorf("warnings = %v", result.Warnings)
	}
	if again, err := Stale(context.Background(), root); err != nil || len(again.Written) != 0 || len(again.Changes) != 0 {
		t.Errorf("a second import changes %v %v, %v", again.Written, again.Changes, err)
	}
}
