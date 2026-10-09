package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

type batchCompletenessRenderer struct {
	mockRenderer
	batchEvents []RawJobEvent
}

func TestGoEmbedInvalidBatchMemberNeverReachesSharedProcess(t *testing.T) {
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "batch-invocations")
	ws, jobs := makeBatchSchedulerFixture(t, logPath)
	writeTestFile(t, filepath.Join(ws.Root, "a", "main.go"), "package a\nimport _ \"embed\"\n//go:embed missing.txt\nvar payload string\n")
	writeTestFile(t, filepath.Join(ws.Root, "b", "main.go"), "package b\n")
	for _, job := range jobs {
		job.JobDef.FilePatterns = []string{"**/*.go", "go-embed:build"}
	}
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))
	result := runSharedScheduler(context.Background(), ws, jobs, SchedulerConfig{MaxParallel: 2, ContinueOnError: true}, cache)
	a, b := result.Results[jobs[0].Key()], result.Results[jobs[1].Key()]
	if a == nil || a.Status != string(TaskStatusFailed) || a.Error == nil || !strings.Contains(a.Error.Message, "missing.txt") {
		t.Fatalf("invalid batch member did not fail before execution: %+v", a)
	}
	if b == nil || b.Status != "success" {
		t.Fatalf("valid batch member did not execute: %+v", b)
	}
	if got := invocationCount(t, logPath); got != 1 {
		t.Fatalf("shared process ran %d times; invalid member must not execute", got)
	}
}

func (r *batchCompletenessRenderer) BatchJobEvent(_ *ScheduledJob, event RawJobEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batchEvents = append(r.batchEvents, event)
}

func (r *batchCompletenessRenderer) BatchMemberJobEvent(job *ScheduledJob, event RawJobEvent) {
	r.JobEvent(job, event)
}

func TestNoisyBatchStreamReachesSessionRendererBeforePerTaskRetention(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	ws, planned := makeBatchSchedulerFixture(t, "")
	command := filepath.Join(ws.Root, "noisy-batch.sh")
	writeExecutable(t, command, `#!/bin/sh
i=0
while [ "$i" -lt 900 ]; do
  printf '{"v":2,"type":"log","level":"info","message":"raw-batch-%s"}\n' "$i"
  i=$((i + 1))
done
printf '%s\n' '{"v":2,"type":"result","data":{"status":"OK","data":{"batchResults":[{"projectId":"/a","status":"OK"},{"projectId":"/b","status":"OK"}]}}}'
`)
	for _, job := range planned {
		job.JobDef.Command = command
	}
	renderer := &batchCompletenessRenderer{}
	result := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 2}, renderer, nil).Run(context.Background())
	if !result.Success {
		t.Fatalf("noisy batch failed: %+v", result.Results)
	}
	if got := len(renderer.batchEvents); got != 901 {
		t.Fatalf("physical batch events = %d, want all 900 logs plus result before retention", got)
	}
	for jobKey, events := range renderer.jobEvents {
		for _, event := range events {
			if strings.HasPrefix(event.Message, "raw-batch-") {
				t.Fatalf("member %s live projection duplicated physical event %q", jobKey, event.Message)
			}
		}
	}
}

func TestBatchMemberProjectionIsBoundedWithoutTruncatingCanonicalTask(t *testing.T) {
	ws, planned := makeBatchSchedulerFixture(t, "")
	diagnostics := make([]batchWireDiagnostic, 0, 1101)
	for i := 0; i < 1100; i++ {
		diagnostics = append(diagnostics, batchWireDiagnostic{Severity: "info", Description: "ordinary"})
	}
	diagnostics = append(diagnostics, batchWireDiagnostic{Severity: "error", Description: "late failure"})
	result := jobResultFromBatchWire(ws.Root, planned[0], batchWireResult{
		ProjectID:   planned[0].Project.ID,
		Status:      "FAILED",
		Diagnostics: diagnostics,
	}, &JobResult{}, nil)
	if result.Canonical == nil || len(result.Canonical.Diagnostics) != len(diagnostics) {
		t.Fatalf("canonical diagnostics = %d, want complete %d", len(result.Canonical.Diagnostics), len(diagnostics))
	}
	if len(result.Events) >= len(diagnostics) {
		t.Fatalf("retained events = %d, want a bounded projection below %d", len(result.Events), len(diagnostics))
	}
	foundLateFailure := false
	for _, event := range result.Events {
		if event.Type == EventTypeDiagnostic && event.Data["message"] == "late failure" {
			foundLateFailure = true
		}
	}
	if !foundLateFailure {
		t.Fatal("bounded batch projection dropped late failure evidence")
	}
}

