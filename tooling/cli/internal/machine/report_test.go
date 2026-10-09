package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The report projection — surface #5 of the canonical reduction.
//
// These tests pin the properties that make the document BINDABLE rather than
// merely produced: it validates against the contract's own validator, its
// bounded job list accounts for what it dropped, its per-command rows partition
// the very task set its verdict describes, and two runs that differ only in
// their reports are byte-identical.

// reportMeta is a settled two-second interval, so every document below states a
// wall its CPU balance can be divided by.
func reportMeta() ReportMeta {
	start := time.Date(2026, 8, 7, 9, 30, 0, 0, time.UTC)
	return ReportMeta{
		SessionID:       "20260807-093000-ab12cd",
		StartTime:       start,
		EndTime:         start.Add(2 * time.Second),
		Origin:          protocolcli.ReportOriginCLI,
		EnforceCoverage: true,
	}
}

// reportTask is one counted row: a plan key, a verdict, a provenance and the
// timings the report sums.
func reportTask(key string, status jobs.TaskStatus, reuse jobs.ReuseKind, wall, cpu time.Duration) Task {
	return Task{
		Identity: identityOfKey(key),
		Result: jobs.TaskResult{
			Status: status,
			Reuse:  reuse,
			Timing: jobs.TaskTiming{Duration: wall, CPUTime: cpu},
		},
	}
}

// gateLikeRun is a small two-command run with the shapes a gate session mixes:
// a fresh success, a local cache hit, a coalesced task, and a failure carrying
// one error and one warning.
func gateLikeRun() Run {
	failing := reportTask("/a:test~exec", jobs.TaskStatusFailed, jobs.ReuseNone, 900*time.Millisecond, 1500*time.Millisecond)
	failing.Result.Error = &jobs.JobError{Message: "2 assertions failed"}
	failing.Result.Diagnostics = []jobs.TaskDiagnostic{
		{Severity: "error", Message: "expected 1, got 2", File: "a_test.go", Line: 12},
		{Severity: "warning", Message: "deprecated helper"},
	}
	return Run{
		Session: &jobs.SessionResult{
			Tasks:    4,
			Status:   jobs.TaskCounts{Succeeded: 3, Failed: 1},
			Fresh:    jobs.TaskCounts{Succeeded: 1, Failed: 1},
			Reuse:    jobs.ReuseCounts{LocalCache: 1, Coalesced: 1},
			Duration: 2 * time.Second,
		},
		Tasks: []Task{
			reportTask("/a:build~compile", jobs.TaskStatusSuccess, jobs.ReuseNone, 700*time.Millisecond, 1200*time.Millisecond),
			reportTask("/b:build~compile", jobs.TaskStatusSuccess, jobs.ReuseLocalCache, 400*time.Millisecond, 0),
			reportTask("/b:test~exec", jobs.TaskStatusSuccess, jobs.ReuseCoalesced, 300*time.Millisecond, 0),
			failing,
		},
	}
}

func encodeReport(t *testing.T, report *protocolcli.ReportFile) []byte {
	t.Helper()
	if report == nil {
		t.Fatal("the projection produced no report")
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentReportFile, data); len(violations) != 0 {
		t.Fatalf("report violates the contract: %v\n%s", violations, data)
	}
	return data
}

// TestReport_ValidatesAgainstTheContract is the gate every other test rides on:
// the document this CLI writes is run through the protocol's own validator, so
// the cross-field clauses (job accounting, the per-command partition, the
// derived key, the CPU tie) are proven on real projected bytes rather than
// restated here.
func TestReport_ValidatesAgainstTheContract(t *testing.T) {
	run := quotaLimitedSession(t)
	report := run.Report(reportMeta(), &jobs.TuningReport{
		Parallel:     jobs.ParallelDecision{Mode: "auto", Workers: 8, LogicalCPU: 16},
		CriticalPath: &jobs.CriticalPath{DurationMs: 1800},
	}, &jobs.CacheStatsSnapshot{
		Hits: 4, Misses: 2, Restored: 3, Uploads: 1,
		TimeSavedMs: 5200, BytesFetched: 900, BytesUploaded: 120,
	})
	data := encodeReport(t, report)

	if report.SessionID != "20260807-093000-ab12cd" || report.Origin != protocolcli.ReportOriginCLI {
		t.Errorf("report identity = %q / %q", report.SessionID, report.Origin)
	}
	if !report.EnforceCoverage {
		t.Error("the enforcing cadence was not recorded")
	}
	// The wall is the settled interval's, and the CPU allocation divides by it —
	// the tie the validator enforces and a consumer relies on.
	if report.Run.DurationMs != 2000 {
		t.Errorf("durationMs = %d, want the settled 2000", report.Run.DurationMs)
	}
	if report.Run.CPU == nil || report.Run.CPU.AllocatedMs != 16000 || report.Run.CPU.ActualMs != 12000 {
		t.Errorf("run CPU = %+v, want 8 cores over 2s against 12s actual", report.Run.CPU)
	}
	if report.Cache == nil || report.Cache.Restored != 3 || report.Scheduler == nil || report.Scheduler.Parallelism != 8 {
		t.Errorf("typed subsets = %+v / %+v", report.Cache, report.Scheduler)
	}
	if report.Scheduler.CriticalPathMs != 1800 {
		t.Errorf("criticalPathMs = %d, want 1800", report.Scheduler.CriticalPathMs)
	}
	// The verdict is stated WITHOUT the unbounded tail the run summary carries.
	if bytes.Contains(data, []byte(`"failures"`)) || bytes.Contains(data, []byte(`"abortedBy"`)) {
		t.Errorf("report run carries a run-ledger member:\n%s", data)
	}
}

