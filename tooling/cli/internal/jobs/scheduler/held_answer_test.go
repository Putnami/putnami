package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/store"
)

// heldTestPlan is a two-job plan, b after a, built afresh as each run plans.
func heldTestPlan() []*ScheduledJob {
	return []*ScheduledJob{
		makeScheduledJob("app", "build", nil),
		makeScheduledJob("app", "test", []string{"/app:build"}),
	}
}

// heldTestKey is the cache key a task of the held run was served at.
func heldTestKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// heldTestRequest is a request a machine worker may hold and answer.
func heldTestRequest(t *testing.T, planned []*ScheduledJob, renderer Renderer, events SessionEventHandler) RunRequest {
	t.Helper()
	return RunRequest{
		Workspace:     testWorkspace(t.TempDir()),
		Plan:          planned,
		Config:        SchedulerConfig{SessionID: "answered-session"},
		Renderer:      renderer,
		Cache:         store.NewCacheManager(store.NewLocalStore(t.TempDir())),
		SessionEvents: events,
	}
}

// servedResult is the result of a run of planned whose every task was served
// from the local cache, each with the events its entry captured.
func servedResult(planned []*ScheduledJob) *SchedulerResult {
	results := map[string]*JobResult{}
	for _, job := range planned {
		results[job.Key()] = &JobResult{
			Status:   "success",
			Reuse:    ReuseLocalCache,
			CacheKey: heldTestKey(job.Key()),
			Events: []RawJobEvent{
				{Type: EventTypeLog, Message: "captured " + job.Key()},
				{Type: EventTypeResult, Message: "result of " + job.Key()},
			},
		}
	}
	outcome := SessionOutcome{Duration: 3 * time.Second}
	session := reduceSchedulerTasks(RunTasks(planned, results), outcome, outcome.Duration, nil)
	return &SchedulerResult{
		Results:  results,
		Success:  session.Success(),
		Outcome:  outcome,
		Duration: outcome.Duration,
		Tuning:   &TuningReport{Parallel: ParallelDecision{Mode: "auto", Workers: 3, LogicalCPU: 8, PlannedJobs: len(planned)}},
		Session:  session,
	}
}

// sessionRecorder records the session records a run emits.
type sessionRecorder struct {
	mu      sync.Mutex
	records []SessionRecord
}

func (r *sessionRecorder) record(record SessionRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
}

// TestAHeldPlanIsAnsweredAsALocalHitIsRendered pins CLI ADR 0116 §6a's answer:
// a plan equal to the held one is answered without running it, the renderer
// and the session see, task by task in plan order, what a local-cache hit
// makes them see, and the result is the held one with the current times.
func TestAHeldPlanIsAnsweredAsALocalHitIsRendered(t *testing.T) {
	t.Parallel()
	kept := heldTestPlan()
	held := holdRecord(heldTestRequest(t, kept, &mockRenderer{}, nil), servedResult(kept))
	if held == nil {
		t.Fatal("a plan served from the local cache left no record to hold")
	}

	renderer := &mockRenderer{}
	sessions := &sessionRecorder{}
	planned := heldTestPlan()
	req := heldTestRequest(t, planned, renderer, sessions.record)
	result, record := answerHeld(context.Background(), req, held)
	if result == nil {
		t.Fatal("an equal plan was not answered from the held record")
	}
	if record != held {
		t.Error("the answer does not keep the record it was answered from")
	}

	want := []string{"/app:build", "/app:test"}
	if !renderer.started || !renderer.finished {
		t.Errorf("renderer started=%v finished=%v, want both", renderer.started, renderer.finished)
	}
	if !slices.Equal(renderer.jobStarts, want) || !slices.Equal(renderer.jobEnds, want) {
		t.Errorf("job starts %v and ends %v, want %v in plan order", renderer.jobStarts, renderer.jobEnds, want)
	}
	for _, key := range want {
		events := renderer.jobEvents[key]
		if len(events) != 2 || events[0].Message != "captured "+key || events[1].Type != EventTypeResult || events[1].Message != "result of "+key {
			t.Errorf("%s replayed %+v, want its captured log and result events", key, events)
		}
	}
	if len(sessions.records) != 3 || sessions.records[0].Type != "scheduler:parallel" ||
		sessions.records[0].Data["sessionId"] != "answered-session" || sessions.records[0].Data["workers"] != 3 {
		t.Fatalf("session records %+v, want this session's opening record with the held decision, then one job:end per task", sessions.records)
	}
	for i, key := range want {
		if r := sessions.records[i+1]; r.Type != SessionRecordJobEnd || r.JobKey != key || r.Task == nil || r.Task.InputDigest == "" {
			t.Errorf("session record %d = %+v, want the job:end of %s with its input digest", i+1, r, key)
		}
	}

	heldResult := held.(*heldPlan).result
	if !result.Success || result.Tuning != heldResult.Tuning || result.Session.Tasks != 2 || result.Session.Cached() != 2 {
		t.Errorf("result success=%v tuning=%p session=%+v, want the held verdict, tuning and counts", result.Success, result.Tuning, result.Session)
	}
	if result.Duration >= 3*time.Second || result.Session.Duration != result.Duration || result.Outcome.Duration != result.Duration {
		t.Errorf("durations %v, %v, %v, want this answer's own wall", result.Duration, result.Session.Duration, result.Outcome.Duration)
	}
	for _, key := range want {
		got, kept := result.Results[key], heldResult.Results[key]
		if got == kept {
			t.Errorf("%s: the answer hands out the held result itself; a caller's change would reach the next answer", key)
		}
		if got.Status != kept.Status || got.Reuse != ReuseLocalCache || got.CacheKey != kept.CacheKey {
			t.Errorf("%s answered %+v, want the held %+v", key, got, kept)
		}
	}
}