func TestTakePendingGroupUsesResolvedConfigAndOptIn(t *testing.T) {
	ws, jobs := makeBatchSchedulerFixture(t, "")
	scheduler := newScheduler(ws, jobs, nil, SchedulerConfig{}, &mockRenderer{}, nil)

	keyBefore := scheduler.readyBatchKey(jobs[0])
	group, rest := scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 2 || len(rest) != 0 {
		t.Fatalf("same-config opted-in jobs grouped as %d + %d, want 2 + 0", len(group.jobs), len(rest))
	}

	writeTestFile(t, filepath.Join(ws.Root, "biome.json"), `{"formatter":{"enabled":false}}`)
	if keyAfter := scheduler.readyBatchKey(jobs[0]); keyAfter == keyBefore {
		t.Fatal("batch key did not change with shared config content")
	}
	writeTestFile(t, filepath.Join(ws.Root, "biome.json"), `{"formatter":{"enabled":true}}`)

	// Identical copied bytes are still a different resolved source: relative
	// imports inside a project-local config can have different semantics.
	writeTestFile(t, filepath.Join(ws.Root, jobs[1].Project.Path, "biome.json"), `{"formatter":{"enabled":true}}`)
	group, rest = scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 1 || len(rest) != 1 {
		t.Fatalf("different resolved configs grouped as %d + %d, want 1 + 1", len(group.jobs), len(rest))
	}

	if err := os.Remove(filepath.Join(ws.Root, jobs[1].Project.Path, "biome.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(ws.Root, jobs[1].Project.Path, "src.txt")); err != nil {
		t.Fatal(err)
	}
	if key := scheduler.readyBatchKey(jobs[1]); key != "" {
		t.Fatalf("project without matching inputs received batch key %q", key)
	}
	writeTestFile(t, filepath.Join(ws.Root, jobs[1].Project.Path, "src.txt"), "b")

	jobs[0].JobDef.Batchable = nil
	jobs[1].JobDef.Batchable = nil
	group, rest = scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 1 || len(rest) != 1 {
		t.Fatalf("manifest without trait changed singleton dispatch: %d + %d", len(group.jobs), len(rest))
	}
}

// A lone batch candidate that carries the queue's longest critical path must
// dispatch immediately instead of yielding: every peer the wait could recruit
// completes inside a shorter chain, so waiting only delays the session's long
// pole (measured ~30s of queueing for the CLI suite on the repo gate). A cold
// store — every CriticalPathMs zero — keeps the historical yield-to-any.
func TestTakePendingGroupLongPoleCandidateDoesNotYield(t *testing.T) {
	ws, jobs := makeBatchSchedulerFixture(t, "")
	scheduler := newScheduler(ws, jobs, nil, SchedulerConfig{}, &mockRenderer{}, nil)
	jobs[1].JobDef.Batchable = nil // ordinary short-tail work behind the candidate

	jobs[0].CriticalPathMs = 90000
	jobs[1].CriticalPathMs = 5000
	group, rest := scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 1 || group.jobs[0] != jobs[0] || len(rest) != 1 {
		t.Fatalf("long-pole candidate yielded: group=%v rest=%d, want solo dispatch of %s",
			keysOf(group.jobs), len(rest), jobs[0].Key())
	}

	// Equal chains (including the all-zero cold store) keep the yield: the
	// ordinary job may be the last prerequisite of a compatible peer.
	for _, chain := range []int64{0, 90000} {
		jobs[0].CriticalPathMs = chain
		jobs[1].CriticalPathMs = chain
		group, rest = scheduler.takePendingGroup(jobs)
		if len(group.jobs) != 1 || group.jobs[0] != jobs[1] || len(rest) != 1 {
			t.Fatalf("chain=%d: candidate did not yield to equal-chain ordinary work: group=%v",
				chain, keysOf(group.jobs))
		}
	}
}

func TestReadyBatchKeyRespectsMaxWorkersEconomicsBound(t *testing.T) {
	ws, jobs := makeBatchSchedulerFixture(t, "")
	jobs[0].JobDef.Batchable.MaxWorkers = 1

	scheduler := newScheduler(ws, jobs, nil, SchedulerConfig{}, &mockRenderer{}, nil)
	scheduler.workerCount = 2
	if key := scheduler.readyBatchKey(jobs[0]); key != "" {
		t.Fatalf("batch key with two workers = %q, want singleton dispatch", key)
	}

	scheduler.workerCount = 1
	if key := scheduler.readyBatchKey(jobs[0]); key == "" {
		t.Fatal("single-worker scheduler did not enable bounded batch policy")
	}
	group, rest := scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 2 || len(rest) != 0 {
		t.Fatalf("bounded single-worker group = %d + %d, want 2 + 0", len(group.jobs), len(rest))
	}
}

func TestBatchClassCPUBudgetStableAcrossSingletonAndRuntimeCohorts(t *testing.T) {
	ws, planned := makeBatchSchedulerFixture(t, "")
	if err := os.MkdirAll(filepath.Join(ws.Root, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(ws.Root, "c", "src.txt"), "c")
	projectC := &workspace.Project{ID: "/c", Name: "c", Path: "c"}
	jobC := *planned[0]
	jobC.Project = projectC
	planned = append(planned, &jobC)
	ws = workspace.NewWorkspace(ws.Root, nil, []*workspace.Project{
		planned[0].Project,
		planned[1].Project,
		projectC,
	})
	ws.Name = "batch-test"

	// All three jobs have weight 2. Their own history recommends 2, 4, and 6
	// cores, so the compatible class's fixed maximum is 6.
	history := loadTaskStats(ws.Root)
	for i, job := range planned {
		stat := &taskStat{
			CPUMs:                    100,
			WallMs:                   100,
			MaxUnweightedConcurrency: i + 1,
			Samples:                  1,
		}
		if i == 0 {
			// This member recommends 2 on its own, but it has evidence that the
			// class ceiling of 6 can occupy 4 CPUs. Admission must follow the
			// actual fixed ceiling rather than retain the pre-class request of 1.
			stat.Observations = map[string]*taskResourceObservation{
				"2x1": {
					CPUMs: 100, WallMs: 100, GrantedConcurrency: 2,
					BatchSize: 1, Samples: 4, Confidence: 1,
				},
				"6x1": {
					CPUMs: 400, WallMs: 100, GrantedConcurrency: 6,
					BatchSize: 1, Samples: 4, Confidence: 1,
				},
			}
		}
		history.entries[taskStatKey(job)] = stat
	}
	history.dirty = true
	history.save()
	// Pinned to the measured policy on purpose: this test is about the CLASS
	// mechanism (every compatible member shares one fixed ceiling), which is
	// policy-independent. All three fixtures have identical wall history, so
	// under critical-path they would each be "as long as the longest" and take
	// the whole machine — a correct grant that would leave nothing for the class
	// maximum to distinguish. Criticality itself is covered in cpu_allocator_test.go.
	scheduler := &Scheduler{
		ws:       ws,
		planned:  planned,
		renderer: &mockRenderer{},
		cfg:      SchedulerConfig{CPUBudgetPolicy: string(cpuBudgetPolicyMeasured)},
	}
	scheduler.prepareScheduling(ParallelDecision{LogicalCPU: 8})
	for _, job := range planned {
		if got := scheduler.batchCPUBudgetByJob[job.Key()]; got != 6 {
			t.Fatalf("fixed class budget for %s = %d, want 6", job.Key(), got)
		}
	}
	if got := scheduler.resourcePlanByJob[planned[0].Key()].cpu; got != 4 {
		t.Fatalf("class-shaped CPU reservation = %d, want observed occupancy 4 at ceiling 6", got)
	}

	// A dispatch that happens to contain only the lightest member still uses the
	// class ceiling, but persists that member's own pre-weight ceiling (1), not 6.
	scheduler.acquireGroupCPUBudget([]taskWork{{job: planned[0]}})
	singletonBudget := planned[0].CPUBudget
	if singletonBudget != 6 || !planned[0].CPUBudgetHistoryEligible ||
		planned[0].CPUBudgetUnweightedCeiling != 1 {
		t.Fatalf("singleton tuning = budget %d eligible=%t unweighted=%d, want 6/true/1",
			planned[0].CPUBudget, planned[0].CPUBudgetHistoryEligible, planned[0].CPUBudgetUnweightedCeiling)
	}
	stats := loadTaskStats(t.TempDir())
	stats.record(planned[0], successResult(time.Second, time.Second))
	if got := stats.entries[taskStatKey(planned[0])].MaxUnweightedConcurrency; got != 1 {
		t.Fatalf("class-inflated singleton persisted ceiling %d, want own unweighted ceiling 1", got)
	}

	for _, indexes := range [][]int{{0, 1}, {0, 2}, {1, 2}, {0, 1, 2}} {
		work := make([]taskWork, 0, len(indexes))
		for _, index := range indexes {
			work = append(work, taskWork{job: planned[index]})
		}
		scheduler.acquireGroupCPUBudget(work)
		leader := batchLeader(work)
		if leader.CPUBudget != singletonBudget {
			t.Fatalf("cohort %v physical budget = %d, want singleton budget %d", indexes, leader.CPUBudget, singletonBudget)
		}

		aggregate := &JobResult{Execution: &Execution{Concurrency: leader.CPUBudget}}
		results := make(map[string]*JobResult, len(work))
		for _, member := range work {
			results[member.job.Key()] = &JobResult{}
		}
		attributeSharedExecution(work, results, aggregate)
		for _, member := range work {
			result := results[member.job.Key()]
			if result.Execution == nil || result.Execution.Concurrency != singletonBudget {
				t.Fatalf("cohort %v task %s execution = %+v, want concurrency %d",
					indexes, member.job.Key(), result.Execution, singletonBudget)
			}
			if member.job.CPUBudgetHistoryEligible || member.job.CPUBudgetUnweightedCeiling != 0 {
				t.Fatalf("cohort %v task %s became history eligible", indexes, member.job.Key())
			}
		}
	}
}

func TestTakePendingGroupPreservesLongestFirstPriority(t *testing.T) {
	ws, batchJobs := makeBatchSchedulerFixture(t, "")
	scheduler := newScheduler(ws, batchJobs, nil, SchedulerConfig{}, &mockRenderer{}, nil)

	ordinary := *batchJobs[0]
	ordinary.Project = &workspace.Project{ID: "/ordinary", Name: "ordinary", Path: "ordinary"}
	ordinaryDef := *batchJobs[0].JobDef
	ordinaryDef.Batchable = nil
	ordinaryDef.FilePatterns = nil
	ordinary.JobDef = &ordinaryDef

	group, rest := scheduler.takePendingGroup([]*ScheduledJob{&ordinary, batchJobs[0], batchJobs[1]})
	if len(group.jobs) != 1 || group.jobs[0] != &ordinary {
		t.Fatalf("first dispatch = %v, want longest-ready ordinary job", group.jobs)
	}
	if len(rest) != 2 || rest[0] != batchJobs[0] || rest[1] != batchJobs[1] {
		t.Fatalf("first remainder = %v, want both batch peers in queue order", rest)
	}

	group, rest = scheduler.takePendingGroup(rest)
	if len(group.jobs) != 2 || group.jobs[0] != batchJobs[0] || group.jobs[1] != batchJobs[1] {
		t.Fatalf("second dispatch = %v, want both batch peers", group.jobs)
	}
	if len(rest) != 0 {
		t.Fatalf("second remainder = %v, want empty", rest)
	}

	group, rest = scheduler.takePendingGroup([]*ScheduledJob{batchJobs[0], &ordinary})
	if len(group.jobs) != 1 || group.jobs[0] != &ordinary {
		t.Fatalf("singleton dispatch = %v, want useful ordinary work", group.jobs)
	}
	if len(rest) != 1 || rest[0] != batchJobs[0] {
		t.Fatalf("singleton remainder = %v, want deferred batch candidate", rest)
	}
}

func TestProjectPathsOverlap(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want bool
	}{
		{a: ".", b: "packages/a", want: true},
		{a: "packages", b: "packages/a", want: true},
		{a: "packages/a", b: "packages/a", want: true},
		{a: "packages/a", b: "packages/b", want: false},
		{a: "apps/a", b: "packages/a", want: false},
	} {
		if got := projectPathsOverlap(test.a, test.b); got != test.want {
			t.Errorf("projectPathsOverlap(%q, %q) = %t, want %t", test.a, test.b, got, test.want)
		}
	}
}

func TestBatchResultEventsRenderGoTestMetricsWithoutLintMetrics(t *testing.T) {
	ws, planned := makeBatchSchedulerFixture(t, "")
	job := planned[0]
	job.JobDef.CommandName = "test"
	job.JobDef.Name = "test~test"
	job.JobDef.Args = []string{"test"}

	result := jobResultFromBatchWire(ws.Root, job, batchWireResult{
		ProjectID: job.Project.ID,
		Status:    "OK",
		Data: map[string]any{
			"testSummary": map[string]any{
				"passed":  float64(3),
				"failed":  float64(0),
				"skipped": float64(1),
				"total":   float64(4),
			},
			"coverageSummary": map[string]any{
				"percentage":        float64(75),
				"totalStatements":   float64(8),
				"coveredStatements": float64(6),
			},
		},
		Artifacts: []batchWireArtifact{{
			ID: "coverage", Name: "Coverage Profile", Kind: "coverage", Path: ".putnami/out/a/test/coverage.out",
		}},
	}, &JobResult{Duration: time.Second}, nil)

	metricNames := make(map[string]bool)
	var summary string
	var artifactPath string
	for _, event := range result.Events {
		if event.Type == EventTypeMetric {
			if name, _ := event.Data["name"].(string); name != "" {
				metricNames[name] = true
			}
		}
		if event.Type == EventTypeSummary {
			summary = event.Message
		}
		if event.Type == EventTypeArtifact {
			artifactPath, _ = event.Data["path"].(string)
		}
	}
	for _, want := range []string{
		"tests-total",
		"tests-passed",
		"tests-failed",
		"tests-skipped",
		"coverage",
		"coverage-statements-total",
		"coverage-statements-covered",
	} {
		if !metricNames[want] {
			t.Errorf("missing test metric %q in %v", want, metricNames)
		}
	}
	if metricNames["lint-errors"] {
		t.Fatalf("Go test result emitted lint metrics: %v", metricNames)
	}
	if summary != "3/4 passed, 1 skipped, 75.0% coverage" {
		t.Fatalf("summary = %q", summary)
	}
	if artifactPath != ".putnami/out/a/test/coverage.out" {
		t.Fatalf("artifact path = %q", artifactPath)
	}
}

func TestSchedulerBatchPreservesRowsCachesAndScalesCPUWeight(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := makeBatchSchedulerFixture(t, logPath)
	localStore := store.NewLocalStore(filepath.Join(t.TempDir(), "store"))
	newCache := func() *store.CacheManager {
		return store.NewCacheManager(localStore)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	coldRenderer := &mockRenderer{}
	coldScheduler := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 2}, coldRenderer, newCache())
	var sessionEnds []string
	coldScheduler.setSessionEventHandler(func(record SessionRecord) {
		if record.Type == SessionRecordJobEnd {
			sessionEnds = append(sessionEnds, record.JobKey)
		}
	})
	cold := coldScheduler.Run(ctx)
	if !cold.Success {
		t.Fatalf("cold batch failed: %+v", cold.Results)
	}
	if got := invocationCount(t, logPath); got != 1 {
		t.Fatalf("cold invocation count = %d, want one batch", got)
	}
	if len(cold.Results) != 2 {
		t.Fatalf("cold result rows = %d, want 2", len(cold.Results))
	}
	if len(sessionEnds) != 2 || sessionEnds[0] == sessionEnds[1] {
		t.Fatalf("session terminal rows = %v, want one per project", sessionEnds)
	}
	if len(cold.Tuning.CPUBudgets) != 1 || cold.Tuning.CPUBudgets[0].Weight != 4 {
		t.Fatalf("batch CPU grants = %+v, want one grant with summed weight 4", cold.Tuning.CPUBudgets)
	}

	warm := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 2}, &mockRenderer{}, newCache()).Run(ctx)
	if !warm.Success {
		t.Fatalf("warm batch failed: %+v", warm.Results)
	}
	if got := invocationCount(t, logPath); got != 1 {
		t.Fatalf("warm invocation count = %d, want no additional subprocess", got)
	}
	for _, job := range planned {
		if result := warm.Results[job.Key()]; result == nil || !result.CacheHit {
			t.Fatalf("warm %s = %+v, want independent task-owned cache hit", job.Key(), result)
		}
	}

	writeTestFile(t, filepath.Join(ws.Root, "a", "src.txt"), "a2")
	mixed := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 2}, &mockRenderer{}, newCache()).Run(ctx)
	if !mixed.Success {
		t.Fatalf("mixed batch failed: %+v", mixed.Results)
	}
	if got := invocationCount(t, logPath); got != 2 {
		t.Fatalf("mixed invocation count = %d, want one additional miss", got)
	}
	if mixed.Results["/a:lint~check-only"].CacheHit {
		t.Fatal("changed project a unexpectedly hit cache")
	}
	if !mixed.Results["/b:lint~check-only"].CacheHit {
		t.Fatal("unchanged project b lost its independent cache hit")
	}
}

