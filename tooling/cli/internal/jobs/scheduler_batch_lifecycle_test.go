package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// What "one runner through the ordinary per-task
// lifecycle" has to mean, measured rather than asserted in prose.
//
// Every test here compares a BATCHED dispatch against the SOLO dispatch of the
// same plan nodes on the same fixture. That comparison is the whole point: a
// batch may share a subprocess and nothing else, so anything a member's own
// lifecycle produces — its cache key, its task-owned entry,
// its diagnostics, its retry — has to come out the same either way.

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// batchShellHeader is the prologue every fixture tool shares: it records one
// log line per invocation, naming the batch selection or the single project it
// was handed, so a test can tell a shared invocation from a solo one instead of
// only counting spawns.
func batchShellHeader(logPath string) string {
	return "#!/bin/sh\n" +
		// The ledger tests assert each execution recorded a non-zero cost;
		// burnCPUShell (test_helpers_test.go) owns the tick-granularity
		// rationale and the CI failure it prevents.
		burnCPUShell() +
		"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n" +
		"  printf 'batch:%s\\n' \"$PUTNAMI_SELECTED_PROJECTS\" >> " + shellQuote(logPath) + "\n" +
		"else\n" +
		"  printf 'solo:%s\\n' \"$PUTNAMI_PROJECT_NAME\" >> " + shellQuote(logPath) + "\n" +
		"fi\n"
}

// runBatchScheduler is the ONE scheduler construction these tests share. Every
// case here needs a REAL scheduler — dispatch grouping, the CPU grant, the cache
// manager and the capture switch all live on it — so they funnel through this
// single site instead of adding a construction each
// (internal/cli/structural_baseline_test.go pins how many exist).
func runBatchScheduler(
	ctx context.Context,
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	cfg SchedulerConfig,
	cache *store.CacheManager,
) *SchedulerResult {
	return newScheduler(ws, planned, nil, cfg, &mockRenderer{}, cache).Run(ctx)
}

func invocationLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// forceSoloDispatch strips the batch trait from a plan so the same nodes run
// one subprocess each. Nothing else about them changes — in particular not
// their cache keys, which never see the trait.
func forceSoloDispatch(planned []*ScheduledJob) []*ScheduledJob {
	solo := make([]*ScheduledJob, 0, len(planned))
	for _, job := range planned {
		copied := *job
		def := *job.JobDef
		def.Batchable = nil
		copied.JobDef = &def
		solo = append(solo, &copied)
	}
	return solo
}

// countTaskEntryDescriptors counts the format-2 task-owned entries published in
// a store: one entry.json descriptor per entry (store/task_entry.go).
func countTaskEntryDescriptors(t *testing.T, storeRoot string) int {
	t.Helper()
	count := 0
	err := filepath.Walk(storeRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && info.Name() == "entry.json" {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// declaredBatchFixture builds two sibling projects whose batchable task carries
// a v3 DECLARATION (a project-rooted .gen directory). The tool writes that
// directory for every project it is handed, batch or solo, so the same fixture
// exercises both dispatch shapes.
func declaredBatchFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{"a", "b", "extension"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []string{"a", "b"} {
		writeTestFile(t, filepath.Join(root, project, "src.txt"), project)
	}

	command := filepath.Join(root, "generate.sh")
	writeExecutable(t, command, batchShellHeader(logPath)+
		// One code path for both dispatch shapes: generate for every selected
		// project, or for this job's own project when there is no selection.
		"targets=\"$PUTNAMI_SELECTED_PROJECT_PATHS\"\n"+
		"if [ -z \"$targets\" ]; then targets=\"$PUTNAMI_PROJECT_NAME\"; fi\n"+
		"IFS=','\n"+
		"for p in $targets; do\n"+
		"  mkdir -p \"$PUTNAMI_WORKSPACE_ROOT/$p/.gen\"\n"+
		"  printf 'generated for %s\\n' \"$p\" > \"$PUTNAMI_WORKSPACE_ROOT/$p/.gen/out.txt\"\n"+
		"done\n"+
		"unset IFS\n"+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":"+
		"{\"batchResults\":[{\"projectId\":\"/a\",\"status\":\"OK\"},{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n"+
		"else\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
		"fi\n")

	projects := []*workspace.Project{
		{ID: "/a", Name: "a", Path: "a"},
		{ID: "/b", Name: "b", Path: "b"},
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "declared-batch-test"
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/typescript",
		Version: "1.0.0",
		Path:    filepath.Join(root, "extension"),
		Tasks: map[string]extension.TaskDefinition{
			"build-generate": {Declares: &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{
					"gen": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen"},
				},
			}},
		},
	}
	batchable := &extension.TaskBatchPolicy{Tool: "putnami-ts-build"}
	planned := make([]*ScheduledJob, 0, len(projects))
	for _, project := range projects {
		planned = append(planned, &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: "generate", Task: "build-generate"},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/typescript",
				Name:          "build~generate",
				CommandName:   "build",
				StepID:        "generate",
				Command:       command,
				Args:          []string{"build-generate"},
				Cwd:           "{projectRoot}",
				Cache:         true,
				FilePatterns:  []string{"src.txt"},
				TaskCachePolicy: &extension.TaskCachePolicy{
					Deterministic: true,
				},
				Batchable: batchable,
			},
		})
	}
	return ws, planned
}

