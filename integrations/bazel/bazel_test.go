package bazel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "monorepo/bazel-delegated-stack"

// demoFile is the Bazel demo of putnami observe: a Bzlmod workspace where
// //api depends on //pricing and //web stands alone, with the export and the
// test outputs its CI wrote.
const demoFile = "../../tooling/observe/internal/observe/testdata/demos/bazel.txtar"

// version is the Bazel version the tests state in .bazelversion.
const version = "7.4.1"

// demo holds the demo's sections by prefix: base/ is the repository, head/
// the change on top of it, ci/ the export, bazel/ the test outputs.
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

// repository writes the demo's base files and .bazelversion, and the export
// when withExport.
func repository(t *testing.T, d demo, withExport bool) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		stackstest.WriteFile(t, root, rel, content)
	}
	stackstest.WriteFile(t, root, ".bazelversion", version+"\n")
	if withExport {
		stackstest.WriteFile(t, root, "bazel-graph.json", d["ci"]["bazel-graph.json"])
	}
	return root
}

// coverageOf keeps the records of a combined LCOV report whose source lies
// in the package directory.
func coverageOf(report, dir string) string {
	var out strings.Builder
	for _, record := range strings.SplitAfter(report, "end_of_record\n") {
		if strings.HasPrefix(record, "SF:"+dir+"/") {
			out.WriteString(record)
		}
	}
	return out.String()
}