func TestSchedulerManifestWithoutBatchTraitKeepsSingletonExecution(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := makeBatchSchedulerFixture(t, logPath)
	for _, job := range planned {
		job.JobDef.Batchable = nil
		job.JobDef.Cache = false
	}

	result := newScheduler(
		ws,
		planned,
		nil,
		SchedulerConfig{MaxParallel: 2},
		&mockRenderer{},
		nil,
	).Run(context.Background())
	if !result.Success {
		t.Fatalf("singleton run failed: %+v", result.Results)
	}
	if got := invocationCount(t, logPath); got != 2 {
		t.Fatalf("manifest without trait invoked %d subprocesses, want historical one per job", got)
	}
	if len(result.Results) != 2 {
		t.Fatalf("manifest without trait produced %d rows, want 2", len(result.Results))
	}
}

func TestSchedulerBatchCompletesFixGroupBeforeCheckGroup(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "phases")
	ws, formats := makeBatchSchedulerFixture(t, "")
	batchResult := `{"v":2,"type":"result","data":{"status":"OK","data":{"batchResults":[{"projectId":"/a","status":"OK"},{"projectId":"/b","status":"OK"}]}}}`
	formatCommand := filepath.Join(ws.Root, "format.sh")
	checkCommand := filepath.Join(ws.Root, "check.sh")
	writeExecutable(t, formatCommand, "#!/bin/sh\n"+
		"printf 'format\\n' >> "+shellQuote(logPath)+"\n"+
		"printf '%s\\n' "+shellQuote(batchResult)+"\n")
	writeExecutable(t, checkCommand, "#!/bin/sh\n"+
		"printf 'check\\n' >> "+shellQuote(logPath)+"\n"+
		"printf '%s\\n' "+shellQuote(batchResult)+"\n")

	planned := make([]*ScheduledJob, 0, len(formats)*2)
	for _, format := range formats {
		format.JobDef.Command = formatCommand
		format.JobDef.Name = "lint~format"
		format.JobDef.StepID = "format"
		format.JobDef.Args = []string{"lint-format"}
		format.Step = &extension.PipelineStep{ID: "format", Task: "lint-format"}

		check := *format
		checkDef := *format.JobDef
		check.JobDef = &checkDef
		check.JobDef.Command = checkCommand
		check.JobDef.Name = "lint~check"
		check.JobDef.StepID = "check"
		check.JobDef.Args = []string{"lint-check"}
		check.Step = &extension.PipelineStep{ID: "check", Task: "lint-check"}
		check.DependsOn = []string{format.Key()}
		planned = append(planned, format, &check)
	}

	result := newScheduler(
		ws,
		planned,
		nil,
		SchedulerConfig{MaxParallel: 2},
		&mockRenderer{},
		nil,
	).Run(context.Background())
	if !result.Success {
		t.Fatalf("fix pipeline failed: %+v", result.Results)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(data)); len(got) != 2 || got[0] != "format" || got[1] != "check" {
		t.Fatalf("batch phase order = %v, want [format check]", got)
	}
	if len(result.Results) != 4 {
		t.Fatalf("fix pipeline rows = %d, want 4", len(result.Results))
	}
}

