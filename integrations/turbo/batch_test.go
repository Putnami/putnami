package turbo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
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

// stubBatchTurbo puts first on PATH a turbo that prints its version, logs
// each call with its working directory to $STUB_LOG, writes $STUB_SUMMARY
// as the run summary of a run --summarize, and exits with STUB_EXIT.
func stubBatchTurbo(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	writeFile(t, bin, "turbo", `#!/bin/sh
echo "$(pwd) turbo $*" >> "$STUB_LOG"
case "$1" in --version) echo 2.5.4; exit 0;; esac
case "$*" in *--summarize*)
  if [ -n "$STUB_SUMMARY" ]; then mkdir -p .turbo/runs; printf '%s' "$STUB_SUMMARY" > .turbo/runs/2run.json; fi;;
esac
exit "${STUB_EXIT:-0}"
`)
	if err := os.Chmod(filepath.Join(bin, "turbo"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
}

func TestAVerbRunsOneTurboRunOverItsPackages(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-turbo-command", "a-verb-runs-one-turbo-run-over-its-packages")
	stubBatchTurbo(t)
	root := repository(t, readDemo(t))
	rootReal, _ := filepath.EvalSymlinks(root)
	t.Setenv("STUB_SUMMARY", `{"execution":{"command":"turbo run lint --filter=@demo/api --filter=@demo/pricing"},"tasks":[]}`)
	results := runBatchJob(t, root, stacks.TaskLint, "apps/api", "packages/pricing")
	log := stubLog(t)
	want := rootReal + " turbo run lint --filter=@demo/api --filter=@demo/pricing --summarize"
	if len(log) != 2 || !strings.HasSuffix(log[0], " turbo --version") || log[1] != want {
		t.Errorf("the batch ran %q, want the version check then %q", log, want)
	}
	for dir, r := range results {
		if r.Status != "OK" || r.Data["delegatedTo"] != "turbo" {
			t.Errorf("%s = %+v, want a pass delegated to turbo", dir, r)
		}
	}
	// The summary the command wrote is removed, even when it passed.
	if entries, _ := os.ReadDir(filepath.Join(root, RunsDir)); len(entries) != 0 {
		t.Errorf("the run summaries %v remain", entries)
	}
}

func TestAFailingPackageFailsAloneWithTurbosError(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-turbo-command", "a-failing-package-fails-alone-with-turbos-error")
	stubBatchTurbo(t)
	root := repository(t, readDemo(t))
	writeFile(t, root, "apps/api/.turbo/turbo-lint.log", "apps/api/src/index.ts: 'x' is never used\n")
	t.Setenv("STUB_EXIT", "1")
	t.Setenv("STUB_SUMMARY", `{"execution":{"command":"turbo run lint --filter=@demo/api --filter=@demo/pricing"},"tasks":[`+
		`{"taskId":"@demo/api#lint","logFile":"apps/api/.turbo/turbo-lint.log","directory":"apps/api","execution":{"exitCode":1,"error":"command (apps/api) npm run lint exited (1)"}},`+
		`{"taskId":"@demo/pricing#lint","directory":"packages/pricing","execution":{"exitCode":0}}]}`)
	results := runBatchJob(t, root, stacks.TaskLint, "apps/api", "packages/pricing")
	api := results["apps/api"]
	if api.Status != "FAILED" || len(api.Diagnostics) != 1 || !strings.Contains(api.Diagnostics[0].Description, "npm run lint exited (1)") ||
		!strings.Contains(api.Diagnostics[0].Description, "'x' is never used") || api.Diagnostics[0].Category != stacks.CodeStepFailed {
		t.Errorf("api = %+v, want a failure with turbo's error and the end of its log", api)
	}
	if pricing := results["packages/pricing"]; pricing.Status != "OK" || len(pricing.Diagnostics) != 0 {
		t.Errorf("pricing = %+v, want the pass turbo reported", pricing)
	}
}

func TestAPackageTurboDidNotRunFailsAsNotRun(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-turbo-command", "a-package-turbo-did-not-run-fails-as-not-run")
	stubBatchTurbo(t)
	root := repository(t, readDemo(t))
	t.Setenv("STUB_EXIT", "1")
	// turbo stopped pricing's lint when api's failed: its summary lists the
	// task with no exit code.
	t.Setenv("STUB_SUMMARY", `{"execution":{"command":"turbo run lint --filter=@demo/api --filter=@demo/pricing"},"tasks":[`+
		`{"taskId":"@demo/api#lint","execution":{"exitCode":1,"error":"exited (1)"}},`+
		`{"taskId":"@demo/pricing#lint","execution":{"startTime":1}}]}`)
	results := runBatchJob(t, root, stacks.TaskLint, "apps/api", "packages/pricing")
	if pricing := results["packages/pricing"]; pricing.Status != "FAILED" || len(pricing.Diagnostics) != 1 || !strings.Contains(pricing.Diagnostics[0].Description, "did not run @demo/pricing") {
		t.Errorf("pricing = %+v, want a failure as not run", pricing)
	}
}

// summaryTask is one task of a run summary a test writes.
func summaryTask(id string, exitCode any, turboErr string) map[string]any {
	execution := map[string]any{"startTime": 1}
	if exitCode != nil {
		execution["exitCode"] = exitCode
	}
	if turboErr != "" {
		execution["error"] = turboErr
	}
	return map[string]any{"taskId": id, "execution": execution}
}

func TestReadBatchReadsEachPackageFromTheRunSummary(t *testing.T) {
	command := []string{"turbo", "run", "test", "--filter=a", "--filter=b", "--summarize"}
	for _, tc := range []struct {
		name  string
		tasks []map[string]any
		want  string
	}{
		{"passed", []map[string]any{summaryTask("a#test", 0, "")}, "passed"},
		{"failed", []map[string]any{summaryTask("a#test", 1, "exited (1)")}, "failed"},
		{"an error with no exit code", []map[string]any{summaryTask("a#test", nil, "spawn failed")}, "failed"},
		{"not run", []map[string]any{summaryTask("b#test", 1, "exited (1)")}, "not named"},
		{"stopped", []map[string]any{summaryTask("a#test", nil, "")}, "not named"},
		{"no execution", []map[string]any{{"taskId": "a#test"}}, "not named"},
		{"only an upstream task", []map[string]any{summaryTask("a#build", 0, "")}, "not named"},
		{"another package of the same name prefix", []map[string]any{summaryTask("ab#test", 0, "")}, "not named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			data, _ := json.Marshal(map[string]any{"execution": map[string]any{"command": "turbo run test --filter=a --filter=b"}, "tasks": tc.tasks})
			writeFile(t, root, RunsDir+"/run.json", string(data))
			out := ReadBatch(stacks.BatchReport{Task: stacks.TaskTest, Root: root, Components: []string{"a", "b"}, Failed: true, Command: command, Started: time.Now().Add(-time.Minute)})
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

func TestReadBatchReadsOnlyTheSummaryOfItsCommandAndRemovesIt(t *testing.T) {
	root := t.TempDir()
	old, _ := json.Marshal(map[string]any{"execution": map[string]any{"command": "turbo run test --filter=a"}, "tasks": []map[string]any{summaryTask("a#test", 0, "")}})
	writeFile(t, root, RunsDir+"/old.json", string(old))
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, RunsDir, "old.json"), past, past); err != nil {
		t.Fatal(err)
	}
	report := stacks.BatchReport{Task: stacks.TaskTest, Root: root, Components: []string{"a"}, Failed: true, Command: []string{"turbo", "run", "test", "--filter=a"}, Started: time.Now()}
	// A summary of an earlier run passes nothing.
	if out := ReadBatch(report); out != nil {
		t.Errorf("an earlier summary was read: %+v", out)
	}
	now, _ := json.Marshal(map[string]any{"execution": map[string]any{"command": "turbo run test --filter=a"}, "tasks": []map[string]any{summaryTask("a#test", 0, "")}})
	writeFile(t, root, RunsDir+"/now.json", string(now))
	report.Failed = false
	if out := ReadBatch(report); out != nil {
		t.Errorf("a passing command's summary answered %+v, want nothing", out)
	}
	if _, err := os.Stat(filepath.Join(root, RunsDir, "now.json")); !os.IsNotExist(err) {
		t.Errorf("the command's summary remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RunsDir, "old.json")); err != nil {
		t.Errorf("an earlier summary was removed: %v", err)
	}
}

func TestReadBatchNamesEveryTaskOfTheCommand(t *testing.T) {
	root := t.TempDir()
	data, _ := json.Marshal(map[string]any{"execution": map[string]any{"command": "turbo run lint test --filter=a --filter=b"}, "tasks": []map[string]any{summaryTask("a#lint", 0, ""), summaryTask("a#test", 0, ""), summaryTask("b#lint", 0, "")}})
	writeFile(t, root, RunsDir+"/run.json", string(data))
	out := ReadBatch(stacks.BatchReport{Root: root, Components: []string{"a", "b"}, Failed: true, Command: []string{"turbo", "run", "lint", "test", "--filter=a", "--filter=b"}, Started: time.Now().Add(-time.Minute)})
	if o, ok := out["a"]; !ok || o.Failed {
		t.Errorf("a = %+v %v, want a pass: both its tasks passed", o, ok)
	}
	if _, ok := out["b"]; ok {
		t.Errorf("b = %+v, want it not named: its test did not run", out["b"])
	}
}

func TestReadBatchReadsWhatTurboWrites(t *testing.T) {
	// The summary turbo 2.11.13 wrote for turbo run test --filter=a
	// --filter=b --filter=c, where b's test fails, turbo stops a's, and c's
	// waits for b's.
	data, err := os.ReadFile("testdata/failed-test-summary.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeFile(t, root, RunsDir+"/3MZgM04YawzzL6MdNefu9dk9tXz.json", string(data))
	out := ReadBatch(stacks.BatchReport{Task: stacks.TaskTest, Root: root, Components: []string{"a", "b", "c"}, Failed: true, Command: []string{"turbo", "run", "test", "--filter=a", "--filter=b", "--filter=c", "--summarize"}, Started: time.Now().Add(-time.Minute)})
	if b := out["b"]; !b.Failed || !strings.Contains(b.Message, "npm run test exited (1)") {
		t.Errorf("b = %+v, want its failure with turbo's error", b)
	}
	for _, c := range []string{"a", "c"} {
		if o, named := out[c]; named {
			t.Errorf("%s = %+v, want it not named: turbo did not finish it", c, o)
		}
	}
}

func TestReadBatchLeavesTheSummaryOfAnotherCommand(t *testing.T) {
	root := t.TempDir()
	other, _ := json.Marshal(map[string]any{"execution": map[string]any{"command": "turbo run test --filter=a --filter=z"}, "tasks": []map[string]any{summaryTask("a#test", 0, "")}})
	writeFile(t, root, RunsDir+"/other.json", string(other))
	own, _ := json.Marshal(map[string]any{"execution": map[string]any{"command": "turbo run test --filter=b --filter=a"}, "tasks": []map[string]any{summaryTask("a#test", 1, "exited (1)")}})
	writeFile(t, root, RunsDir+"/own.json", string(own))
	// The program as it ran, by its path, and --summarize, which turbo leaves
	// out of the summary, still name the command.
	report := stacks.BatchReport{Task: stacks.TaskTest, Root: root, Components: []string{"a", "b"}, Failed: true, Command: []string{"/repo/node_modules/.bin/turbo", "run", "test", "--filter=a", "--filter=b", "--summarize"}, Started: time.Now().Add(-time.Minute)}
	if a := ReadBatch(report)["a"]; !a.Failed {
		t.Errorf("a = %+v, want the failure its command's summary states", a)
	}
	if _, err := os.Stat(filepath.Join(root, RunsDir, "other.json")); err != nil {
		t.Errorf("the summary of another command was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RunsDir, "own.json")); !os.IsNotExist(err) {
		t.Errorf("the command's summary remains: %v", err)
	}
	// Only another command's summary: nothing is read.
	writeFile(t, root, RunsDir+"/other2.json", string(other))
	if out := ReadBatch(report); out != nil {
		t.Errorf("another command's summary answered %+v", out)
	}
}

func TestSameRunReadsTheTasksAndTheFiltersOfACommand(t *testing.T) {
	own := []string{"turbo", "run", "test", "--filter=a", "--filter=b", "--summarize"}
	for command, want := range map[string]bool{
		"turbo run test --filter=a --filter=b":                          true,
		"turbo run test --filter=b --filter=a":                          true,
		"npx turbo run test --filter a -F b --continue --concurrency=4": true,
		"pnpm turbo run test -Fa -F=b --summarize=true":                 true,
		"/repo/node_modules/.bin/turbo run test --filter=a --filter=b":  true,
		"turbo run test --filter=a":                                     false,
		"turbo run test --filter=a --filter=b --filter=c":               false,
		"turbo run lint --filter=a --filter=b":                          false,
		"turbo run lint test --filter=a --filter=b":                     false,
		"turbo test --filter=a --filter=b":                              false,
		"run test --filter=a --filter=b":                                false,
		"":                                                              false,
	} {
		if got := sameRun(strings.Fields(command), own); got != want {
			t.Errorf("sameRun(%q) = %v, want %v", command, got, want)
		}
	}
}

func TestReadBatchRemovesOnlyTheSummaryItRead(t *testing.T) {
	root := t.TempDir()
	run := func(code int) string {
		data, _ := json.Marshal(map[string]any{"execution": map[string]any{"command": "turbo run test --filter=a"}, "tasks": []map[string]any{summaryTask("a#test", code, "")}})
		return string(data)
	}
	writeFile(t, root, RunsDir+"/earlier.json", run(0))
	writeFile(t, root, RunsDir+"/newer.json", run(1))
	now := time.Now()
	if err := os.Chtimes(filepath.Join(root, RunsDir, "earlier.json"), now.Add(-time.Second), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	report := stacks.BatchReport{Task: stacks.TaskTest, Root: root, Components: []string{"a"}, Failed: true, Command: []string{"turbo", "run", "test", "--filter=a", "--summarize"}, Started: now.Add(-time.Minute)}
	if a := ReadBatch(report)["a"]; !a.Failed {
		t.Errorf("a = %+v, want the newest summary's failure", a)
	}
	if _, err := os.Stat(filepath.Join(root, RunsDir, "earlier.json")); err != nil {
		t.Errorf("a summary the reader did not read was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, RunsDir, "newer.json")); !os.IsNotExist(err) {
		t.Errorf("the summary read remains: %v", err)
	}
}