// stubBazel puts first on PATH a bazel that prints its version, prints the
// demo's export for query, and for test or coverage //<pkg>:all writes the
// build-event file of that package's test runs, whose outputs lie in an
// output tree outside the workspace, as Bazel's do; a coverage output only
// for coverage, or for test with --collect_code_coverage. It logs each call,
// with its working directory, to $STUB_LOG, and returns the output tree's
// testlogs directory.
func stubBazel(t *testing.T, d demo) string {
	t.Helper()
	bin, ci, outBase := t.TempDir(), t.TempDir(), t.TempDir()
	stackstest.WriteFile(t, ci, "bazel-graph.json", d["ci"]["bazel-graph.json"])
	testlogs := filepath.Join(outBase, "execroot", "_main", "bazel-out", "darwin-fastbuild", "testlogs")
	events, uncovered := map[string][]string{}, map[string][]string{}
	for rel, content := range d["bazel"] {
		logRel, ok := strings.CutPrefix(rel, "testlogs/")
		if !ok || !strings.HasSuffix(logRel, "/test.xml") {
			continue
		}
		logDir := filepath.Dir(logRel)
		pkg, target := filepath.Dir(logDir), filepath.Base(logDir)
		stackstest.WriteFile(t, testlogs, logRel, content)
		stackstest.WriteFile(t, testlogs, logDir+"/coverage.dat", coverageOf(d["bazel"]["out/_coverage/_coverage_report.dat"], pkg))
		uri := func(name string) string { return "file://" + filepath.ToSlash(filepath.Join(testlogs, logDir, name)) }
		event := func(outputs ...string) string {
			var listed []map[string]string
			for _, o := range outputs {
				file := o
				if o == "test.lcov" {
					file = "coverage.dat"
				}
				listed = append(listed, map[string]string{"name": o, "uri": uri(file)})
			}
			data, _ := json.Marshal(map[string]any{
				"id":         map[string]any{"testResult": map[string]any{"label": "//" + pkg + ":" + target, "run": 1, "shard": 1, "attempt": 1}},
				"testResult": map[string]any{"status": "PASSED", "testActionOutput": listed},
			})
			return string(data)
		}
		events[pkg] = append(events[pkg], event("test.log", "test.xml", "test.lcov"))
		uncovered[pkg] = append(uncovered[pkg], event("test.log", "test.xml"))
	}
	for pkg, lines := range events {
		flat := strings.ReplaceAll(pkg, "/", "_")
		stackstest.WriteFile(t, ci, "events/"+flat+".json", `{"id":{"started":{}},"started":{"command":"coverage"}}`+"\n"+strings.Join(lines, "\n")+"\n")
		stackstest.WriteFile(t, ci, "events/"+flat+".uncovered.json", `{"id":{"started":{}},"started":{"command":"test"}}`+"\n"+strings.Join(uncovered[pkg], "\n")+"\n")
	}
	script := `#!/bin/sh
set -e
echo "$(pwd) bazel $*" >> "$STUB_LOG"
case "$1" in --version) echo "bazel ${STUB_VERSION:-` + version + `}"; exit 0;; esac
for a in "$@"; do last="$a"; done
case "$1" in
  query) cat '` + ci + `/bazel-graph.json'; exit 0;;
  test|coverage)
    pkg="${last#//}"; pkg="${pkg%:all}"; flat="$(echo "$pkg" | tr / _)"
    kind=".uncovered"; [ "$1" = coverage ] && kind=""
    for a in "$@"; do case "$a" in --build_event_json_file=*) bep="${a#--build_event_json_file=}";; --collect_code_coverage) kind="";; esac; done
    if [ -e '` + ci + `'/events/"$flat".json ]; then cp '` + ci + `'/events/"$flat$kind".json "$bep"; else echo '{"id":{"started":{}}}' > "$bep"; fi
    exit "${STUB_TEST_EXIT:-0}";;
esac
exit 0
`
	stackstest.WriteFile(t, bin, "bazel", script)
	if err := os.Chmod(filepath.Join(bin, "bazel"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
	return testlogs
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
		Paths: []string{".", "api", "pricing", "web", "tools"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestThePackagesOfABazelWorkspaceBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "packages-become-projects", "the-packages-of-a-bazel-workspace-become-projects-with-their-edges")
	d := readDemo(t)
	root := repository(t, d, true)
	// The root package and a directory with a BUILD file but no rule of the
	// export are bare.
	stackstest.WriteFile(t, root, "BUILD.bazel", "exports_files([\"README.md\"])\n")
	stackstest.WriteFile(t, root, "tools/BUILD", "")
	result := probe(t, root)
	if len(result.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v", result.Diagnostics)
	}
	got := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		got[p.Path] = p
	}
	if bare := got["tools"]; !reflect.DeepEqual(bare, wsproto.ProbeProject{Path: "tools", SourceName: "tools", SourceFile: "tools/BUILD"}) {
		t.Errorf("tools = %+v, want a bare project", bare)
	}
	if bare := got["."]; bare.SourceFile != "BUILD.bazel" || len(bare.Extensions) != 0 {
		t.Errorf("the root = %+v, want a bare project", bare)
	}
	want := map[string][]string{"api": {"pricing"}, "pricing": nil, "web": nil}
	for path, deps := range want {
		p := got[path]
		if p.SourceName != path || p.SourceFile != path+"/BUILD.bazel" || !reflect.DeepEqual(p.Dependencies, deps) ||
			!reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"bazel"}) {
			t.Errorf("%s = %+v", path, p)
		}
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil ||
			!reflect.DeepEqual(meta, stacks.Metadata{Component: path, Root: ".", Toolchain: stacks.Expected{Version: version, Source: ".bazelversion"}, Manifest: path + "/BUILD.bazel"}) {
			t.Errorf("%s metadata = %s (%v)", path, p.Metadata, err)
		}
	}
	if len(got) != 5 {
		t.Errorf("projects = %+v", result.Projects)
	}

	// Without the export, a BUILD file is not parsed: before the sync every
	// directory with one is bare, so a directory with no rule runs no task.
	stubBazel(t, d)
	root = repository(t, d, false)
	stackstest.WriteFile(t, root, "tools/BUILD", "exports_files([\"lint.sh\"])\n")
	result = probe(t, root)
	if len(result.Projects) != 4 || len(result.Diagnostics) != 0 {
		t.Errorf("without the export: projects %+v, diagnostics %v", result.Projects, result.Diagnostics)
	}
	for _, p := range result.Projects {
		if len(p.Extensions) != 0 {
			t.Errorf("before the sync %s = %+v, want a bare project", p.Path, p)
		}
	}
	// After the sync, the packages of the query are the projects that name
	// the extension, and their edges are those the sync wrote in their
	// putnami.json, each attributed to the Bazel graph to a request that asks.
	// tools, which holds no rule, stays bare.
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	s := stack(t)
	attributed, err := s.Probe(root, wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension,
		DependencySources: true, Paths: []string{".", "api", "pricing", "web", "tools"}})
	if err != nil {
		t.Fatal(err)
	}
	if diags := wsproto.ValidateProbeResult(&attributed); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	got = map[string]wsproto.ProbeProject{}
	for _, p := range attributed.Projects {
		got[p.Path] = p
	}
	graph := wsproto.DependencySourceBuildGraph
	wantEdges := map[string]map[string]wsproto.DependencySource{"api": {"pricing": graph}, "pricing": {}, "web": {}}
	for path, sources := range wantEdges {
		p := got[path]
		var deps []string
		for dep := range sources {
			deps = append(deps, dep)
		}
		if !reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || len(p.Dependencies) != len(deps) ||
			!reflect.DeepEqual(p.DependencySources, sources) {
			t.Errorf("after the sync %s = %+v", path, p)
		}
	}
	if tools := got["tools"]; len(tools.Extensions) != 0 || tools.SourceFile != "tools/BUILD" {
		t.Errorf("after the sync tools = %+v, want a bare project", tools)
	}

	// A directory that is no Bazel workspace answers nothing.
	if result := probe(t, t.TempDir()); len(result.Projects) != 0 {
		t.Errorf("no workspace: %+v", result.Projects)
	}
}

