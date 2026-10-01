package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The failure cache, exercised through the REAL scheduler, REAL subprocesses
// and a REAL store: a task fails for a reason its own inputs explain, and the
// next run must reach the same verdict without spawning anything.
//
// The subprocess log is the only honest answer to "did this run again", which
// is the whole claim of the feature.

// failureReplayFixture plans two ordered nodes over one project: `build~probe`,
// whose outcome is a function of its keyed source file, and `build~consume`,
// which depends on it. Every invocation of either script appends a line to
// logPath, so a test can count subprocesses instead of trusting a status.
func failureReplayFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app", "main.go"), "package main // BROKEN\n")

	probe := filepath.Join(root, "probe.sh")
	writeExecutable(t, probe,
		"#!/bin/sh\n"+
			"printf 'probe\\n' >> "+shellQuote(logPath)+"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n"+
			// The failure is a FUNCTION OF THE KEYED INPUT: the same bytes always
			// produce the same verdict, which is exactly the premise a negative
			// entry rests on.
			"if grep -q BROKEN main.go; then\n"+
			"  printf '%s\\n' '{\"v\":2,\"type\":\"diagnostic\",\"data\":{\"severity\":\"error\",\"message\":\"main.go is broken\"}}'\n"+
			"  printf '%s\\n' '{\"v\":2,\"type\":\"log\",\"level\":\"error\",\"message\":\"probe failed on main.go\"}'\n"+
			"  exit 3\n"+
			"fi\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
			"exit 0\n")

	consume := filepath.Join(root, "consume.sh")
	writeExecutable(t, consume,
		"#!/bin/sh\n"+
			"printf 'consume\\n' >> "+shellQuote(logPath)+"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n"+
			"exit 0\n")

	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	ws.Name = "failure-replay-ws"

	ext := &extension.ExtensionDescription{
		Name:    "@test/probe",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ext"),
		Tasks: map[string]extension.TaskDefinition{
			"probe-task":   {Declares: &extension.TaskDeclaration{}},
			"consume-task": {Declares: &extension.TaskDeclaration{}},
		},
	}

	newJob := func(name, task, command string) *ScheduledJob {
		commandName, step := jobCommandAndStep(name)
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: step, Task: task},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@test/probe",
				Name:          name,
				CommandName:   commandName,
				StepID:        step,
				Command:       command,
				Cwd:           "{projectRoot}",
				Cache:         true,
				FilePatterns:  []string{"main.go"},
				// Deliberately NOT declaring cache.deterministic: the failure
				// cache must work for an ordinary task, or it would be inert.
				TaskCachePolicy: &extension.TaskCachePolicy{NoOutput: true},
			},
		}
	}
	planned := []*ScheduledJob{
		newJob("build~probe", "probe-task", probe),
		newJob("build~consume", "consume-task", consume),
	}
	planned[1].DependsOn = []string{planned[0].Key()}
	return ws, planned
}

func subprocessRuns(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read invocation log: %v", err)
	}
	return len(strings.Fields(string(data)))
}

func resultOf(t *testing.T, run *SchedulerResult, planned []*ScheduledJob, index int) *JobResult {
	t.Helper()
	result := run.Results[planned[index].Key()]
	if result == nil {
		t.Fatalf("no result for %s", planned[index].Key())
	}
	return result
}

