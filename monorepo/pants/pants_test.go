package pants

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
	"time"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "monorepo/pants-delegated-stack"

// demoFile is the Pants demo of putnami observe: a Pants 2.25 repository
// where api imports pricing and web stands alone, with the export and the
// reports its CI wrote.
const demoFile = "../../tooling/observe/internal/observe/testdata/demos/pants.txtar"

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

// repository writes the demo's base files, and the export when withExport.
func repository(t *testing.T, d demo, withExport bool) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range d["base"] {
		writeFile(t, root, rel, content)
	}
	if withExport {
		writeFile(t, root, "pants-graph.json", d["ci"]["pants-graph.json"])
	}
	return root
}

// stubPants puts first on PATH a pants that prints the demo's version, prints
// the demo's export for peek ::, and for test <dir>: writes the demo's JUnit
// reports of that directory and its coverage report under dist/. It logs each
// call, with its working directory, to $STUB_LOG.
func stubPants(t *testing.T, d demo) {
	t.Helper()
	bin, reports := t.TempDir(), t.TempDir()
	for rel, content := range d["ci"] {
		writeFile(t, reports, rel, content)
	}
	script := `#!/bin/sh
set -e
echo "$(pwd) pants $*" >> "$STUB_LOG"
case "$1" in --version) echo "${STUB_VERSION:-2.25.0}"; exit 0;; esac
for a in "$@"; do last="$a"; done
case " $* " in
  *" peek :: "*) cat "` + reports + `/pants-graph.json"; exit 0;;
  *" test "*)
    dir="${last%:}"
    prefix="$(echo "$dir" | tr / .)."
    mkdir -p dist/test/reports dist/coverage/python
    for f in "` + reports + `"/dist/test/reports/"$prefix"*; do [ -e "$f" ] && cp "$f" dist/test/reports/; done
    cp "` + reports + `/dist/coverage/python/coverage.xml" dist/coverage/python/coverage.xml
    exit "${STUB_TEST_EXIT:-0}";;
esac
`
	writeFile(t, bin, "pants", script)
	if err := os.Chmod(filepath.Join(bin, "pants"), 0o755); err != nil {
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
		Paths: []string{".", "3rdparty/python", "src/python/api", "src/python/pricing", "src/python/web"}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestThePackagesOfAPantsRepositoryBecomeProjects(t *testing.T) {
	spectest.Proves(t, feature, "packages-become-projects", "the-packages-of-a-pants-repository-become-projects-with-their-edges")
	d := readDemo(t)
	result := probe(t, repository(t, d, true))
	if len(result.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v", result.Diagnostics)
	}
	got := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		got[p.Path] = p
	}
	// A directory whose BUILD file declares only third-party targets is bare:
	// named by its directory, with no extension; the root, which holds no
	// BUILD file, is not answered.
	if bare, ok := got["3rdparty/python"]; !ok || !reflect.DeepEqual(bare, wsproto.ProbeProject{Path: "3rdparty/python", SourceName: "3rdparty/python", SourceFile: "3rdparty/python/BUILD"}) {
		t.Errorf("3rdparty/python = %+v, want a bare project", bare)
	}
	delete(got, "3rdparty/python")
	want := map[string][]string{"src/python/api": {"src/python/pricing"}, "src/python/pricing": nil, "src/python/web": nil}
	if len(got) != len(want) {
		t.Fatalf("projects = %+v, want the packages %v", result.Projects, want)
	}
	for path, deps := range want {
		p := got[path]
		if p.SourceName != path || p.SourceFile != path+"/BUILD" || !reflect.DeepEqual(p.Dependencies, deps) ||
			!reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"pants"}) {
			t.Errorf("%s = %+v", path, p)
		}
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil ||
			!reflect.DeepEqual(meta, stacks.Metadata{Component: path, Root: ".", Toolchain: stacks.Expected{Version: "2.25.0", Source: "pants.toml"}, Manifest: path + "/BUILD"}) {
			t.Errorf("%s metadata = %s (%v)", path, p.Metadata, err)
		}
	}

	// Without the export, the BUILD files name the packages, and the edges
	// come from each package's putnami.json.
	result = probe(t, repository(t, d, false))
	if len(result.Projects) != 4 || len(result.Diagnostics) != 0 || len(result.Projects[1].Dependencies) != 0 {
		t.Errorf("without the export: projects %+v, diagnostics %v", result.Projects, result.Diagnostics)
	}
}