func TestADirectoryBazelIgnoresBecomesNoProject(t *testing.T) {
	spectest.Proves(t, feature, "packages-become-projects", "a-directory-bazel-ignores-becomes-no-project")
	d := readDemo(t)
	for _, withExport := range []bool{true, false} {
		root := repository(t, d, withExport)
		stackstest.WriteFile(t, root, "REPO.bazel", "ignore_directories([\"tools\"])\n")
		stackstest.WriteFile(t, root, "tools/BUILD", "")
		var got []string
		for _, p := range probe(t, root).Projects {
			got = append(got, p.Path)
		}
		slices.Sort(got)
		if want := []string{"api", "pricing", "web"}; !reflect.DeepEqual(got, want) {
			t.Errorf("with the export %v: projects %v, want %v", withExport, got, want)
		}
	}
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

// modules are the filePatterns the sync writes for a package one directory
// below the root of the demo, which holds MODULE.bazel, .bazelrc and
// .bazelversion.
var modules = []any{"../MODULE.bazel", "../.bazelrc", "../.bazelversion"}

func TestTheSyncWritesEachPackagePutnamiJSONFromTheExport(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-package-graph", "the-sync-writes-each-package-putnami-json-from-the-export")
	d := readDemo(t)
	t.Setenv("PATH", t.TempDir())

	// Without the export and without bazel, nothing is written.
	root := repository(t, d, false)
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || result.Export != "" || len(result.Written) != 0 {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("no putnami.json is written without an export: %v", err)
	}

	// With the export: each package gets the extension, its edges and the
	// module files.
	root = repository(t, d, true)
	stackstest.WriteFile(t, root, "web/putnami.json", `{"name": "storefront", "dependencies": ["/api", "@acme/design"], "disable": {"jobs": ["publish"]}}`+"\n")
	result, err = Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api/putnami.json", "pricing/putnami.json", "web/putnami.json"}; result.Export != "bazel-graph.json" || !reflect.DeepEqual(result.Written, want) {
		t.Errorf("result %+v, want %v written from the export", result, want)
	}
	options := map[string]any{ExtensionName: map[string]any{"filePatterns": modules}}
	api := readProjectFile(t, root, "api")
	if want := map[string]any{"$schema": stacks.ProjectSchema, "extensions": []any{ExtensionName}, "dependencies": []any{"/pricing"}, "options": options}; !reflect.DeepEqual(api, want) {
		t.Errorf("api = %v, want %v", api, want)
	}
	// An existing file keeps what the sync does not own; a stale Bazel edge
	// goes, and web, which holds no test rule, has its test disabled.
	web := readProjectFile(t, root, "web")
	if want := map[string]any{"name": "storefront", "extensions": []any{ExtensionName}, "dependencies": []any{"@acme/design"}, "disable": map[string]any{"jobs": []any{"publish", "test"}}, "options": options}; !reflect.DeepEqual(web, want) {
		t.Errorf("web = %v, want %v", web, want)
	}

	// A second sync changes nothing.
	again, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || len(again.Written) != 0 || again.Unchanged != 3 {
		t.Errorf("a second sync = %+v, %v", again, err)
	}

	// A package that depends on a target of the root package reads the root
	// BUILD file, and a nested package names the module files from its depth.
	stackstest.WriteFile(t, root, "BUILD.bazel", "filegroup(name = \"config\")\n")
	stackstest.WriteFile(t, root, "api/v2/BUILD.bazel", "go_library(name = \"v2\")\n")
	stackstest.WriteFile(t, root, "bazel-graph.json", d["ci"]["bazel-graph.json"]+
		`{"type":"RULE","rule":{"name":"//:config","ruleClass":"filegroup"}}`+"\n"+
		`{"type":"RULE","rule":{"name":"//api/v2:v2","ruleClass":"go_library","ruleInput":["//:config","//pricing"]}}`+"\n")
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	v2 := readProjectFile(t, root, "api/v2")
	if deps, inputs := v2["dependencies"], v2["options"].(map[string]any)[ExtensionName].(map[string]any)["filePatterns"]; !reflect.DeepEqual(deps, []any{"/pricing"}) ||
		!reflect.DeepEqual(inputs, []any{"../../MODULE.bazel", "../../.bazelrc", "../../.bazelversion", "../../BUILD.bazel"}) {
		t.Errorf("api/v2 = %v", v2)
	}

	// A root that is no Bazel workspace gets an empty result.
	if result, err := Sync(context.Background(), t.TempDir(), &bytes.Buffer{}); err != nil || result.Export != "" {
		t.Errorf("result %+v, err %v", result, err)
	}
}