// TestReport_CommandsPartitionTheRun pins the per-command block as a PARTITION:
// each command once, the totals summing to the run's own, and the fresh-only
// wall and CPU that keep a cached command from looking as expensive as the run
// that populated the cache.
func TestReport_CommandsPartitionTheRun(t *testing.T) {
	report := gateLikeRun().Report(reportMeta(), nil, nil)
	if report == nil {
		t.Fatal("the projection produced no report")
	}
	encodeReport(t, report)

	if len(report.Commands) != 2 || report.Commands[0].Command != "build" || report.Commands[1].Command != "test" {
		t.Fatalf("commands = %+v, want build then test", report.Commands)
	}

	build, test := report.Commands[0], report.Commands[1]
	if build.Counts.Total != 2 || build.Counts.Succeeded != 2 || build.Reuse.LocalCache != 1 {
		t.Errorf("build row = %+v", build)
	}
	// Only the fresh compile spends wall and CPU; the cache hit spends neither.
	if build.FreshWallMs != 700 || build.CPUMs != 1200 {
		t.Errorf("build fresh wall/cpu = %dms / %dms, want 700 / 1200", build.FreshWallMs, build.CPUMs)
	}
	if test.Counts.Total != 2 || test.Counts.Failed != 1 || test.Reuse.Coalesced != 1 {
		t.Errorf("test row = %+v", test)
	}
	if test.FreshWallMs != 900 || test.CPUMs != 1500 {
		t.Errorf("test fresh wall/cpu = %dms / %dms, want 900 / 1500", test.FreshWallMs, test.CPUMs)
	}
	// The totals are the diagnostics the tasks REPORTED, not the ones a bounded
	// job list happens to carry.
	if test.Errors != 1 || test.Warnings != 1 {
		t.Errorf("test diagnostics = %d errors / %d warnings, want 1 / 1", test.Errors, test.Warnings)
	}

	var summed int
	for _, command := range report.Commands {
		summed += command.Counts.Total
	}
	if summed != report.Run.Counts.Total {
		t.Errorf("per-command totals sum to %d, run counts %d", summed, report.Run.Counts.Total)
	}
}

// TestReport_CommandRowsCarryEveryVerdictAndProvenance walks the closed
// vocabularies end to end: a command row is a histogram over the same four
// verdicts and three provenances the run block reports, so an aborted or
// dependency-skipped task is stated rather than dropped into the successes.
func TestReport_CommandRowsCarryEveryVerdictAndProvenance(t *testing.T) {
	run := Run{
		Session: &jobs.SessionResult{
			Tasks:  4,
			Status: jobs.TaskCounts{Succeeded: 2, Canceled: 1, Skipped: 1},
			Reuse:  jobs.ReuseCounts{RemoteCache: 1, Coalesced: 1},
		},
		Tasks: []Task{
			reportTask("/a:lint~check", jobs.TaskStatusSuccess, jobs.ReuseRemoteCache, time.Second, 0),
			reportTask("/b:lint~check", jobs.TaskStatusSuccess, jobs.ReuseCoalesced, time.Second, 0),
			reportTask("/c:lint~check", jobs.TaskStatusCanceled, jobs.ReuseNone, 200*time.Millisecond, 0),
			reportTask("/d:lint~check", jobs.TaskStatusSkipped, jobs.ReuseNone, 0, 0),
		},
	}
	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	if len(report.Commands) != 1 {
		t.Fatalf("commands = %+v, want the single lint row", report.Commands)
	}
	row := report.Commands[0]
	want := protocolcli.RunCounts{Total: 4, Succeeded: 2, Canceled: 1, Skipped: 1}
	if row.Counts != want {
		t.Errorf("lint counts = %+v, want %+v", row.Counts, want)
	}
	if (row.Reuse != protocolcli.RunReuse{RemoteCache: 1, Coalesced: 1}) {
		t.Errorf("lint reuse = %+v, want one remote hit and one coalesced task", row.Reuse)
	}
	// Only the two tasks that executed spend wall, and neither reused row does.
	if row.FreshWallMs != 200 {
		t.Errorf("lint fresh wall = %dms, want the 200 the canceled task spent", row.FreshWallMs)
	}
	for _, job := range report.Jobs {
		if job.Reuse == protocolcli.TaskReuseRemoteCache {
			return
		}
	}
	t.Errorf("no job carried the remote-cache provenance: %+v", report.Jobs)
}

// TestReport_AbsentIsNotZero: what the run did not measure is OMITTED. A zero
// cpuMs would claim a cache hit ran and cost nothing, and an all-zero cache
// block would claim the cache was asked and answered nothing.
func TestReport_AbsentIsNotZero(t *testing.T) {
	report := gateLikeRun().Report(reportMeta(), nil, nil)
	data := encodeReport(t, report)

	if report.Cache != nil || report.Scheduler != nil {
		t.Errorf("a run with no remote cache and no tuning report claims %+v / %+v", report.Cache, report.Scheduler)
	}
	// Nothing this run does not measure appears at all: no task of this run
	// reported a test or coverage payload, so neither member is stated at 0, and
	// a job with no diagnostics (or none dropped) states no list and no
	// truncatedCount — proven below on the jobs that reported nothing, since the
	// failing task's diagnostics ARE measured.
	for _, member := range []string{`"coverage"`, `"tests"`, `"cpu"`} {
		if bytes.Contains(data, []byte(member)) {
			t.Errorf("report claims %s it never measured:\n%s", member, data)
		}
	}
	for _, job := range report.Jobs {
		if job.Reuse != protocolcli.TaskReuseNone && job.CPUMs != 0 {
			t.Errorf("reused job %q reports %dms of CPU", job.Key, job.CPUMs)
		}
		if job.Key != "/a:test~exec" && len(job.Diagnostics) != 0 {
			t.Errorf("job %q claims diagnostics it never reported: %v", job.Key, job.Diagnostics)
		}
		if job.TruncatedCount != 0 {
			t.Errorf("job %q claims a truncatedCount on a complete list", job.Key)
		}
	}
}