func TestTheProbeAttributesThePantsEdgesToTheBuildGraph(t *testing.T) {
	spectest.Proves(t, feature, "packages-become-projects", "the-packages-of-a-pants-repository-become-projects-with-their-edges")
	d := readDemo(t)
	stubPants(t, d)
	root := repository(t, d, false)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// An edge the author keeps on a directory with no BUILD file is no Pants
	// edge: the core judges it as a declaration.
	writeFile(t, root, "docs/README.md", "")
	doc := readProjectFile(t, root, "src/python/api")
	doc["dependencies"] = append(doc["dependencies"].([]any), "/docs")
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/python/api/putnami.json", string(data))

	s := stack(t)
	request := wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: s.Extension, DependencySources: true,
		Paths: []string{".", "3rdparty/python", "src/python/api", "src/python/pricing", "src/python/web"}}
	result, err := s.Probe(root, request)
	if err != nil {
		t.Fatal(err)
	}
	if diags := wsproto.ValidateProbeResult(&result); len(diags) != 0 {
		t.Fatalf("the answer breaks the protocol: %v", diags)
	}
	got := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		got[p.Path] = p
	}
	// Without the export, the edges are those the sync wrote from pants peek.
	api := got["src/python/api"]
	graph := wsproto.DependencySourceBuildGraph
	if want := []string{"3rdparty/python", "src/python/pricing"}; !reflect.DeepEqual(api.Dependencies, want) {
		t.Errorf("api dependencies = %v, want %v", api.Dependencies, want)
	}
	if want := map[string]wsproto.DependencySource{"3rdparty/python": graph, "src/python/pricing": graph}; !reflect.DeepEqual(api.DependencySources, want) {
		t.Errorf("api dependencySources = %v, want %v", api.DependencySources, want)
	}
	// Every package is attributed, each edge to the Pants graph; a bare
	// directory is not.
	for _, path := range []string{"src/python/pricing", "src/python/web"} {
		p := got[path]
		if p.DependencySources == nil || len(p.DependencySources) != len(p.Dependencies) {
			t.Errorf("%s dependencySources = %v for dependencies %v", path, p.DependencySources, p.Dependencies)
		}
		for _, source := range p.DependencySources {
			if source != graph {
				t.Errorf("%s dependencySources = %v, want build-graph", path, p.DependencySources)
			}
		}
	}
	if bare := got["3rdparty/python"]; bare.DependencySources != nil {
		t.Errorf("a bare directory answers provenance: %v", bare.DependencySources)
	}

	// With the export, its edges and the sync's are one set.
	root = repository(t, d, true)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	result, err = s.Probe(root, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range result.Projects {
		if p.Path == "src/python/api" && !reflect.DeepEqual(p.Dependencies, []string{"3rdparty/python", "src/python/pricing"}) {
			t.Errorf("api dependencies with the export = %v", p.Dependencies)
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

func TestTheSyncWritesEachPackagePutnamiJSONFromTheExport(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-package-graph", "the-sync-writes-each-package-putnami-json-from-the-export")
	d := readDemo(t)
	t.Setenv("PATH", t.TempDir())

	// Without the export and without pants, nothing is written.
	root := repository(t, d, false)
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || result.Export != "" || len(result.Written) != 0 {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "src/python/api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("no putnami.json is written without an export: %v", err)
	}
	if !strings.Contains(syncSummary(result), "pants peek :: > pants-graph.json") {
		t.Errorf("the summary names the export command: %s", syncSummary(result))
	}

	// With the export: each package gets the extension and its edges.
	root = repository(t, d, true)
	writeFile(t, root, "src/python/web/putnami.json", `{"name": "storefront", "dependencies": ["/src/python/api", "@acme/design"], "disable": {"jobs": ["test", "publish"]}}`+"\n")
	result, err = Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/python/api/putnami.json", "src/python/pricing/putnami.json", "src/python/web/putnami.json"}; result.Export != "pants-graph.json" || !reflect.DeepEqual(result.Written, want) {
		t.Errorf("result %+v, want %v written from the export", result, want)
	}
	api := readProjectFile(t, root, "src/python/api")
	if !reflect.DeepEqual(api, map[string]any{"$schema": ProjectSchema, "extensions": []any{ExtensionName}, "dependencies": []any{"/3rdparty/python", "/src/python/pricing"}, "options": map[string]any{ExtensionName: map[string]any{"filePatterns": []any{"../../../3rdparty/python/default.lock", "../../../3rdparty/python/requirements.txt"}}}}) {
		t.Errorf("api = %v", api)
	}
	// An existing file keeps what the sync does not own; a stale Pants edge
	// and a disabled test of a tested package go.
	web := readProjectFile(t, root, "src/python/web")
	want := map[string]any{"name": "storefront", "extensions": []any{ExtensionName}, "dependencies": []any{"/3rdparty/python", "@acme/design"}, "disable": map[string]any{"jobs": []any{"publish"}}, "options": map[string]any{ExtensionName: map[string]any{"filePatterns": []any{"../../../3rdparty/python/default.lock", "../../../3rdparty/python/requirements.txt"}}}}
	if !reflect.DeepEqual(web, want) {
		t.Errorf("web = %v, want %v", web, want)
	}

	// A second sync changes nothing.
	again, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || len(again.Written) != 0 || again.Unchanged != 3 {
		t.Errorf("a second sync = %+v, %v", again, err)
	}

	// A package with no test target has its test disabled.
	var targets []map[string]any
	if err := json.Unmarshal([]byte(d["ci"]["pants-graph.json"]), &targets); err != nil {
		t.Fatal(err)
	}
	targets = slices.DeleteFunc(targets, func(target map[string]any) bool {
		return strings.HasPrefix(target["address"].(string), "src/python/web") && strings.HasSuffix(target["target_type"].(string), "test")
	})
	targets = slices.DeleteFunc(targets, func(target map[string]any) bool {
		return strings.HasPrefix(target["address"].(string), "src/python/web") && strings.HasSuffix(target["target_type"].(string), "tests")
	})
	export, _ := json.Marshal(targets)
	writeFile(t, root, "pants-graph.json", string(export))
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if jobs := readProjectFile(t, root, "src/python/web")["disable"]; !reflect.DeepEqual(jobs, map[string]any{"jobs": []any{"publish", "test"}}) {
		t.Errorf("an untested package's disable = %v", jobs)
	}
}

func TestTheSyncRunsPantsPeekWithoutAnExport(t *testing.T) {
	d := readDemo(t)
	stubPants(t, d)
	root := repository(t, d, false)
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || result.Export != "pants peek ::" || len(result.Written) != 3 {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if deps := readProjectFile(t, root, "src/python/api")["dependencies"]; !reflect.DeepEqual(deps, []any{"/3rdparty/python", "/src/python/pricing"}) {
		t.Errorf("api dependencies = %v", deps)
	}
	if log := stubLog(t); len(log) != 1 || !strings.HasSuffix(log[0], " pants peek ::") {
		t.Errorf("pants calls = %v", log)
	}

	// A package that uses third-party targets is selected by what declares
	// them, wherever those files lie.
	inputs := readProjectFile(t, root, "src/python/api")["options"].(map[string]any)[ExtensionName].(map[string]any)["filePatterns"]
	if want := []any{"../../../3rdparty/python/default.lock", "../../../3rdparty/python/requirements.txt"}; !reflect.DeepEqual(inputs, want) {
		t.Errorf("api filePatterns = %v, want %v", inputs, want)
	}

	// A check compares without writing, and names the file that drifted.
	spectest.Proves(t, feature, "validate-fails-on-a-stale-graph", "a-stale-package-graph-fails-validate")
	if stale, err := Stale(context.Background(), root, &bytes.Buffer{}); err != nil || len(stale.Written) != 0 || len(stale.Retracted) != 0 {
		t.Errorf("a current graph is not stale: %+v %v", stale, err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "src/python/web/putnami.json"))
	writeFile(t, root, "src/python/web/putnami.json", `{"extensions": ["@putnami/pants"]}`)
	stale, err := Stale(context.Background(), root, &bytes.Buffer{})
	if err != nil || !reflect.DeepEqual(stale.Written, []string{"src/python/web/putnami.json"}) {
		t.Errorf("a drifted package is stale: %+v %v", stale, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "src/python/web/putnami.json")); string(after) != `{"extensions": ["@putnami/pants"]}` {
		t.Errorf("the check wrote %s", after)
	}
	writeFile(t, root, "src/python/web/putnami.json", string(before))

	// An export that lists no package retracts nothing.
	writeFile(t, root, "pants-graph.json", "[]")
	empty, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil || !empty.EmptyExport || len(empty.Retracted) != 0 {
		t.Errorf("an empty export writes nothing: %+v %v", empty, err)
	}
	if ext := readProjectFile(t, root, "src/python/api")["extensions"]; !reflect.DeepEqual(ext, []any{ExtensionName}) {
		t.Errorf("api extensions after an empty export = %v", ext)
	}

	// An export that is no export fails the sync and writes nothing.
	root = repository(t, d, false)
	writeFile(t, root, "pants-graph.json", "not json")
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("an unreadable export fails the sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src/python/api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a failed sync writes nothing: %v", err)
	}

	// A repository without pants.toml is no Pants repository.
	if result, err := Sync(context.Background(), t.TempDir(), &bytes.Buffer{}); err != nil || result.Export != "" {
		t.Errorf("result %+v, err %v", result, err)
	}
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

func TestATaskRunsPantsOnItsPackageAndRecordsTheDelegation(t *testing.T) {
	spectest.Proves(t, feature, "tasks-delegate-to-pants", "a-task-runs-pants-on-its-package-and-records-the-delegation")
	d := readDemo(t)
	stubPants(t, d)
	root := repository(t, d, true)
	rootReal, _ := filepath.EvalSymlinks(root)

	for task, want := range map[string]string{
		stacks.TaskLint:  "pants lint src/python/pricing:",
		stacks.TaskBuild: "pants package src/python/pricing:",
		stacks.TaskTest:  "pants --test-report --test-use-coverage --coverage-py-report=xml test src/python/pricing:",
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		status, data, events := runJob(t, root, "src/python/pricing", task)
		if status != "OK" || data["delegatedTo"] != "pants" {
			t.Errorf("%s: status %s, data %v, events %s", task, status, data, events)
		}
		log := stubLog(t)
		if len(log) != 2 || !strings.HasSuffix(log[0], " pants --version") || log[1] != rootReal+" "+want {
			t.Errorf("%s ran %q, want the version check then %q in %s", task, log, want, rootReal)
		}
	}

	t.Setenv("STUB_VERSION", "2.24.1")
	if status, _, events := runJob(t, root, "src/python/pricing", stacks.TaskLint); status != "FAILED" || !strings.Contains(events, stacks.CodeToolchainMismatch) {
		t.Errorf("a Pants version other than pants.toml's fails the task: %s %s", status, events)
	}
	t.Setenv("STUB_VERSION", "")
	t.Setenv("STUB_TEST_EXIT", "1")
	if status, _, events := runJob(t, root, "src/python/pricing", stacks.TaskTest); status != "FAILED" || !strings.Contains(events, stacks.CodeStepFailed) {
		t.Errorf("a failing pants test fails the task: %s %s", status, events)
	}
}

func TestThePantsReportsBecomeTheTestAndCoverageSummary(t *testing.T) {
	spectest.Proves(t, feature, "reports-become-the-summary", "the-pants-junit-and-coverage-become-the-test-and-coverage-summary")
	d := readDemo(t)
	stubPants(t, d)
	root := repository(t, d, true)
	// A report another package's run left under dist/ is never read as this
	// one's.
	writeFile(t, root, "dist/test/reports/src.python.web.pages_test.py.tests.xml", d["ci"]["dist/test/reports/src.python.web.pages_test.py.tests.xml"])

	status, data, events := runJob(t, root, "src/python/pricing", stacks.TaskTest)
	if status != "OK" {
		t.Fatalf("status %s: %s", status, events)
	}
	if got := data["testSummary"].(map[string]any); got["total"] != 4 || got["passed"] != 4 {
		t.Errorf("testSummary = %v, want pricing's 4 cases alone", got)
	}
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 17 || got["total"] != 17 {
		t.Errorf("coverageSummary = %v, want pricing's 17 lines alone", got)
	}

	// The override a repository without the Python backend states reads no
	// coverage report, and the file patterns the sync writes beside it are
	// no member of the profile.
	writeFile(t, root, "src/python/pricing/putnami.json", `{"options": {"@putnami/pants": {"filePatterns": ["../../../python-default.lock"], "tasks": {"test": {"steps": [["pants", "--test-report", "test", "{component}:"]], "reports": {"junit": ["{root}/dist/test/reports/*.xml"], "coverage": []}}}}}}`)
	status, data, events = runJob(t, root, "src/python/pricing", stacks.TaskTest)
	if _, covered := data["coverageSummary"]; status != "OK" || covered {
		t.Errorf("the documented override: %s %v %s", status, data, events)
	}

	// A test run that writes no report fails.
	writeFile(t, root, "src/python/pricing/putnami.json", `{"options": {"@putnami/pants": {"tasks": {"test": {"steps": [["pants", "lint", "{component}:"]]}}}}}`)
	if status, _, events := runJob(t, root, "src/python/pricing", stacks.TaskTest); status != "FAILED" || !strings.Contains(events, "wrote no JUnit report") {
		t.Errorf("a test that writes no report fails: %s %s", status, events)
	}
}

func TestTheManifestConformsToTheStackCore(t *testing.T) {
	spectest.Proves(t, feature, "manifest-follows-the-profile", "the-manifest-conforms-to-the-stack-core")
	s := stack(t)
	if !s.Profile.Delegated {
		t.Fatal("the Pants profile is delegated")
	}
	stackstest.CheckManifest(t, "putnami.extension.json", s)
}

func TestTheSyncRetractsAFormerPackage(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-package-graph", "the-sync-writes-each-package-putnami-json-from-the-export")
	d := readDemo(t)
	t.Setenv("PATH", t.TempDir())
	root := repository(t, d, true)
	// A former package keeps a putnami.json that names the extension; api
	// still depends on it and on a package whose directory is gone.
	writeFile(t, root, "src/python/legacy/putnami.json", `{"name": "legacy", "extensions": ["@putnami/pants", "@acme/lint"], "dependencies": ["/src/python/pricing", "@acme/design"]}`+"\n")
	writeFile(t, root, "src/python/api/putnami.json", `{"extensions": ["@putnami/pants"], "dependencies": ["/src/python/legacy", "/src/python/gone", "@acme/design"]}`+"\n")
	result, err := Sync(context.Background(), root, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/python/legacy/putnami.json"}; !reflect.DeepEqual(result.Retracted, want) {
		t.Errorf("retracted = %v, want %v", result.Retracted, want)
	}
	if legacy := readProjectFile(t, root, "src/python/legacy"); !reflect.DeepEqual(legacy, map[string]any{"name": "legacy", "extensions": []any{"@acme/lint"}, "dependencies": []any{"@acme/design"}}) {
		t.Errorf("legacy = %v, want the extension and its Pants edges gone", legacy)
	}
	if deps := readProjectFile(t, root, "src/python/api")["dependencies"]; !reflect.DeepEqual(deps, []any{"/3rdparty/python", "/src/python/pricing", "@acme/design"}) {
		t.Errorf("api dependencies = %v, want the export's edge and the non-Pants one", deps)
	}
	if !strings.Contains(syncSummary(result), "1 former packages retracted") {
		t.Errorf("summary = %s", syncSummary(result))
	}
}

func TestTheSyncLeavesAFileWhoseOwnedMembersAreCurrent(t *testing.T) {
	d := readDemo(t)
	t.Setenv("PATH", t.TempDir())
	root := repository(t, d, true)
	handWritten := "{\n    \"name\": \"web\",\n    \"extensions\": [\"@putnami/pants\"],\n    \"dependencies\": [\"/3rdparty/python\"],\n    \"options\": {\"ratio\": 1.0, \"big\": 12345678901234567890, \"@putnami/pants\": {\"filePatterns\": [\"../../../3rdparty/python/default.lock\", \"../../../3rdparty/python/requirements.txt\"]}}\n}\n"
	writeFile(t, root, "src/python/web/putnami.json", handWritten)
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "src/python/web/putnami.json")); string(data) != handWritten {
		t.Errorf("a file whose owned members are current is kept byte for byte:\n%s", data)
	}
	// A change to an owned member rewrites it, numbers kept.
	writeFile(t, root, "src/python/web/putnami.json", strings.Replace(handWritten, `["@putnami/pants"]`, `[]`, 1))
	if _, err := Sync(context.Background(), root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "src/python/web/putnami.json")); !strings.Contains(string(data), `"big": 12345678901234567890`) || !strings.Contains(string(data), `"ratio": 1.0`) {
		t.Errorf("a rewrite keeps the numbers as written:\n%s", data)
	}
}

