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

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

const feature = "tooling/pants-delegated-stack"

// demoFile is the Pants demo of putnami observe: a Pants 2.25 repository
// where api imports pricing and web stands alone, with the export and the
// reports its CI wrote.
const demoFile = "../observe/internal/observe/testdata/demos/pants.txtar"

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
	want := map[string][]string{"src/python/api": {"src/python/pricing"}, "src/python/pricing": nil, "src/python/web": nil}
	if len(got) != len(want) {
		t.Fatalf("projects = %+v, want %v: third-party directories and the root are no project", result.Projects, want)
	}
	for path, deps := range want {
		p := got[path]
		if p.SourceName != path || p.SourceFile != path+"/BUILD" || !reflect.DeepEqual(p.Dependencies, deps) ||
			!reflect.DeepEqual(p.Extensions, []string{ExtensionName}) || !reflect.DeepEqual(p.Tags, []string{"pants"}) {
			t.Errorf("%s = %+v", path, p)
		}
		var meta stacks.Metadata
		if err := json.Unmarshal(p.Metadata, &meta); err != nil ||
			meta != (stacks.Metadata{Component: path, Root: ".", Toolchain: stacks.Expected{Version: "2.25.0", Source: "pants.toml"}}) {
			t.Errorf("%s metadata = %s (%v)", path, p.Metadata, err)
		}
	}

	// Without the export, the BUILD files name the packages, and the edges
	// come from each package's putnami.json.
	result = probe(t, repository(t, d, false))
	if len(result.Projects) != 3 || len(result.Diagnostics) != 0 || len(result.Projects[0].Dependencies) != 0 {
		t.Errorf("without the export: projects %+v, diagnostics %v", result.Projects, result.Diagnostics)
	}
}

func readProject(t *testing.T, root, dir string) map[string]any {
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
	api := readProject(t, root, "src/python/api")
	if !reflect.DeepEqual(api, map[string]any{"$schema": ProjectSchema, "extensions": []any{ExtensionName}, "dependencies": []any{"/src/python/pricing"}}) {
		t.Errorf("api = %v", api)
	}
	// An existing file keeps what the sync does not own; a stale Pants edge
	// and a disabled test of a tested package go.
	web := readProject(t, root, "src/python/web")
	want := map[string]any{"name": "storefront", "extensions": []any{ExtensionName}, "dependencies": []any{"@acme/design"}, "disable": map[string]any{"jobs": []any{"publish"}}}
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
	if jobs := readProject(t, root, "src/python/web")["disable"]; !reflect.DeepEqual(jobs, map[string]any{"jobs": []any{"publish", "test"}}) {
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
	if deps := readProject(t, root, "src/python/api")["dependencies"]; !reflect.DeepEqual(deps, []any{"/src/python/pricing"}) {
		t.Errorf("api dependencies = %v", deps)
	}
	if log := stubLog(t); len(log) != 1 || !strings.HasSuffix(log[0], " pants peek ::") {
		t.Errorf("pants calls = %v", log)
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
	if got := data["coverageSummary"].(map[string]any); got["covered"] != 33 || got["total"] != 33 {
		t.Errorf("coverageSummary = %v", got)
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