func TestTheSyncRunsBazelQueryWithoutAnExport(t *testing.T) {
	d := readDemo(t)
	stubBazel(t, d)
	root := repository(t, d, false)
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || result.Export != "bazel query //... --output=streamed_jsonproto" || len(result.Written) != 3 {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if deps := readProjectFile(t, root, "api")["dependencies"]; !reflect.DeepEqual(deps, []any{"/pricing"}) {
		t.Errorf("api dependencies = %v", deps)
	}
	if log := stubLog(t); len(log) != 1 || !strings.HasSuffix(log[0], " bazel query //... --output=streamed_jsonproto") {
		t.Errorf("bazel calls = %v", log)
	}

	// A check compares without writing, and names the file that drifted.
	spectest.Proves(t, feature, "validate-fails-on-a-stale-graph", "a-stale-package-graph-fails-validate")
	if stale, err := Stale(context.Background(), root, &bytes.Buffer{}); err != nil || len(stale.Written) != 0 || len(stale.Retracted) != 0 {
		t.Errorf("a current graph is not stale: %+v %v", stale, err)
	}
	stackstest.WriteFile(t, root, "api/putnami.json", `{"extensions": ["@putnami/bazel"]}`)
	stale, err := Stale(context.Background(), root, &bytes.Buffer{})
	if err != nil || !reflect.DeepEqual(stale.Written, []string{"api/putnami.json"}) {
		t.Errorf("a drifted package is stale: %+v %v", stale, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "api/putnami.json")); string(after) != `{"extensions": ["@putnami/bazel"]}` {
		t.Errorf("the check wrote %s", after)
	}
	ctx := &pctx.Context{WorkspaceRoot: root}
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "FAILED" || err == nil || err.Error() != CodeSyncStale {
		t.Errorf("the validate job on a stale graph: %s %v", status, err)
	}
	if status, _, err := RunSync(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("the sync job: %s %v", status, err)
	}
	if status, _, err := RunCheck(ctx, jsonl.New(), nil); status != "OK" || err != nil {
		t.Errorf("the validate job after the sync: %s %v", status, err)
	}

	// An export that is no export fails the sync and writes nothing.
	root = repository(t, d, false)
	stackstest.WriteFile(t, root, "bazel-graph.json", "not json")
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("an unreadable export fails the sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a failed sync writes nothing: %v", err)
	}

	// A query that fails fails the sync.
	root = repository(t, d, false)
	stackstest.WriteFile(t, filepath.Dir(os.Getenv("STUB_LOG")), "bin/bazel", "#!/bin/sh\nexit 7\n")
	if err := os.Chmod(filepath.Join(filepath.Dir(os.Getenv("STUB_LOG")), "bin/bazel"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(filepath.Dir(os.Getenv("STUB_LOG")), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "bazel query") {
		t.Errorf("a failed query fails the sync: %v", err)
	}
}

// runJob runs a task job of the stack on one package of root, the way the
// CLI does, and returns its status, its data and what it emitted.
func runJob(t *testing.T, root, dir, task string) (string, map[string]any, string) {
	t.Helper()
	s := stack(t)
	s.Runtime = testRuntime
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

// runtime builds the extension's runtime, which the test task's report step
// runs as {runtime}.
func runtime(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "putnami-bazel")
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the runtime is built with go, which is not on PATH: %v", err)
	}
	if data, err := exec.Command(goBinary, "build", "-o", out, "./cmd/putnami-bazel").CombinedOutput(); err != nil {
		t.Fatalf("build the runtime: %v\n%s", err, data)
	}
	return out
}

func TestATaskRunsBazelOnItsPackageAndRecordsTheDelegation(t *testing.T) {
	spectest.Proves(t, feature, "tasks-delegate-to-bazel", "a-task-runs-bazel-on-its-package-and-records-the-delegation")
	d := readDemo(t)
	stubBazel(t, d)
	setRuntime(t)
	root := repository(t, d, true)
	rootReal, _ := filepath.EvalSymlinks(root)
	out := filepath.Join(root, ".putnami/out/pricing/test")

	for task, want := range map[string]string{
		stacks.TaskBuild: "bazel build //pricing:all",
		stacks.TaskTest:  "bazel test --build_event_json_file=" + out + "/build-events.json //pricing:all",
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		status, data, events := runJob(t, root, "pricing", task)
		if status != "OK" || data["delegatedTo"] != "bazel" {
			t.Errorf("%s: status %s, data %v, events %s", task, status, data, events)
		}
		log := stubLog(t)
		if len(log) != 2 || !strings.HasSuffix(log[0], " bazel --version") || log[1] != rootReal+" "+want {
			t.Errorf("%s ran %q, want the version check then %q in %s", task, log, want, rootReal)
		}
	}
	if _, ok := stack(t).Jobs()[stacks.TaskLint]; ok {
		t.Error("the runtime serves a lint job, and Bazel declares no lint convention")
	}

	t.Setenv("STUB_VERSION", "7.3.0")
	if status, _, events := runJob(t, root, "pricing", stacks.TaskBuild); status != "FAILED" || !strings.Contains(events, stacks.CodeToolchainMismatch) {
		t.Errorf("a Bazel version other than .bazelversion's fails the task: %s %s", status, events)
	}
	t.Setenv("STUB_VERSION", "")
	t.Setenv("STUB_TEST_EXIT", "3")
	if status, _, events := runJob(t, root, "pricing", stacks.TaskTest); status != "FAILED" || !strings.Contains(events, stacks.CodeStepFailed) {
		t.Errorf("a failing bazel test fails the task: %s %s", status, events)
	}
}

// testRuntime is the path {runtime} expands to in runJob.
var testRuntime string

// setRuntime makes {runtime} the runtime built from this tree: a test binary
// is no runtime.
func setRuntime(t *testing.T) {
	t.Helper()
	testRuntime = runtime(t)
	t.Cleanup(func() { testRuntime = "" })
}

func TestTheBazelReportsBecomeTheTestAndCoverageSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "the-bazel-junit-and-coverage-become-the-test-and-coverage-summary")
	d := readDemo(t)
	testlogs := stubBazel(t, d)
	setRuntime(t)
	root := repository(t, d, true)
	optIntoCoverage(t, root, "pricing")
	// A log a removed target left in the output tree is no run of this one.
	stackstest.WriteFile(t, testlogs, "pricing/old_test/test.xml", `<testsuites><testsuite name="old" tests="1" failures="1"><testcase classname="old" name="TestGone"><failure message="gone"/></testcase></testsuite></testsuites>`)

	status, data, events := runJob(t, root, "pricing", stacks.TaskTest)
	if status != "OK" {
		t.Fatalf("status %s: %s", status, events)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 3 || got["passed"] != 3 {
		t.Errorf("testSummary = %v, want pricing's 3 cases alone", got)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 4 || got["total"] != 4 {
		t.Errorf("coverageSummary = %v, want pricing's 4 lines alone", got)
	}
	// Bazel's outputs stay where Bazel wrote them, for its cache.
	if _, err := os.Stat(filepath.Join(testlogs, "pricing/pricing_test/test.xml")); err != nil {
		t.Errorf("the test log was removed: %v", err)
	}

	// web holds no test rule: a run that lists no test fails on its missing
	// JUnit report, which is why the sync disables its test.
	if status, _, events := runJob(t, root, "web", stacks.TaskTest); status != "FAILED" || !strings.Contains(events, "wrote no JUnit report") {
		t.Errorf("a test run with no report fails: %s %s", status, events)
	}
}

func TestCollectReportsReadsTheBuildEventFile(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "the-bazel-junit-and-coverage-become-the-test-and-coverage-summary")
	root, into := t.TempDir(), t.TempDir()
	// A run whose outputs are remote is read from bazel-testlogs.
	stackstest.WriteFile(t, root, "bazel-testlogs/api/api_test/test.xml", "<testsuites/>")
	stackstest.WriteFile(t, root, "bazel-testlogs/api/api_test/coverage.dat", "SF:api/api.go\nDA:1,1\nend_of_record\n")
	outputs := t.TempDir()
	stackstest.WriteFile(t, outputs, "testlogs/pricing/pricing_test/shard_1_of_2/test.xml", "<testsuites/>")
	events := filepath.Join(t.TempDir(), "build-events.json")
	stackstest.WriteFile(t, filepath.Dir(events), "build-events.json", fmt.Sprintf(`{"id":{"started":{}}}
{"id":{"testResult":{"label":"@@//api:api_test"}},"testResult":{"testActionOutput":[{"name":"test.xml","uri":"bytestream://cache/blobs/1"}]}}
{"id":{"testResult":{"label":"//pricing:pricing_test","shard":1}},"testResult":{"testActionOutput":[{"name":"test.xml","uri":"file://%s/testlogs/pricing/pricing_test/shard_1_of_2/test.xml"},{"name":"test.log","uri":"file:///x/test.log"}]}}
{"id":{"testResult":{"label":"@other//x:x_test"}}}
`, filepath.ToSlash(outputs)))
	if err := CollectReports([]string{"--events", events, "--root", root, "--into", into}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"api/api_test/test.xml", "api/api_test/coverage.dat", "pricing/pricing_test/shard_1_of_2/test.xml"} {
		if _, err := os.Stat(filepath.Join(into, rel)); err != nil {
			t.Errorf("%s was not collected: %v", rel, err)
		}
	}

	// A missing build-event file collects nothing; a malformed one and a
	// call with no events fail.
	if err := CollectReports([]string{"--events", filepath.Join(root, "none.json"), "--root", root, "--into", into}, nil, nil); err != nil {
		t.Errorf("a missing build-event file: %v", err)
	}
	stackstest.WriteFile(t, root, "bad.json", "not json\n")
	if err := CollectReports([]string{"--events", filepath.Join(root, "bad.json"), "--root", root, "--into", into}, nil, nil); err == nil {
		t.Error("a malformed build-event file collects")
	}
	if err := CollectReports([]string{"--root", root}, nil, nil); err == nil {
		t.Error("a call with no events collects")
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	s := stack(t)
	if !s.Profile.Delegated {
		t.Fatal("the Bazel profile is delegated")
	}
	if _, ok := s.Profile.Tasks[stacks.TaskLint]; ok {
		t.Error("the Bazel profile declares a lint task")
	}
	stackstest.CheckManifest(t, "putnami.extension.json", s)
}

func TestTheSyncRetractsAFormerPackage(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-package-graph", "the-sync-writes-each-package-putnami-json-from-the-export")
	d := readDemo(t)
	t.Setenv("PATH", t.TempDir())
	root := repository(t, d, true)
	stackstest.WriteFile(t, root, "legacy/putnami.json", `{"name": "legacy", "extensions": ["@putnami/bazel", "@acme/lint"], "dependencies": ["/pricing", "@acme/design"]}`+"\n")
	stackstest.WriteFile(t, root, "api/putnami.json", `{"extensions": ["@putnami/bazel"], "dependencies": ["/legacy", "/gone", "@acme/design"]}`+"\n")
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"legacy/putnami.json"}; !reflect.DeepEqual(result.Retracted, want) {
		t.Errorf("retracted = %v, want %v", result.Retracted, want)
	}
	if legacy := readProjectFile(t, root, "legacy"); !reflect.DeepEqual(legacy, map[string]any{"name": "legacy", "extensions": []any{"@acme/lint"}, "dependencies": []any{"@acme/design"}}) {
		t.Errorf("legacy = %v, want the extension and its Bazel edges gone", legacy)
	}
	if deps := readProjectFile(t, root, "api")["dependencies"]; !reflect.DeepEqual(deps, []any{"/pricing", "@acme/design"}) {
		t.Errorf("api dependencies = %v, want the export's edge and the non-Bazel one", deps)
	}
}

