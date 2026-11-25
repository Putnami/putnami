package bazel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// batchResult is what the test reads of one project's result in a batch.
type batchResult struct {
	ProjectID   string         `json:"projectId"`
	Status      string         `json:"status"`
	Data        map[string]any `json:"data"`
	Diagnostics []struct {
		Category    string `json:"category"`
		Description string `json:"description"`
	} `json:"diagnostics"`
}

// runBatchJob runs a task job of the stack as the leader of a batch over the
// packages at dirs, the way the CLI does, and returns each package's result
// by its directory.
func runBatchJob(t *testing.T, root, task string, dirs ...string) map[string]batchResult {
	t.Helper()
	s := stack(t)
	s.Runtime = testRuntime
	probed := map[string]json.RawMessage{}
	for _, p := range probe(t, root).Projects {
		probed[p.Path] = p.Metadata
	}
	var selected []pctx.ProjectRef
	for _, dir := range dirs {
		selected = append(selected, pctx.ProjectRef{ID: "/" + dir, Name: dir, Path: dir, FullPath: filepath.Join(root, filepath.FromSlash(dir)), OutputPath: filepath.Join(root, ".putnami/out", dir)})
	}
	ctx := &pctx.Context{WorkspaceRoot: root, SelectedProjects: selected}
	ctx.Project.Path = dirs[0]
	ctx.Project.FullPath = selected[0].FullPath
	ctx.Project.Metadata = map[string]json.RawMessage{ExtensionName: probed[dirs[0]]}
	status, data, err := s.Jobs()[task](ctx, jsonl.New(), []string{"--report-dir", selected[0].OutputPath})
	if status != "OK" || err != nil {
		t.Fatalf("batch status %s: %v", status, err)
	}
	raw, _ := json.Marshal(data["batchResults"])
	var results []batchResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("batch results %s: %v", raw, err)
	}
	out := map[string]batchResult{}
	for _, r := range results {
		out[strings.TrimPrefix(r.ProjectID, "/")] = r
	}
	if len(out) != len(dirs) {
		t.Fatalf("batch results = %s, want one per package", raw)
	}
	return out
}

// event is one line of a build-event file: the id of its kind about label,
// and its payload.
func event(kind, label string, payload map[string]any) string {
	e := map[string]any{"id": map[string]any{kind: map[string]any{"label": label}}}
	for k, v := range payload {
		e[k] = v
	}
	data, _ := json.Marshal(e)
	return string(data)
}

// lastMessage is the last event Bazel writes in a file it finished.
const lastMessage = `{"id":{"progress":{}},"lastMessage":true}`

// configuredTest is the event of a test target's configuration.
func configuredTest(label string) string {
	return event("targetConfigured", label, map[string]any{"configured": map[string]any{"targetKind": "sh_test rule", "testSize": "MEDIUM"}})
}

func completed(label string, success bool) string {
	return event("targetCompleted", label, map[string]any{"completed": map[string]any{"success": success}})
}

func summary(label, status string, failedLog ...string) string {
	s := map[string]any{"overallStatus": status}
	if len(failedLog) > 0 {
		s["failed"] = []map[string]string{{"uri": "file://" + filepath.ToSlash(failedLog[0])}}
	}
	return event("testSummary", label, map[string]any{"testSummary": s})
}