// TestReport_BoundsTheJobListAndCountsTheRest is the contract's whole premise: a
// stated worst case. Past the cap the list holds exactly ReportMaxJobs, what it
// dropped is COUNTED, and the failures are what survives — a report elides
// successes before failures, which is why the run block needs no failures[].
func TestReport_BoundsTheJobListAndCountsTheRest(t *testing.T) {
	const total = protocolcli.ReportMaxJobs + 40
	run := Run{Session: &jobs.SessionResult{Tasks: total, Status: jobs.TaskCounts{Succeeded: total - 2, Failed: 2}}}
	for i := 0; i < total-2; i++ {
		// Ascending wall, so the untruncated successes would win on wall alone if
		// the class ordering were not applied first.
		task := reportTask(fmt.Sprintf("/p%03d:build~compile", i), jobs.TaskStatusSuccess,
			jobs.ReuseNone, time.Duration(i)*time.Millisecond, 0)
		if i == 0 {
			task.Result.Diagnostics = []jobs.TaskDiagnostic{{Severity: "warning", Message: "slow"}}
		}
		run.Tasks = append(run.Tasks, task)
	}
	// The two failures are the SHORTEST tasks in the run.
	for i := 0; i < 2; i++ {
		run.Tasks = append(run.Tasks, reportTask(fmt.Sprintf("/f%d:build~compile", i),
			jobs.TaskStatusFailed, jobs.ReuseNone, time.Millisecond, 0))
	}

	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	if len(report.Jobs) != protocolcli.ReportMaxJobs {
		t.Fatalf("jobs = %d, want the full budget of %d", len(report.Jobs), protocolcli.ReportMaxJobs)
	}
	if len(report.Jobs)+report.ElidedJobs != report.Run.Counts.Total {
		t.Errorf("%d jobs + %d elided does not account for %d selected tasks",
			len(report.Jobs), report.ElidedJobs, report.Run.Counts.Total)
	}
	if report.Jobs[0].Outcome != protocolcli.TaskStatusFailed || report.Jobs[1].Outcome != protocolcli.TaskStatusFailed {
		t.Errorf("the bounded list did not sort failures first: %+v", report.Jobs[:2])
	}
	if report.Jobs[2].Key != "/p000:build~compile" {
		t.Errorf("job[2] = %q, want the diagnostic-bearing task ahead of the long successes", report.Jobs[2].Key)
	}
	// The command row still counts every task and every diagnostic the bound
	// dropped: truncation costs the examples, never the totals.
	if report.Commands[0].Counts.Total != total || report.Commands[0].Warnings != 1 {
		t.Errorf("command row = %+v, want all %d tasks and the elided warning", report.Commands[0], total)
	}
}

// TestReport_ElidesNothingUntilTheListIsFull: a short run carries every task and
// declares nothing elided. Otherwise a producer could ship one job, call the
// rest elided, and satisfy the accounting rule carrying no information.
func TestReport_ElidesNothingUntilTheListIsFull(t *testing.T) {
	report := gateLikeRun().Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	if len(report.Jobs) != 4 || report.ElidedJobs != 0 {
		t.Errorf("short run reported %d jobs and %d elided, want 4 and 0", len(report.Jobs), report.ElidedJobs)
	}
	for _, job := range report.Jobs {
		if job.Key != job.Project+":"+job.Task {
			t.Errorf("job key %q is not derived from %q and %q", job.Key, job.Project, job.Task)
		}
		if job.Command == "" {
			t.Errorf("job %q names no command to join its per-command row", job.Key)
		}
	}
}

// TestReport_IsDeterministic: two runs that differ only in their reports are the
// same run, so the same run has to produce the same bytes. Command rows come out
// of a map and the job selection out of a sort, which is exactly where an order
// could leak.
func TestReport_IsDeterministic(t *testing.T) {
	first := encodeReport(t, gateLikeRun().Report(reportMeta(), nil, nil))
	for i := 0; i < 8; i++ {
		if next := encodeReport(t, gateLikeRun().Report(reportMeta(), nil, nil)); !bytes.Equal(first, next) {
			t.Fatalf("report bytes moved between projections:\n%s\n%s", first, next)
		}
	}
}

// TestReport_FinalizerNeverReachesTheDocument is the counting rule the whole
// projection hangs on. A `runOn: finally` finalizer is listed, rendered and
// recorded, but it does NOT vote — so counting it here would put the report's
// job list and its per-command partition at odds with its own verdict, which is
// the one thing the contract's accounting clauses reject.
func TestReport_FinalizerNeverReachesTheDocument(t *testing.T) {
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	node := func(stepID, task, runOn string) *jobs.ScheduledJob {
		step := &extension.PipelineStep{ID: stepID, Task: task, RunOn: runOn}
		if runOn == extensionproto.StepRunOnFinally {
			step.Finalizes = &extensionproto.FinalizesRelation{Producer: "setup", Consumers: []string{"run"}}
		}
		return &jobs.ScheduledJob{
			Project: project,
			Step:    step,
			JobDef: &extension.JobDefinition{
				Name:        extension.StepJobName("test", stepID),
				CommandName: "test",
				StepID:      stepID,
			},
		}
	}
	producer := node("setup", "test-env-up", "")
	consumer := node("run", "test-exec", "")
	finalizer := node("teardown", "test-env-down", extensionproto.StepRunOnFinally)
	results := map[string]*jobs.JobResult{
		producer.Key():  {Status: "success"},
		consumer.Key():  {Status: "success"},
		finalizer.Key(): {Status: "failed", Error: &jobs.JobError{Message: "provider unreachable"}},
	}

	run := RunOf([]*jobs.ScheduledJob{producer, consumer, finalizer}, results, jobs.SessionOutcome{})
	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	if report.Run.Outcome != protocolcli.RunOutcomeSuccess || report.Run.Counts.Total != 2 {
		t.Fatalf("run block = %s / %+v, want success over the two voting tasks",
			report.Run.Outcome, report.Run.Counts)
	}
	if len(report.Jobs) != 2 || report.ElidedJobs != 0 {
		t.Errorf("jobs = %d (+%d elided), want the two voting tasks", len(report.Jobs), report.ElidedJobs)
	}
	for _, job := range report.Jobs {
		if job.Outcome == protocolcli.TaskStatusFailed {
			t.Errorf("the finalizer reached the job list as a failure: %+v", job)
		}
	}
	if len(report.Commands) != 1 || report.Commands[0].Counts.Total != 2 || report.Commands[0].Counts.Failed != 0 {
		t.Errorf("command row = %+v, want the two voting tasks", report.Commands)
	}
}