func TestTheQueryWaitsForTheTasksOfTheStackRoot(t *testing.T) {
	d := readDemo(t)
	stubBazel(t, d)
	root := repository(t, d, false)
	lock, err := stacks.LockRoot(root, profileID, wsproto.ProbeRootPath)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Stale(context.Background(), root, &bytes.Buffer{})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the query ran beside a task that holds the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the query never ran after the lock was released")
	}
}

// bazel test //<pkg>:all selects no test tagged manual, and exits 4 when it
// selects none: a package whose tests are all manual has its test disabled
// (#1081).
func TestTheSyncDisablesTheTestOfAPackageWhoseTestsAreAllManual(t *testing.T) {
	d := readDemo(t)
	t.Setenv("PATH", t.TempDir())
	root := repository(t, d, true)
	manual := `{"name":"tags","type":"STRING_LIST","stringListValue":["manual","exclusive"],"explicitlySpecified":true}`
	stackstest.WriteFile(t, root, "store/linearizability/BUILD.bazel", "go_test(name = \"linearizability_test\", tags = [\"manual\"])\n")
	stackstest.WriteFile(t, root, "bazel-graph.json", d["ci"]["bazel-graph.json"]+
		`{"type":"RULE","rule":{"name":"//store/linearizability:linearizability_test","ruleClass":"go_test","attribute":[`+manual+`],"ruleInput":["//pricing:pricing"]}}`+"\n"+
		`{"type":"RULE","rule":{"name":"//pricing:pricing_soak_test","ruleClass":"go_test","attribute":[`+manual+`],"ruleInput":["//pricing:pricing"]}}`+"\n")
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if disable := readProjectFile(t, root, "store/linearizability")["disable"]; !reflect.DeepEqual(disable, map[string]any{"jobs": []any{"test"}}) {
		t.Errorf("a package of manual tests alone has disable = %v, want its test disabled", disable)
	}
	if disable, ok := readProjectFile(t, root, "pricing")["disable"]; ok {
		t.Errorf("a package with a test beside a manual one has disable = %v, want its test kept", disable)
	}
}