func TestAVerbRunsOneBazelTestOverItsPackages(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-bazel-command", "a-verb-runs-one-bazel-test-over-its-packages")
	d := readDemo(t)
	stubBazel(t, d)
	setRuntime(t)
	root := repository(t, d, true)
	rootReal, _ := filepath.EvalSymlinks(root)
	for task, want := range map[string][2]string{
		stacks.TaskTest:  {"bazel test --instrumentation_filter=^// --build_event_json_file=", "/build-events.json //api:all //pricing:all"},
		stacks.TaskBuild: {"bazel build --instrumentation_filter=^// --build_event_json_file=", "/build-events.json //api:all //pricing:all"},
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		results := runBatchJob(t, root, task, "api", "pricing")
		log := stubLog(t)
		if len(log) != 2 || !strings.HasSuffix(log[0], " bazel --version") || !strings.HasPrefix(log[1], rootReal+" "+want[0]) || !strings.HasSuffix(log[1], want[1]) {
			t.Errorf("%s ran %q, want the version check then %s<batch>%s", task, log, want[0], want[1])
		}
		for dir, r := range results {
			if r.Status != "OK" || r.Data["delegatedTo"] != "bazel" {
				t.Errorf("%s %s = %+v, want a pass delegated to bazel", task, dir, r)
			}
		}
		if task != stacks.TaskTest {
			continue
		}
		// Each package collects the reports of its own test runs from the
		// one build-event file.
		for dir, want := range map[string]float64{"api": 2, "pricing": 3} {
			if got := results[dir].Data["testSummary"].(map[string]any); got["total"] != want {
				t.Errorf("%s testSummary = %v, want its own %v cases", dir, got, want)
			}
		}
	}
}

func TestAFailingTestFailsItsPackageAlone(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-bazel-command", "a-failing-test-fails-its-package-alone")
	d := readDemo(t)
	stubBazel(t, d)
	setRuntime(t)
	root := repository(t, d, true)
	testLog := filepath.Join(t.TempDir(), "test.log")
	stackstest.WriteFile(t, filepath.Dir(testLog), "test.log", "--- FAIL: TestTotal (0.00s)\n    pricing_test.go:9: got 3, want 4\nFAIL\n")
	t.Setenv("STUB_TEST_EXIT", "3")
	t.Setenv("STUB_EVENTS", strings.Join([]string{
		completed("//api:api_test", true), summary("//api:api_test", "PASSED"),
		completed("//pricing:pricing_test", true), summary("//pricing:pricing_test", "FAILED", testLog), lastMessage,
	}, "\n"))
	results := runBatchJob(t, root, stacks.TaskTest, "api", "pricing")
	pricing := results["pricing"]
	if pricing.Status != "FAILED" || len(pricing.Diagnostics) == 0 || pricing.Diagnostics[0].Category != stacks.CodeStepFailed ||
		!strings.HasPrefix(pricing.Diagnostics[0].Description, "bazel test //pricing:pricing_test FAILED\n") || !strings.Contains(pricing.Diagnostics[0].Description, "got 3, want 4") {
		t.Errorf("pricing = %+v, want a failure with Bazel's status and the end of the test's log", pricing)
	}
	if api := results["api"]; api.Status != "OK" || len(api.Diagnostics) != 0 {
		t.Errorf("api = %+v, want the pass Bazel reported", api)
	}
}

func TestAPackageBazelDidNotRunFailsAsNotRun(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-bazel-command", "a-package-bazel-did-not-run-fails-as-not-run")
	d := readDemo(t)
	stubBazel(t, d)
	setRuntime(t)
	root := repository(t, d, true)
	// Bazel stopped at api's failed build, before pricing's targets.
	t.Setenv("STUB_BUILD_EXIT", "1")
	t.Setenv("STUB_EVENTS", event("targetCompleted", "//api:api", map[string]any{"completed": map[string]any{"failureDetail": map[string]any{"message": "compile api/api.go failed"}}})+"\n"+lastMessage)
	results := runBatchJob(t, root, stacks.TaskBuild, "api", "pricing")
	if api := results["api"]; api.Status != "FAILED" || len(api.Diagnostics) != 1 || api.Diagnostics[0].Description != "bazel build failed to build //api:api: compile api/api.go failed" {
		t.Errorf("api = %+v, want its build failure", api)
	}
	if pricing := results["pricing"]; pricing.Status != "FAILED" || len(pricing.Diagnostics) != 1 || !strings.Contains(pricing.Diagnostics[0].Description, "did not run pricing: it failed on api") {
		t.Errorf("pricing = %+v, want a failure as not run", pricing)
	}
}