// TestFailureReplay_UnchangedInputsReplayWithoutExecuting is the feature, end to
// end: a cold failure, then a run that reaches the same verdict without
// spawning anything, then --retry-failed forcing the work, then a fixed input
// executing for real.
func TestFailureReplay_UnchangedInputsReplayWithoutExecuting(t *testing.T) {
	t.Parallel()
	for _, check := range []string{
		"an-unchanged-failure-replays-without-executing",
		"a-replayed-failure-blocks-its-dependents",
		"retry-failed-forces-re-execution",
		"a-changed-input-runs-again",
	} {
		spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache", check)
	}
	requireShell(t)

	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := failureReplayFixture(t, logPath)
	storeRoot := filepath.Join(t.TempDir(), "store")
	// One machine-global store, a fresh CacheManager per run: that is what a
	// second `putnami` invocation is, and it keeps this test from reading a
	// memoized file digest a real second process would never have.
	newCache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(storeRoot))
	}
	source := filepath.Join(ws.Root, "app", "main.go")

	// --- 1. cold: the task executes and fails, and its dependent is blocked.
	first := runSharedScheduler(context.Background(), ws, planned, SchedulerConfig{MaxParallel: 1}, newCache())
	if got := resultOf(t, first, planned, 0).Status; got != "failed" {
		t.Fatalf("cold probe status = %q, want failed", got)
	}
	if got := subprocessRuns(t, logPath); got != 1 {
		t.Fatalf("cold run spawned %d subprocesses, want exactly the probe", got)
	}
	coldDependent := resultOf(t, first, planned, 1).Status
	if coldDependent == "success" {
		t.Fatalf("cold dependent status = %q: it must not run behind a failed dependency", coldDependent)
	}
	coldKey := resultOf(t, first, planned, 0).CacheKey
	if coldKey == "" {
		t.Fatal("the failed task reported no cache key, so nothing could be recorded against it")
	}

	// --- 2. warm: same inputs, same verdict, no subprocess.
	second := runSharedScheduler(context.Background(), ws, planned, SchedulerConfig{MaxParallel: 1}, newCache())
	replayed := resultOf(t, second, planned, 0)
	if replayed.Status != "failed" {
		t.Fatalf("replayed status = %q, want failed: a replay is a failure in every respect", replayed.Status)
	}
	if got := subprocessRuns(t, logPath); got != 1 {
		t.Fatalf("the replay spawned a subprocess (%d total), which is the cost the feature removes", got)
	}
	if replayed.ReplayedFailure == nil {
		t.Fatal("the replayed failure carries no provenance, so no renderer can say it was replayed")
	}
	if replayed.ReplayedFailure.Attempts != 2 {
		t.Errorf("replay attempts = %d, want 2", replayed.ReplayedFailure.Attempts)
	}
	if replayed.ReplayedFailure.FirstFailedAt.IsZero() {
		t.Error("the replayed failure has no first-failure time, so its age cannot be reported")
	}
	if replayed.CacheHit || replayed.Outcome() != "failed" {
		t.Errorf("the replay was folded into the cache-hit summary: cacheHit=%v outcome=%q",
			replayed.CacheHit, replayed.Outcome())
	}
	if replayed.ExitCode != 3 {
		t.Errorf("replayed exit code = %d, want the original 3", replayed.ExitCode)
	}
	// Point 5: a replayed failure is not quieter than a fresh one.
	if !hasEvent(replayed.Events, EventTypeDiagnostic, "main.go is broken") {
		t.Errorf("the replay lost the original diagnostics: %+v", replayed.Events)
	}
	if !hasEvent(replayed.Events, EventTypeLog, "probe failed on main.go") {
		t.Errorf("the replay lost the original error logs the failure detail is built from: %+v", replayed.Events)
	}
	// Point 6: a replayed failure stops downstream work EXACTLY as a fresh one
	// — same dependent verdict, and the dependent's subprocess still never runs
	// (the count above already proves nothing at all was spawned).
	if got := resultOf(t, second, planned, 1).Status; got != coldDependent {
		t.Fatalf("dependent status after a replayed failure = %q, want the cold run's %q",
			got, coldDependent)
	}

	// --- 3. --retry-failed forces the work and records the new observation.
	third := runSharedScheduler(context.Background(), ws, planned,
		SchedulerConfig{MaxParallel: 1, RetryFailed: true}, newCache())
	if got := subprocessRuns(t, logPath); got != 2 {
		t.Fatalf("--retry-failed spawned %d subprocesses in total, want the probe twice", got)
	}
	retried := resultOf(t, third, planned, 0)
	if retried.ReplayedFailure != nil {
		t.Error("--retry-failed served a replay instead of executing")
	}
	if record := newCache().LookupTaskFailure(coldKey); record == nil || record.Attempts != 3 {
		t.Errorf("attempts after --retry-failed = %+v, want 3", record)
	}

	// --- 4. a changed input is a changed key: the task runs again and passes.
	writeTestFile(t, source, "package main // fixed\n")
	fourth := runSharedScheduler(context.Background(), ws, planned, SchedulerConfig{MaxParallel: 1}, newCache())
	if got := resultOf(t, fourth, planned, 0).Status; got != "success" {
		t.Fatalf("fixed probe status = %q, want success", got)
	}
	if got := subprocessRuns(t, logPath); got != 4 {
		consumed := resultOf(t, fourth, planned, 1)
		t.Fatalf("the fixed run spawned %d subprocesses in total, want probe+consume after the two probes (consume: status=%q cacheHit=%v replayed=%v error=%+v)",
			got, consumed.Status, consumed.CacheHit, consumed.ReplayedFailure != nil, consumed.Error)
	}

	// --- 5. a NEW failure at a NEW key starts its own count: no attempt number
	// is inherited from a verdict that belonged to different inputs.
	writeTestFile(t, source, "package main // BROKEN again\n")
	fifth := runSharedScheduler(context.Background(), ws, planned, SchedulerConfig{MaxParallel: 1}, newCache())
	reBroken := resultOf(t, fifth, planned, 0)
	if reBroken.ReplayedFailure != nil {
		t.Error("a task with changed inputs replayed an old failure")
	}
	if reBroken.CacheKey == coldKey {
		t.Fatal("changing a keyed source file did not change the cache key")
	}
	if record := newCache().LookupTaskFailure(reBroken.CacheKey); record == nil || record.Attempts != 1 {
		t.Errorf("attempts for a failure at a new key = %+v, want 1", record)
	}
}

