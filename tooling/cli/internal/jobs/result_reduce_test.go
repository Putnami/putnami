package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The engine-side half of the equivalence contract. The pure reducer and
// canonical-projection tests live beside the model they exercise,
// in cli-model/jobs/result_reduce_test.go; what stays here is what also needs
// the batch wire schema, the local store or a real scheduler run.
//
// Wire fixtures are unmarshaled into batchWireResult — the entrypoint
// production uses. Hand-assembling renderer payloads here would test the
// fixtures rather than the parse.

func parseTestWire(t *testing.T, payload string) batchWireResult {
	t.Helper()
	var wire batchWireResult
	if err := json.Unmarshal([]byte(payload), &wire); err != nil {
		t.Fatalf("fixture wire result is not valid JSON: %v", err)
	}
	return wire
}

func canonicalTestJob(project, jobName, taskKind string) *ScheduledJob {
	step := (*extension.PipelineStep)(nil)
	if taskKind != "" {
		step = &extension.PipelineStep{ID: jobName, Task: taskKind}
	}
	return &ScheduledJob{
		Project:   &workspace.Project{ID: "/" + project, Name: project, Path: project},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test"},
		JobDef:    &extension.JobDefinition{Name: jobName, CommandName: jobName},
		Step:      step,
	}
}

// legacyBuckets is the six-bucket tally every renderer repeats verbatim today:
// internal/output/json.go Finish, internal/output/jsonl.go Finish,
// internal/output/text_finish.go Finish, internal/output/cloud_logging.go
// Finish, internal/mcp/adapter.go summarizeRun, internal/cli/jobs_helpers.go
// buildSessionStats and internal/watch/session_iteration.go
// buildWatchSessionStats (deleted when the watch loop became a replan policy
// over Engine.Run). Transcribed unchanged.
type legacyBuckets struct {
	succeeded, failed, canceled, skipped, cached, coalesced int
	total                                                   int
	durationMs                                              int64
}

func tallyLegacyBuckets(results map[string]*JobResult) legacyBuckets {
	var b legacyBuckets
	for _, result := range results {
		b.total++
		if result.Coalesced {
			b.coalesced++
			continue
		}
		if result.CacheHit {
			b.cached++
			continue
		}
		switch result.Status {
		case "success":
			b.succeeded++
		case "failed":
			b.failed++
		case "canceled":
			b.canceled++
		case "skipped":
			b.skipped++
		}
		// buildSessionStats/buildWatchSessionStats accumulate truncated
		// milliseconds per job, and only for jobs that executed.
		b.durationMs += result.Duration.Milliseconds()
	}
	return b
}