// TestReport_RefusesToDescribeADifferentTaskSet: when the reduction and the task
// list disagree, no report is written. A missing file is a non-event (the report
// is a projection, never an input); a document whose job list does not account
// for its own verdict is one every consumer must reject.
func TestReport_RefusesToDescribeADifferentTaskSet(t *testing.T) {
	run := gateLikeRun()
	run.Session.Status.Succeeded = 9
	if report := run.Report(reportMeta(), nil, nil); report != nil {
		t.Errorf("a report was produced for a reduction its tasks do not describe: %+v", report.Run.Counts)
	}
}

// TestReport_UncountableStatusStaysOutOfBothSides: a verdict outside the closed
// vocabulary reaches no bucket in the reduction, so it reaches no row here — the
// same filter on both sides, which is what keeps the totals agreeing.
func TestReport_UncountableStatusStaysOutOfBothSides(t *testing.T) {
	run := gateLikeRun()
	run.Tasks = append(run.Tasks, reportTask("/c:build~compile", jobs.TaskStatus("weird"), jobs.ReuseNone, time.Second, 0))
	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	if len(report.Jobs) != 4 {
		t.Errorf("jobs = %d, want the 4 counted tasks", len(report.Jobs))
	}
	for _, job := range report.Jobs {
		if job.Key == "/c:build~compile" {
			t.Errorf("an uncounted verdict reached the job list: %+v", job)
		}
	}
}

// TestReport_OriginIsClosed: the vocabulary is cli|mcp, and a producer that
// named no surface is the command line — the weaker claim, since a reader
// filters adapter runs out by default and guessing "mcp" would hide a real run.
func TestReport_OriginIsClosed(t *testing.T) {
	for _, tc := range []struct{ origin, want string }{
		{"", protocolcli.ReportOriginCLI},
		{"cli", protocolcli.ReportOriginCLI},
		{"mcp", protocolcli.ReportOriginMCP},
		{"agent", protocolcli.ReportOriginCLI},
	} {
		meta := reportMeta()
		meta.Origin = tc.origin
		report := gateLikeRun().Report(meta, nil, nil)
		encodeReport(t, report)
		if report.Origin != tc.want {
			t.Errorf("origin %q projected to %q, want %q", tc.origin, report.Origin, tc.want)
		}
	}
}

// TestReportCache_RestoredNeverExceedsHits: a key the cache did not hold cannot
// have been materialized, so a drifted counter is floored rather than published
// as a document a consumer must reject.
func TestReportCache_RestoredNeverExceedsHits(t *testing.T) {
	report := gateLikeRun().Report(reportMeta(), nil, &jobs.CacheStatsSnapshot{Hits: 2, Restored: 5, Misses: 1})
	encodeReport(t, report)
	if report.Cache.Restored != 2 {
		t.Errorf("restored = %d, want it floored at the 2 hits", report.Cache.Restored)
	}
}

// --- the per-job diagnostics -------------------------------------------

// TestReport_JobsCarryTheirDiagnostics: the diagnostics slice attaches the
// task's structured diagnostics to its job line — severity closed, message
// stated, position carried — and the resulting document still validates.
func TestReport_JobsCarryTheirDiagnostics(t *testing.T) {
	report := gateLikeRun().Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	job := findJob(t, report, "/a:test~exec")
	if len(job.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %v, want the failing task's 2", job.Diagnostics)
	}
	if job.Diagnostics[0].Severity != "error" || job.Diagnostics[0].Message != "expected 1, got 2" {
		t.Errorf("first diagnostic = %+v, want the error first", job.Diagnostics[0])
	}
	if job.Diagnostics[0].File != "a_test.go" || job.Diagnostics[0].Line != 12 {
		t.Errorf("position dropped: %+v", job.Diagnostics[0])
	}
	if job.TruncatedCount != 0 {
		t.Errorf("truncatedCount = %d on a complete list", job.TruncatedCount)
	}
}

