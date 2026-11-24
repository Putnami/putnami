package moon

import (
	"context"
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
	probed := probe(t, root)
	var selected []pctx.ProjectRef
	for _, dir := range dirs {
		selected = append(selected, pctx.ProjectRef{ID: "/" + dir, Name: dir, Path: dir, FullPath: filepath.Join(root, filepath.FromSlash(dir)), OutputPath: filepath.Join(root, ".putnami/out", dir)})
	}
	ctx := &pctx.Context{WorkspaceRoot: root, SelectedProjects: selected}
	ctx.Project.Path = dirs[0]
	ctx.Project.FullPath = selected[0].FullPath
	ctx.Project.Metadata = map[string]json.RawMessage{ExtensionName: probed[dirs[0]].Metadata}
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

func TestAVerbRunsOneMoonRunOverItsProjects(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-moon-command", "a-verb-runs-one-moon-run-over-its-projects")
	d := readDemo(t)
	stubMoon(t)
	root := repository(t, d, ModeDelegate, false)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	rootReal, _ := filepath.EvalSymlinks(root)
	// pricing tags its unit task test: its own target joins api's command.
	results := runBatchJob(t, root, stacks.TaskTest, "apps/api", "packages/pricing")
	log := stubLog(t)
	if len(log) != 2 || !strings.HasSuffix(log[0], " moon --version") || log[1] != rootReal+" moon run api:test pricing:unit" {
		t.Errorf("the batch ran %q, want the version check then one moon run api:test pricing:unit", log)
	}
	for dir, r := range results {
		if r.Status != "OK" || r.Data["delegatedTo"] != "moon" {
			t.Errorf("%s = %+v, want a pass delegated to moon", dir, r)
		}
	}
}

func TestAFailingProjectFailsAloneWithMoonsMessage(t *testing.T) {
	spectest.Proves(t, feature, "a-delegated-verb-runs-one-moon-command", "a-failing-project-fails-alone-with-moons-message")
	d := readDemo(t)
	stubMoon(t)
	root := repository(t, d, ModeDelegate, false)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// moon writes its run report and the output it kept for api's build.
	report := `{"actions":[` +
		`{"label":"SyncWorkspace","status":"passed","node":{"action":"sync-workspace"}},` +
		`{"label":"RunTask(api:build)","status":"failed","error":"Process tsc failed with a 2 exit code.","node":{"action":"run-task","params":{"target":"api:build"}}},` +
		`{"label":"RunTask(pricing:build)","status":"cached","error":null,"node":{"action":"run-task","params":{"target":"pricing:build"}}}]}`
	stackstest.WriteFile(t, root, ".moon/cache/states/api/build/stderr.log", "src/index.ts(3,1): error TS2304: Cannot find name 'x'.\n")
	reportFile := filepath.Join(t.TempDir(), "runReport.json")
	stackstest.WriteFile(t, filepath.Dir(reportFile), "runReport.json", report)
	t.Setenv("STUB_REPORT", reportFile)
	t.Setenv("STUB_EXIT", "1")
	results := runBatchJob(t, root, stacks.TaskBuild, "apps/api", "packages/pricing")

	api := results["apps/api"]
	if api.Status != "FAILED" || api.Data["delegatedTo"] != "moon" || len(api.Diagnostics) != 1 || api.Diagnostics[0].Category != stacks.CodeStepFailed ||
		!strings.Contains(api.Diagnostics[0].Description, "moon run api:build failed: Process tsc failed with a 2 exit code.") ||
		!strings.Contains(api.Diagnostics[0].Description, "Cannot find name 'x'") {
		t.Errorf("api = %+v, want moon's error and the output it kept", api)
	}
	if pricing := results["packages/pricing"]; pricing.Status != "OK" || len(pricing.Diagnostics) != 0 {
		t.Errorf("pricing = %+v, want the cached pass moon reported", pricing)
	}
}

func TestAnEarlierRunReportIsNeverRead(t *testing.T) {
	d := readDemo(t)
	stubMoon(t)
	root := repository(t, d, ModeDelegate, false)
	if _, err := Sync(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// An earlier run left its report; this one fails before it writes one.
	stackstest.WriteFile(t, root, RunReport, `{"actions":[`+
		`{"label":"RunTask(api:build)","status":"failed","node":{"action":"run-task","params":{"target":"api:build"}}},`+
		`{"label":"RunTask(pricing:build)","status":"passed","node":{"action":"run-task","params":{"target":"pricing:build"}}}]}`)
	t.Setenv("STUB_EXIT", "1")
	results := runBatchJob(t, root, stacks.TaskBuild, "apps/api", "packages/pricing")
	for dir, r := range results {
		if r.Status != "FAILED" || len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Description, "exit status 1") {
			t.Errorf("%s = %+v, want the command's failure, not the earlier report's", dir, r)
		}
	}
	if _, err := os.Stat(filepath.Join(root, RunReport)); !os.IsNotExist(err) {
		t.Errorf("the earlier report is still there: %v", err)
	}
}