func TestSchedulerDoesNotPrequeuePastWorkerCapacityBeforeBatchPeersAreReady(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, tests := makeBatchSchedulerFixture(t, logPath)
	prepCommand := filepath.Join(ws.Root, "prepare.sh")
	writeExecutable(t, prepCommand,
		"#!/bin/sh\nprintf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n")

	planned := make([]*ScheduledJob, 0, len(tests)*2)
	for _, testJob := range tests {
		prep := &ScheduledJob{
			Project:   testJob.Project,
			Extension: testJob.Extension,
			Step:      &extension.PipelineStep{ID: "prepare", Task: "prepare"},
			JobDef: &extension.JobDefinition{
				ExtensionName: testJob.Extension.Name,
				Name:          "lint~prepare",
				CommandName:   "lint",
				StepID:        "prepare",
				Command:       prepCommand,
				Cwd:           "{projectRoot}",
			},
		}
		testJob.DependsOn = []string{prep.Key()}
		planned = append(planned, prep, testJob)
	}

	result := newScheduler(
		ws,
		planned,
		nil,
		SchedulerConfig{MaxParallel: 1, NoCache: true},
		&mockRenderer{},
		nil,
	).Run(context.Background())
	if !result.Success {
		t.Fatalf("dependency-backed batch failed: %+v", result.Results)
	}
	if got := invocationCount(t, logPath); got != 1 {
		t.Fatalf("batch invocations = %d, want one after both prerequisites", got)
	}
	if len(result.Results) != 4 {
		t.Fatalf("result rows = %d, want two prerequisites plus two split tests", len(result.Results))
	}
}