// TestReport_JobDiagnosticsAreBoundedAndHonest: past the cap the list keeps the
// most severe head in a deterministic order, truncatedCount states exactly what
// was dropped, and the per-command counters still fold EVERY diagnostic — the
// contract's "truncation costs the examples, never the totals".
func TestReport_JobDiagnosticsAreBoundedAndHonest(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-test-output", "report-diagnostics-obey-limits-with-omission-accounting")
	noisy := reportTask("/a:lint~check", jobs.TaskStatusSuccess, jobs.ReuseNone, 100*time.Millisecond, 0)
	for i := range protocolcli.ReportMaxJobDiagnostics + 5 {
		noisy.Result.Diagnostics = append(noisy.Result.Diagnostics, jobs.TaskDiagnostic{
			Severity: "warning", Message: fmt.Sprintf("w%d", i),
		})
	}
	noisy.Result.Diagnostics = append(noisy.Result.Diagnostics, jobs.TaskDiagnostic{
		Severity: "error", Message: "the one error",
	})
	run := Run{
		Session: &jobs.SessionResult{
			Tasks:    1,
			Status:   jobs.TaskCounts{Succeeded: 1},
			Fresh:    jobs.TaskCounts{Succeeded: 1},
			Duration: 2 * time.Second,
		},
		Tasks: []Task{noisy},
	}
	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	job := findJob(t, report, "/a:lint~check")
	if len(job.Diagnostics) != protocolcli.ReportMaxJobDiagnostics {
		t.Fatalf("diagnostics = %d, want the cap %d", len(job.Diagnostics), protocolcli.ReportMaxJobDiagnostics)
	}
	if job.Diagnostics[0].Severity != "error" || job.Diagnostics[0].Message != "the one error" {
		t.Errorf("first diagnostic = %+v, want the error sorted first", job.Diagnostics[0])
	}
	if job.Diagnostics[1].Message != "w0" || job.Diagnostics[2].Message != "w1" {
		t.Errorf("warnings not in document order: %v then %v", job.Diagnostics[1], job.Diagnostics[2])
	}
	if want := 6; job.TruncatedCount != want {
		t.Errorf("truncatedCount = %d, want %d", job.TruncatedCount, want)
	}
	command := report.Commands[0]
	if command.Errors != 1 || command.Warnings != protocolcli.ReportMaxJobDiagnostics+5 {
		t.Errorf("command counters = %d errors / %d warnings, want every diagnostic counted", command.Errors, command.Warnings)
	}
}

func TestReport_ProducerFailureOmissionsReachEveryAccountingSurface(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-test-output", "report-diagnostics-obey-limits-with-omission-accounting")
	task := reportTask("/a:test~exec", jobs.TaskStatusFailed, jobs.ReuseNone, time.Second, 0)
	task.Result.Error = &jobs.JobError{Message: "test run failed"}
	task.Result.Tests = &runtimeproto.TestSummary{
		Total: 1, Failed: 1, FailureDetailsTruncated: 3,
	}
	task.Result.Diagnostics = append(task.Result.Diagnostics, jobs.TaskDiagnostic{
		Severity: "error", Code: "COVERAGE_THRESHOLD_NOT_MET", Message: "coverage below threshold",
	})
	for i := 1; i < protocolcli.ReportMaxJobDiagnostics; i++ {
		task.Result.Diagnostics = append(task.Result.Diagnostics, jobs.TaskDiagnostic{
			Severity: "error", Code: "TEST_FAILED", Message: fmt.Sprintf("failure %d", i),
		})
	}
	task.Result.Diagnostics = append(task.Result.Diagnostics, jobs.TaskDiagnostic{
		Severity: "info", Code: protocolcli.TestFailureDetailsTruncatedCode, Message: "3 failure details omitted",
	})
	run := Run{
		Session: &jobs.SessionResult{
			Tasks: 1, Status: jobs.TaskCounts{Failed: 1}, Fresh: jobs.TaskCounts{Failed: 1}, Duration: 2 * time.Second,
		},
		Tasks: []Task{task},
	}

	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)
	job := findJob(t, report, "/a:test~exec")
	if len(job.Diagnostics) != protocolcli.ReportMaxJobDiagnostics {
		t.Fatalf("diagnostics = %d, want %d", len(job.Diagnostics), protocolcli.ReportMaxJobDiagnostics)
	}
	if job.Diagnostics[0].Code != protocolcli.TestFailureDetailsTruncatedCode {
		t.Fatalf("synthetic accounting diagnostic was lost: first = %+v", job.Diagnostics[0])
	}
	if job.Diagnostics[1].Code != "COVERAGE_THRESHOLD_NOT_MET" || job.Diagnostics[2].Message != "failure 1" {
		t.Fatalf("non-marker error order changed: second=%+v third=%+v", job.Diagnostics[1], job.Diagnostics[2])
	}
	// The report dropped one of the 17 reported diagnostics and the producer had
	// already omitted three, so the job must account for all four.
	if job.TruncatedCount != 4 {
		t.Fatalf("truncatedCount = %d, want reducer 1 + producer 3", job.TruncatedCount)
	}
	if job.FailureDetailsTruncated != 3 {
		t.Fatalf("failureDetailsTruncated = %d, want producer 3", job.FailureDetailsTruncated)
	}
	command := report.Commands[0]
	if command.Tests == nil || command.Tests.FailureDetailsTruncated != 3 {
		t.Fatalf("command tests = %+v, want producer omission count 3", command.Tests)
	}
	// Sixteen real reported errors plus three omitted causal details contribute
	// to the honest total; the synthetic accounting marker is not another error.
	if command.Errors != 19 {
		t.Fatalf("command errors = %d, want 16 reported + 3 producer-omitted", command.Errors)
	}
}

