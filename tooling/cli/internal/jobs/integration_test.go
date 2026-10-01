package jobs

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// makeIntegrationWorkspace lays out a real workspace on disk with two
// interdependent projects (utils ← app) so the scheduler can actually
// execute jobs in their project directories during a Plan→Schedule→Run
// integration test.
func makeIntegrationWorkspace(t *testing.T) (*workspace.Workspace, *extension.ExtensionDescription) {
	t.Helper()
	wsRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsRoot, "packages", "utils"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wsRoot, "packages", "app"), 0o755); err != nil {
		t.Fatal(err)
	}

	projects := []*workspace.Project{
		{ID: "/packages/utils", Name: "utils", Path: "packages/utils", Extensions: []string{"@test/ts"}},
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"utils"}, Extensions: []string{"@test/ts"}},
	}
	ws := workspace.NewWorkspace(wsRoot, nil, projects)
	ws.Name = "integration-ws"

	// The program exits 0 and ignores all args, so each pipeline step runs
	// successfully and the dependency wiring (^generate, transpile→generate,
	// cross-project utils→app) becomes the only thing under test.
	proc := fixtureproc.Write(t, filepath.Join(t.TempDir(), "true"), fixtureproc.Program{})

	ext := &extension.ExtensionDescription{
		Name: "@test/ts",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName: "@test/ts",
				Name:          "build",
				Kind:          "command",
				Command:       proc,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate":  {Kind: "command", Command: proc, Declares: &extension.TaskDeclaration{}},
			"build-transpile": {Kind: "command", Command: proc, Declares: &extension.TaskDeclaration{}},
		},
	}
	return ws, ext
}

func requireOrder(t *testing.T, pos map[string]int, earlier, later string) {
	t.Helper()
	earlierPos, earlierOK := pos[earlier]
	laterPos, laterOK := pos[later]
	if !earlierOK || !laterOK {
		t.Errorf("missing job in completion order (have %v); want %q before %q", pos, earlier, later)
		return
	}
	if earlierPos >= laterPos {
		t.Errorf("expected %q (pos %d) to complete before %q (pos %d)", earlier, earlierPos, later, laterPos)
	}
}

// TestPlanSchedule_EndToEnd is the integration test the audit asked for:
// jobs.Plan generates the ScheduledJobs from a workspace +
// extension; newScheduler(...).Run consumes them; we assert the
// dependency hand-off is intact (pipeline order and cross-project order)
// and that every planned job actually executes.
func TestPlanSchedule_EndToEnd(t *testing.T) {
	ws, ext := makeIntegrationWorkspace(t)

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// 2 projects × 2 pipeline steps (generate, transpile) = 4 jobs
	if len(planned) != 4 {
		t.Fatalf("expected 4 planned jobs, got %d", len(planned))
	}

	renderer := &mockRenderer{}
	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel: 4,
		NoCache:     true,
	}, renderer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	// Every job must have run and the run must succeed end-to-end.
	if !result.Success {
		t.Fatalf("scheduler reported failure; results=%+v", result.Results)
	}
	if len(result.Results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(result.Results))
	}

	// Verify completion order respects:
	//   utils:build~generate  → utils:build~transpile
	//   utils:build~generate  → app:build~generate     (cross-project, via ^generate)
	//   app:build~generate    → app:build~transpile
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	pos := map[string]int{}
	for i, k := range renderer.jobEnds {
		pos[k] = i
	}
	requireOrder(t, pos, "/packages/utils:build~generate", "/packages/utils:build~transpile")
	requireOrder(t, pos, "/packages/utils:build~generate", "/packages/app:build~generate")
	requireOrder(t, pos, "/packages/app:build~generate", "/packages/app:build~transpile")
}

// TestPlanSchedule_FailurePropagatesAcrossProjects verifies the
// dependency hand-off under failure: when utils:build~generate fails,
// the dependent app:build~generate must end up canceled rather than
// silently running. This exercises the planner→scheduler integration
// for the negative path.
func TestPlanSchedule_FailurePropagatesAcrossProjects(t *testing.T) {
	ws, ext := makeIntegrationWorkspace(t)
	// Force utils:build~generate to fail by pointing build-generate at a
	// non-existent binary; transpile and app jobs would normally succeed.
	ext.Tasks["build-generate"] = extension.TaskDefinition{
		Kind:     "command",
		Command:  "/nonexistent-binary-xyzzy",
		Declares: &extension.TaskDeclaration{},
	}

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	renderer := &mockRenderer{}
	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel: 4,
		NoCache:     true,
	}, renderer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	if result.Success {
		t.Error("expected scheduler to report failure when utils generate fails")
	}
	utilsGen := result.Results["/packages/utils:build~generate"]
	if utilsGen == nil || utilsGen.Status != "failed" {
		t.Errorf("expected utils generate to fail, got %+v", utilsGen)
	}
	appGen := result.Results["/packages/app:build~generate"]
	if appGen == nil || (appGen.Status != "canceled" && appGen.Status != "skipped") {
		t.Errorf("expected app generate to be canceled/skipped, got %+v", appGen)
	}
}