func TestSplitBatchResultAttributesDiagnosticsPerProject(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-test-output", "attributed-events-survive-batch-splitting")
	_, planned := makeBatchSchedulerFixture(t, "")
	work := []taskWork{{job: planned[0]}, {job: planned[1]}}
	aggregate := &JobResult{
		Status: "success",
		Data: map[string]any{BatchResultsDataKey: []any{
			map[string]any{
				"projectId": "/a",
				"status":    "FAILED",
				"diagnostics": []any{map[string]any{
					"category":    "lint/a",
					"severity":    "error",
					"description": "a failed",
					"file":        "a/src.ts",
				}},
				"summary": map[string]any{"errors": 1},
			},
			map[string]any{"projectId": "/b", "status": "OK"},
		}},
		Duration: 10 * time.Millisecond,
	}

	results, ok := splitBatchResult("", work, aggregate)
	if !ok {
		t.Fatal("valid batch result did not split")
	}
	if results[planned[0].Key()].Status != "failed" || results[planned[1].Key()].Status != "success" {
		t.Fatalf("split statuses = a:%s b:%s", results[planned[0].Key()].Status, results[planned[1].Key()].Status)
	}
	var aDiagnostics, bDiagnostics int
	for _, event := range results[planned[0].Key()].Events {
		if event.Type == EventTypeDiagnostic {
			aDiagnostics++
		}
	}
	for _, event := range results[planned[1].Key()].Events {
		if event.Type == EventTypeDiagnostic {
			bDiagnostics++
		}
	}
	if aDiagnostics != 1 || bDiagnostics != 0 {
		t.Fatalf("diagnostic attribution = a:%d b:%d, want 1/0", aDiagnostics, bDiagnostics)
	}
}

func TestSplitBatchResultReattachesOnlyOwnedTestTranscripts(t *testing.T) {
	_, planned := makeBatchSchedulerFixture(t, "")
	for _, job := range planned {
		job.JobDef.CommandName = "test"
		job.JobDef.Name = "test~test"
		job.JobDef.Args = []string{"test"}
	}
	work := []taskWork{{job: planned[0]}, {job: planned[1]}}
	aggregate := &JobResult{
		Status: "success",
		Data: map[string]any{BatchResultsDataKey: []any{
			map[string]any{"projectId": "/a", "status": "OK"},
			map[string]any{"projectId": "/b", "status": "OK"},
		}},
		Events: []RawJobEvent{
			{Version: 2, Type: EventTypeLog, Level: "debug", Message: "go: TestA passed", Data: map[string]any{
				"context": map[string]any{protocolcli.BatchProjectLogContextKey: "/a"},
			}},
			{Version: 2, Type: EventTypeLog, Level: "debug", Message: "bun: test B passed", Data: map[string]any{
				"context": map[string]any{protocolcli.BatchProjectLogContextKey: "/b"},
			}},
			{Version: 2, Type: EventTypeLog, Level: "debug", Message: "shared toolchain failure", Data: map[string]any{
				"context": map[string]any{protocolcli.BatchProjectLogsContextKey: []any{"/a", "/b"}},
			}},
			{Version: 2, Type: EventTypeLog, Level: "info", Message: "unattributed batch note"},
		},
	}

	results, ok := splitBatchResult("", work, aggregate)
	if !ok {
		t.Fatal("valid mixed-runtime batch result did not split")
	}
	for i, test := range []struct {
		want string
		not  string
	}{
		{want: "go: TestA passed", not: "bun: test B passed"},
		{want: "bun: test B passed", not: "go: TestA passed"},
	} {
		var messages []string
		transcriptIndex, phaseEndIndex := -1, -1
		for eventIndex, event := range results[planned[i].Key()].Events {
			if event.Type == EventTypePhase && event.Data["action"] == "end" {
				phaseEndIndex = eventIndex
			}
			if event.Type != EventTypeLog {
				continue
			}
			messages = append(messages, event.Message)
			if event.Message == test.want {
				transcriptIndex = eventIndex
				if event.Level != "debug" {
					t.Fatalf("project %d transcript level = %q, want debug", i, event.Level)
				}
			}
			if contextData, hasContext := event.Data["context"].(map[string]any); hasContext {
				if _, tagged := contextData[protocolcli.BatchProjectLogContextKey]; tagged {
					t.Fatalf("project %d retained internal routing tag: %+v", i, event)
				}
				if _, tagged := contextData[protocolcli.BatchProjectLogsContextKey]; tagged {
					t.Fatalf("project %d retained shared routing tag: %+v", i, event)
				}
			}
		}
		joined := strings.Join(messages, "\n")
		if !strings.Contains(joined, test.want) || strings.Contains(joined, test.not) || strings.Contains(joined, "unattributed") {
			t.Fatalf("project %d logs = %q, want only %q transcript", i, joined, test.want)
		}
		if !strings.Contains(joined, "shared toolchain failure") {
			t.Fatalf("project %d lost shared group transcript: %q", i, joined)
		}
		if transcriptIndex < 0 || phaseEndIndex < 0 || transcriptIndex >= phaseEndIndex {
			t.Fatalf("project %d transcript index=%d phase-end=%d; transcript must survive in JobResult.Events before phase end", i, transcriptIndex, phaseEndIndex)
		}
	}
}