func TestReport_ShortDiagnosticListMayCarryProducerOmissions(t *testing.T) {
	task := reportTask("/a:test~exec", jobs.TaskStatusFailed, jobs.ReuseNone, time.Second, 0)
	task.Result.Error = &jobs.JobError{Message: "test run failed"}
	task.Result.Tests = &runtimeproto.TestSummary{
		Total: 1, Failed: 1, FailureDetailsTruncated: 5,
	}
	task.Result.Diagnostics = []jobs.TaskDiagnostic{
		{Severity: "error", Code: "TEST_FAILED", Message: "failure one"},
		{Severity: "error", Code: "TEST_FAILED", Message: "failure two"},
		{Severity: "info", Code: protocolcli.TestFailureDetailsTruncatedCode, Message: "5 failure details omitted"},
	}
	run := Run{
		Session: &jobs.SessionResult{
			Tasks: 1, Status: jobs.TaskCounts{Failed: 1}, Fresh: jobs.TaskCounts{Failed: 1}, Duration: 2 * time.Second,
		},
		Tasks: []Task{task},
	}

	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)
	job := findJob(t, report, "/a:test~exec")
	if len(job.Diagnostics) != 3 || job.TruncatedCount != 5 {
		t.Fatalf("job diagnostics=%d truncatedCount=%d, want short list 3 with producer omissions 5", len(job.Diagnostics), job.TruncatedCount)
	}
	if job.FailureDetailsTruncated != 5 {
		t.Fatalf("failureDetailsTruncated = %d, want 5", job.FailureDetailsTruncated)
	}
	if report.Commands[0].Tests == nil || report.Commands[0].Tests.FailureDetailsTruncated != 5 {
		t.Fatalf("command tests = %+v, want producer omission count 5", report.Commands[0].Tests)
	}
}

// TestReport_JobDiagnosticMessageIsByteBoundedKeepingTheTail: the cap is UTF-8
// bytes, the cut keeps the END of the message where a failure's verdict lives,
// the dropped head is marked, and the cut never leaves a torn rune behind.
func TestReport_JobDiagnosticMessageIsByteBoundedKeepingTheTail(t *testing.T) {
	long := bytes.Repeat([]byte{'x'}, protocolcli.ReportMaxMessageBytes)
	message := "é" + string(long) + "--- FAIL: TestTail" // the two-byte rune straddles the tail cut
	task := reportTask("/a:test~exec", jobs.TaskStatusFailed, jobs.ReuseNone, time.Second, 0)
	task.Result.Error = &jobs.JobError{Message: "failed"}
	task.Result.Diagnostics = []jobs.TaskDiagnostic{{Severity: "error", Message: message}}
	run := Run{
		Session: &jobs.SessionResult{
			Tasks:    1,
			Status:   jobs.TaskCounts{Failed: 1},
			Fresh:    jobs.TaskCounts{Failed: 1},
			Duration: 2 * time.Second,
		},
		Tasks: []Task{task},
	}
	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	got := findJob(t, report, "/a:test~exec").Diagnostics[0].Message
	if len(got) > protocolcli.ReportMaxMessageBytes {
		t.Errorf("message = %d bytes, want at most %d", len(got), protocolcli.ReportMaxMessageBytes)
	}
	if !strings.HasSuffix(got, "--- FAIL: TestTail") {
		t.Errorf("message = %q…, want the tail kept", got[:40])
	}
	if !strings.HasPrefix(got, "… ") {
		t.Errorf("message starts %q, want the dropped head marked with an ellipsis", got[:8])
	}
	if !utf8.ValidString(got) {
		t.Error("truncated message is not valid UTF-8")
	}
}

// --- the coverage and test measurements ---------------------------------

// measuredTask is a passing test task carrying the typed payloads an extension
// emits in its result data — the shape jobs.TaskResultOf now records.
func measuredTask(key string, tests *runtimeproto.TestSummary, coverage *runtimeproto.CoverageSummary) Task {
	task := reportTask(key, jobs.TaskStatusSuccess, jobs.ReuseNone, 500*time.Millisecond, 0)
	task.Result.Tests = tests
	task.Result.Coverage = coverage
	return task
}

// measuredRun wraps a set of passing tasks in the reduction that describes them,
// so the projection's accounting check passes and the document validates.
func measuredRun(tasks ...Task) Run {
	return Run{
		Session: &jobs.SessionResult{
			Tasks:    len(tasks),
			Status:   jobs.TaskCounts{Succeeded: len(tasks)},
			Fresh:    jobs.TaskCounts{Succeeded: len(tasks)},
			Duration: 2 * time.Second,
		},
		Tasks: tasks,
	}
}

// TestReport_CommandsAggregateTheMeasurements: a command's coverage is
// UNIT-WEIGHTED, not the mean of percentages — a 5-statement project must not
// weigh as much as a 500-statement one — and its test block is the four
// counters summed, with total the sum of the three classified ones the contract
// defines it as.
func TestReport_CommandsAggregateTheMeasurements(t *testing.T) {
	big := measuredTask("/a:test~exec",
		&runtimeproto.TestSummary{Total: 10, Passed: 8, Failed: 1, Skipped: 1},
		&runtimeproto.CoverageSummary{Percentage: 80, Granularity: "statements", Covered: 80, Total: 100})
	small := measuredTask("/b:test~exec",
		&runtimeproto.TestSummary{Total: 4, Passed: 4},
		&runtimeproto.CoverageSummary{Percentage: 55, Granularity: "statements", Covered: 55, Total: 100})
	report := measuredRun(big, small).Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	row := report.Commands[0]
	if row.Coverage == nil {
		t.Fatal("the command states no coverage its tasks measured")
	}
	if row.Coverage.Percentage != 67.5 || row.Coverage.Covered != 135 || row.Coverage.Total != 200 {
		t.Errorf("command coverage = %+v, want the weighted 135/200", row.Coverage)
	}
	if row.Coverage.Granularity != protocolcli.CoverageStatements || !row.Coverage.Enforced {
		t.Errorf("coverage unit/cadence = %q / %v, want statements under the enforcing run",
			row.Coverage.Granularity, row.Coverage.Enforced)
	}
	want := protocolcli.ReportTests{Total: 14, Passed: 12, Failed: 1, Skipped: 1}
	if row.Tests == nil || *row.Tests != want {
		t.Errorf("command tests = %+v, want %+v", row.Tests, want)
	}
	// The job line states the task's OWN measurement, never the command's.
	if job := findJob(t, report, "/a:test~exec"); job.Coverage == nil || job.Coverage.Percentage != 80 {
		t.Errorf("job coverage = %+v, want this project's own 80%%", job.Coverage)
	}
}