func TestPeekWaitsForTheTasksOfTheStackRoot(t *testing.T) {
	d := readDemo(t)
	stubPants(t, d)
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
		t.Fatalf("peek ran beside a task that holds the lock: %v", err)
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
		t.Fatal("peek never ran after the lock was released")
	}
}

func TestTheSyncJobWritesNothingInADryRun(t *testing.T) {
	spectest.Proves(t, feature, "sync-writes-the-package-graph", "the-sync-writes-each-package-putnami-json-from-the-export")
	d := readDemo(t)
	t.Setenv("PATH", t.TempDir())
	root := repository(t, d, true)
	var events strings.Builder
	emit := jsonl.New()
	emit.Observe(func(message string) { events.WriteString(message + "\n") })
	ctx := &pctx.Context{WorkspaceRoot: root, Params: pctx.Params{"dryRun": json.RawMessage(`true`)}}
	status, data, err := RunSync(ctx, emit, nil)
	if err != nil || status != "OK" || data["dryRun"] != true {
		t.Fatalf("status %s, data %v, err %v", status, data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "src/python/api/putnami.json")); !os.IsNotExist(err) {
		t.Errorf("a dry run writes no putnami.json: %v", err)
	}
	if !strings.Contains(events.String(), "src/python/api/putnami.json would change") {
		t.Errorf("a dry run names what would change: %s", events.String())
	}
}
