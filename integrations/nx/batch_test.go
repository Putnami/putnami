package nx

import (
	"encoding/json"
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