func hasEvent(events []RawJobEvent, eventType, message string) bool {
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		if strings.Contains(event.Message, message) {
			return true
		}
		if msg, _ := event.Data["message"].(string); strings.Contains(msg, message) {
			return true
		}
	}
	return false
}

// --- carve-outs -------------------------------------------------------------

func failureFixtureJob() *ScheduledJob {
	return declaredJob("build~probe", "probe-task", &extension.TaskDeclaration{})
}

func metaEvent() RawJobEvent {
	return RawJobEvent{Version: 2, Type: EventTypeMeta}
}

// recordThrough runs the executed-task write side exactly as the scheduler does
// and returns whatever is recorded for the key afterwards.
func recordThrough(t *testing.T, f *captureFixture, result *JobResult, cacheEnabled bool, hash string) *store.TaskFailure {
	t.Helper()
	var mu sync.Mutex
	f.sched.finalizeExecutedJob(context.Background(), failureFixtureJob(), result,
		cacheEnabled, hash, "", &mu, map[string]string{})
	return f.cache.LookupTaskFailure(hash)
}

// TestFailureCacheRefusesVerdictsItsKeyCannotExplain is the correctness half of
// the feature: a verdict that depends on host state, on the session being cut
// short, or on a deadline must never be pinned to a cache key.
func TestFailureCacheRefusesVerdictsItsKeyCannotExplain(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"only-a-failure-the-key-explains-is-recorded")

	cases := []struct {
		name         string
		result       *JobResult
		cacheEnabled bool
		hash         string
	}{
		{
			name:         "a timeout depends on host load, not on the key",
			result:       &JobResult{Status: "failed", TimedOut: true, Events: []RawJobEvent{metaEvent()}},
			cacheEnabled: true, hash: captureHashA,
		},
		{
			name: "a sensitive-leak verdict rests on values the key does not describe",
			result: &JobResult{
				Status: "failed",
				Error:  &JobError{Code: extensionproto.FailureSensitiveLeakDetected},
				Events: []RawJobEvent{metaEvent()},
			},
			cacheEnabled: true, hash: captureHashA,
		},
		{
			name:         "a wrapper bail-out never reached the job's own logic",
			result:       &JobResult{Status: "failed"},
			cacheEnabled: true, hash: captureHashA,
		},
		{
			name:         "a canceled task says nothing about its inputs",
			result:       &JobResult{Status: "canceled", Events: []RawJobEvent{metaEvent()}},
			cacheEnabled: true, hash: captureHashA,
		},
		{
			name:         "a skipped task has its own caching rule",
			result:       &JobResult{Status: "skipped", Events: []RawJobEvent{metaEvent()}},
			cacheEnabled: true, hash: captureHashA,
		},
		{
			name:         "--no-cache records nothing",
			result:       &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}},
			cacheEnabled: false, hash: captureHashA,
		},
		{
			name:         "a task with no cache key records nothing",
			result:       &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}},
			cacheEnabled: true, hash: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCaptureFixture(t)
			if got := recordThrough(t, f, tc.result, tc.cacheEnabled, tc.hash); got != nil {
				t.Fatalf("recorded a negative entry the cache key cannot explain: %+v", got)
			}
		})
	}
}