// optIntoCoverage overrides the test steps of the package dir with bazel
// coverage, the way a project opts into coverage.
func optIntoCoverage(t *testing.T, root, dir string) {
	t.Helper()
	stackstest.WriteFile(t, root, dir+"/putnami.json", `{"options": {"`+ExtensionName+`": {"tasks": {"test": {"steps": [["bazel", "coverage", "--build_event_json_file={output}/build-events.json", "//{component}:all"]]}}}}}`+"\n")
}

// bazel coverage can fail where bazel test passes, as on a C++ test whose
// coverage tree holds a dangling link: test runs bazel test, coverage is
// opted into, and a coverage file that is not LCOV, such as a raw LLVM
// profile, is skipped with a warning (#1081).
func TestTheTestTaskRunsBazelTestAndReadsOnlyAnLCOVCoverage(t *testing.T) {
	d := readDemo(t)
	testlogs := stubBazel(t, d)
	setRuntime(t)
	root := repository(t, d, true)
	rootReal, _ := filepath.EvalSymlinks(root)
	out := filepath.Join(root, ".putnami/out/pricing/test")

	status, data, events := runJob(t, root, "pricing", stacks.TaskTest)
	if want := rootReal + " bazel test --build_event_json_file=" + out + "/build-events.json //pricing:all"; status != "OK" || !slices.Contains(stubLog(t), want) {
		t.Fatalf("status %s, calls %q, want %q: %s", status, stubLog(t), want, events)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 3 || got["passed"] != 3 {
		t.Errorf("testSummary = %v, want pricing's 3 cases", got)
	}
	if got, ok := data["coverageSummary"]; ok {
		t.Errorf("a run that collects no coverage has coverageSummary %v", got)
	}

	// A project opts into coverage; a coverage file that is no LCOV
	// tracefile is skipped with a warning, and the tests still count.
	optIntoCoverage(t, root, "pricing")
	stackstest.WriteFile(t, testlogs, "pricing/pricing_test/coverage.dat", "\xfflprofi\x81\x00\x00\x00\x0a\x00\x00\x00")
	status, data, events = runJob(t, root, "pricing", stacks.TaskTest)
	if status != "OK" {
		t.Fatalf("a run with a coverage file that is not LCOV: status %s: %s", status, events)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 3 {
		t.Errorf("testSummary = %v, want pricing's 3 cases", got)
	}
	if got, ok := data["coverageSummary"]; ok {
		t.Errorf("a coverage file that is not LCOV was read: coverageSummary %v", got)
	}
	if strings.Contains(events, "readers.malformed") {
		t.Errorf("events = %s, want no malformed report", events)
	}

	// The warning is one line of the step's output.
	var stderr strings.Builder
	into := t.TempDir()
	if err := CollectReports([]string{"--events", filepath.Join(out, "build-events.json"), "--root", root, "--into", into}, nil, &stderr); err != nil {
		t.Fatal(err)
	}
	if want := "warning: pricing/pricing_test/coverage.dat is not an LCOV tracefile: its coverage is skipped\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
	if _, err := os.Stat(filepath.Join(into, "pricing/pricing_test/coverage.dat")); !os.IsNotExist(err) {
		t.Errorf("a coverage file that is not LCOV was collected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(into, "pricing/pricing_test/test.xml")); err != nil {
		t.Errorf("the JUnit report of a run with no LCOV coverage was not collected: %v", err)
	}
}