func TestBatchResultZeroCountFailureEmitsFailedRecap(t *testing.T) {
	ws, planned := makeBatchSchedulerFixture(t, "")
	job := planned[0]
	job.JobDef.CommandName = "test"
	job.JobDef.Name = "test~test"
	job.JobDef.Args = []string{"test"}
	result := jobResultFromBatchWire(ws.Root, job, batchWireResult{
		ProjectID: job.Project.ID,
		Status:    "FAILED",
		Data: map[string]any{"testSummary": map[string]any{
			"passed": 0, "failed": 0, "skipped": 0, "total": 0,
		}},
	}, &JobResult{Duration: time.Second}, nil)

	var summary string
	for _, event := range result.Events {
		if event.Type == EventTypeSummary {
			summary = event.Message
		}
	}
	if summary != "test run failed" {
		t.Fatalf("summary = %q, want deterministic zero-count failure recap", summary)
	}
}

func TestMixedRuntimeBatchSummaryUsesOneOutcomeVocabulary(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		data   map[string]any
		failed bool
		want   string
	}{
		{
			name: "Go all pass",
			data: map[string]any{
				"testSummary":     map[string]any{"passed": 3, "failed": 0, "skipped": 0, "total": 3},
				"coverageSummary": map[string]any{"percentage": 75.0, "totalStatements": 8, "coveredStatements": 6},
			},
			want: "3/3 passed, 75.0% coverage",
		},
		{
			name: "TypeScript skipped",
			data: map[string]any{
				"testSummary":     map[string]any{"passed": float64(2), "failed": float64(0), "skipped": float64(1), "total": float64(3)},
				"coverageSummary": map[string]any{"percentage": float64(80), "granularity": "lines", "total": float64(10), "covered": float64(8)},
			},
			want: "2/3 passed, 1 skipped, 80.0% coverage",
		},
		{
			name: "bounded failure",
			data: map[string]any{
				"testSummary": map[string]any{"passed": 1, "failed": 2, "skipped": 0, "total": 3, "failureDetailsTruncated": 4},
			},
			failed: true,
			want:   "1/3 passed, 2 failed, 4 failure detail(s) omitted",
		},
		{
			name:   "module failure with no parsed tests",
			data:   map[string]any{"testSummary": map[string]any{"passed": 0, "failed": 0, "skipped": 0, "total": 0}},
			failed: true,
			want:   "test run failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := testBatchSummary(test.data, test.failed); got != test.want {
				t.Fatalf("summary = %q, want %q", got, test.want)
			}
		})
	}
}

// makeBuildBatchSchedulerFixture builds two sibling build-transpile producer
// jobs. Build producers use the extension-loop batch form: their batch policy
// declares only a stable tool label and NO configFiles, so config compatibility
// never gates grouping.
func makeBuildBatchSchedulerFixture(t *testing.T) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{"packages/a", "packages/b", "extension"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []string{"packages/a", "packages/b"} {
		writeTestFile(t, filepath.Join(root, project, "src.txt"), project)
	}

	projects := []*workspace.Project{
		{ID: "/packages/a", Name: "a", Path: "packages/a"},
		{ID: "/packages/b", Name: "b", Path: "packages/b"},
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "build-batch-test"
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/typescript",
		Version: "1.0.0",
		Path:    filepath.Join(root, "extension"),
		Tasks: map[string]extension.TaskDefinition{
			"build-transpile": {Declares: &extension.TaskDeclaration{}},
			"build-types":     {Declares: &extension.TaskDeclaration{}},
		},
	}
	// Extension-loop form: a stable tool label, NO configFiles. The absence of a
	// per-project config file is the whole point — listing one that always exists
	// (e.g. {projectRoot}/tsconfig.json) would give each project a distinct digest
	// and silently disable batching (the P1 bug).
	batchable := &extension.TaskBatchPolicy{Tool: "putnami-ts-build"}
	cpuWeight := 2.0
	planned := make([]*ScheduledJob, 0, len(projects))
	for _, project := range projects {
		planned = append(planned, &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: "transpile", Task: "build-transpile"},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/typescript",
				Name:          "build~transpile",
				CommandName:   "build",
				StepID:        "transpile",
				Command:       "/bin/true",
				Args:          []string{"cmd/putnami-ts", "build-transpile"},
				Cwd:           "{projectRoot}",
				Cache:         true,
				FilePatterns:  []string{"src.txt"},
				TaskCachePolicy: &extension.TaskCachePolicy{
					Deterministic: true,
				},
				Batchable: batchable,
				CPUWeight: &cpuWeight,
			},
		})
	}
	return ws, planned
}

func TestReadyBatchKeyGroupsSiblingBuildProducers(t *testing.T) {
	ws, jobs := makeBuildBatchSchedulerFixture(t)
	scheduler := newScheduler(ws, jobs, nil, SchedulerConfig{}, &mockRenderer{}, nil)

	keyA := scheduler.readyBatchKey(jobs[0])
	keyB := scheduler.readyBatchKey(jobs[1])
	if keyA == "" {
		t.Fatal("build producer did not opt into batching")
	}
	if keyA != keyB {
		t.Fatalf("sibling build producers resolved different batch keys:\n a=%s\n b=%s", keyA, keyB)
	}
	group, rest := scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 2 || len(rest) != 0 {
		t.Fatalf("same-kind build producers grouped as %d + %d, want 2 + 0", len(group.jobs), len(rest))
	}

	// A different build kind (types) must NOT co-batch with transpile: distinct
	// task args + cacheTaskName keep the kinds in separate groups.
	typesJob := *jobs[1]
	typesDef := *jobs[1].JobDef
	typesDef.Name = "build~types"
	typesDef.StepID = "types"
	typesDef.Args = []string{"cmd/putnami-ts", "build-types"}
	typesJob.JobDef = &typesDef
	typesJob.Step = &extension.PipelineStep{ID: "types", Task: "build-types"}
	if key := scheduler.readyBatchKey(&typesJob); key == keyA {
		t.Fatal("transpile and types producers share a batch key, would co-batch distinct kinds")
	}
}