func generatedText(t *testing.T, ws *workspace.Workspace, project string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws.Root, project, ".gen", "out.txt"))
	if err != nil {
		t.Fatalf("read generated output for %s: %v", project, err)
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// the declared output path, for batch members
// ---------------------------------------------------------------------------

// A batch member with a v3 declaration must store and hit through the SAME
// task-owned entry model a solo run uses. The proof is
// structural rather than a code-path assertion: the declared output lives in
// <project>/.gen. If a warm run restores .gen without spawning anything, only
// a task-owned entry can have carried it.
func TestBatchMembersStoreAndHitDeclaredEntries(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := declaredBatchFixture(t, logPath)
	storeRoot := filepath.Join(t.TempDir(), "store")
	localStore := store.NewLocalStore(storeRoot)
	newCache := func() *store.CacheManager { return store.NewCacheManager(localStore) }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cold := runBatchScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, newCache())
	if !cold.Success {
		t.Fatalf("cold declared batch failed: %+v", cold.Results)
	}
	if got := invocationLog(t, logPath); len(got) != 1 || !strings.HasPrefix(got[0], "batch:") {
		t.Fatalf("cold invocations = %v, want a single shared batch invocation", got)
	}
	for _, project := range []string{"a", "b"} {
		if got, want := generatedText(t, ws, project), "generated for "+project+"\n"; got != want {
			t.Fatalf("cold .gen for %s = %q, want %q", project, got, want)
		}
	}
	if got := countTaskEntryDescriptors(t, storeRoot); got != len(planned) {
		t.Fatalf("format-2 entry descriptors after a cold batch = %d, want one per member (%d)",
			got, len(planned))
	}

	// Destroy both declared outputs. A warm run must put them back from the
	// members' own entries without running the tool again.
	for _, project := range []string{"a", "b"} {
		if err := os.RemoveAll(filepath.Join(ws.Root, project, ".gen")); err != nil {
			t.Fatal(err)
		}
	}

	warm := runBatchScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, newCache())
	if !warm.Success {
		t.Fatalf("warm declared batch failed: %+v", warm.Results)
	}
	if got := invocationLog(t, logPath); len(got) != 1 {
		t.Fatalf("warm invocations = %v, want no additional subprocess", got)
	}
	for _, job := range planned {
		if result := warm.Results[job.Key()]; result == nil || !result.CacheHit {
			t.Fatalf("warm %s = %+v, want an independent cache hit", job.Key(), result)
		}
	}
	for _, project := range []string{"a", "b"} {
		if got, want := generatedText(t, ws, project), "generated for "+project+"\n"; got != want {
			t.Fatalf("restored .gen for %s = %q, want %q", project, got, want)
		}
	}
}

