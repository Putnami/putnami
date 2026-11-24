package nx

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
// projects at dirs, the way the CLI does, and returns each project's result
// by its directory.
func runBatchJob(t *testing.T, root, task string, dirs ...string) map[string]batchResult {
	t.Helper()
	s := stack(t)
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == dirs[0] {
			meta = p.Metadata
		}
	}
	var selected []pctx.ProjectRef
	for _, dir := range dirs {
		selected = append(selected, pctx.ProjectRef{ID: "/" + dir, Name: dir, Path: dir, FullPath: filepath.Join(root, filepath.FromSlash(dir)), OutputPath: filepath.Join(root, ".putnami/out", dir)})
	}
	ctx := &pctx.Context{WorkspaceRoot: root, SelectedProjects: selected}
	ctx.Project.Path = dirs[0]
	ctx.Project.FullPath = selected[0].FullPath
	ctx.Project.Metadata = map[string]json.RawMessage{ExtensionName: meta}
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

func TestAVerbRunsOneNxRunManyOverItsProjects(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-nx-command", "a-verb-runs-one-nx-run-many-over-its-projects")
	d := readDemo(t)
	stubNx(t, d)
	root := repository(t, d, ModeDelegate)
	rootReal, _ := filepath.EvalSymlinks(root)

	results := runBatchJob(t, root, stacks.TaskLint, "apps/api", "packages/pricing")
	log := stubLog(t)
	if want := rootReal + " npx --no nx run-many -t lint -p @demo/api,@demo/pricing --outputStyle=static"; len(log) != 2 || !strings.HasSuffix(log[0], " npx --no -- nx --version") || log[1] != want {
		t.Errorf("the batch ran %q, want the version check then %q", log, want)
	}
	for dir, r := range results {
		if r.Status != "OK" || r.Data["delegatedTo"] != "nx" {
			t.Errorf("%s = %+v, want a pass delegated to nx", dir, r)
		}
	}
}

// runManyFailure is the static output of nx run-many when the api's lint
// fails and the pricing's passes, in Nx 17 and later's summary, colored.
const runManyFailure = "\n\x1b[1m> nx run @demo/pricing:lint\x1b[22m\n\nall good\n\n" +
	"> nx run @demo/api:lint  [existing outputs match the cache, left as is]\n\n" +
	"/w/apps/api/src/index.ts\n  1:1  error  'x' is not defined  no-undef\n\n1 problem\n\n" +
	" \x1b[31mNX\x1b[39m   Ran target lint for 2 projects (1s)\n\n" +
	"   ✔  1/2 succeeded [0 read from cache]\n\n" +
	"   ✖  1/2 targets failed, including the following:\n\n" +
	"      - nx run @demo/api:lint\n\n"

func TestAFailingProjectFailsAloneWithNxsOutput(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-nx-command", "a-failing-project-fails-alone-with-nxs-output")
	d := readDemo(t)
	stubNx(t, d)
	root := repository(t, d, ModeDelegate)
	writeFile(t, root, "out.txt", runManyFailure)
	t.Setenv("STUB_OUTPUT", filepath.Join(root, "out.txt"))
	t.Setenv("STUB_EXIT", "1")

	results := runBatchJob(t, root, stacks.TaskLint, "apps/api", "packages/pricing")
	api := results["apps/api"]
	if api.Status != "FAILED" || api.Data["delegatedTo"] != "nx" || len(api.Diagnostics) != 1 || api.Diagnostics[0].Category != stacks.CodeStepFailed ||
		!strings.Contains(api.Diagnostics[0].Description, "nx run @demo/api:lint failed") ||
		!strings.Contains(api.Diagnostics[0].Description, "'x' is not defined") || strings.Contains(api.Diagnostics[0].Description, "all good") {
		t.Errorf("api = %+v, want its own section of Nx's output", api)
	}
	if pricing := results["packages/pricing"]; pricing.Status != "OK" || len(pricing.Diagnostics) != 0 {
		t.Errorf("pricing = %+v, want the pass Nx reported", pricing)
	}
}