func TestBatchResultEventsRenderBuildPhaseAndMetrics(t *testing.T) {
	ws, planned := makeBuildBatchSchedulerFixture(t)
	job := planned[0]

	result := jobResultFromBatchWire(ws.Root, job, batchWireResult{
		ProjectID: job.Project.ID,
		Status:    "OK",
		Metrics: []batchWireMetric{
			{Name: "transpiled-files", Value: 7, Unit: "count"},
		},
	}, &JobResult{Duration: time.Second}, nil)

	var startPhase, endPhase string
	metrics := make(map[string]float64)
	for _, event := range result.Events {
		if event.Type == EventTypePhase {
			name, _ := event.Data["name"].(string)
			if event.Data["action"] == "start" {
				startPhase = name
			}
			if event.Data["action"] == "end" {
				endPhase = name
			}
		}
		if event.Type == EventTypeMetric {
			name, _ := event.Data["name"].(string)
			value, _ := event.Data["value"].(float64)
			metrics[name] = value
		}
	}
	if startPhase != "transpile" || endPhase != "transpile" {
		t.Fatalf("build phase names = start:%q end:%q, want transpile (matching solo emit.PhaseStart/End)", startPhase, endPhase)
	}
	if metrics["transpiled-files"] != 7 {
		t.Fatalf("transpiled-files metric = %v, want 7", metrics["transpiled-files"])
	}
	if metrics["lint-errors"] != 0 {
		t.Fatalf("build result leaked lint metrics: %v", metrics)
	}
}

func TestSplitBatchResultBuildIsolatesFailedPeer(t *testing.T) {
	_, planned := makeBuildBatchSchedulerFixture(t)
	work := []taskWork{{job: planned[0]}, {job: planned[1]}}
	aggregate := &JobResult{
		Status: "success",
		Data: map[string]any{BatchResultsDataKey: []any{
			map[string]any{
				"projectId": "/packages/a",
				"status":    "FAILED",
				"diagnostics": []any{map[string]any{
					"category":    "TRANSPILE_ERROR",
					"severity":    "error",
					"description": "a failed to transpile",
				}},
			},
			map[string]any{
				"projectId": "/packages/b",
				"status":    "OK",
				"metrics": []any{map[string]any{
					"name":  "transpiled-files",
					"value": float64(3),
					"unit":  "count",
				}},
			},
		}},
		Duration: 5 * time.Millisecond,
	}

	results, ok := splitBatchResult("", work, aggregate)
	if !ok {
		t.Fatal("valid build batch result did not split")
	}
	if results[planned[0].Key()].Status != "failed" {
		t.Fatalf("failed project a status = %q, want failed", results[planned[0].Key()].Status)
	}
	if results[planned[1].Key()].Status != "success" {
		t.Fatalf("peer project b status = %q, want success (isolation)", results[planned[1].Key()].Status)
	}
	var bMetric float64
	var bHasMetric bool
	for _, event := range results[planned[1].Key()].Events {
		if event.Type == EventTypeMetric {
			if name, _ := event.Data["name"].(string); name == "transpiled-files" {
				bMetric, _ = event.Data["value"].(float64)
				bHasMetric = true
			}
		}
	}
	if !bHasMetric || bMetric != 3 {
		t.Fatalf("peer b transpiled-files metric = %v (present=%v), want 3", bMetric, bHasMetric)
	}
}

func makeBatchSchedulerFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{"a", "b", "extension/config"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "biome.json"), `{"formatter":{"enabled":true}}`)
	for _, project := range []string{"a", "b"} {
		writeTestFile(t, filepath.Join(root, project, "src.txt"), project)
	}

	command := "/bin/true"
	if logPath != "" {
		command = filepath.Join(root, "batch.sh")
		script := "#!/bin/sh\n" +
			"printf 'run\\n' >> " + shellQuote(logPath) + "\n" +
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\",\"data\":{\"batchResults\":[{\"projectId\":\"/a\",\"status\":\"OK\"},{\"projectId\":\"/b\",\"status\":\"OK\"}]}}}'\n"
		writeExecutable(t, command, script)
	}

	projects := []*workspace.Project{
		{ID: "/a", Name: "a", Path: "a"},
		{ID: "/b", Name: "b", Path: "b"},
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "batch-test"
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/typescript",
		Version: "1.0.0",
		Path:    filepath.Join(root, "extension"),
		Tasks: map[string]extension.TaskDefinition{
			"lint-all": {Declares: &extension.TaskDeclaration{}},
		},
	}
	batchable := &extension.TaskBatchPolicy{
		Tool: "biome",
		ConfigFiles: []string{
			"{projectRoot}/biome.json",
			"{workspaceRoot}/biome.json",
			"{extensionRoot}/config/biome.json",
		},
	}
	cpuWeight := 2.0
	planned := make([]*ScheduledJob, 0, len(projects))
	for _, project := range projects {
		planned = append(planned, &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: "check-only", Task: "lint-all"},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/typescript",
				Name:          "lint~check-only",
				CommandName:   "lint",
				StepID:        "check-only",
				Command:       command,
				Cwd:           "{projectRoot}",
				Cache:         true,
				FilePatterns:  []string{"src.txt"},
				TaskCachePolicy: &extension.TaskCachePolicy{
					NoOutput: true,
				},
				Batchable: batchable,
				CPUWeight: &cpuWeight,
			},
		})
	}
	return ws, planned
}

func invocationCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(data)))
}

