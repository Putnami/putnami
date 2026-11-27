package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// heldTestStore publishes, in a store of its own, the entry each task of
// heldTestPlan is served from, and waits until a lookup witnesses them.
func heldTestStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, job := range heldTestPlan() {
		publishHeldTestEntry(t, root, job.Key())
	}
	for _, job := range heldTestPlan() {
		awaitWitnessed(t, root, heldTestKey(job.Key()))
	}
	return root
}

// awaitWitnessed waits until a lookup of key in the store at root witnesses
// its entry: once the entry's files are the file index's margin old
// (store.EntryWitness).
func awaitWitnessed(t *testing.T, root, key string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		entry, err := store.NewCacheManager(store.NewLocalStore(root)).LookupTaskEntry(key)
		if err != nil || entry == nil {
			t.Fatalf("lookup of %s = %v, %v; want the published entry", key, entry, err)
		}
		if entry.Witness.Sound() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lookup of %s witnessed its entry", key)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// publishHeldTestEntry publishes the entry the task jobKey is served from.
func publishHeldTestEntry(t *testing.T, root, jobKey string) {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(root))
	if _, err := cache.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
		Key:      heldTestKey(jobKey),
		Result:   &store.EntryResult{Status: "success"},
		Metadata: &store.EntryMetadata{Extension: "@putnami/test", Task: "build", Project: "app"},
	}); err != nil {
		t.Fatalf("publish the entry of %s: %v", jobKey, err)
	}
}

// heldTestEntryDir is the blob directory of the entry the task jobKey is
// served from in the store at root (store.LocalStore's layout).
func heldTestEntryDir(root, jobKey string) string {
	address := store.TaskEntryAddress(heldTestKey(jobKey))
	return filepath.Join(root, "blobs", address[:2], address)
}