func TestReadBatchReadsTheOlderFailedTasksSummary(t *testing.T) {
	output := "> nx run a:build\n\nboom\n\n> nx run b:build\n\nok\n\n" +
		" >  NX   Running target build for 3 projects failed\n\n   Failed tasks:\n   \n   - a:build\n"
	got := ReadBatch(stacks.BatchReport{Components: []string{"a", "b", "c"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "build", "-p", "a,b,c"}, Output: []byte(output)})
	if !got["a"].Failed || !strings.Contains(got["a"].Message, "boom") || got["b"].Failed {
		t.Errorf("outcomes = %+v, want a failed with its section and b passed", got)
	}
	if _, ok := got["c"]; ok {
		t.Errorf("c = %+v, want it not named: Nx ran no section of it", got["c"])
	}
	if got := ReadBatch(stacks.BatchReport{Components: []string{"a"}, Command: []string{"nx", "run-many", "-t", "build"}, Output: []byte("Cannot find configuration for task\n")}); got != nil {
		t.Errorf("an output with no section reads %v, want none", got)
	}
}

func TestReadBatchReadsAConfigurationAndAGitHubGroup(t *testing.T) {
	output := "::group::✔ > nx run b:build:production\n\nok\n::endgroup::\n" +
		"::group::✖ > nx run a:build:production\n\nboom\n::endgroup::\n" +
		" NX   Ran target build for 2 projects\n\n   ✖  1/2 targets failed, including the following:\n\n      - nx run a:build:production\n"
	got := ReadBatch(stacks.BatchReport{Components: []string{"a", "b"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "build", "-p", "a,b"}, Output: []byte(output)})
	if !got["a"].Failed || !strings.Contains(got["a"].Message, "nx run a:build:production failed") || !strings.Contains(got["a"].Message, "boom") {
		t.Errorf("a = %+v, want its failed section", got["a"])
	}
	if b, ok := got["b"]; !ok || b.Failed {
		t.Errorf("b = %+v (%v), want the pass Nx reported", b, ok)
	}
}

func TestReadBatchReadsAQuotedTarget(t *testing.T) {
	output := "> nx run a:\"test:lib\"\n\nboom\n\n> nx run b:\"test:lib\"\n\nok\n\n" +
		" NX   Ran target test:lib for 2 projects\n\n   ✖  1/2 targets failed, including the following:\n\n      - nx run a:test:lib\n"
	got := ReadBatch(stacks.BatchReport{Components: []string{"a", "b"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "test:lib", "-p", "{components}"}, Output: []byte(output)})
	if !got["a"].Failed || !strings.Contains(got["a"].Message, "boom") {
		t.Errorf("a = %+v, want its failed section", got["a"])
	}
	if b, ok := got["b"]; !ok || b.Failed {
		t.Errorf("b = %+v (%v), want the pass Nx reported", b, ok)
	}
}

func TestReadBatchReadsOnlyTheFailedTasksAndTheExactSection(t *testing.T) {
	// a:test passed; its dependency a:test:lib failed, and b:test did not run.
	output := "> nx run a:\"test:lib\"\n\nlib boom\n\n> nx run a:test\n\nok\n\n" +
		" NX   Ran target test for 2 projects\n\n" +
		"   ✖  1/3 targets failed, including the following:\n\n      - nx run a:test:lib\n\n" +
		"   Tasks not run because their dependencies failed or --nx-bail=true:\n\n      - b:test\n"
	got := ReadBatch(stacks.BatchReport{Components: []string{"a", "b"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "test", "-p", "a,b"}, Output: []byte(output)})
	if a, ok := got["a"]; !ok || a.Failed {
		t.Errorf("a = %+v (%v), want the pass of its own a:test section", a, ok)
	}
	if b := got["b"]; !b.Failed || !strings.Contains(b.Message, "did not run b:test") {
		t.Errorf("b = %+v, want a failure as not run: Nx lists it among the tasks it did not run", b)
	}
}