// TestReport_CoverageStaysWithinOneGranularity: Go counts statements where
// TypeScript counts lines, and the emitted block states ONE unit — so the
// command aggregates the granularity most of its tasks measured at and never
// averages across units the contract keeps distinguishable.
func TestReport_CoverageStaysWithinOneGranularity(t *testing.T) {
	statements := func(key string, covered, total int) Task {
		return measuredTask(key, nil, &runtimeproto.CoverageSummary{
			Percentage: 100 * float64(covered) / float64(total), Granularity: "statements",
			Covered: covered, Total: total,
		})
	}
	lines := measuredTask("/ts:test~exec", nil, &runtimeproto.CoverageSummary{
		Percentage: 10, Granularity: "lines", Covered: 100, Total: 1000,
	})
	report := measuredRun(statements("/a:test~exec", 90, 100), statements("/b:test~exec", 70, 100), lines).
		Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	row := report.Commands[0]
	if row.Coverage.Granularity != protocolcli.CoverageStatements {
		t.Fatalf("command granularity = %q, want the majority unit", row.Coverage.Granularity)
	}
	if row.Coverage.Total != 200 || row.Coverage.Percentage != 80 {
		t.Errorf("command coverage = %+v, want the line measurement left out of the statement fold", row.Coverage)
	}
	// The line-granularity task keeps its own measurement on its own job line.
	if job := findJob(t, report, "/ts:test~exec"); job.Coverage == nil || job.Coverage.Granularity != protocolcli.CoverageLines {
		t.Errorf("job coverage = %+v, want the task's own line measurement", job.Coverage)
	}
}

// TestReport_CoverageFallsBackToTheMeanWithoutCounts: a producer that reported
// only a percentage stated no denominator, so the aggregate is the plain mean
// and the counts are OMITTED rather than invented.
func TestReport_CoverageFallsBackToTheMeanWithoutCounts(t *testing.T) {
	report := measuredRun(
		measuredTask("/a:test~exec", nil, &runtimeproto.CoverageSummary{Percentage: 90, Granularity: "lines"}),
		measuredTask("/b:test~exec", nil, &runtimeproto.CoverageSummary{Percentage: 70, Granularity: "lines"}),
	).Report(reportMeta(), nil, nil)
	data := encodeReport(t, report)

	row := report.Commands[0]
	if row.Coverage == nil || row.Coverage.Percentage != 80 {
		t.Fatalf("command coverage = %+v, want the mean 80", row.Coverage)
	}
	if row.Coverage.Covered != 0 || row.Coverage.Total != 0 {
		t.Errorf("counts invented from percentages alone: %+v", row.Coverage)
	}
	if bytes.Contains(data, []byte(`"covered"`)) {
		t.Errorf("a count nobody reported reached the document:\n%s", data)
	}
}

// TestReport_MeasurementsAreAbsentNotZero: the report DISPLAYS what the run
// measured and never forces instrumentation. An uninstrumented command omits
// the block instead of claiming 0%, and a measurement whose unit is outside the
// contract's vocabulary is not one this document can state at all.
func TestReport_MeasurementsAreAbsentNotZero(t *testing.T) {
	report := measuredRun(
		measuredTask("/a:build~compile", nil, nil),
		measuredTask("/b:build~compile", &runtimeproto.TestSummary{Total: 0},
			&runtimeproto.CoverageSummary{Percentage: 62, Granularity: "instructions"}),
	).Report(reportMeta(), nil, nil)
	data := encodeReport(t, report)

	if report.Commands[0].Coverage != nil || report.Commands[0].Tests != nil {
		t.Errorf("command claims %+v / %+v it never measured",
			report.Commands[0].Coverage, report.Commands[0].Tests)
	}
	for _, member := range []string{`"coverage"`, `"tests"`} {
		if bytes.Contains(data, []byte(member)) {
			t.Errorf("report states %s from an unstatable measurement:\n%s", member, data)
		}
	}
	// An unstatable unit must not buy a task the coverage-bearing rank either.
	for i := range report.Jobs {
		if report.Jobs[i].Coverage != nil {
			t.Errorf("job %q carries a measurement in a unit the contract does not name", report.Jobs[i].Key)
		}
	}
}

// TestReport_CoverageCountsCannotContradictThemselves: covered and total state
// the same measurement twice, and the percentage is the contract's only
// non-integer member. A producer whose numbers cannot both be true costs its own
// figure precision, never the whole document.
func TestReport_CoverageCountsCannotContradictThemselves(t *testing.T) {
	report := measuredRun(
		measuredTask("/a:test~exec", nil, &runtimeproto.CoverageSummary{
			Percentage: 130, Granularity: "statements", Covered: 120, Total: 100,
		}),
		measuredTask("/b:test~exec", nil, &runtimeproto.CoverageSummary{
			Percentage: -5, Granularity: "statements",
		}),
	).Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	job := findJob(t, report, "/a:test~exec")
	if job.Coverage.Covered != 100 || job.Coverage.Percentage != 100 {
		t.Errorf("job coverage = %+v, want covered floored at total and the share at 100", job.Coverage)
	}
	if row := report.Commands[0].Coverage; row == nil || row.Covered != 100 || row.Total != 100 {
		t.Errorf("command coverage = %+v, want the clamped counts folded", row)
	}
}