// TestTestRunBatchGroupsSiblingsOnSharedWorkspaceConfig pins a hard-won
// lesson for the test-run policy shape: two sibling test projects that share a
// workspace-scoped config file (bun.lock) resolve the SAME batch key and group,
// so batching actually engages. A per-project always-present candidate would
// have given each a distinct digest and silently disabled batching.
func TestTestRunBatchGroupsSiblingsOnSharedWorkspaceConfig(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"packages/a", "packages/b"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The shared workspace lockfile is the first-existing config candidate for
	// both projects, so both resolve the identical source-qualified digest.
	writeTestFile(t, filepath.Join(root, "bun.lock"), `{"lockfileVersion":1}`)

	projects := []*workspace.Project{
		{ID: "/packages/a", Name: "a", Path: "packages/a"},
		{ID: "/packages/b", Name: "b", Path: "packages/b"},
	}
	ws := workspace.NewWorkspace(root, nil, projects)
	ws.Name = "test-batch"
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/typescript",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ts"),
		Tasks: map[string]extension.TaskDefinition{
			"test-run": {Declares: &extension.TaskDeclaration{}},
		},
	}
	batchable := &extension.TaskBatchPolicy{
		Tool:        "bun",
		MaxWorkers:  1,
		MaxProjects: 4,
		ConfigFiles: []string{"{workspaceRoot}/bun.lock", "{workspaceRoot}/bunfig.toml"},
	}
	planned := make([]*ScheduledJob, 0, len(projects))
	for _, project := range projects {
		planned = append(planned, &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: "test", Task: "test-run"},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@putnami/typescript",
				Name:          "test~test",
				CommandName:   "test",
				StepID:        "test",
				Command:       "/bin/true",
				Args:          []string{"test"},
				Cwd:           "{projectRoot}",
				Cache:         true,
				Batchable:     batchable,
			},
		})
	}

	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{}, &mockRenderer{}, nil)
	// maxWorkers:1 gates batching to a single-worker scheduler.
	scheduler.workerCount = 1

	keyA := scheduler.readyBatchKey(planned[0])
	keyB := scheduler.readyBatchKey(planned[1])
	if keyA == "" || keyA != keyB {
		t.Fatalf("sibling test projects got batch keys a=%q b=%q, want equal non-empty", keyA, keyB)
	}
	group, rest := scheduler.takePendingGroup(planned)
	if len(group.jobs) != 2 || len(rest) != 0 {
		t.Fatalf("shared-config test siblings grouped as %d + %d, want 2 + 0", len(group.jobs), len(rest))
	}

	// The economics gate: at more than one worker the batch key must disappear
	// so heavy suites keep cross-project parallelism.
	scheduler.workerCount = 2
	if key := scheduler.readyBatchKey(planned[0]); key != "" {
		t.Fatalf("test batch key at two workers = %q, want singleton dispatch", key)
	}
}

// TestBatchLeaderTimeoutMsScalesWithGroup pins the batch-deadline fix: a shared
// process running n suites sequentially gets n× the single-suite budget, while
// the sentinels (no-timeout, use-default) and the singleton case are preserved.
func TestBatchLeaderTimeoutMsScalesWithGroup(t *testing.T) {
	cases := []struct {
		name             string
		perTask, n, want int
	}{
		{"group scales per-suite budget", 600_000, 4, 2_400_000},
		{"singleton unchanged", 600_000, 1, 600_000},
		{"default resolved then scaled", 0, 3, DefaultTimeoutMs * 3},
		{"singleton default sentinel preserved", 0, 1, 0},
		{"no-timeout preserved regardless of group", -1, 4, -1},
	}
	for _, c := range cases {
		if got := batchLeaderTimeoutMs(c.perTask, c.n); got != c.want {
			t.Errorf("%s: batchLeaderTimeoutMs(%d, %d) = %d, want %d", c.name, c.perTask, c.n, got, c.want)
		}
	}
}

// TestReadyBatchKeyIsolatesInvocationConsumers pins the batching half of the
// invocation primitive.
//
// A batch is ONE subprocess with ONE job context, so every member sees the
// LEADER's `invocation` member and, through it, the leader's private artifact
// root. A consumer of an invocation-scoped resource therefore may not share a
// dispatch with anything: batching it with an unprovisioned peer would either
// hand that peer a credential it never asked for, or silently run the
// provisioned project's suite against nothing.
//
// The discriminator is the PRODUCER's plan key, stamped at plan time, so the
// key is stable whether or not the producer has already run.
func TestReadyBatchKeyIsolatesInvocationConsumers(t *testing.T) {
	ws, jobs := makeBatchSchedulerFixture(t, "")
	scheduler := newScheduler(ws, jobs, nil, SchedulerConfig{}, &mockRenderer{}, nil)

	shared := scheduler.readyBatchKey(jobs[0])
	if shared == "" || shared != scheduler.readyBatchKey(jobs[1]) {
		t.Fatalf("fixture peers do not co-batch (%q vs %q); the isolation below would prove nothing",
			shared, scheduler.readyBatchKey(jobs[1]))
	}

	// Stamp the first job as the consumer of a relation, exactly as
	// resolveInvocationRelations does.
	producer := &ScheduledJob{
		Project:   jobs[0].Project,
		Extension: jobs[0].Extension,
		JobDef:    &extension.JobDefinition{Name: "test~setup", ExtensionName: jobs[0].Extension.Name},
	}
	jobs[0].InvocationProducer = producer
	t.Cleanup(func() { jobs[0].InvocationProducer = nil })

	isolated := scheduler.readyBatchKey(jobs[0])
	if isolated == shared {
		t.Fatal("a consumer of an invocation-scoped resource kept its peers' batch key; " +
			"it would receive the batch leader's private artifact root")
	}
	if scheduler.readyBatchKey(jobs[1]) != shared {
		t.Error("stamping one consumer moved an unrelated job's batch key")
	}

	group, rest := scheduler.takePendingGroup(jobs)
	if len(group.jobs) != 1 || len(rest) != 1 {
		t.Fatalf("dispatch grouped %d + %d, want the consumer alone (1 + 1)", len(group.jobs), len(rest))
	}
}

// Under the default policy the class ceiling is the maximum over members, and
// criticality participates in that maximum: a batch whose longest member bounds
// the run must not be held to the shortest member's measured occupancy.
func TestSchedulerClassCeilingLiftsWithCriticalPathPolicy(t *testing.T) {
	ws, planned := makeBatchSchedulerFixture(t, "")

	history := loadTaskStats(ws.Root)
	for i, job := range planned {
		// Both members read one core of occupancy; only their wall differs.
		history.entries[taskStatKey(job)] = &taskStat{
			CPUMs:   100,
			WallMs:  float64(100 * (i + 1)),
			Samples: 1,
		}
	}
	history.dirty = true
	history.save()

	scheduler := &Scheduler{ws: ws, planned: planned, renderer: &mockRenderer{}}
	scheduler.prepareScheduling(ParallelDecision{LogicalCPU: 8})

	for _, job := range planned {
		if got := scheduler.batchCPUBudgetByJob[job.Key()]; got != 8 {
			t.Fatalf("class ceiling for %s = %d, want the longest member's whole machine 8", job.Key(), got)
		}
	}
	// The lift is plan-scoped: nothing about it may reach the stats store.
	for _, job := range planned {
		if got := scheduler.resourcePlanByJob[job.Key()].recommendation.unweightedCeiling; got != 1 {
			t.Fatalf("persisted unweighted ceiling for %s = %d, want the plan-independent occupancy 1",
				job.Key(), got)
		}
	}
}