// TestFailureCacheRecordsAFailureItsKeyExplains is the positive control for the
// table above: the same write side DOES record an ordinary failed task.
func TestFailureCacheRecordsAFailureItsKeyExplains(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t)
	result := &JobResult{
		Status:   "failed",
		ExitCode: 3,
		Error:    &JobError{Message: "assertion failed"},
		Events:   []RawJobEvent{metaEvent()},
	}
	record := recordThrough(t, f, result, true, captureHashA)
	if record == nil {
		t.Fatal("an ordinary failure was not recorded")
	}
	if record.Attempts != 1 || record.ExitCode != 3 {
		t.Errorf("record = %+v, want attempts 1 and exit code 3", record)
	}
}

// TestSuccessAtTheSameKeyForgetsTheRecordedFailure pins the third invalidation
// path: a fix that does not move the key still clears the verdict.
func TestSuccessAtTheSameKeyForgetsTheRecordedFailure(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"a-success-clears-the-recorded-failure")
	f := newCaptureFixture(t)

	failed := &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}}
	if recordThrough(t, f, failed, true, captureHashA) == nil {
		t.Fatal("the failure under test was not recorded")
	}
	success := &JobResult{Status: "success", Events: []RawJobEvent{metaEvent()}}
	if got := recordThrough(t, f, success, true, captureHashA); got != nil {
		t.Fatalf("a success at the same key left the failure behind: %+v", got)
	}
}

// TestOneRunCountsOneAttemptPerKey pins the attempt number against a plan-level
// shared node, whose leader and followers all finalize against one key.
func TestOneRunCountsOneAttemptPerKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"one-run-counts-one-observation-per-key")
	f := newCaptureFixture(t)
	failed := func() *JobResult {
		return &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}}
	}
	recordThrough(t, f, failed(), true, captureHashA)
	record := recordThrough(t, f, failed(), true, captureHashA)
	if record == nil || record.Attempts != 1 {
		t.Fatalf("record = %+v, want one attempt counted for one run", record)
	}
}