// makeResultEmittingWorkspace lays out a real workspace with one project per
// language extension (go, ts), each running a "test" command whose single
// pipeline step emits a meta event, a coverage metric, and a structured result
// event carrying a testSummary. This exercises the cache-hit replay path
// against representative Go AND TypeScript test/coverage jobs.
func makeResultEmittingWorkspace(t *testing.T) (*workspace.Workspace, []*extension.ExtensionDescription) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()

	// The script emits the same report-relevant events a cold test run would:
	// meta (proves the job body ran), a coverage metric, and a result event
	// whose Data carries testSummary + coverageSummary + the per-case results.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"tool\":\"testrunner\"}}'\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"metric\",\"message\":\"coverage\",\"data\":{\"name\":\"coverage\",\"value\":87.5}}'\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"success\",\"data\":{\"testSummary\":{\"total\":4,\"passed\":4,\"failed\":0},\"coverageSummary\":{\"pct\":87.5}," +
		"\"testCasesDropped\":2,\"testCases\":[{\"name\":\"TestA\",\"suite\":\"s\",\"status\":\"passed\",\"durationMs\":3},{\"name\":\"TestB\",\"suite\":\"s\",\"status\":\"passed\"}]}}}'\n"
	scriptPath := filepath.Join(wsRoot, "emit-test-result.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write emit script: %v", err)
	}

	langs := []struct{ name, ext, path string }{
		{"go-lib", "@test/go", "packages/go-lib"},
		{"ts-lib", "@test/ts", "packages/ts-lib"},
	}
	var projects []*workspace.Project
	var exts []*extension.ExtensionDescription
	for _, l := range langs {
		if err := os.MkdirAll(filepath.Join(wsRoot, l.path), 0o755); err != nil {
			t.Fatal(err)
		}
		projects = append(projects, &workspace.Project{
			ID: "/" + l.path, Name: l.name, Path: l.path, Extensions: []string{l.ext},
		})
		exts = append(exts, &extension.ExtensionDescription{
			Name: l.ext,
			Jobs: map[string]*extension.JobDefinition{
				"test": {
					ExtensionName: l.ext,
					Name:          "test",
					Kind:          "command",
					Command:       scriptPath,
					PipelineSteps: []extension.PipelineStep{
						{ID: "run", Task: "test-run"},
					},
				},
			},
			Tasks: map[string]extension.TaskDefinition{
				"test-run": {Kind: "command", Command: scriptPath, Declares: &extension.TaskDeclaration{}},
			},
		})
	}
	ws := workspace.NewWorkspace(wsRoot, nil, projects)
	ws.Name = "replay-ws"
	return ws, exts
}

