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

// repository writes the demo's base files, .prototools and a workspace
// config in the mode, and the export when withExport.
func repository(t *testing.T, d demo, mode string, withExport bool) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		writeFile(t, root, rel, content)
	}
	writeFile(t, root, ".prototools", "moon = \""+version+"\"\n")
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"name": "demo", "extensions": ["@putnami/moon"], "options": {"@putnami/moon": {"mode": "`+mode+`"}}}`+"\n")
	if withExport {
		writeFile(t, root, detect.DefaultGraphFile, d["ci"][detect.DefaultGraphFile])
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
	result, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
		Paths: []string{".", "apps/api", "apps/web", "packages/pricing", "docs"}})
	if err != nil {
		t.Fatal(err)
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
	writeFile(t, root, "tools/legacy/putnami.json", `{"name": "legacy", "tags": ["moon", "team"], "extensions": ["@putnami/moon", "@acme/lint"], "dependencies": ["/packages/pricing", "@acme/design"], "options": {"@putnami/moon": {"tasks": {"test": {"steps": [["moon", "run", "{component}:check"]]}}}}}`+"\n")
	writeFile(t, root, "apps/api/putnami.json", `{"name": "api", "options": {"@putnami/moon": {"tasks": {"test": {"reports": {"junit": ["reports/junit.xml"]}}}}}}`+"\n")

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

	want := map[string]map[string]any{
		"apps/api": {"$schema": nil, "name": "api", "tags": []any{"moon"}, "extensions": []any{ExtensionName}, "dependencies": []any{"/packages/pricing"},
			"options": map[string]any{ExtensionName: map[string]any{"tasks": map[string]any{"test": map[string]any{"reports": map[string]any{"junit": []any{"reports/junit.xml"}}}}}}},
		"apps/web": {"$schema": stacks.ProjectSchema, "tags": []any{"moon"}, "extensions": []any{ExtensionName}, "disable": map[string]any{"jobs": []any{"build", "test"}}},
		"packages/pricing": {"$schema": stacks.ProjectSchema, "tags": []any{"moon"}, "extensions": []any{ExtensionName}, "disable": map[string]any{"jobs": []any{"lint"}},
			"options": map[string]any{ExtensionName: map[string]any{"tasks": map[string]any{"test": map[string]any{"steps": []any{[]any{"moon", "run", "{component}:unit"}}}}}}},
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
	result, err := Sync(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Extensions, []string{"@putnami/rust", "@putnami/typescript"}) || !slices.Contains(result.Written, wsproto.WorkspaceConfigFilename) {
		t.Errorf("extensions %v, written %v", result.Extensions, result.Written)
	}
	for dir, ext := range map[string]string{"apps/api": "@putnami/typescript", "packages/pricing": "@putnami/rust", "apps/web": ExtensionName} {
		doc := readProjectFile(t, root, dir)
		if !reflect.DeepEqual(doc["extensions"], []any{ext}) || !reflect.DeepEqual(doc["tags"], []any{"moon"}) {
			t.Errorf("%s = %v, want %s", dir, doc, ext)
		}
		if _, ok := doc["options"]; ok {
			t.Errorf("%s holds a moon step: %v", dir, doc["options"])
		}
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
	// The probe answers only the project that stays delegated.
	got := probe(t, root)
	if len(got) != 1 || got["apps/web"].SourceName != "web" {
		t.Errorf("probe %+v", got)
	}

	// Back to delegate mode, every project runs moon again; the language
	// extensions stay declared in the workspace.
	writeFile(t, root, wsproto.WorkspaceConfigFilename, `{"extensions": ["@putnami/moon", "@putnami/rust", "@putnami/typescript"], "options": {"@putnami/moon": {"mode": "delegate"}}}`)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"apps/api", "packages/pricing"} {
		if doc := readProjectFile(t, root, dir); !reflect.DeepEqual(doc["extensions"], []any{ExtensionName}) {
			t.Errorf("%s = %v", dir, doc)
		}
	}
	if Mode(t.TempDir()) != ModeDelegate {
		t.Error("a workspace without a mode delegates")
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
	writeFile(t, bin, "moon", script)
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
	writeFile(t, root, "apps/api/putnami.json", `{"options": {"@putnami/moon": {"tasks": {"test": {"reports": {"junit": ["{root}/apps/api/reports/junit.xml"]}}}}}}`+"\n")
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
	writeFile(t, root, "apps/web/moon.yml", "dependsOn: ['api']\n")
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