// heldTestLastUse is the last use stamped on the entry the task jobKey is
// served from, as a hit's lookup stamps it.
func heldTestLastUse(t *testing.T, root, jobKey string) time.Time {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(heldTestEntryDir(root, jobKey), "lastused"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		UsedAt int64 `json:"usedAt"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return time.Unix(0, record.UsedAt)
}

// heldTestRequest is a request a machine worker may hold and answer, on the
// store at root.
func heldTestRequest(t *testing.T, root string, planned []*ScheduledJob, renderer Renderer, events SessionEventHandler) RunRequest {
	t.Helper()
	return RunRequest{
		Workspace:     testWorkspace(t.TempDir()),
		Plan:          planned,
		Config:        SchedulerConfig{SessionID: "answered-session"},
		Renderer:      renderer,
		Cache:         store.NewCacheManager(store.NewLocalStore(root)),
		SessionEvents: events,
	}
}

// servedResult is the result of a run of planned whose every task was served
// from the local cache of the store at root, each with the events its entry
// captured and the witness of the entry its lookup read.
func servedResult(t *testing.T, root string, planned []*ScheduledJob) *SchedulerResult {
	t.Helper()
	results := map[string]*JobResult{}
	served := map[string]store.EntryWitness{}
	for _, job := range planned {
		entry, err := store.NewCacheManager(store.NewLocalStore(root)).LookupTaskEntry(heldTestKey(job.Key()))
		if err != nil || entry == nil {
			t.Fatalf("lookup of the entry of %s = %v, %v", job.Key(), entry, err)
		}
		served[job.Key()] = entry.Witness
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
		served:   served,
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
	root := heldTestStore(t)
	kept := heldTestPlan()
	held := holdRecord(heldTestRequest(t, root, kept, &mockRenderer{}, nil), servedResult(t, root, kept))
	if held == nil {
		t.Fatal("a plan served from the local cache left no record to hold")
	}

	renderer := &mockRenderer{}
	sessions := &sessionRecorder{}
	planned := heldTestPlan()
	req := heldTestRequest(t, root, planned, renderer, sessions.record)
	result, record, refused := answerHeld(context.Background(), req, held)
	if refused != "" {
		t.Fatalf("an equal plan on untouched entries was refused: %s", refused)
	}
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
// whose every task was served from the local cache, from an entry its lookup
// witnessed, or skipped, in a run that succeeded and consulted the cache.
// Anything else leaves an untyped nil, so the worker holds no record for the
// run.
func TestOnlyAPlanThatExecutedNothingIsHeld(t *testing.T) {
	t.Parallel()
	root := heldTestStore(t)
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
		"a local hit's entry was not witnessed": func(_ *RunRequest, result *SchedulerResult) {
			delete(result.served, "/app:test")
		},
		"a local hit read an entry younger than the margin": func(_ *RunRequest, result *SchedulerResult) {
			result.served["/app:test"] = store.EntryWitness{}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			planned := heldTestPlan()
			req, result := heldTestRequest(t, root, planned, &mockRenderer{}, nil), servedResult(t, root, planned)
			change(&req, result)
			if record := holdRecord(req, result); record != nil {
				t.Errorf("held %#v, want nothing", record)
			}
		})
	}

	planned := heldTestPlan()
	result := servedResult(t, root, planned)
	result.Results["/app:test"] = &JobResult{Status: "skipped", Error: &JobError{Message: "every consumer was served from cache"}}
	delete(result.served, "/app:test")
	if holdRecord(heldTestRequest(t, root, planned, &mockRenderer{}, nil), result) == nil {
		t.Error("a plan with a skipped task and a local hit left no record")
	}
}

// TestAPlanThatDiffersFromTheHeldOneRuns pins the fallback: a plan whose jobs,
// order or dependencies differ from the held plan's is not answered, and the
// renderer sees nothing from the held record.
func TestAPlanThatDiffersFromTheHeldOneRuns(t *testing.T) {
	t.Parallel()
	root := heldTestStore(t)
	kept := heldTestPlan()
	held := holdRecord(heldTestRequest(t, root, kept, &mockRenderer{}, nil), servedResult(t, root, kept))
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
		if result, _, _ := answerHeld(context.Background(), heldTestRequest(t, root, planned, renderer, nil), held); result != nil {
			t.Errorf("%s: answered from the held record", name)
		}
		if renderer.started || len(renderer.jobStarts) > 0 {
			t.Errorf("%s: the renderer saw the held record", name)
		}
	}
	if result, _, _ := answerHeld(context.Background(), heldTestRequest(t, root, heldTestPlan(), &mockRenderer{}, nil), "not a record"); result != nil {
		t.Error("answered from something that is no record")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if result, _, _ := answerHeld(canceled, heldTestRequest(t, root, heldTestPlan(), &mockRenderer{}, nil), held); result != nil {
		t.Error("answered a run whose context was already done")
	}
}

// TestAFinalizerThatAmendsAnAnsweredRunDecidesItsVerdict pins the fail-closed
// edge: a result finalizer that adds a failure to an answered run makes it
// fail, as it would a run that computed, and the run holds nothing.
func TestAFinalizerThatAmendsAnAnsweredRunDecidesItsVerdict(t *testing.T) {
	t.Parallel()
	root := heldTestStore(t)
	kept := heldTestPlan()
	held := holdRecord(heldTestRequest(t, root, kept, &mockRenderer{}, nil), servedResult(t, root, kept))
	req := heldTestRequest(t, root, heldTestPlan(), &mockRenderer{}, nil)
	req.Config.FinalizeResults = func(results map[string]*JobResult) {
		results["/app:spec-gate"] = &JobResult{Status: "failed", Error: &JobError{Message: "unproven requirement"}}
	}
	result, record, _ := answerHeld(context.Background(), req, held)
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
	refused  string
}

func (f *fakeHeldRuns) Take(root string) any {
	f.root = root
	return f.offer
}

func (f *fakeHeldRuns) Keep(root string, record any, answered bool) {
	f.record, f.answered = record, answered
	f.keeps++
}

func (f *fakeHeldRuns) Refused(_, reason string) {
	f.refused = reason
}

// TestRunOrAnswerRunsUnlessTheWorkerOffersTheSamePlan pins the scheduler's
// side of the worker: outside a worker the plan runs and nothing is kept; in
// a worker that offers no record the plan runs and its record is kept; in one
// that offers the same plan's record the plan does not run.
func TestRunOrAnswerRunsUnlessTheWorkerOffersTheSamePlan(t *testing.T) {
	t.Parallel()
	root := heldTestStore(t)
	planned := heldTestPlan()
	req := heldTestRequest(t, root, planned, &mockRenderer{}, nil)
	ran := 0
	run := func() *SchedulerResult {
		ran++
		return servedResult(t, root, planned)
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
	result := runOrAnswer(context.Background(), heldTestRequest(t, root, heldTestPlan(), &mockRenderer{}, nil), offered, run)
	if ran != 2 || result == nil || !result.Success || offered.keeps != 1 || !offered.answered || offered.record != memory.record || offered.refused != "" {
		t.Fatalf("with the same plan's record offered: ran %d, memory %+v, want it answered and kept", ran, offered)
	}

	// An entry the held run was served from moved: the worker is told why,
	// the plan runs, and what it leaves is kept.
	if err := os.RemoveAll(heldTestEntryDir(root, "/app:build")); err != nil {
		t.Fatal(err)
	}
	moved := &fakeHeldRuns{offer: memory.record}
	runOrAnswer(context.Background(), heldTestRequest(t, root, heldTestPlan(), &mockRenderer{}, nil), moved, func() *SchedulerResult {
		ran++
		return &SchedulerResult{}
	})
	if ran != 3 || moved.keeps != 1 || moved.answered || moved.refused != "a served task's cache entry moved: /app:build" {
		t.Fatalf("with an entry moved: ran %d, memory %+v, want the plan run, the reason told and its record kept", ran, moved)
	}
}

// TestAHeldPlanIsAnsweredOnlyWhileTheStoreHoldsItsEntries pins CLI ADR 0116
// §6a's entry witness: the answer reports each task the held run served from
// the local cache as served from it again, so it answers only while the store
// holds the very entry each one read. An untouched store is answered, and the
// answer stamps each entry's last use as a hit's lookup does; an entry
// evicted since, or published again at its address, refuses the answer before
// the renderer sees anything, and stamps no entry.
func TestAHeldPlanIsAnsweredOnlyWhileTheStoreHoldsItsEntries(t *testing.T) {
	t.Parallel()
	moves := map[string]func(t *testing.T, root string){
		"untouched": func(*testing.T, string) {},
		"evicted": func(t *testing.T, root string) {
			if err := os.RemoveAll(heldTestEntryDir(root, "/app:test")); err != nil {
				t.Fatal(err)
			}
		},
		"published again": func(t *testing.T, root string) {
			if err := os.RemoveAll(heldTestEntryDir(root, "/app:test")); err != nil {
				t.Fatal(err)
			}
			publishHeldTestEntry(t, root, "/app:test")
		},
		"cleaned": func(t *testing.T, root string) {
			if _, _, err := store.CleanStore(root); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, move := range moves {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := heldTestStore(t)
			kept := heldTestPlan()
			held := holdRecord(heldTestRequest(t, root, kept, &mockRenderer{}, nil), servedResult(t, root, kept))
			if held == nil {
				t.Fatal("a plan served from witnessed entries left no record to hold")
			}
			stamped := heldTestLastUse(t, root, "/app:build")
			move(t, root)

			renderer := &mockRenderer{}
			before := time.Now()
			result, record, refused := answerHeld(context.Background(), heldTestRequest(t, root, heldTestPlan(), renderer, nil), held)
			if name == "untouched" {
				if result == nil || record != held || refused != "" {
					t.Fatalf("untouched entries: answered %v, record kept %v, refused %q; want answered and kept", result != nil, record == held, refused)
				}
				for _, key := range []string{"/app:build", "/app:test"} {
					if used := heldTestLastUse(t, root, key); used.Before(before) {
						t.Errorf("%s: last use %v, want stamped after the answer at or after %v", key, used, before)
					}
				}
				return
			}
			if result != nil || record != nil || !strings.HasPrefix(refused, "a served task's cache entry moved: ") {
				t.Fatalf("%s: answered %v, record %v, refused %q; want refused for the moved entry", name, result != nil, record, refused)
			}
			if renderer.started || len(renderer.jobStarts) > 0 {
				t.Errorf("%s: the renderer saw the held record", name)
			}
			if name != "cleaned" {
				if refused != "a served task's cache entry moved: /app:test" {
					t.Errorf("%s: refused %q, want the moved entry's task named", name, refused)
				}
				if used := heldTestLastUse(t, root, "/app:build"); !used.Equal(stamped) {
					t.Errorf("%s: a refused answer stamped another entry's last use: %v, was %v", name, used, stamped)
				}
			}
		})
	}
}

// TestAWorkersRunWitnessesTheEntriesItsHitsRead pins the scheduler's half of
// the witness on a real run: a run a worker serves records, for each task it
// served from the local cache, the witness of the entry the task's lookup
// read, and the record the run leaves answers a later plan until that entry
// moves. A run outside a worker records none.
func TestAWorkersRunWitnessesTheEntriesItsHitsRead(t *testing.T) {
	t.Parallel()
	requireShell(t)
	ws, planned := inputDigestFixture(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	request := func() RunRequest {
		return RunRequest{
			Workspace: ws, Plan: planned, Config: SchedulerConfig{MaxParallel: 1},
			Renderer: &mockRenderer{}, Cache: store.NewCacheManager(store.NewLocalStore(storeRoot)),
		}
	}
	// What RunPlan builds in a worker.
	worker := func(req RunRequest) *SchedulerResult {
		s := req.scheduler()
		s.servedEntries = map[string]store.EntryWitness{}
		return s.Run(context.Background())
	}

	cold := worker(request())
	if holdRecord(request(), cold) != nil {
		t.Fatal("a run that executed its tasks left a record")
	}
	for _, job := range planned {
		awaitWitnessed(t, storeRoot, cold.Results[job.Key()].CacheKey)
	}
	if outside := request().scheduler().Run(context.Background()); outside.served != nil {
		t.Errorf("a run outside a worker recorded witnesses: %v", outside.served)
	}
	req := request()
	warm := worker(req)
	for _, job := range planned {
		if r := warm.Results[job.Key()]; r == nil || r.Reuse != ReuseLocalCache || !warm.served[job.Key()].Sound() {
			t.Fatalf("%s: result %+v, witness %+v; want a local hit with the witness of its entry", job.Key(), r, warm.served[job.Key()])
		}
	}
	held := holdRecord(req, warm)
	if held == nil {
		t.Fatal("a run served from witnessed entries left no record")
	}
	if result, _, refused := answerHeld(context.Background(), request(), held); result == nil {
		t.Fatalf("the same plan on untouched entries was not answered: %q", refused)
	}
	if _, _, err := store.CleanStore(storeRoot); err != nil {
		t.Fatal(err)
	}
	if result, _, refused := answerHeld(context.Background(), request(), held); result != nil ||
		!strings.HasPrefix(refused, "a served task's cache entry moved: ") {
		t.Fatalf("after a clean: answered %v, refused %q; want refused for a moved entry", result != nil, refused)
	}
}

// The tests stamp an answer's last use in place, so a test reads it as soon
// as the answer returned and its store outlives the stamping.
func init() { stampLastUse = func(stamp func()) { stamp() } }