// TestOnlyAPlanThatExecutedNothingIsHeld pins what a worker may hold: a plan
// whose every task was served from the local cache or skipped, in a run that
// succeeded and consulted the cache. Anything else leaves an untyped nil, so
// the worker holds no record for the run.
func TestOnlyAPlanThatExecutedNothingIsHeld(t *testing.T) {
	t.Parallel()
	cases := map[string]func(req *RunRequest, result *SchedulerResult){
		"a task executed": func(_ *RunRequest, result *SchedulerResult) {
			result.Results["/app:test"].Reuse = ReuseNone
		},
		"a task reused a remote entry": func(_ *RunRequest, result *SchedulerResult) {
			result.Results["/app:test"].Reuse = ReuseRemoteCache
		},
		"a task is missing": func(_ *RunRequest, result *SchedulerResult) {
			delete(result.Results, "/app:test")
		},
		"the run failed": func(_ *RunRequest, result *SchedulerResult) {
			result.Success = false
		},
		"the run was cut short": func(_ *RunRequest, result *SchedulerResult) {
			result.Outcome.Aborted = true
		},
		"the run refused the cache": func(req *RunRequest, _ *SchedulerResult) {
			req.Config.NoCache = true
		},
		"the run verified the cache": func(req *RunRequest, _ *SchedulerResult) {
			req.Config.CacheVerification = true
		},
		"the run had no cache": func(req *RunRequest, _ *SchedulerResult) {
			req.Cache = nil
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			planned := heldTestPlan()
			req, result := heldTestRequest(t, planned, &mockRenderer{}, nil), servedResult(planned)
			change(&req, result)
			if record := holdRecord(req, result); record != nil {
				t.Errorf("held %#v, want nothing", record)
			}
		})
	}

	planned := heldTestPlan()
	result := servedResult(planned)
	result.Results["/app:test"] = &JobResult{Status: "skipped", Error: &JobError{Message: "every consumer was served from cache"}}
	if holdRecord(heldTestRequest(t, planned, &mockRenderer{}, nil), result) == nil {
		t.Error("a plan with a skipped task and a local hit left no record")
	}
}