// TestOneRunReplaysOneObservationPerKey is the read-side twin of the test
// above, and it is not hypothetical: the negative lookup runs BEFORE
// enterSharedExecution, so every member of a plan-level shared node reaches it
// with the same key. Counting each would report "attempt 4" for a failure the
// workspace has been told about twice.
func TestOneRunReplaysOneObservationPerKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"one-run-counts-one-observation-per-key")
	f := newCaptureFixture(t)
	job := failureFixtureJob()
	if recordThrough(t, f, &JobResult{
		Status: "failed",
		Data:   map[string]any{"testSummary": "1 failing"},
		Events: []RawJobEvent{metaEvent()},
	}, true, captureHashA) == nil {
		t.Fatal("the failure under test was not recorded")
	}

	// Three members of one shared node, arriving concurrently as they do.
	const members = 3
	var wg sync.WaitGroup
	replayed := make([]*JobResult, members)
	for i := range members {
		wg.Go(func() { replayed[i] = f.sched.replayFailedEntry(job, captureHashA) })
	}
	wg.Wait()

	for i, result := range replayed {
		if result == nil || result.ReplayedFailure == nil {
			t.Fatalf("member %d was not served the recorded failure: %+v", i, result)
		}
		if result.ReplayedFailure.Attempts != 2 {
			t.Errorf("member %d reported attempt %d, want the run's single observation (2)",
				i, result.ReplayedFailure.Attempts)
		}
	}
	if record := f.cache.LookupTaskFailure(captureHashA); record == nil || record.Attempts != 2 {
		t.Fatalf("persisted record = %+v, want one observation counted for one run", record)
	}
	// Each member owns its payload map. The memoized RECORD is shared; a
	// mutable alias of its data is not, or one member's redaction or renderer
	// would rewrite another's.
	replayed[0].Data["testSummary"] = "rewritten by member 0"
	if replayed[1].Data["testSummary"] != "1 failing" {
		t.Errorf("members share one payload map: member 1 sees %q",
			replayed[1].Data["testSummary"])
	}
}

// TestReplayIsRefusedUnderRetryFailedAndNoCache pins the two escapes at the read
// side, where a recorded entry exists and must still not be served.
func TestReplayIsRefusedUnderRetryFailedAndNoCache(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t)
	job := failureFixtureJob()
	if recordThrough(t, f, &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}},
		true, captureHashA) == nil {
		t.Fatal("the failure under test was not recorded")
	}

	if f.sched.replayFailedEntry(job, captureHashA) == nil {
		t.Fatal("a recorded failure did not replay with no escape in force")
	}
	for name, cfg := range map[string]SchedulerConfig{
		"--retry-failed": {MaxParallel: 1, RetryFailed: true},
		"--no-cache":     {MaxParallel: 1, NoCache: true},
	} {
		t.Run(name, func(t *testing.T) {
			f.sched.cfg = cfg
			if got := f.sched.replayFailedEntry(job, captureHashA); got != nil {
				t.Errorf("%s served a replayed failure: %+v", name, got)
			}
		})
	}
}

// TestRecordedFailureKeepsOnlyTheEventsAReplayNeeds pins the payload boundary:
// error and warning logs survive (the failure detail is rebuilt from them),
// narration does not.
func TestRecordedFailureKeepsOnlyTheEventsAReplayNeeds(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t)
	result := &JobResult{
		Status: "failed",
		Events: []RawJobEvent{
			metaEvent(),
			{Version: 2, Type: EventTypeLog, Level: "info", Message: "starting"},
			{Version: 2, Type: EventTypeLog, Level: "error", Message: "boom"},
			{Version: 2, Type: EventTypeProgress, Message: "50%"},
			{Version: 2, Type: EventTypeDiagnostic, Message: "bad code"},
		},
	}
	record := recordThrough(t, f, result, true, captureHashA)
	if record == nil {
		t.Fatal("the failure under test was not recorded")
	}
	kept := map[string]string{}
	for _, event := range record.Result.Events {
		kept[event.Type] += event.Message
	}
	if !strings.Contains(kept[EventTypeLog], "boom") {
		t.Errorf("the error log was dropped, so a replayed failure would be quieter: %+v", record.Result.Events)
	}
	if strings.Contains(kept[EventTypeLog], "starting") {
		t.Errorf("an info log was persisted: %+v", record.Result.Events)
	}
	if _, ok := kept[EventTypeProgress]; ok {
		t.Errorf("progress narration was persisted: %+v", record.Result.Events)
	}
	if _, ok := kept[EventTypeDiagnostic]; !ok {
		t.Errorf("diagnostics were dropped: %+v", record.Result.Events)
	}
}