func TestReadBatchReadsEachPackageFromTheBuildEventFile(t *testing.T) {
	aborted := func(kind, label string) string {
		return event(kind, label, map[string]any{"aborted": map[string]any{"reason": "SKIPPED", "description": "a dependency failed"}})
	}
	for _, tc := range []struct {
		name   string
		events []string
		want   string
	}{
		{"built", []string{completed("//a:lib", true)}, "passed"},
		{"tested", []string{completed("//a:t", true), summary("//a:t", "PASSED")}, "passed"},
		{"flaky", []string{completed("//a:t", true), summary("//a:t", "FLAKY")}, "passed"},
		{"failed", []string{completed("//a:t", true), summary("//a:t", "FAILED")}, "failed"},
		{"timed out", []string{completed("//a:t", true), summary("//a:t", "TIMEOUT")}, "failed"},
		{"no status", []string{completed("//a:t", true), summary("//a:t", "NO_STATUS")}, "failed"},
		{"incomplete", []string{completed("//a:t", true), summary("//a:t", "INCOMPLETE")}, "failed"},
		{"failed to build a test", []string{summary("//a:t", "FAILED_TO_BUILD")}, "failed"},
		{"failed to build", []string{completed("//a:lib", false)}, "failed"},
		{"one target of several failed", []string{completed("//a:lib", true), completed("//a:bin", false)}, "failed"},
		{"aborted", []string{aborted("targetCompleted", "//a:lib")}, "failed"},
		{"aborted when configured", []string{aborted("configuredLabel", "//a:lib")}, "failed"},
		{"aborted before configured", []string{aborted("unconfiguredLabel", "//a:lib")}, "failed"},
		{"aborted after another passed", []string{completed("//a:lib", true), aborted("targetCompleted", "//a:bin")}, "failed"},
		{"not run", []string{completed("//b:lib", false)}, "not named"},
		{"tested and never completed", []string{summary("//a:t", "PASSED"), completed("//b:lib", false)}, "not named"},
		{"a subpackage", []string{completed("//a/sub:lib", true), completed("//b:lib", false)}, "not named"},
		{"another repository", []string{completed("@other//a:lib", true), completed("//b:lib", false)}, "not named"},
		{"the main repository by name", []string{completed("@@//a:lib", true), completed("//b:lib", false)}, "passed"},
		{"a test built and never run", []string{configuredTest("//a:t"), completed("//a:t", true), completed("//b:lib", false)}, "not named"},
		{"a test run after another was built", []string{configuredTest("//a:t"), configuredTest("//a:u"), completed("//a:t", true), completed("//a:u", true), summary("//a:t", "PASSED"), completed("//b:lib", false)}, "not named"},
		{"every test run", []string{configuredTest("//a:t"), completed("//a:t", true), summary("//a:t", "PASSED"), completed("//b:lib", false)}, "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stackstest.WriteFile(t, dir, BatchEvents, `{"id":{"started":{}},"started":{"command":"test"}}`+"\n"+strings.Join(tc.events, "\n")+"\n"+lastMessage+"\n")
			out := ReadBatch(stacks.BatchReport{Task: stacks.TaskTest, Components: []string{"a", "b"}, Failed: true, Command: []string{"bazel", "test"}, Dir: dir})
			o, named := out["a"]
			got := "not named"
			switch {
			case named && o.Failed:
				got = "failed"
			case named:
				got = "passed"
			}
			if got != tc.want {
				t.Errorf("a is %s, want %s: %+v", got, tc.want, out)
			}
		})
	}
}