// TestBatchWire_CanonicalMatchesSyntheticEvents is the single-parse contract.
//
// The batch path converts its wire schema straight to canonical records and
// then fabricates a per-project event stream from those same records, purely so
// the legacy renderers see what a solo run would have emitted. This pins that
// the fabricated stream carries NOTHING the canonical conversion lacks — which
// is precisely the precondition for A2b deleting the fabrication. It fails the
// moment the two paths are allowed to drift.
func TestBatchWire_CanonicalMatchesSyntheticEvents(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "canonical-result", "machine-surfaces-reduce-through-the-canonical-model")
	lintJob := canonicalTestJob("a", "lint~check", "lint")
	lintJob.JobDef.Args = []string{"lint"}
	testJob := canonicalTestJob("b", "test~test", "test")
	testJob.JobDef.Args = []string{"test"}
	buildJob := canonicalTestJob("c", "build~compile", "build-compile")
	buildJob.JobDef.Args = []string{"build-compile"}

	for _, test := range []struct {
		name string
		job  *ScheduledJob
		wire string
	}{
		{
			name: "lint with diagnostics and summary",
			job:  lintJob,
			// The second diagnostic has no file: the rendered event carries no
			// location, so the canonical record must not invent one.
			wire: `{"projectId":"/a","status":"OK","summary":{"errors":2,"warnings":1,"infos":0},
				"diagnostics":[
					{"category":"lint/unused","severity":"error","description":"unused",
					 "file":"src/a.ts","line":3,"column":4},
					{"category":"lint/config","severity":"warning","description":"no file",
					 "line":9,"column":1}]}`,
		},
		{
			name: "test with metrics, coverage and artifacts",
			job:  testJob,
			wire: `{"projectId":"/b","status":"OK",
				"data":{"testSummary":{"total":4,"passed":3,"skipped":1},
				        "coverageSummary":{"percentage":75,"totalStatements":8,"coveredStatements":6}},
				"artifacts":[{"id":"coverage","name":"Coverage","kind":"coverage","path":"out/coverage.out"}]}`,
		},
		{
			name: "build with explicit wire metrics",
			job:  buildJob,
			wire: `{"projectId":"/c","status":"OK","metrics":[
				{"name":"compiled-executables","value":2,"unit":"count"},
				{"name":"transpiled-files","value":17,"unit":"count"}]}`,
		},
		{
			name: "failed status",
			job:  lintJob,
			wire: `{"projectId":"/a","status":"FAILED","summary":{"errors":1}}`,
		},
		{
			name: "invalid status falls back to failed",
			job:  lintJob,
			wire: `{"projectId":"/a","status":"banana"}`,
		},
	} {
		wire := parseTestWire(t, test.wire)
		result := jobResultFromBatchWire("", test.job, wire, &JobResult{Duration: time.Second}, nil)
		if result.Canonical == nil {
			t.Fatalf("%s: batch conversion produced no canonical task result", test.name)
		}

		fromEvents := TaskResult{}
		fromEvents.Diagnostics, fromEvents.Artifacts, fromEvents.Metrics, fromEvents.Publications = recordsFromEvents(result.Events)
		applyTaskIdentity(&fromEvents, test.job)

		canonical := TaskResultOf(test.job, result)
		if !reflect.DeepEqual(canonical.Diagnostics, fromEvents.Diagnostics) {
			t.Errorf("%s: canonical diagnostics %+v, synthetic-event diagnostics %+v",
				test.name, canonical.Diagnostics, fromEvents.Diagnostics)
		}
		if !reflect.DeepEqual(canonical.Artifacts, fromEvents.Artifacts) {
			t.Errorf("%s: canonical artifacts %+v, synthetic-event artifacts %+v",
				test.name, canonical.Artifacts, fromEvents.Artifacts)
		}
		if !reflect.DeepEqual(canonical.Metrics, fromEvents.Metrics) {
			t.Errorf("%s: canonical metrics %+v, synthetic-event metrics %+v",
				test.name, canonical.Metrics, fromEvents.Metrics)
		}
	}
}

// TestTaskResultOf_CarriesTheResultMeasurements pins the one converging
// point: the typed test and coverage payloads both extensions already emit
// reach the canonical model on EVERY path — the solo stream, a batch member
// whose records came from the typed wire, and a warm hit replaying a persisted
// result.json — because all three arrive here carrying the same data map.
func TestTaskResultOf_CarriesTheResultMeasurements(t *testing.T) {
	t.Parallel()
	job := canonicalTestJob("a", "test", "test")
	measured := parseTestWire(t, `{"projectId":"/a","status":"OK","data":{
		"testSummary":{"total":4,"passed":3,"failed":0,"skipped":1,"failureDetailsTruncated":2},
		"coverageSummary":{"percentage":75,"granularity":"statements","covered":6,"total":8}}}`).Data

	for _, test := range []struct {
		name   string
		result *JobResult
	}{
		{"solo stream", &JobResult{Status: "success", Data: measured}},
		{"batch member", &JobResult{Status: "success", Data: measured, Canonical: &TaskResult{Status: TaskStatusSuccess}}},
		{"warm cache hit", &JobResult{Status: "success", Data: measured, CacheHit: true}},
	} {
		task := TaskResultOf(job, test.result)
		if task.Tests == nil || task.Tests.Total != 4 || task.Tests.Passed != 3 || task.Tests.Skipped != 1 ||
			task.Tests.FailureDetailsTruncated != 2 {
			t.Errorf("%s: tests = %+v, want counters and omission accounting the payload stated", test.name, task.Tests)
		}
		if task.Coverage == nil || task.Coverage.Percentage != 75 || task.Coverage.Granularity != "statements" {
			t.Errorf("%s: coverage = %+v, want 75%% of statements", test.name, task.Coverage)
		}
		if task.Coverage != nil && (task.Coverage.Covered != 6 || task.Coverage.Total != 8) {
			t.Errorf("%s: coverage counts = %d/%d, want 6/8", test.name, task.Coverage.Covered, task.Coverage.Total)
		}
	}
}