// TestReplayedFailureIsNeverPublishedRemotely is contract point 3 at the seam
// the scheduler owns: the negative record lives at an address no remote code
// path derives, so it cannot leave this machine.
func TestReplayedFailureIsNeverPublishedRemotely(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"a-recorded-failure-stays-local")
	f := newCaptureFixture(t)
	if recordThrough(t, f, &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}},
		true, captureHashA) == nil {
		t.Fatal("the failure under test was not recorded")
	}

	// The one address every remote publisher and fetcher derives.
	entry, err := f.cache.LookupTaskEntry(captureHashA)
	if entry != nil || err != nil {
		t.Fatalf("the remote-visible address serves the negative record: entry=%v err=%v", entry, err)
	}
	if store.TaskFailureAddress(captureHashA) == store.TaskEntryAddress(captureHashA) {
		t.Fatal("the negative and positive addresses collide")
	}
}

// TestBatchMembersInheritTheAggregateDeadline keeps the timeout carve-out true
// on the batch path: when one subprocess ran several tasks and the DEADLINE cut
// it short, every member it could not attribute must carry the same structural
// marker, or the failure cache would pin a host-load verdict for each of them.
func TestBatchMembersInheritTheAggregateDeadline(t *testing.T) {
	t.Parallel()
	work := []taskWork{{job: declaredJob("build~a", "task-a", &extension.TaskDeclaration{})}}
	aggregate := &JobResult{Status: "failed", TimedOut: true, Events: []RawJobEvent{metaEvent()}}

	for key, result := range sharedFailureResults(work, aggregate) {
		if !result.TimedOut {
			t.Errorf("batch member %s lost the aggregate's deadline marker: %+v", key, result)
		}
		if recordableFailure(work[0].job, result) {
			t.Errorf("batch member %s would record a timeout as its key's verdict", key)
		}
	}
}

// TestReplayedFailureAgeSurvivesAReRecord pins that the reported age is the age
// of the FAILURE, not of the last observation.
func TestReplayedFailureAgeSurvivesAReRecord(t *testing.T) {
	t.Parallel()
	f := newCaptureFixture(t)
	if recordThrough(t, f, &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}},
		true, captureHashA) == nil {
		t.Fatal("the failure under test was not recorded")
	}
	first := f.cache.LookupTaskFailure(captureHashA).FirstFailedAt
	if time.Since(first) > time.Minute {
		t.Fatalf("first failure time %s is not this run's", first)
	}
	f.sched.failureRecords = nil // a second run re-records at the same key
	if recordThrough(t, f, &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}},
		true, captureHashA) == nil {
		t.Fatal("the re-record did not happen")
	}
	if got := f.cache.LookupTaskFailure(captureHashA); !got.FirstFailedAt.Equal(first) {
		t.Errorf("first failure time moved to %s, want %s", got.FirstFailedAt, first)
	}
}

// --- a green run under a cache bypass -----------------------------------

// flakeFixture plans ONE cacheable task whose verdict is a function of a file
// OUTSIDE the workspace, so nothing in the cache key describes it.
//
// That is the shape of a flake, and it is the ONLY shape in which a task can
// pass at a key that already recorded a failure: a real fix moves the key, and
// then the record is simply unreachable. Reproducing a flake needs the other
// case — same key, new verdict — which is exactly the case the failure cache
// exists to make loud, and therefore the one whose escape must work.
func flakeFixture(t *testing.T, logPath, verdictPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app", "main.go"), "package main\n")

	probe := filepath.Join(root, "probe.sh")
	writeExecutable(t, probe,
		"#!/bin/sh\n"+
			"printf 'probe\\n' >> "+shellQuote(logPath)+"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n"+
			"if grep -q fail "+shellQuote(verdictPath)+"; then\n"+
			"  printf '%s\\n' '{\"v\":2,\"type\":\"diagnostic\",\"data\":{\"severity\":\"error\",\"message\":\"the probe flaked\"}}'\n"+
			"  exit 3\n"+
			"fi\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
			"exit 0\n")

	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	ws.Name = "flake-replay-ws"

	ext := &extension.ExtensionDescription{
		Name:    "@test/probe",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ext"),
		Tasks: map[string]extension.TaskDefinition{
			"probe-task": {Declares: &extension.TaskDeclaration{}},
		},
	}
	commandName, step := jobCommandAndStep("build~probe")
	job := &ScheduledJob{
		Project:   project,
		Extension: ext,
		Step:      &extension.PipelineStep{ID: step, Task: "probe-task"},
		JobDef: &extension.JobDefinition{
			ExtensionName:   "@test/probe",
			Name:            "build~probe",
			CommandName:     commandName,
			StepID:          step,
			Command:         probe,
			Cwd:             "{projectRoot}",
			Cache:           true,
			FilePatterns:    []string{"main.go"},
			TaskCachePolicy: &extension.TaskCachePolicy{NoOutput: true},
		},
	}
	return ws, []*ScheduledJob{job}
}