// TestPlanSchedule_CacheHitReplaysResultEvents is a regression test: a cache
// hit must replay the structured result event (and other report-relevant events)
// to the renderer, so a warm run folds to the SAME test/coverage summaries as the
// cold run that populated the cache. Run 1 executes every job (events streamed
// live); run 2 is a pure cache hit and must re-emit the captured result event via
// JobEvent, while keeping job:end.cache=true intact.
func TestPlanSchedule_CacheHitReplaysResultEvents(t *testing.T) {
	ws, exts := makeResultEmittingWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cold := &mockRenderer{}
	planned, err := Plan(ws, []string{"test"}, ws.Projects, exts, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(planned) != 2 {
		t.Fatalf("expected 2 planned jobs, got %d", len(planned))
	}
	res1 := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 2}, cold, cache).Run(ctx)
	if !res1.Success {
		t.Fatalf("cold run failed; results=%+v", res1.Results)
	}

	warm := &mockRenderer{}
	planned2, err := Plan(ws, []string{"test"}, ws.Projects, exts, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan 2: %v", err)
	}
	res2 := newScheduler(ws, planned2, nil, SchedulerConfig{MaxParallel: 2}, warm, cache).Run(ctx)
	if !res2.Success {
		t.Fatalf("warm run failed; results=%+v", res2.Results)
	}

	for key, result := range res2.Results {
		if !result.CacheHit {
			t.Fatalf("warm run expected pure task-owned cache hits, %s executed", key)
		}
	}

	for _, job := range planned2 {
		key := job.Key()
		if result := res2.Results[key]; result == nil || !result.CacheHit {
			t.Fatalf("%s: expected cache hit result, got %+v", key, result)
		}

		warmResults := warm.eventsOfType(key, EventTypeResult)
		if len(warmResults) != 1 {
			t.Fatalf("%s: warm run replayed %d result events, want 1", key, len(warmResults))
		}
		warmSummary := testSummaryFromResultEvent(t, warmResults[0])

		coldResults := cold.eventsOfType(key, EventTypeResult)
		if len(coldResults) != 1 {
			t.Fatalf("%s: cold run emitted %d result events, want 1", key, len(coldResults))
		}
		coldSummary := testSummaryFromResultEvent(t, coldResults[0])
		if warmSummary["passed"] != coldSummary["passed"] || warmSummary["total"] != coldSummary["total"] {
			t.Fatalf("%s: warm summary %v != cold summary %v", key, warmSummary, coldSummary)
		}
		if warmSummary["passed"] != float64(4) || warmSummary["total"] != float64(4) {
			t.Fatalf("%s: unexpected replayed testSummary %v", key, warmSummary)
		}

		// The entry keeps the result data, so the warm task decodes the same
		// test cases, and the same dropped count, its test:case records carry.
		coldTask := TaskResultOf(job, res1.Results[key])
		warmTask := TaskResultOf(job, res2.Results[key])
		if len(coldTask.TestCases) != 2 || coldTask.TestCasesDropped != 2 {
			t.Fatalf("%s: cold cases=%+v dropped=%d, want 2 cases and 2 dropped", key, coldTask.TestCases, coldTask.TestCasesDropped)
		}
		if !reflect.DeepEqual(warmTask.TestCases, coldTask.TestCases) || warmTask.TestCasesDropped != coldTask.TestCasesDropped {
			t.Fatalf("%s: warm cases=%+v dropped=%d, want the cold run's %+v dropped=%d",
				key, warmTask.TestCases, warmTask.TestCasesDropped, coldTask.TestCases, coldTask.TestCasesDropped)
		}
	}
}

func testSummaryFromResultEvent(t *testing.T, event RawJobEvent) map[string]any {
	t.Helper()
	data, ok := event.Data["data"].(map[string]any)
	if !ok {
		t.Fatalf("result event missing nested data map: %#v", event.Data)
	}
	summary, ok := data["testSummary"].(map[string]any)
	if !ok {
		t.Fatalf("result event missing testSummary: %#v", data)
	}
	return summary
}

func TestPlanSchedule_DynamicCPUBudgets(t *testing.T) {
	ws, ext := makeIntegrationWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	planned, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	result := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache).Run(ctx)
	if !result.Success {
		t.Fatalf("run 1 failed; results=%+v", result.Results)
	}
	if got := len(result.Tuning.CPUBudgets); got != 4 {
		t.Fatalf("run 1 granted %d budgets, want 4 (every job executed)", got)
	}
	for _, grant := range result.Tuning.CPUBudgets {
		if grant.Budget != runtime.NumCPU() {
			t.Fatalf("serial execution must hand each job the whole machine; %s got %d of %d",
				grant.Job, grant.Budget, runtime.NumCPU())
		}
	}
	if _, err := os.Stat(taskStatsPath(ws.Root)); err != nil {
		t.Fatalf("expected run history to be persisted: %v", err)
	}

	planned2, err := Plan(ws, []string{"build"}, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan 2: %v", err)
	}
	result2 := newScheduler(ws, planned2, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache).Run(ctx)
	if !result2.Success {
		t.Fatalf("run 2 failed; results=%+v", result2.Results)
	}
	for key, result := range result2.Results {
		if !result.CacheHit {
			t.Fatalf("run 2 expected pure task-owned cache hits, %s executed", key)
		}
	}
	if got := len(result2.Tuning.CPUBudgets); got != 0 {
		t.Fatalf("cache hits consumed %d CPU budgets, want 0", got)
	}
}