// A batch member's cache entry and a solo run's are the SAME entry: batching
// touches neither the key nor the entry model, so each dispatch shape must be
// able to consume what the other published. Both directions are checked —
// batch→solo and solo→batch — because only one of them would still pass if a
// batch member's key silently picked up something about its group.
func TestSoloAndBatchDispatchShareOneCacheEntry(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}

	for _, test := range []struct {
		name               string
		coldSolo, warmSolo bool
	}{
		{name: "batch publishes, solo consumes", coldSolo: false, warmSolo: true},
		{name: "solo publishes, batch consumes", coldSolo: true, warmSolo: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			logPath := filepath.Join(t.TempDir(), "invocations")
			ws, planned := declaredBatchFixture(t, logPath)
			localStore := store.NewLocalStore(filepath.Join(t.TempDir(), "store"))
			newCache := func() *store.CacheManager { return store.NewCacheManager(localStore) }

			coldPlan, warmPlan := planned, planned
			if test.coldSolo {
				coldPlan = forceSoloDispatch(planned)
			}
			if test.warmSolo {
				warmPlan = forceSoloDispatch(planned)
			}

			cold := runBatchScheduler(ctx, ws, coldPlan, SchedulerConfig{MaxParallel: 2}, newCache())
			if !cold.Success {
				t.Fatalf("cold run failed: %+v", cold.Results)
			}
			coldInvocations := len(invocationLog(t, logPath))
			wantCold := 1
			if test.coldSolo {
				wantCold = len(planned)
			}
			if coldInvocations != wantCold {
				t.Fatalf("cold invocations = %d, want %d", coldInvocations, wantCold)
			}

			for _, project := range []string{"a", "b"} {
				if err := os.RemoveAll(filepath.Join(ws.Root, project, ".gen")); err != nil {
					t.Fatal(err)
				}
			}

			warm := runBatchScheduler(ctx, ws, warmPlan, SchedulerConfig{MaxParallel: 2}, newCache())
			if !warm.Success {
				t.Fatalf("warm run failed: %+v", warm.Results)
			}
			if got := len(invocationLog(t, logPath)); got != coldInvocations {
				t.Fatalf("warm invocations = %d, want no additional subprocess (still %d)", got, coldInvocations)
			}
			for _, job := range warmPlan {
				if result := warm.Results[job.Key()]; result == nil || !result.CacheHit {
					t.Fatalf("warm %s = %+v, want a cache hit on the other shape's entry", job.Key(), result)
				}
			}
			for _, project := range []string{"a", "b"} {
				if got, want := generatedText(t, ws, project), "generated for "+project+"\n"; got != want {
					t.Fatalf("restored .gen for %s = %q, want %q", project, got, want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// per-task retry
// ---------------------------------------------------------------------------

// retryBatchFixture builds a two-project batch whose tool reports project a as
// FAILED in the batch answer and succeeds when re-invoked for a single project.
// The solo answer additionally emits a metric no batch answer carries, so a
// test can tell which execution produced the published result.
func retryBatchFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	ws, planned := makeBatchSchedulerFixture(t, "")
	command := filepath.Join(ws.Root, "retry.sh")
	writeExecutable(t, command, batchShellHeader(logPath)+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":["+
		"{\"projectId\":\"/a\",\"status\":\"FAILED\",\"summary\":{\"errors\":1},"+
		"\"diagnostics\":[{\"category\":\"lint/x\",\"severity\":\"error\",\"description\":\"a is broken\"}]},"+
		"{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n"+
		"else\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"metric\",\"name\":\"solo-retry\",\"value\":1,\"unit\":\"count\"}'\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
		"fi\n")
	for _, job := range planned {
		job.JobDef.Command = command
	}
	return ws, planned
}

// The B5a retry decision, executable: a failed batch member re-executes SOLO
// through the ordinary path, its healthy batch-mates are not re-run, and the
// result that gets published (and cached) is the one its OWN execution produced
// — not a slice of the aggregate it failed in.
func TestFailedBatchMemberRetriesSoloAndIsNotReBatched(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := retryBatchFixture(t, logPath)
	localStore := store.NewLocalStore(filepath.Join(t.TempDir(), "store"))
	newCache := func() *store.CacheManager { return store.NewCacheManager(localStore) }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := runBatchScheduler(ctx, ws, planned,
		SchedulerConfig{MaxParallel: 2, Retry: 1}, newCache())
	if !result.Success {
		t.Fatalf("retried batch failed: %+v", result.Results)
	}

	log := invocationLog(t, logPath)
	if len(log) != 2 || !strings.HasPrefix(log[0], "batch:") || log[1] != "solo:a" {
		t.Fatalf("invocations = %v, want one batch then a SOLO retry of the failed member only", log)
	}

	failedJob, healthyJob := planned[0], planned[1]
	if got := result.Results[failedJob.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("retried member = %+v, want success", got)
	}
	if got := result.Results[healthyJob.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("healthy batch-mate = %+v, want success", got)
	}

	task := TaskResultOf(failedJob, result.Results[failedJob.Key()])
	if names := metricNamesOf(task.Metrics); !reflect.DeepEqual(names, []string{"solo-retry"}) {
		t.Fatalf("retried member metrics = %v, want exactly the solo execution's", names)
	}
	if len(task.Diagnostics) != 0 {
		t.Fatalf("retried member kept the failed batch slice's diagnostics: %+v", task.Diagnostics)
	}

	warm := runBatchScheduler(ctx, ws, planned,
		SchedulerConfig{MaxParallel: 2, Retry: 1}, newCache())
	if !warm.Success {
		t.Fatalf("warm run after retry failed: %+v", warm.Results)
	}
	if got := invocationLog(t, logPath); len(got) != 2 {
		t.Fatalf("warm invocations = %v, want no additional subprocess", got)
	}
	for _, job := range planned {
		if got := warm.Results[job.Key()]; got == nil || !got.CacheHit {
			t.Fatalf("warm %s = %+v, want a task-owned cache hit", job.Key(), got)
		}
	}
}

func TestBatchWithoutRetryBudgetNeverReExecutesAMember(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := retryBatchFixture(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := runBatchScheduler(ctx, ws, planned,
		SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, nil)
	if result.Success {
		t.Fatal("a batch whose member failed reported a successful run")
	}
	if got := invocationLog(t, logPath); len(got) != 1 || !strings.HasPrefix(got[0], "batch:") {
		t.Fatalf("invocations = %v, want exactly one shared invocation", got)
	}
	if got := result.Results[planned[0].Key()]; got == nil || got.Status != "failed" {
		t.Fatalf("failed member = %+v, want failed", got)
	}
	if got := result.Results[planned[1].Key()]; got == nil || got.Status != "success" {
		t.Fatalf("healthy batch-mate = %+v, want success (failure isolation)", got)
	}
	task := TaskResultOf(planned[0], result.Results[planned[0].Key()])
	if len(task.Diagnostics) != 1 || task.Diagnostics[0].Message != "a is broken" {
		t.Fatalf("failed member diagnostics = %+v, want the batch slice's", task.Diagnostics)
	}
}

// ---------------------------------------------------------------------------
// solo <-> batch record equivalence
// ---------------------------------------------------------------------------

// equivalenceFixture builds a tool that reports the SAME per-project failure
// through both answer shapes: as a streamed diagnostic event when it runs for
// one project, and as a batch wire slice when it runs for the group. Whatever
// the dispatch shape, the canonical task result a consumer reads must be the
// same records.
func equivalenceFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	ws, planned := makeBatchSchedulerFixture(t, "")
	command := filepath.Join(ws.Root, "equivalent.sh")
	diagnostic := `{"v":2,"type":"diagnostic","severity":"error","message":"broken","code":"lint/x",` +
		`"location":{"file":"src.txt","line":3,"column":4}}`
	writeExecutable(t, command, batchShellHeader(logPath)+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":["+
		"{\"projectId\":\"/a\",\"status\":\"FAILED\",\"diagnostics\":[{\"category\":\"lint/x\",\"severity\":\"error\","+
		"\"description\":\"broken\",\"file\":\"src.txt\",\"line\":3,\"column\":4}]},"+
		"{\"projectId\":\"/b\",\"status\":\"FAILED\",\"diagnostics\":[{\"category\":\"lint/x\",\"severity\":\"error\","+
		"\"description\":\"broken\",\"file\":\"src.txt\",\"line\":3,\"column\":4}]}]}}}'\n"+
		"else\n"+
		"  printf '%s\\n' "+shellQuote(diagnostic)+"\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"FAILED\"}}'\n"+
		"fi\n")
	for _, job := range planned {
		job.JobDef.Command = command
	}
	return ws, planned
}

// Solo and batched dispatch of the same plan produce the same canonical task
// results: same verdict, same diagnostics, same identity on each record.
func TestSoloAndBatchDispatchProduceEqualCanonicalResults(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run := func(solo bool) map[string]TaskResult {
		logPath := filepath.Join(t.TempDir(), "invocations")
		ws, planned := equivalenceFixture(t, logPath)
		if solo {
			planned = forceSoloDispatch(planned)
		}
		result := runBatchScheduler(ctx, ws, planned,
			SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, nil)

		wantInvocations := 1
		if solo {
			wantInvocations = len(planned)
		}
		if got := invocationLog(t, logPath); len(got) != wantInvocations {
			t.Fatalf("solo=%v invocations = %v, want %d", solo, got, wantInvocations)
		}
		tasks := make(map[string]TaskResult, len(planned))
		for _, job := range planned {
			tasks[job.Key()] = TaskResultOf(job, result.Results[job.Key()])
		}
		return tasks
	}

	batched, solo := run(false), run(true)
	if len(batched) != 2 {
		t.Fatalf("batched run produced %d tasks, want 2", len(batched))
	}
	for key, want := range solo {
		got, ok := batched[key]
		if !ok {
			t.Fatalf("batched run produced no task for %s", key)
		}
		if got.Status != want.Status {
			t.Errorf("%s status: batch %q, solo %q", key, got.Status, want.Status)
		}
		if !reflect.DeepEqual(got.Diagnostics, want.Diagnostics) {
			t.Errorf("%s diagnostics:\n batch %+v\n solo  %+v", key, got.Diagnostics, want.Diagnostics)
		}
		if got.Project != want.Project || got.Job != want.Job || got.TaskKind != want.TaskKind {
			t.Errorf("%s identity: batch %+v, solo %+v", key, got, want)
		}
	}
}

// A tool that reports no rule id — `go test`, and every producer that leaves the
// batch wire's `category` empty — must produce the SAME diagnostic event either
// way. The projection used to write `"code": ""` where a solo stream carries no
// `code` key at all, so a member's --output=jsonl record and the entry it cached
// differed from its own solo run by one field (measured against the real
// @putnami/go test task).
func TestSoloAndBatchDiagnosticEventsAgreeOnAnAbsentCode(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run := func(solo bool) []any {
		ws, planned := makeBatchSchedulerFixture(t, "")
		command := filepath.Join(ws.Root, "no-code.sh")
		diagnostic := `{"v":2,"type":"diagnostic","severity":"error","message":"no rule id",` +
			`"location":{"file":"src.txt","line":7,"column":1}}`
		writeExecutable(t, command, "#!/bin/sh\n"+
			"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n"+
			"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":["+
			"{\"projectId\":\"/a\",\"status\":\"FAILED\",\"diagnostics\":[{\"severity\":\"error\","+
			"\"description\":\"no rule id\",\"file\":\"src.txt\",\"line\":7,\"column\":1}]},"+
			"{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n"+
			"else\n"+
			"  if [ \"$PUTNAMI_PROJECT_NAME\" = a ]; then printf '%s\\n' "+shellQuote(diagnostic)+"\n"+
			"    printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"FAILED\"}}'\n"+
			"  else printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
			"  fi\n"+
			"fi\n")
		for _, job := range planned {
			job.JobDef.Command = command
		}
		if solo {
			planned = forceSoloDispatch(planned)
		}
		result := runBatchScheduler(ctx, ws, planned,
			SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, nil)

		// The payloads are compared as opaque values (deeply, so an int that should
		// have been a float64 still fails) rather than re-declaring the event
		// payload type here.
		var data []any
		for _, job := range planned {
			for _, event := range result.Results[job.Key()].Events {
				if event.Type != EventTypeDiagnostic {
					continue
				}
				// A live stream's event payload is the decoded wire object, so it
				// echoes the envelope's own "type" back inside itself; the projection
				// builds the payload and never does. That echo is not a record, so it
				// is normalized away rather than pinned.
				delete(event.Data, "type")
				data = append(data, event.Data)
			}
		}
		return data
	}

	batched, solo := run(false), run(true)
	if len(solo) != 1 {
		t.Fatalf("solo diagnostic events = %d, want 1; the fixture proves nothing", len(solo))
	}
	if strings.Contains(fmt.Sprint(solo[0]), "code:") {
		t.Fatalf("the solo stream itself carries a code (%v); rewrite the fixture", solo[0])
	}
	if !reflect.DeepEqual(batched, solo) {
		t.Fatalf("diagnostic events diverged:\n batch %+v\n solo  %+v", batched, solo)
	}
}

// ---------------------------------------------------------------------------
// attribution
// ---------------------------------------------------------------------------

// A batch answer is attributed to its members through the TYPED identity, which
// is total where the raw field read was not: a node the plan cannot name
// answers "unknown", which no wire slice can claim, so the group fails closed
// instead of panicking or silently adopting a stranger's result.
func TestBatchSplitAttributesMembersByTypedIdentity(t *testing.T) {
	_, planned := makeBatchSchedulerFixture(t, "")
	// Decoded from JSON rather than hand-built, so the split reads the same
	// shape a subprocess actually delivers.
	aggregate := func() *JobResult {
		var data map[string]any
		if err := json.Unmarshal([]byte(
			`{"batchResults":[{"projectId":"/a","status":"OK"},{"projectId":"/b","status":"FAILED"}]}`,
		), &data); err != nil {
			t.Fatal(err)
		}
		return &JobResult{Status: "success", Data: data}
	}

	work := []taskWork{{job: planned[0]}, {job: planned[1]}}
	for i := range work {
		if got, want := batchMemberID(work[i].job), work[i].job.TypedIdentity().Project.ID; got != want {
			t.Fatalf("member id = %q, want the typed identity's project id %q", got, want)
		}
	}
	results, ok := splitBatchResult("", work, aggregate())
	if !ok {
		t.Fatal("a complete batch answer did not split")
	}
	if results[planned[0].Key()].Status != "success" || results[planned[1].Key()].Status != "failed" {
		t.Fatalf("attribution crossed members: %+v", results)
	}

	// A member the wire does not name fails the whole group; no member is served
	// another's slice.
	stranger := *planned[1]
	stranger.Project = &workspace.Project{ID: "/c", Name: "c", Path: "c"}
	if _, ok := splitBatchResult("", []taskWork{{job: planned[0]}, {job: &stranger}}, aggregate()); ok {
		t.Fatal("a batch answer missing a member was accepted")
	}

	// An unnameable node is "unknown", never a panic and never a match.
	unnamed := *planned[1]
	unnamed.Project = nil
	if got := batchMemberID(&unnamed); got != "unknown" {
		t.Fatalf("unnameable member id = %q, want %q", got, "unknown")
	}
	if _, ok := splitBatchResult("", []taskWork{{job: planned[0]}, {job: &unnamed}}, aggregate()); ok {
		t.Fatal("a batch answer was accepted for a member with no identity")
	}
}

// ---------------------------------------------------------------------------
// cancellation and failure isolation
// ---------------------------------------------------------------------------

// A member the batch answer reports as CANCELED must take nothing down with it:
// its batch-mate keeps its own success verdict AND publishes its own task-owned
// entry, while the canceled member publishes none. The proof is the warm run:
// only the canceled member re-executes.
func TestCanceledBatchMemberLeavesSiblingVerdictAndEntryIntact(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := makeBatchSchedulerFixture(t, "")
	command := filepath.Join(ws.Root, "cancel-one.sh")
	writeExecutable(t, command, batchShellHeader(logPath)+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":["+
		"{\"projectId\":\"/a\",\"status\":\"canceled\"},"+
		"{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n"+
		"else\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
		"fi\n")
	for _, job := range planned {
		job.JobDef.Command = command
	}

	localStore := store.NewLocalStore(filepath.Join(t.TempDir(), "store"))
	newCache := func() *store.CacheManager { return store.NewCacheManager(localStore) }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := runBatchScheduler(ctx, ws, planned,
		SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, newCache())
	canceled, healthy := planned[0], planned[1]
	if got := result.Results[canceled.Key()]; got == nil || got.Status != "canceled" {
		t.Fatalf("canceled member = %+v, want canceled", got)
	}
	if got := result.Results[healthy.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("batch-mate of a canceled member = %+v, want its own success", got)
	}
	if got := invocationLog(t, logPath); len(got) != 1 || !strings.HasPrefix(got[0], "batch:") {
		t.Fatalf("invocations = %v, want exactly one shared invocation", got)
	}

	warm := runBatchScheduler(ctx, ws, planned,
		SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, newCache())
	if got := warm.Results[healthy.Key()]; got == nil || !got.CacheHit {
		t.Fatalf("warm batch-mate = %+v, want a task-owned cache hit on the entry it published", got)
	}
	if got := warm.Results[canceled.Key()]; got == nil || got.CacheHit {
		t.Fatalf("warm canceled member = %+v, want a re-execution (a canceled result is never stored)", got)
	}
	if log := invocationLog(t, logPath); len(log) != 2 || log[1] != "solo:a" {
		t.Fatalf("invocations = %v, want the canceled member alone on the second run", log)
	}
}

func TestRunCancellationDuringABatchNeverPublishesAGreenMember(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	ws, planned := makeBatchSchedulerFixture(t, "")
	startedPath := filepath.Join(ws.Root, "started")
	command := filepath.Join(ws.Root, "block.sh")
	writeExecutable(t, command, "#!/bin/sh\n"+
		"printf 'started\\n' > "+shellQuote(startedPath)+"\n"+
		"sleep 30\n"+
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":["+
		"{\"projectId\":\"/a\",\"status\":\"OK\"},{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n")
	for _, job := range planned {
		job.JobDef.Command = command
	}

	storeRoot := filepath.Join(t.TempDir(), "store")
	cacheManager := store.NewCacheManager(store.NewLocalStore(storeRoot))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(startedPath); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()

	result := runBatchScheduler(ctx, ws, planned,
		SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, cacheManager)

	// The cancellation has to have landed on a RUNNING batch; a run cut short
	// before its subprocess started would satisfy every assertion below
	// vacuously.
	if _, err := os.Stat(startedPath); err != nil {
		t.Fatalf("the shared subprocess never started, so nothing was canceled mid-batch: %v", err)
	}
	for _, job := range planned {
		got := result.Results[job.Key()]
		if got == nil {
			t.Fatalf("%s produced no row", job.Key())
		}
		if got.Status == "success" {
			t.Fatalf("%s reported success out of a batch cut short: %+v", job.Key(), got)
		}
	}
	if got := countTaskEntryDescriptors(t, storeRoot); got != 0 {
		t.Fatalf("format-2 entries published by a canceled batch = %d, want 0", got)
	}
}

// A member that FAILS keeps its siblings whole: they publish their own entries
// and hit them on a warm run, while the failed member re-executes. The verdicts
// are identical to the ones the same fixture produces under SOLO dispatch, so
// failure isolation is not a property of the batch answer but of the task
// lifecycle both shapes share.
func TestFailedBatchMemberIsolatedIdenticallyToSoloDispatch(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run := func(solo bool) (map[string]string, int) {
		logPath := filepath.Join(t.TempDir(), "invocations")
		ws, planned := equivalenceFailureFixture(t, logPath)
		if solo {
			planned = forceSoloDispatch(planned)
		}
		storeRoot := filepath.Join(t.TempDir(), "store")
		localStore := store.NewLocalStore(storeRoot)
		newCache := func() *store.CacheManager { return store.NewCacheManager(localStore) }

		cold := runBatchScheduler(ctx, ws, planned,
			SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, newCache())
		statuses := make(map[string]string, len(planned))
		for _, job := range planned {
			statuses[job.Key()] = cold.Results[job.Key()].Status
			if job.Project.ID == "/b" && cold.Results[job.Key()].Status != "success" {
				t.Logf("healthy sibling failed (solo=%v): %+v", solo, cold.Results[job.Key()])
			}
		}

		// The healthy sibling published; the failed one did not. A warm run makes
		// that observable per task rather than through a store walk.
		warm := runBatchScheduler(ctx, ws, planned,
			SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, newCache())
		for _, job := range planned {
			got := warm.Results[job.Key()]
			wantHit := statuses[job.Key()] == "success"
			if got == nil || got.CacheHit != wantHit {
				t.Fatalf("solo=%v warm %s = %+v, want cacheHit=%v", solo, job.Key(), got, wantHit)
			}
		}
		return statuses, countTaskEntryDescriptors(t, storeRoot)
	}

	batchStatuses, batchEntries := run(false)
	soloStatuses, soloEntries := run(true)
	if !reflect.DeepEqual(batchStatuses, soloStatuses) {
		t.Fatalf("verdicts diverged:\n batch %v\n solo  %v", batchStatuses, soloStatuses)
	}
	if got := statusCount(batchStatuses, "failed"); got != 1 {
		t.Fatalf("members that failed = %d, want exactly one (the fixture proves nothing otherwise): %v",
			got, batchStatuses)
	}
	if got := statusCount(batchStatuses, "success"); got != 1 {
		t.Fatalf("members that succeeded beside the failure = %d, want exactly one: %v", got, batchStatuses)
	}
	if batchEntries != soloEntries {
		t.Fatalf("published entries: batch %d, solo %d", batchEntries, soloEntries)
	}
}

// equivalenceFailureFixture is the batch whose member a fails and whose member b
// succeeds, reported through BOTH answer shapes (batch wire and solo result) so
// the same plan can be dispatched either way.
func equivalenceFailureFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	ws, planned := declaredBatchFixture(t, logPath)
	command := filepath.Join(ws.Root, "one-fails.sh")
	writeExecutable(t, command, batchShellHeader(logPath)+
		"targets=\"$PUTNAMI_SELECTED_PROJECT_PATHS\"\n"+
		"if [ -z \"$targets\" ]; then targets=\"$PUTNAMI_PROJECT_NAME\"; fi\n"+
		"IFS=','\n"+
		"for p in $targets; do\n"+
		"  mkdir -p \"$PUTNAMI_WORKSPACE_ROOT/$p/.gen\"\n"+
		"  printf 'generated for %s\\n' \"$p\" > \"$PUTNAMI_WORKSPACE_ROOT/$p/.gen/out.txt\"\n"+
		"done\n"+
		"unset IFS\n"+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":["+
		"{\"projectId\":\"/a\",\"status\":\"FAILED\",\"diagnostics\":[{\"category\":\"build/x\",\"severity\":\"error\","+
		"\"description\":\"a is broken\"}]},{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n"+
		"elif [ \"$PUTNAMI_PROJECT_NAME\" = a ]; then\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"diagnostic\",\"severity\":\"error\",\"message\":\"a is broken\",\"code\":\"build/x\"}'\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"FAILED\"}}'\n"+
		"else\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
		"fi\n")
	for _, job := range planned {
		job.JobDef.Command = command
	}
	return ws, planned
}

// statusCount counts the members that recorded one verdict, so a test can pin
// the SHAPE of an isolation outcome (one failure beside one success) without
// hard-coding a plan index.
func statusCount(statuses map[string]string, want string) int {
	count := 0
	for _, status := range statuses {
		if status == want {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// mutating lint: the FIX WRITES a batch member makes
// ---------------------------------------------------------------------------

// fixWriteFixture builds a source-rewriting lint whose tool appends a fix
// marker to each project's source. One code path serves both dispatch shapes:
// it rewrites every selected project, or its own when there is no selection.
func fixWriteFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	ws, planned := makeBatchSchedulerFixture(t, "")
	command := filepath.Join(ws.Root, "fix.sh")
	writeExecutable(t, command, batchShellHeader(logPath)+
		"targets=\"$PUTNAMI_SELECTED_PROJECT_PATHS\"\n"+
		"if [ -z \"$targets\" ]; then targets=\"$PUTNAMI_PROJECT_NAME\"; fi\n"+
		"IFS=','\n"+
		"for p in $targets; do\n"+
		"  printf 'fixed\\n' >> \"$PUTNAMI_WORKSPACE_ROOT/$p/src.txt\"\n"+
		"done\n"+
		"unset IFS\n"+
		"if [ -n \"$PUTNAMI_SELECTED_PROJECTS\" ]; then\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":["+
		"{\"projectId\":\"/a\",\"status\":\"OK\"},{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n"+
		"else\n"+
		"  printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
		"fi\n")
	task := planned[0].Extension.Tasks["lint-all"]
	task.Declares = &extension.TaskDeclaration{MutatesSources: true}
	planned[0].Extension.Tasks["lint-all"] = task
	for _, job := range planned {
		job.JobDef.Command = command
		job.JobDef.Writes = []extension.ResourceRef{{
			ID: sourcesResourceID, Scope: extension.ResourceScopeProject,
		}}
	}
	return ws, planned
}

// A batched `lint --fix` must leave the worktree in the state solo dispatch
// leaves it in — every member's own file rewritten exactly once — and must
// carry the same source-mutation verdict into the cache, so neither shape can
// serve a green result for a tree it never fixed.
func TestBatchAndSoloFixWritesAreEquivalent(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run := func(solo bool) (map[string]string, map[string]bool) {
		logPath := filepath.Join(t.TempDir(), "invocations")
		ws, planned := fixWriteFixture(t, logPath)
		if solo {
			planned = forceSoloDispatch(planned)
		}
		cacheManager := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))
		result := runBatchScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, cacheManager)
		if !result.Success {
			t.Fatalf("solo=%v fix run failed: %+v", solo, result.Results)
		}
		wantInvocations := 1
		if solo {
			wantInvocations = len(planned)
		}
		if got := invocationLog(t, logPath); len(got) != wantInvocations {
			t.Fatalf("solo=%v invocations = %v, want %d", solo, got, wantInvocations)
		}

		sources := make(map[string]string, len(planned))
		mutated := make(map[string]bool, len(planned))
		for _, job := range planned {
			data, err := os.ReadFile(filepath.Join(ws.Root, job.Project.Path, "src.txt"))
			if err != nil {
				t.Fatal(err)
			}
			// Keyed by project rather than by job key so the two shapes compare on
			// the worktree they produced, not on plan identity.
			sources[job.Project.Path] = string(data)
			mutated[job.Project.Path] = result.Results[job.Key()].SourceMutated
		}
		return sources, mutated
	}

	batchSources, batchMutated := run(false)
	soloSources, soloMutated := run(true)

	for project, want := range soloSources {
		if got := batchSources[project]; got != want {
			t.Errorf("%s/src.txt: batch %q, solo %q", project, got, want)
		}
		if !strings.HasSuffix(want, "fixed\n") {
			t.Errorf("%s/src.txt was never fixed (%q); the fixture proves nothing", project, want)
		}
	}
	if !reflect.DeepEqual(batchMutated, soloMutated) {
		t.Fatalf("source-mutation verdicts diverged:\n batch %v\n solo  %v", batchMutated, soloMutated)
	}
	for project, mutated := range batchMutated {
		if !mutated {
			t.Errorf("%s source rewrite was not marked as mutated", project)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func metricNamesOf(metrics []TaskMetric) []string {
	names := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		names = append(names, metric.Name)
	}
	sort.Strings(names)
	return names
}