// TestSuccessUnderACacheBypassClearsTheRecordedFailure exercises this end to end and
// through the REAL scheduler: a flake recorded once used to poison every later
// cached run, because the one flag a user reaches for to get past it —
// --no-cache — was the single path that provably could not clear it.
//
// The bypass suppresses ENTRIES, not identity. A bypassed run still publishes
// nothing positive (step 3) and records no new failure (step 5), but a bypassed
// SUCCESS deletes the stale verdict it just disproved.
func TestSuccessUnderACacheBypassClearsTheRecordedFailure(t *testing.T) {
	t.Parallel()
	for _, check := range []string{
		"a-success-clears-the-recorded-failure",
		"a-success-clears-the-recorded-failure-under-a-cache-bypass",
	} {
		spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache", check)
	}
	requireShell(t)

	logPath := filepath.Join(t.TempDir(), "invocations")
	verdict := filepath.Join(t.TempDir(), "verdict")
	ws, planned := flakeFixture(t, logPath, verdict)
	storeRoot := filepath.Join(t.TempDir(), "store")
	// One machine-global store, a fresh CacheManager per run: that is what a
	// second `putnami` invocation is.
	newCache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(storeRoot))
	}
	warm := SchedulerConfig{MaxParallel: 1}
	bypassed := SchedulerConfig{MaxParallel: 1, NoCache: true}

	// --- 1. the flake fires: the task executes, fails, and is recorded.
	writeTestFile(t, verdict, "fail\n")
	first := runSharedScheduler(context.Background(), ws, planned, warm, newCache())
	if got := resultOf(t, first, planned, 0).Status; got != "failed" {
		t.Fatalf("cold status = %q, want failed", got)
	}
	key := resultOf(t, first, planned, 0).CacheKey
	if key == "" {
		t.Fatal("the failed task reported no cache key, so nothing could be recorded against it")
	}
	if newCache().LookupTaskFailure(key) == nil {
		t.Fatal("the failure under test was not recorded")
	}

	// --- 2. the poison: an ordinary run replays it without executing.
	second := runSharedScheduler(context.Background(), ws, planned, warm, newCache())
	if resultOf(t, second, planned, 0).ReplayedFailure == nil {
		t.Fatal("the recorded failure did not replay, so this test proves nothing about clearing it")
	}
	if got := subprocessRuns(t, logPath); got != 1 {
		t.Fatalf("the replay spawned a subprocess (%d total)", got)
	}

	// --- 3. the escape: --no-cache executes the task and it PASSES at the very
	// same key. That success is proof the recorded verdict is stale.
	writeTestFile(t, verdict, "pass\n")
	third := runSharedScheduler(context.Background(), ws, planned, bypassed, newCache())
	green := resultOf(t, third, planned, 0)
	if green.Status != "success" {
		t.Fatalf("bypassed status = %q, want success", green.Status)
	}
	if got := subprocessRuns(t, logPath); got != 2 {
		t.Fatalf("the bypassed run spawned %d subprocesses in total, want 2", got)
	}
	if green.CacheKey != key {
		t.Fatalf("the bypassed run keyed the task %q, want the recorded %q: "+
			"a bypass suppresses entries, not identity", green.CacheKey, key)
	}
	if entry, err := newCache().LookupTaskEntry(key); entry != nil || err != nil {
		t.Fatalf("the bypassed run published a positive entry: entry=%v err=%v", entry, err)
	}
	if record := newCache().LookupTaskFailure(key); record != nil {
		t.Fatalf("a green bypassed run left the failure it disproved behind: %+v", record)
	}

	// --- 4. the point of all of it: no later run replays the cleared verdict.
	fourth := runSharedScheduler(context.Background(), ws, planned, warm, newCache())
	later := resultOf(t, fourth, planned, 0)
	if later.ReplayedFailure != nil {
		t.Fatalf("a later run replayed a failure a green run disproved: %+v", later.ReplayedFailure)
	}
	if later.Status != "success" {
		t.Fatalf("later status = %q, want success", later.Status)
	}
	if got := subprocessRuns(t, logPath); got != 3 {
		t.Fatalf("spawns = %d, want 3: the later run had to execute, since the bypass published nothing", got)
	}

	// --- 5. the other direction stays refused: a bypassed FAILURE records
	// nothing. Its verdict is not describable by the key it declined to read,
	// which is the whole meaning of the bypass.
	writeTestFile(t, verdict, "fail\n")
	fifth := runSharedScheduler(context.Background(), ws, planned, bypassed, newCache())
	if got := resultOf(t, fifth, planned, 0).Status; got != "failed" {
		t.Fatalf("bypassed re-flake status = %q, want failed", got)
	}
	if record := newCache().LookupTaskFailure(key); record != nil {
		t.Fatalf("a bypassed run recorded a verdict at the key it refused to read: %+v", record)
	}
}