func TestAFailedConfigurationFailsTheTargetBesideAPassingSibling(t *testing.T) {
	// -t build: a's sibling build:assets passed, a's default configuration
	// failed, and so did b's.
	output := "> nx run a:\"build:assets\"\n\nok\n\n> nx run a:build:production\n\na boom\n\n> nx run b:build:production\n\nb boom\n\n" +
		" NX   Ran target build for 2 projects\n\n   ✖  2/3 targets failed, including the following:\n\n" +
		"      - nx run a:build:production\n      - nx run b:build:production\n"
	got := ReadBatch(stacks.BatchReport{Components: []string{"a", "b"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "build", "-p", "a,b"}, Output: []byte(output)})
	for _, c := range []string{"a", "b"} {
		if o := got[c]; !o.Failed || !strings.Contains(o.Message, c+" boom") {
			t.Errorf("%s = %+v, want its failed default configuration", c, o)
		}
	}
}

func TestASyncedTargetRunsAsItsOwnRunMany(t *testing.T) {
	got := batchSteps("test", [][]string{runStep("test:lib"), runStep("e2e")})
	want := [][]string{
		{"npx", "--no", "nx", "run-many", "-t", "test:lib", "-p", "{components}", "--outputStyle=static"},
		{"npx", "--no", "nx", "run-many", "-t", "e2e", "-p", "{components}", "--outputStyle=static"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("batchSteps = %q, want %q", got, want)
	}
	if got := commandTargets([]string{"nx", "run-many", "--targets=lint,test", "-p", "a"}); !reflect.DeepEqual(got, []string{"lint", "test"}) {
		t.Errorf("commandTargets = %q", got)
	}
}

// TestReadBatchJudgesATargetByItsOwnRunsAlone pins the Nx reader over the
// matrix of a's own run of build (exact, default configuration, none) and its
// outcome (passed, failed, not run, stopped), beside a quoted sibling target
// a:"build:x" (absent, passed, failed), with b:build failing so the command
// fails.
func TestReadBatchJudgesATargetByItsOwnRunsAlone(t *testing.T) {
	type outcome string
	const (
		passed  outcome = "passed"
		failed  outcome = "failed"
		notRun  outcome = "not run"
		stopped outcome = "stopped"
		absent  outcome = "absent"
	)
	for _, own := range []string{"a:build", "a:build:production", ""} {
		for _, result := range []outcome{passed, failed, notRun, stopped} {
			for _, sibling := range []outcome{absent, passed, failed} {
				if own == "" && result != notRun && result != stopped {
					continue
				}
				var out, failedList, notRunList, stoppedList strings.Builder
				header := func(id, quoted string, o outcome) {
					switch o {
					case passed, failed:
						out.WriteString("> nx run " + quoted + "\n\n" + id + " output\n\n")
					case stopped:
						// Nx 24 prints a stopped task's partial output as
						// a section, unless it never started.
						if own != "" {
							out.WriteString("> nx run " + quoted + "\n\n" + id + " partial\n\n")
						}
					}
					switch o {
					case stopped:
						stoppedList.WriteString("      - " + id + "\n")
					case failed:
						failedList.WriteString("      - nx run " + id + "\n")
					case notRun:
						notRunList.WriteString("      - " + id + "\n")
					}
				}
				id := own
				if id == "" {
					id = "a:build"
				}
				header(id, id, result)
				header("a:build:x", `a:"build:x"`, sibling)
				header("b:build", "b:build", failed)
				out.WriteString(" NX   Ran target build for 2 projects\n\n   ✖  targets failed, including the following:\n\n" + failedList.String() + "\n")
				if notRunList.Len() > 0 {
					out.WriteString("   Tasks not run because their dependencies failed or --nx-bail=true:\n\n" + notRunList.String() + "\n")
				}
				if stoppedList.Len() > 0 {
					out.WriteString("   Tasks stopped before they finished:\n\n" + stoppedList.String())
				}
				got := ReadBatch(stacks.BatchReport{Components: []string{"a", "b"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "build", "-p", "a,b"}, Output: []byte(out.String())})
				name := fmt.Sprintf("own %q %s, sibling %s", own, result, sibling)
				a, named := got["a"]
				switch result {
				case passed:
					if !named || a.Failed {
						t.Errorf("%s: a = %+v (%v), want a pass", name, a, named)
					}
				case failed:
					if !a.Failed || !strings.Contains(a.Message, id+" output") {
						t.Errorf("%s: a = %+v, want its own failed section", name, a)
					}
				case notRun:
					if !a.Failed || !strings.Contains(a.Message, "did not run") {
						t.Errorf("%s: a = %+v, want a failure as not run", name, a)
					}
				case stopped:
					if !a.Failed || !strings.Contains(a.Message, "before it finished") {
						t.Errorf("%s: a = %+v, want a failure as stopped", name, a)
					}
				}
				if !got["b"].Failed {
					t.Errorf("%s: b = %+v, want its failure", name, got["b"])
				}
			}
		}
	}
}

func TestADependencyThatFailedBeforeTheTargetRanFailsIt(t *testing.T) {
	// a:build depends on a:"build:x", which passed, and a:codegen, which
	// failed: Nx never ran a:build. The ^build variant lists it the same way.
	for name, output := range map[string]string{
		"own dependency": "> nx run a:\"build:x\"\n\nx ok\n\n> nx run a:codegen\n\ncodegen boom\n\n> nx run b:build\n\nb boom\n\n",
		"^build":         "> nx run c:build\n\nc boom\n\n> nx run b:build\n\nb boom\n\n",
	} {
		output += " NX   Ran target build for 2 projects\n\n   ✖  targets failed, including the following:\n\n      - nx run a:codegen\n      - nx run c:build\n      - nx run b:build\n\n" +
			"   Tasks not run because their dependencies failed or --nx-bail=true:\n\n      - a:build\n"
		got := ReadBatch(stacks.BatchReport{Components: []string{"a", "b"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "build", "-p", "a,b"}, Output: []byte(output)})
		if a := got["a"]; !a.Failed || !strings.Contains(a.Message, "did not run a:build") {
			t.Errorf("%s: a = %+v, want a failure as not run", name, a)
		}
	}
}

func TestReadBatchReadsEachSummaryListByItsHeading(t *testing.T) {
	run := func(summary string) map[string]stacks.BatchOutcome {
		output := "> nx run a:build\n\na ok\n\n> nx run b:build\n\nb boom\n\n NX   Ran target build for 2 projects\n\n" + summary
		return ReadBatch(stacks.BatchReport{Components: []string{"a", "b"}, Command: []string{"npx", "--no", "nx", "run-many", "-t", "build", "-p", "a,b"}, Output: []byte(output)})
	}
	failedB := "   ✖  targets failed, including the following:\n\n      - nx run b:build\n\n"
	// A skipped task fails as not run.
	if a := run(failedB + "   Skipped tasks:\n\n      - a:build\n")["a"]; !a.Failed || !strings.Contains(a.Message, "did not run a:build") {
		t.Errorf("skipped: a = %+v, want a failure as not run", a)
	}
	// A heading of another list ends the list before it.
	if a, ok := run(failedB + "   Hints for these tasks:\n\n      - a:build\n")["a"]; !ok || a.Failed {
		t.Errorf("another list: a = %+v (%v), want the pass of its section", a, ok)
	}
	// A failed task with no section of its own, under a's target, is
	// another target when a's own run printed a section.
	if a, ok := run("   ✖  targets failed, including the following:\n\n      - nx run b:build\n      - nx run a:build:other\n")["a"]; !ok || a.Failed {
		t.Errorf("an unknown sibling: a = %+v (%v), want the pass of its own run", a, ok)
	}
}