// TestAPlanThatDiffersFromTheHeldOneRuns pins the fallback: a plan whose jobs,
// order or dependencies differ from the held plan's is not answered, and the
// renderer sees nothing from the held record.
func TestAPlanThatDiffersFromTheHeldOneRuns(t *testing.T) {
	t.Parallel()
	kept := heldTestPlan()
	held := holdRecord(heldTestRequest(t, kept, &mockRenderer{}, nil), servedResult(kept))
	if held == nil {
		t.Fatal("a plan served from the local cache left no record to hold")
	}
	plans := map[string][]*ScheduledJob{
		"another job": {makeScheduledJob("app", "build", nil), makeScheduledJob("app", "lint", []string{"/app:build"})},
		"another order": {
			makeScheduledJob("app", "test", []string{"/app:build"}), makeScheduledJob("app", "build", nil),
		},
		"another dependency": {makeScheduledJob("app", "build", nil), makeScheduledJob("app", "test", nil)},
		"one job fewer":      {makeScheduledJob("app", "build", nil)},
	}
	for name, planned := range plans {
		renderer := &mockRenderer{}
		if result, _ := answerHeld(context.Background(), heldTestRequest(t, planned, renderer, nil), held); result != nil {
			t.Errorf("%s: answered from the held record", name)
		}
		if renderer.started || len(renderer.jobStarts) > 0 {
			t.Errorf("%s: the renderer saw the held record", name)
		}
	}
	if result, _ := answerHeld(context.Background(), heldTestRequest(t, heldTestPlan(), &mockRenderer{}, nil), "not a record"); result != nil {
		t.Error("answered from something that is no record")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if result, _ := answerHeld(canceled, heldTestRequest(t, heldTestPlan(), &mockRenderer{}, nil), held); result != nil {
		t.Error("answered a run whose context was already done")
	}
}

// TestAFinalizerThatAmendsAnAnsweredRunDecidesItsVerdict pins the fail-closed
// edge: a result finalizer that adds a failure to an answered run makes it
// fail, as it would a run that computed, and the run holds nothing.
func TestAFinalizerThatAmendsAnAnsweredRunDecidesItsVerdict(t *testing.T) {
	t.Parallel()
	kept := heldTestPlan()
	held := holdRecord(heldTestRequest(t, kept, &mockRenderer{}, nil), servedResult(kept))
	req := heldTestRequest(t, heldTestPlan(), &mockRenderer{}, nil)
	req.Config.FinalizeResults = func(results map[string]*JobResult) {
		results["/app:spec-gate"] = &JobResult{Status: "failed", Error: &JobError{Message: "unproven requirement"}}
	}
	result, record := answerHeld(context.Background(), req, held)
	if result == nil {
		t.Fatal("an equal plan was not answered from the held record")
	}
	if result.Success || result.Session.Success() || len(result.Session.Failures) != 1 {
		t.Errorf("success=%v session=%+v, want the finalizer's failure to decide the verdict", result.Success, result.Session)
	}
	if record != nil {
		t.Errorf("held %#v after a finalizer amended the answer, want nothing", record)
	}
}

// fakeHeldRuns is a worker's memory that offers one record and remembers
// what it was handed.
type fakeHeldRuns struct {
	offer    any
	root     string
	record   any
	answered bool
	keeps    int
}

func (f *fakeHeldRuns) Take(root string) any {
	f.root = root
	return f.offer
}

func (f *fakeHeldRuns) Keep(root string, record any, answered bool) {
	f.record, f.answered = record, answered
	f.keeps++
}

// TestRunOrAnswerRunsUnlessTheWorkerOffersTheSamePlan pins the scheduler's
// side of the worker: outside a worker the plan runs and nothing is kept; in
// a worker that offers no record the plan runs and its record is kept; in one
// that offers the same plan's record the plan does not run.
func TestRunOrAnswerRunsUnlessTheWorkerOffersTheSamePlan(t *testing.T) {
	t.Parallel()
	planned := heldTestPlan()
	req := heldTestRequest(t, planned, &mockRenderer{}, nil)
	ran := 0
	run := func() *SchedulerResult {
		ran++
		return servedResult(planned)
	}

	runOrAnswer(context.Background(), req, nil, run)
	if ran != 1 {
		t.Fatalf("outside a worker the plan ran %d times, want once", ran)
	}

	memory := &fakeHeldRuns{}
	runOrAnswer(context.Background(), req, memory, run)
	if ran != 2 || memory.root != req.Workspace.Root || memory.keeps != 1 || memory.answered || memory.record == nil {
		t.Fatalf("with no record offered: ran %d, memory %+v, want the plan run and its record kept", ran, memory)
	}

	offered := &fakeHeldRuns{offer: memory.record}
	result := runOrAnswer(context.Background(), heldTestRequest(t, heldTestPlan(), &mockRenderer{}, nil), offered, run)
	if ran != 2 || result == nil || !result.Success || offered.keeps != 1 || !offered.answered || offered.record != memory.record {
		t.Fatalf("with the same plan's record offered: ran %d, memory %+v, want it answered and kept", ran, offered)
	}
}