// TestBypassedSuccessForgetsWhileBypassedFailureRecordsNothing is the write
// side of the same asymmetry, at the seam finalizeExecutedJob owns: one
// predicate, two directions.
func TestBypassedSuccessForgetsWhileBypassedFailureRecordsNothing(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"a-success-clears-the-recorded-failure-under-a-cache-bypass")
	f := newCaptureFixture(t)

	failed := &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}}
	if recordThrough(t, f, failed, true, captureHashA) == nil {
		t.Fatal("the failure under test was not recorded")
	}
	// A bypassed failure leaves the record exactly as it found it: no new
	// observation, no reset of the first-failure time.
	bypassedFailure := &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}}
	f.sched.failureRecords = nil
	if got := recordThrough(t, f, bypassedFailure, false, captureHashA); got == nil || got.Attempts != 1 {
		t.Fatalf("a bypassed failure rewrote the record: %+v", got)
	}
	// A bypassed success deletes it.
	success := &JobResult{Status: "success", Events: []RawJobEvent{metaEvent()}}
	if got := recordThrough(t, f, success, false, captureHashA); got != nil {
		t.Fatalf("a bypassed success left the failure behind: %+v", got)
	}
	// A task with no identity has nothing to forget, and must stay a no-op.
	if recordThrough(t, f, failed, true, captureHashA) == nil {
		t.Fatal("the failure could not be re-recorded")
	}
	if got := recordThrough(t, f, success, false, ""); got != nil {
		t.Fatalf("an empty key forgot something: %+v", got)
	}
	if f.cache.LookupTaskFailure(captureHashA) == nil {
		t.Fatal("a keyless task's success deleted another key's record")
	}
}

// TestAHostedRunRecordsNoFailure keeps a hosted run's failures out of the
// failure cache: the run installs offline, which the key of a task that does
// not declare the offline signal does not describe.
func TestAHostedRunRecordsNoFailure(t *testing.T) {
	job := declaredJob("build~a", "task-a", &extension.TaskDeclaration{})
	failed := &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent()}}
	if !recordableFailure(job, failed) {
		t.Fatal("the failure under test is not recordable on a local run")
	}
	t.Cleanup(runcredential.SetForTest("run-bearer"))
	if recordableFailure(job, failed) {
		t.Error("a hosted run would record its failure for replay")
	}
}