func TestReadBatchNamesNoPackageItCannotTellFailed(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) {
		stackstest.WriteFile(t, dir, BatchEvents, strings.Join(lines, "\n")+"\n")
	}
	report := stacks.BatchReport{Task: stacks.TaskTest, Components: []string{"a", "b"}, Failed: true, Command: []string{"bazel", "test"}, Dir: dir}
	// A test of another package, as one a suite of a runs, failed.
	write(completed("//a:suite", true), completed("//z:t", true), summary("//z:t", "FAILED"), lastMessage)
	if out := ReadBatch(report); out != nil {
		t.Errorf("a failure of no package of the command answered %+v, want nothing", out)
	}
	// A file Bazel did not finish names nothing.
	write(completed("//a:t", true), summary("//a:t", "PASSED"), completed("//b:t", true), summary("//b:t", "FAILED"))
	if out := ReadBatch(report); out != nil {
		t.Errorf("a file Bazel did not finish answered %+v, want nothing", out)
	}
	// A built test is passed only by the build command.
	write(configuredTest("//a:t"), completed("//a:t", true), completed("//b:lib", false), lastMessage)
	report.Command = []string{"bazel", "build"}
	if o, named := ReadBatch(report)["a"]; !named || o.Failed {
		t.Errorf("a built test = %+v %v, want a pass of the build", o, named)
	}
	report.Command = []string{"bazel", "test"}
	// A passing command, a missing file and a malformed one answer nothing.
	write(completed("//a:t", false))
	report.Failed = false
	if out := ReadBatch(report); out != nil {
		t.Errorf("a passing command answered %+v", out)
	}
	report.Failed = true
	write("not json")
	if out := ReadBatch(report); out != nil {
		t.Errorf("a malformed file answered %+v", out)
	}
	report.Dir = filepath.Join(dir, "none")
	if out := ReadBatch(report); out != nil {
		t.Errorf("a missing file answered %+v", out)
	}
}

func TestCollectReportsKeepsTheRunsOfItsPackage(t *testing.T) {
	root, into := t.TempDir(), t.TempDir()
	for _, run := range []string{"api/api_test", "api/sub/t", "pricing/pricing_test"} {
		stackstest.WriteFile(t, root, "bazel-testlogs/"+run+"/test.xml", "<testsuites/>")
	}
	events := filepath.Join(t.TempDir(), BatchEvents)
	stackstest.WriteFile(t, filepath.Dir(events), BatchEvents, fmt.Sprintf("%s\n%s\n%s\n",
		`{"id":{"testResult":{"label":"//api:api_test"}}}`, `{"id":{"testResult":{"label":"//api/sub:t"}}}`, `{"id":{"testResult":{"label":"//pricing:pricing_test"}}}`))
	if err := CollectReports([]string{"--events", events, "--root", root, "--package", "api", "--runs-of-package", "--into", into}, nil, nil); err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]bool{"api/api_test": true, "api/sub/t": false, "pricing/pricing_test": false} {
		if _, err := os.Stat(filepath.Join(into, run, "test.xml")); (err == nil) != want {
			t.Errorf("%s collected: %v, want %v", run, err == nil, want)
		}
	}
	if err := CollectReports([]string{"--events", events, "--root", root, "--runs-of-package", "--into", into}, nil, nil); err == nil {
		t.Error("--runs-of-package without --package collects")
	}
}

func TestReadBatchReadsWhatBazelWrites(t *testing.T) {
	// The events Bazel 7.4.1 wrote for bazel test, then bazel build, over
	// //a:all //b:all //c:all, where b's test fails, then c's genrule.
	for file, want := range map[string]map[string]bool{
		"testdata/failed-test.json":  {"a": false, "b": true, "c": false},
		"testdata/failed-build.json": {"a": false, "b": false, "c": true},
	} {
		verb := "test"
		if strings.Contains(file, "build") {
			verb = "build"
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		stackstest.WriteFile(t, dir, BatchEvents, string(data))
		out := ReadBatch(stacks.BatchReport{Components: []string{"a", "b", "c"}, Failed: true, Command: []string{"bazel", verb}, Dir: dir})
		for component, failed := range want {
			if o, named := out[component]; !named || o.Failed != failed {
				t.Errorf("%s: %s = %+v %v, want failed %v", file, component, o, named, failed)
			}
		}
	}
}