// TestTaskResultOf_MeasurementsAreAbsentNotZero: a task that measured nothing —
// and a task whose optional payload will not decode — carries no measurement at
// all. A zeroed summary would read as "ran and covered nothing", and a
// malformed optional block never turns a successful task into a failed one.
func TestTaskResultOf_MeasurementsAreAbsentNotZero(t *testing.T) {
	t.Parallel()
	job := canonicalTestJob("a", "build", "build-compile")
	unrelated := parseTestWire(t, `{"projectId":"/a","status":"OK","data":{"lintSummary":{"errors":0}}}`).Data
	malformed := parseTestWire(t, `{"projectId":"/a","status":"OK","data":{"coverageSummary":"78%"}}`).Data
	for _, test := range []struct {
		name   string
		result *JobResult
	}{
		{"no result", nil},
		{"no data", &JobResult{Status: "success"}},
		{"unrelated data", &JobResult{Status: "success", Data: unrelated}},
		{"malformed payload", &JobResult{Status: "success", Data: malformed}},
	} {
		task := TaskResultOf(job, test.result)
		if task.Tests != nil || task.Coverage != nil {
			t.Errorf("%s: claimed measurements %+v / %+v", test.name, task.Tests, task.Coverage)
		}
	}
}

// TestSchedulerRun_PopulatesCanonicalSession pins the production wiring: a real
// scheduler run reduces its own results, and its second run consumes the same
// task-owned entries that the first run published. Without this every other
// reducer test could pass while the scheduler never attached a session.
func TestSchedulerRun_PopulatesCanonicalSession(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "canonical-result", "the-scheduler-populates-the-canonical-session")
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	ws, planned := makeBatchSchedulerFixture(t, filepath.Join(t.TempDir(), "invocations"))
	localStore := store.NewLocalStore(filepath.Join(t.TempDir(), "store"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 2},
		&mockRenderer{}, store.NewCacheManager(localStore))
	result := scheduler.Run(ctx)

	session := result.Session
	if session == nil {
		t.Fatal("scheduler produced no canonical session result")
	}
	if session.Tasks != len(result.Results) {
		t.Errorf("session Tasks = %d, Results = %d", session.Tasks, len(result.Results))
	}
	if session.Success() != result.Success {
		t.Errorf("session Success() = %t, SchedulerResult.Success = %t", session.Success(), result.Success)
	}
	if session.Duration != result.Duration || session.Cache != result.Cache {
		t.Error("session duration/cache summary is not the run's own")
	}

	legacy := tallyLegacyBuckets(result.Results)
	if session.Fresh.Succeeded != legacy.succeeded || session.Cached() != legacy.cached {
		t.Errorf("cold run: reducer succeeded=%d cached=%d, legacy %d/%d",
			session.Fresh.Succeeded, session.Cached(), legacy.succeeded, legacy.cached)
	}

	// The second run serves both projects from the task-owned local cache, so
	// the canonical reuse histogram must move while the verdict does not.
	warm := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 2},
		&mockRenderer{}, store.NewCacheManager(localStore)).Run(ctx)
	if warm.Session == nil || warm.Session.Reuse.LocalCache != len(planned) {
		t.Fatalf("warm run reuse histogram = %+v, want %d local hits", warm.Session, len(planned))
	}
	if warm.Session.Fresh.Total() != 0 || !warm.Session.Success() {
		t.Errorf("warm run reduced to %+v", warm.Session)
	}
}
