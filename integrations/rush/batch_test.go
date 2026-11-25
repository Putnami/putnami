package rush

import (
	"encoding/json"
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
// projects at dirs, the way the CLI does, and returns each project's result
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
		t.Fatalf("batch results = %s, want one per project", raw)
	}
	return out
}

// stubBatchRush puts first on PATH a rush that prints its banner for --help,
// and otherwise logs its working directory and arguments to $STUB_LOG,
// prints $STUB_OUTPUT and exits with STUB_EXIT.
func stubBatchRush(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	stackstest.WriteFile(t, bin, "rush", `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "Rush Multi-Project Build Tool `+rushVersion+` - https://rushjs.io"
  exit 0
fi
echo "$(pwd -P) rush $*" >> "$STUB_LOG"
printf '%s\n' "$STUB_OUTPUT"
exit "${STUB_EXIT:-0}"
`)
	if err := os.Chmod(filepath.Join(bin, "rush"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
}

func TestAVerbRunsOneRushCommandOverItsProjects(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-rush-command", "a-verb-runs-one-rush-command-over-its-projects")
	stubBatchRush(t)
	root := repository(t, readDemo(t), ModeDelegate, true)
	rootReal, _ := filepath.EvalSymlinks(root)
	// A lint and a test take their projects alone, a build takes them with
	// the projects upstream of them.
	for task, want := range map[string]string{
		stacks.TaskLint:  "rush lint --only=@demo/api --only=@demo/pricing",
		stacks.TaskTest:  "rush test --only=@demo/api --only=@demo/pricing",
		stacks.TaskBuild: "rush build --to=@demo/api --to=@demo/pricing",
	} {
		t.Setenv("STUB_LOG", filepath.Join(t.TempDir(), "log"))
		results := runBatchJob(t, root, task, "apps/api", "libs/pricing")
		if log := stubLog(t); len(log) != 1 || log[0] != rootReal+" "+want {
			t.Errorf("%s ran %q, want %q", task, log, want)
		}
		for dir, r := range results {
			if r.Status != "OK" || r.Data["delegatedTo"] != "rush" {
				t.Errorf("%s %s = %+v, want a pass delegated to rush", task, dir, r)
			}
		}
	}
}

// failedBuild is the end of what rush build --to=@demo/api --to=@demo/pricing
// prints when @demo/api fails, in color.
const failedBuild = "\x1b[1m==[ @demo/pricing ]=================================================[ 1 of 2 ]==\x1b[22m\n" +
	"\"@demo/pricing\" completed successfully in 1.20 seconds.\n" +
	"==[ @demo/api ]=====================================================[ 2 of 2 ]==\n" +
	"src/index.ts(3,1): error TS2304: Cannot find name 'x'.\n" +
	"\n\n\n" +
	"\x1b[32m==[ SUCCESS: 1 operation ]=====================================================\x1b[39m\n\n" +
	"These operations completed successfully:\n" +
	"  @demo/pricing    1.20 seconds\n\n" +
	"\x1b[31m==[ FAILURE: 1 operation ]=====================================================\x1b[39m\n\n" +
	"\x1b[90m--[\x1b[39m \x1b[31mFAILURE: @demo/api\x1b[39m \x1b[90m]-------------------------------------[\x1b[39m 2.31 seconds \x1b[90m]--\x1b[39m\n\n" +
	"src/index.ts(3,1): error TS2304: Cannot find name 'x'.\n" +
	"Returned error code: 2\n\n" +
	"\x1b[31mOperations failed.\x1b[39m\n\n" +
	"rush build (3.60 seconds)\n"

func TestAFailingProjectFailsAloneWithRushsDetails(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-rush-command", "a-failing-project-fails-alone-with-rushs-details")
	stubBatchRush(t)
	root := repository(t, readDemo(t), ModeDelegate, true)
	t.Setenv("STUB_EXIT", "1")
	t.Setenv("STUB_OUTPUT", failedBuild)
	results := runBatchJob(t, root, stacks.TaskBuild, "apps/api", "libs/pricing")
	api := results["apps/api"]
	if api.Status != "FAILED" || len(api.Diagnostics) != 1 || api.Diagnostics[0].Category != stacks.CodeStepFailed ||
		api.Diagnostics[0].Description != "rush build failed for @demo/api\nsrc/index.ts(3,1): error TS2304: Cannot find name 'x'.\nReturned error code: 2" {
		t.Errorf("api = %+v, want a failure with the details Rush printed", api)
	}
	if pricing := results["libs/pricing"]; pricing.Status != "OK" || len(pricing.Diagnostics) != 0 {
		t.Errorf("pricing = %+v, want the pass Rush reported", pricing)
	}
}

func TestAProjectRushBlockedFailsAsNotRun(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-rush-command", "a-project-rush-blocked-fails-as-not-run")
	stubBatchRush(t)
	root := repository(t, readDemo(t), ModeDelegate, true)
	t.Setenv("STUB_EXIT", "1")
	t.Setenv("STUB_OUTPUT", "==[ BLOCKED: 1 operation ]=====\n\nThese operations were blocked by dependencies that failed:\n  @demo/api\n\n"+
		"==[ FAILURE: 1 operation ]=====\n\n--[ FAILURE: @demo/pricing ]-----[ 1.01 seconds ]--\n\nReturned error code: 1\n\nOperations failed.\n")
	results := runBatchJob(t, root, stacks.TaskBuild, "apps/api", "libs/pricing")
	if api := results["apps/api"]; api.Status != "FAILED" || len(api.Diagnostics) != 1 || !strings.Contains(api.Diagnostics[0].Description, "did not run @demo/api: it failed on @demo/pricing") {
		t.Errorf("api = %+v, want a failure as not run", api)
	}
}

// summary writes Rush's closing summary of the statuses, each with its
// operations, in Rush's order.
func summary(statuses map[string][]string) string {
	var b strings.Builder
	for _, status := range []string{"SKIPPED", "NO OP", "FROM CACHE", "SUCCESS", "SUCCESS WITH WARNINGS", "ABORTED", "BLOCKED", "FAILURE"} {
		ops := statuses[status]
		if len(ops) == 0 {
			continue
		}
		b.WriteString("==[ " + status + ": 1 operation ]========================\n\n")
		switch status {
		case "FAILURE", "SUCCESS WITH WARNINGS":
			short := "FAILURE"
			if status != "FAILURE" {
				short = "WARNING"
			}
			for _, op := range ops {
				b.WriteString("--[ " + short + ": " + op + " ]-----------[ 1.00 seconds ]--\n\n" + op + " says why\n\n")
			}
		default:
			b.WriteString("These operations were listed:\n")
			for _, op := range ops {
				b.WriteString("  " + op + "    1.00 seconds\n")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("Operations failed.\n\nrush test (2.00 seconds)\n")
	return b.String()
}

func TestReadBatchReadsEachProjectFromRushsSummary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses map[string][]string
		want     string
	}{
		{"succeeded", map[string][]string{"SUCCESS": {"a"}, "FAILURE": {"b"}}, "passed"},
		{"from cache", map[string][]string{"FROM CACHE": {"a"}, "FAILURE": {"b"}}, "passed"},
		{"up to date", map[string][]string{"SKIPPED": {"a"}, "FAILURE": {"b"}}, "passed"},
		{"no work", map[string][]string{"NO OP": {"a"}, "FAILURE": {"b"}}, "passed"},
		{"failed", map[string][]string{"FAILURE": {"a"}}, "failed"},
		{"with warnings", map[string][]string{"SUCCESS WITH WARNINGS": {"a"}}, "failed"},
		{"blocked", map[string][]string{"BLOCKED": {"a"}, "FAILURE": {"b"}}, "not named"},
		{"aborted", map[string][]string{"ABORTED": {"a"}, "FAILURE": {"b"}}, "not named"},
		{"not listed", map[string][]string{"FAILURE": {"b"}}, "not named"},
		{"a phase failed", map[string][]string{"SUCCESS": {"a (build)"}, "FAILURE": {"a (test)"}}, "failed"},
		{"a phase blocked", map[string][]string{"SUCCESS": {"a (build)"}, "BLOCKED": {"a (test)"}, "FAILURE": {"b (build)"}}, "not named"},
		{"a phase aborted", map[string][]string{"SUCCESS": {"a (build)"}, "ABORTED": {"a (test)"}, "FAILURE": {"b (build)"}}, "not named"},
		{"every phase passed", map[string][]string{"SUCCESS": {"a (build)", "a (test)"}, "FAILURE": {"b (build)"}}, "passed"},
		{"a project of a longer name", map[string][]string{"SUCCESS": {"ab"}, "FAILURE": {"b"}}, "not named"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := ReadBatch(stacks.BatchReport{Task: stacks.TaskTest, Components: []string{"a", "b"}, Failed: true, Command: []string{"rush", "test", "--only=a", "--only=b"}, Output: []byte(summary(tc.statuses))})
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
			if got == "failed" && !strings.Contains(o.Message, "says why") {
				t.Errorf("a's message %q quotes no details", o.Message)
			}
		})
	}
}

func TestReadBatchReadsOnlyTheLastRunAndOnlyAFailedCommand(t *testing.T) {
	// An operation's header after a summary starts a new run of the
	// operations: the earlier summary is no report of the last one.
	output := summary(map[string][]string{"SUCCESS": {"a"}}) + "==[ a ]=========[ 1 of 1 ]==\n" + summary(map[string][]string{"FAILURE": {"b"}})
	report := stacks.BatchReport{Task: stacks.TaskTest, Components: []string{"a", "b"}, Failed: true, Command: []string{"rush", "test"}, Output: []byte(output)}
	if o, named := ReadBatch(report)["a"]; named {
		t.Errorf("a = %+v, want it not named: the last run did not list it", o)
	}
	report.Failed = false
	if out := ReadBatch(report); out != nil {
		t.Errorf("a passing command answered %+v, want nothing", out)
	}
	if out := ReadBatch(stacks.BatchReport{Components: []string{"a"}, Failed: true, Output: []byte("ERROR: Link flag invalid.\n")}); len(out) != 0 {
		t.Errorf("an output with no summary answered %+v", out)
	}
}

func TestReadBatchReadsWhatRushPrints(t *testing.T) {
	// What rush test --only=a --only=b --only=c printed with Rush 5.182.1,
	// where b's test fails and c depends on b.
	output, err := os.ReadFile("testdata/failed-test.txt")
	if err != nil {
		t.Fatal(err)
	}
	out := ReadBatch(stacks.BatchReport{Task: stacks.TaskTest, Components: []string{"a", "b", "c"}, Failed: true, Command: []string{"rush", "test", "--only=a", "--only=b", "--only=c"}, Output: output})
	if a, ok := out["a"]; !ok || a.Failed {
		t.Errorf("a = %+v %v, want the pass Rush listed", a, ok)
	}
	if b := out["b"]; !b.Failed || !strings.Contains(b.Message, "rush test failed for b") || !strings.Contains(b.Message, "Returned error code: 1") {
		t.Errorf("b = %+v, want its failure with Rush's details", b)
	}
	if c, ok := out["c"]; ok {
		t.Errorf("c = %+v, want it not named: Rush blocked it", c)
	}
}