// TestReport_CoverageBearingJobsSurviveTheBound is the selection priority the
// contract states: failed, then diagnostic-bearing, then coverage-bearing, then
// the longest wall. A short passing test task carries the one number no other
// member of the document states for that project, so the bound keeps it ahead
// of every long ordinary build.
func TestReport_CoverageBearingJobsSurviveTheBound(t *testing.T) {
	const ordinary = protocolcli.ReportMaxJobs + 20
	run := measuredRun()
	for i := range ordinary {
		// Ascending wall, so the ordinary tasks would win on wall alone if the
		// class ordering were not applied first.
		run.Tasks = append(run.Tasks, reportTask(fmt.Sprintf("/p%03d:build~compile", i),
			jobs.TaskStatusSuccess, jobs.ReuseNone, time.Duration(i+10)*time.Second, 0))
	}
	noisy := reportTask("/n:lint~check", jobs.TaskStatusSuccess, jobs.ReuseNone, time.Millisecond, 0)
	noisy.Result.Diagnostics = []jobs.TaskDiagnostic{{Severity: "warning", Message: "slow"}}
	covered := measuredTask("/c:test~exec", &runtimeproto.TestSummary{Total: 3, Passed: 3},
		&runtimeproto.CoverageSummary{Percentage: 91.5, Granularity: "statements", Covered: 183, Total: 200})
	covered.Result.Timing.Duration = time.Millisecond
	run.Tasks = append(run.Tasks, noisy, covered)
	run.Session.Tasks = len(run.Tasks)
	run.Session.Status = jobs.TaskCounts{Succeeded: len(run.Tasks)}
	run.Session.Fresh = run.Session.Status

	report := run.Report(reportMeta(), nil, nil)
	encodeReport(t, report)

	if report.Jobs[0].Key != "/n:lint~check" {
		t.Errorf("job[0] = %q, want the diagnostic-bearing task first", report.Jobs[0].Key)
	}
	if report.Jobs[1].Key != "/c:test~exec" {
		t.Fatalf("job[1] = %q, want the coverage-bearing task ahead of the long builds", report.Jobs[1].Key)
	}
	if report.Jobs[1].Coverage == nil || report.Jobs[1].Coverage.Percentage != 91.5 {
		t.Errorf("selected job coverage = %+v, want the measurement it was selected for", report.Jobs[1].Coverage)
	}
	if len(report.Jobs)+report.ElidedJobs != report.Run.Counts.Total {
		t.Errorf("%d jobs + %d elided does not account for %d tasks",
			len(report.Jobs), report.ElidedJobs, report.Run.Counts.Total)
	}
}

// TestReport_CoverageEnforcementTracksTheRunCadence: `enforced` is the
// difference between "coverage was observed" and "coverage could have failed
// the run", and it comes from the run's own --enforce-coverage cadence — the
// same marker the document's top-level enforceCoverage states.
func TestReport_CoverageEnforcementTracksTheRunCadence(t *testing.T) {
	for _, enforcing := range []bool{true, false} {
		meta := reportMeta()
		meta.EnforceCoverage = enforcing
		report := measuredRun(measuredTask("/a:test~exec", nil,
			&runtimeproto.CoverageSummary{Percentage: 88, Granularity: "statements", Covered: 88, Total: 100})).
			Report(meta, nil, nil)
		encodeReport(t, report)

		if report.EnforceCoverage != enforcing {
			t.Fatalf("run cadence = %v, want %v", report.EnforceCoverage, enforcing)
		}
		if report.Commands[0].Coverage.Enforced != enforcing {
			t.Errorf("command coverage enforced = %v under a %v cadence",
				report.Commands[0].Coverage.Enforced, enforcing)
		}
		if findJob(t, report, "/a:test~exec").Coverage.Enforced != enforcing {
			t.Errorf("job coverage disagrees with the run's cadence %v", enforcing)
		}
	}
}

// TestReport_FixStatesTheRunFlag: the run's --fix value reaches the document as
// given, and a run without the flag carries no member.
func TestReport_FixStatesTheRunFlag(t *testing.T) {
	yes, no := true, false
	for _, fix := range []*bool{nil, &no, &yes} {
		meta := reportMeta()
		meta.Fix = fix
		data := encodeReport(t, gateLikeRun().Report(meta, nil, nil))

		var members map[string]json.RawMessage
		if err := json.Unmarshal(data, &members); err != nil {
			t.Fatalf("parse report: %v", err)
		}
		got, present := members["fix"]
		if fix == nil {
			if present {
				t.Errorf("a run without --fix states fix = %s", got)
			}
			continue
		}
		if want := fmt.Sprint(*fix); string(got) != want {
			t.Errorf("fix = %s, want %s", got, want)
		}
	}
}

// TestReport_MeasurementsAreDeterministic: two projections of the same measured
// run are the same bytes. The granularity pick reads a map, which is exactly
// where an iteration order could leak into a document meant to be diffable.
func TestReport_MeasurementsAreDeterministic(t *testing.T) {
	build := func() Run {
		return measuredRun(
			measuredTask("/a:test~exec", &runtimeproto.TestSummary{Total: 2, Passed: 2},
				&runtimeproto.CoverageSummary{Percentage: 80, Granularity: "statements", Covered: 80, Total: 100}),
			measuredTask("/b:test~exec", &runtimeproto.TestSummary{Total: 3, Passed: 3},
				&runtimeproto.CoverageSummary{Percentage: 50, Granularity: "lines", Covered: 50, Total: 100}),
		)
	}
	first := encodeReport(t, build().Report(reportMeta(), nil, nil))
	for range 8 {
		if next := encodeReport(t, build().Report(reportMeta(), nil, nil)); !bytes.Equal(first, next) {
			t.Fatalf("measured report bytes moved between projections:\n%s\n%s", first, next)
		}
	}
}

// findJob returns the job line for a key, failing the test when the report
// carries none.
func findJob(t *testing.T, report *protocolcli.ReportFile, key string) protocolcli.ReportJob {
	t.Helper()
	for _, job := range report.Jobs {
		if job.Key == key {
			return job
		}
	}
	t.Fatalf("no job %q in %v", key, report.Jobs)
	return protocolcli.ReportJob{}
}
