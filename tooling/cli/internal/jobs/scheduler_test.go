package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// mockRenderer is a thread-safe test renderer that records calls.
type mockRenderer struct {
	mu        sync.Mutex
	started   bool
	finished  bool
	jobStarts []string
	jobEnds   []string
	jobEvents map[string][]RawJobEvent
	results   map[string]*JobResult
	outcome   SessionOutcome
}

// schedulerLeaseTestCoalescer exercises the generic lease loop against the
// task-owned store address without involving task materialization.
type schedulerLeaseTestCoalescer struct {
	cache *store.CacheManager
}

func (c schedulerLeaseTestCoalescer) claim(hash string, estimatedCost time.Duration) (bool, func()) {
	return c.cache.TryClaimTaskEntry(hash, estimatedCost)
}

func (c schedulerLeaseTestCoalescer) wait(ctx context.Context, hash string, timeout time.Duration) error {
	return c.cache.WaitForTaskEntry(ctx, hash, timeout)
}

func (schedulerLeaseTestCoalescer) restore(context.Context, string) *JobResult {
	return nil
}

func (m *mockRenderer) Start(planned []*ScheduledJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = true
}

func (m *mockRenderer) JobStart(job *ScheduledJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobStarts = append(m.jobStarts, job.Key())
}

func (m *mockRenderer) JobEvent(job *ScheduledJob, event RawJobEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobEvents == nil {
		m.jobEvents = map[string][]RawJobEvent{}
	}
	m.jobEvents[job.Key()] = append(m.jobEvents[job.Key()], event)
}

// eventsOfType returns the recorded job:event records of a given type for a job
// key, in emission order.
func (m *mockRenderer) eventsOfType(jobKey, eventType string) []RawJobEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []RawJobEvent
	for _, ev := range m.jobEvents[jobKey] {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

func (m *mockRenderer) JobComplete(job *ScheduledJob, result *JobResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobEnds = append(m.jobEnds, job.Key())
}

func (m *mockRenderer) Finish(results map[string]*JobResult, outcome SessionOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finished = true
	m.results = results
	m.outcome = outcome
}

func makeScheduledJob(projName, jobName string, deps []string) *ScheduledJob {
	return &ScheduledJob{
		Project: &workspace.Project{
			ID:   "/" + projName,
			Name: projName,
			Path: projName,
		},
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/test",
			Path: "/ext/test",
		},
		JobDef: &extension.JobDefinition{
			Name:          jobName,
			ExtensionName: "@putnami/test",
			Command:       "echo",
			Args:          []string{"hello"},
		},
		DependsOn: deps,
	}
}

// TestReplayCacheEvents_SynthesizesResultForLegacyEntry pins the
// upgrade-compatibility fix: cache keys omit the CLI version, so an entry written
// before result events were captured stays valid across an upgrade with
// result.Data but no cached result event. replayCacheEvents must synthesize the
// result event from the restored result so that legacy warm hit still emits the
// testSummary a cold run streams — replayed last, after the captured metrics.
func TestReplayCacheEvents_SynthesizesResultForLegacyEntry(t *testing.T) {
	mock := &mockRenderer{}
	s := &Scheduler{renderer: mock}
	job := makeScheduledJob("app", "test~test", nil)

	result := &JobResult{
		Status:   "success",
		CacheHit: true,
		Data:     map[string]any{"testSummary": map[string]any{"passed": float64(7)}},
		// The pre-fix allow-list captured metrics/diagnostics but not the result.
		Events: []RawJobEvent{{Version: 1, Type: EventTypeMetric, Data: map[string]any{"name": "coverage", "value": float64(88)}}},
	}
	s.replayCacheEvents(job, result)

	got := mock.eventsOfType(job.Key(), EventTypeResult)
	if len(got) != 1 {
		t.Fatalf("expected one synthesized result event, got %d", len(got))
	}
	data, _ := got[0].Data["data"].(map[string]any)
	ts, _ := data["testSummary"].(map[string]any)
	if ts == nil || ts["passed"] != float64(7) {
		t.Fatalf("synthesized result event lost testSummary: %#v", got[0])
	}
	if len(mock.eventsOfType(job.Key(), EventTypeMetric)) != 1 {
		t.Fatal("captured metric event must still replay alongside the synthesized result")
	}
	all := mock.jobEvents[job.Key()]
	if all[len(all)-1].Type != EventTypeResult {
		t.Errorf("result event must be replayed last, order = %v", all)
	}
}

// TestReplayCacheEvents_NoSynthesisWhenResultCaptured ensures a modern entry that
// already captured its result event replays it once, with no synthesized dup.
func TestReplayCacheEvents_NoSynthesisWhenResultCaptured(t *testing.T) {
	mock := &mockRenderer{}
	s := &Scheduler{renderer: mock}
	job := makeScheduledJob("app", "test~test", nil)

	result := &JobResult{
		Status: "success",
		Data:   map[string]any{"testSummary": map[string]any{"passed": float64(7)}},
		Events: []RawJobEvent{{Version: 1, Type: EventTypeResult, Data: map[string]any{
			"status": "success",
			"data":   map[string]any{"testSummary": map[string]any{"passed": float64(7)}},
		}}},
	}
	s.replayCacheEvents(job, result)

	if got := mock.eventsOfType(job.Key(), EventTypeResult); len(got) != 1 {
		t.Fatalf("captured result event must replay exactly once (no synthesized dup), got %d", len(got))
	}
}

// TestReplayCacheEvents_NoSynthesisForDatalessJob ensures a plain build hit with
// no report payload does not gain a spurious result event, only job:start.
func TestReplayCacheEvents_NoSynthesisForDatalessJob(t *testing.T) {
	mock := &mockRenderer{}
	s := &Scheduler{renderer: mock}
	job := makeScheduledJob("app", "build~cross-compile", nil)

	s.replayCacheEvents(job, &JobResult{Status: "success", CacheHit: true})

	if got := mock.eventsOfType(job.Key(), EventTypeResult); len(got) != 0 {
		t.Fatalf("data-less job must not synthesize a result event, got %d", len(got))
	}
	if len(mock.jobStarts) != 1 {
		t.Fatalf("JobStart must still fire on a cache hit, got %v", mock.jobStarts)
	}
}

func TestDAGState_Ready(t *testing.T) {
	// A → B → C
	jobA := makeScheduledJob("proj", "a", nil)
	jobB := makeScheduledJob("proj", "b", []string{"/proj:a"})
	jobC := makeScheduledJob("proj", "c", []string{"/proj:b"})

	state := newDAGState([]*ScheduledJob{jobA, jobB, jobC})

	ready := state.ready()
	if len(ready) != 1 {
		t.Fatalf("expected 1 ready job, got %d", len(ready))
	}
	if ready[0].Key() != "/proj:a" {
		t.Errorf("expected /proj:a ready, got %s", ready[0].Key())
	}
}

func TestDAGState_CompleteUnlocksDependents(t *testing.T) {
	jobA := makeScheduledJob("proj", "a", nil)
	jobB := makeScheduledJob("proj", "b", []string{"/proj:a"})
	jobC := makeScheduledJob("proj", "c", []string{"/proj:a"})

	state := newDAGState([]*ScheduledJob{jobA, jobB, jobC})

	// Complete A should unlock B and C
	newReady := state.complete("/proj:a")
	if len(newReady) != 2 {
		t.Fatalf("expected 2 newly ready, got %d", len(newReady))
	}

	names := make(map[string]bool)
	for _, j := range newReady {
		names[j.Key()] = true
	}
	if !names["/proj:b"] || !names["/proj:c"] {
		t.Errorf("expected /proj:b and /proj:c, got %v", names)
	}
}

func TestDAGState_DiamondDependency(t *testing.T) {
	//   A
	//  / \
	// B   C
	//  \ /
	//   D
	jobA := makeScheduledJob("proj", "a", nil)
	jobB := makeScheduledJob("proj", "b", []string{"/proj:a"})
	jobC := makeScheduledJob("proj", "c", []string{"/proj:a"})
	jobD := makeScheduledJob("proj", "d", []string{"/proj:b", "/proj:c"})

	state := newDAGState([]*ScheduledJob{jobA, jobB, jobC, jobD})

	// Initially only A is ready
	ready := state.ready()
	if len(ready) != 1 || ready[0].Key() != "/proj:a" {
		t.Fatalf("expected only A ready, got %v", ready)
	}

	// Complete A → B and C become ready
	newReady := state.complete("/proj:a")
	if len(newReady) != 2 {
		t.Fatalf("expected 2 ready after A, got %d", len(newReady))
	}

	// Complete B → D not yet ready (still waiting for C)
	newReady = state.complete("/proj:b")
	if len(newReady) != 0 {
		t.Fatalf("expected 0 ready after B (D waits for C), got %d", len(newReady))
	}

	// Complete C → D becomes ready
	newReady = state.complete("/proj:c")
	if len(newReady) != 1 || newReady[0].Key() != "/proj:d" {
		t.Fatalf("expected D ready after C, got %v", newReady)
	}
}

func TestDAGState_ParallelReady(t *testing.T) {
	// Independent jobs: A, B, C (no deps)
	jobA := makeScheduledJob("p1", "build", nil)
	jobB := makeScheduledJob("p2", "build", nil)
	jobC := makeScheduledJob("p3", "build", nil)

	state := newDAGState([]*ScheduledJob{jobA, jobB, jobC})

	ready := state.ready()
	if len(ready) != 3 {
		t.Fatalf("expected 3 ready, got %d", len(ready))
	}
}

func TestSchedulerConfig_MaxParallel(t *testing.T) {
	cfg := SchedulerConfig{MaxParallel: 2}
	if cfg.MaxParallel != 2 {
		t.Errorf("MaxParallel = %d, want 2", cfg.MaxParallel)
	}
}

// TestScheduler_CoalescesConcurrentColdMissAcrossWorktrees exercises the
// production miss path with two independent schedulers and workspace roots
// sharing one machine-global store. The winner blocks inside the real job long
// enough for the sibling to observe the cold miss; only one subprocess may run,
// and the loser must return through the cache-hit restoration path.
func TestScheduler_CoalescesConcurrentColdMissAcrossWorktrees(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	sharedRoot := t.TempDir()
	storeRoot := filepath.Join(sharedRoot, "store")
	counterPath := filepath.Join(sharedRoot, "executions")
	releasePath := filepath.Join(sharedRoot, "release")
	leaseWaitEntered := make(chan struct{}, 1)
	notifyLeaseWait := func(*ScheduledJob) {
		select {
		case leaseWaitEntered <- struct{}{}:
		default:
		}
	}
	scriptPath := filepath.Join(sharedRoot, "cold-job.sh")
	script := "#!/bin/sh\n" +
		"printf 'run\\n' >> '" + counterPath + "'\n" +
		boundedReleaseWaitScript(releasePath, os.Getpid(), releaseWaitMaxPolls)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write cold-job script: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(releasePath, []byte("release"), 0o644) })

	type schedulerRun struct {
		scheduler *Scheduler
		jobKey    string
	}
	newRun := func() schedulerRun {
		root := t.TempDir()
		projectDir := filepath.Join(root, "proj")
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectDir, "input.txt"), []byte("same input\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
		ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
		ws.Name = "lease-integration"
		const task = "lint-cold"
		job := &ScheduledJob{
			Project: project,
			Extension: &extension.ExtensionDescription{
				Name: "@putnami/test", Path: sharedRoot,
				Tasks: map[string]extension.TaskDefinition{
					task: {Declares: &extension.TaskDeclaration{}},
				},
			},
			Step: &extension.PipelineStep{Task: task},
			JobDef: &extension.JobDefinition{
				Name:          "lint~cold",
				ExtensionName: "@putnami/test",
				Command:       scriptPath,
				TimeoutMs:     unboundedJobTimeoutMs,
				Cache:         true,
				FilePatterns:  []string{"input.txt"},
				TaskCachePolicy: &extension.TaskCachePolicy{
					NoOutput: true,
				},
			},
		}
		cache := store.NewCacheManager(store.NewLocalStore(storeRoot))
		scheduler := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
		scheduler.onCacheLeaseWait = notifyLeaseWait
		return schedulerRun{
			scheduler: scheduler,
			jobKey:    job.Key(),
		}
	}

	runs := []schedulerRun{newRun(), newRun()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan struct{}, len(runs))
	start := make(chan struct{})
	results := make(chan *SchedulerResult, len(runs))
	for _, run := range runs {
		run := run
		go func() {
			ready <- struct{}{}
			<-start
			results <- run.scheduler.Run(ctx)
		}()
	}
	for range runs {
		<-ready
	}
	close(start)

	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(counterPath)
		if len(strings.Fields(string(data))) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("neither scheduler started the cold job")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The release barrier is the scheduler's actual loser-before-wait boundary,
	// not a guessed delay after the winner happened to start its subprocess.
	select {
	case <-leaseWaitEntered:
	case <-ctx.Done():
		t.Fatalf("sibling scheduler did not enter cache lease wait: %v", ctx.Err())
	case <-time.After(3 * time.Second):
		t.Fatal("sibling scheduler did not enter cache lease wait")
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o644); err != nil {
		t.Fatalf("release cold job: %v", err)
	}

	cacheHits := 0
	coalesced := 0
	for i := range runs {
		result := <-results
		jobResult := result.Results[runs[i].jobKey]
		if jobResult == nil || jobResult.Status != "success" {
			t.Fatalf("scheduler %d result = %+v, want success", i, jobResult)
		}
		if jobResult.CacheHit {
			cacheHits++
		}
		if jobResult.Coalesced {
			coalesced++
		}
	}
	if cacheHits != 0 {
		t.Fatalf("cache-hit schedulers = %d, want coalesced waiter kept distinct", cacheHits)
	}
	if coalesced != 1 {
		t.Fatalf("coalesced schedulers = %d, want one waiter", coalesced)
	}
	data, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read execution counter: %v", err)
	}
	if executions := len(strings.Fields(string(data))); executions != 1 {
		t.Fatalf("cold job executed %d times, want exactly one", executions)
	}
}

func TestScheduler_CoalescedSourceFixReexecutesInEveryWorktree(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	sharedRoot := t.TempDir()
	storeRoot := filepath.Join(sharedRoot, "store")
	counterPath := filepath.Join(sharedRoot, "executions")
	releasePath := filepath.Join(sharedRoot, "release")
	leaseWaitEntered := make(chan struct{}, 1)
	notifyLeaseWait := func(*ScheduledJob) {
		select {
		case leaseWaitEntered <- struct{}{}:
		default:
		}
	}
	scriptPath := filepath.Join(sharedRoot, "source-fix.sh")
	script := "#!/bin/sh\n" +
		"printf 'run\\n' >> '" + counterPath + "'\n" +
		boundedReleaseWaitScript(releasePath, os.Getpid(), releaseWaitMaxPolls) +
		"printf 'fixed\\n' > input.txt\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write source-fix script: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(releasePath, []byte("release"), 0o644) })

	type schedulerRun struct {
		scheduler *Scheduler
		jobKey    string
		inputPath string
	}
	newRun := func() schedulerRun {
		root := t.TempDir()
		projectDir := filepath.Join(root, "proj")
		if err := os.MkdirAll(projectDir, 0o755); err != nil {
			t.Fatal(err)
		}
		inputPath := filepath.Join(projectDir, "input.txt")
		if err := os.WriteFile(inputPath, []byte("unfixed\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
		ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
		ws.Name = "source-fix-integration"
		const taskName = "source-fix"
		job := &ScheduledJob{
			Project: project,
			Extension: &extension.ExtensionDescription{
				Name: "@putnami/test",
				Path: sharedRoot,
				Tasks: map[string]extension.TaskDefinition{
					taskName: {Declares: &extension.TaskDeclaration{MutatesSources: true}},
				},
			},
			Step: &extension.PipelineStep{Task: taskName},
			JobDef: &extension.JobDefinition{
				Name:          "lint~format",
				ExtensionName: "@putnami/test",
				Command:       scriptPath,
				TimeoutMs:     unboundedJobTimeoutMs,
				Cache:         true,
				FilePatterns:  []string{"input.txt"},
				Writes: []extension.ResourceRef{{
					ID: sourcesResourceID, Scope: extension.ResourceScopeProject,
				}},
				TaskCachePolicy: &extension.TaskCachePolicy{
					NoOutput: true,
				},
			},
		}
		scheduler := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, store.NewCacheManager(store.NewLocalStore(storeRoot)))
		scheduler.onCacheLeaseWait = notifyLeaseWait
		return schedulerRun{
			scheduler: scheduler,
			jobKey:    job.Key(),
			inputPath: inputPath,
		}
	}

	runs := []schedulerRun{newRun(), newRun()}
	// This suite runs beside hundreds of CPU-heavy jobs in the workspace gate.
	// Give the two scheduler goroutines enough admission time before judging the
	// lease protocol; the happy path remains immediate, and the bounded child
	// plus this context still make a broken test terminate.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ready := make(chan struct{}, len(runs))
	start := make(chan struct{})
	results := make(chan *SchedulerResult, len(runs))
	for _, run := range runs {
		run := run
		go func() {
			ready <- struct{}{}
			<-start
			results <- run.scheduler.Run(ctx)
		}()
	}
	for range runs {
		<-ready
	}
	close(start)

	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(counterPath)
		if len(strings.Fields(string(data))) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("neither scheduler started the source fixer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The release barrier is the scheduler's actual loser-before-wait boundary,
	// not a guessed delay after the winner happened to start its subprocess.
	select {
	case <-leaseWaitEntered:
	case <-ctx.Done():
		t.Fatalf("sibling scheduler did not enter cache lease wait: %v", ctx.Err())
	case <-time.After(3 * time.Second):
		t.Fatal("sibling scheduler did not enter cache lease wait")
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o644); err != nil {
		t.Fatalf("release source fixer: %v", err)
	}

	for range runs {
		result := <-results
		jobResult := result.Results[runs[0].jobKey]
		if jobResult == nil || jobResult.Status != "success" || jobResult.CacheHit || jobResult.Coalesced {
			t.Fatalf("scheduler result = %+v, want a local successful source-fix execution", jobResult)
		}
	}
	for i, run := range runs {
		content, err := os.ReadFile(run.inputPath)
		if err != nil || string(content) != "fixed\n" {
			t.Fatalf("scheduler %d source = %q, err=%v; want fixed", i, content, err)
		}
	}
	data, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatalf("read execution counter: %v", err)
	}
	if executions := len(strings.Fields(string(data))); executions != 2 {
		t.Fatalf("source fixer executions = %d, want owner plus re-executed follower", executions)
	}
}

// releaseWaitMaxPolls keeps the scheduler fixtures patient enough for their
// parent to observe contention. Each child also watches that parent's PID, and
// this finite fallback covers a liveness check defeated by rapid PID reuse.
// Without both child-owned exits, a helper can be reparented to launchd and
// sleep forever, contaminating unrelated runs.
const releaseWaitMaxPolls = 600

func boundedReleaseWaitScript(releasePath string, parentPID, maxPolls int) string {
	return "waited=0\n" +
		"while [ ! -f " + shellQuote(releasePath) + " ]; do\n" +
		fmt.Sprintf("  if ! kill -0 %d 2>/dev/null; then exit 125; fi\n", parentPID) +
		"  waited=$((waited + 1))\n" +
		fmt.Sprintf("  if [ \"$waited\" -ge %d ]; then exit 124; fi\n", maxPolls) +
		"  sleep 0.01\n" +
		"done\n"
}

func TestBoundedReleaseWaitScriptExitsWithoutItsParentRelease(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	for _, tc := range []struct {
		name      string
		parentPID int
		maxPolls  int
		wantExit  int
	}{
		{name: "live parent hits hard bound", parentPID: os.Getpid(), maxPolls: 2, wantExit: 124},
		{name: "missing parent exits immediately", parentPID: 99_999_999, maxPolls: releaseWaitMaxPolls, wantExit: 125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scriptPath := filepath.Join(t.TempDir(), "bounded-wait.sh")
			script := "#!/bin/sh\n" + boundedReleaseWaitScript(
				filepath.Join(t.TempDir(), "never"), tc.parentPID, tc.maxPolls,
			)
			if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			err := exec.Command(scriptPath).Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.wantExit {
				t.Fatalf("bounded waiter error = %v, want exit %d", err, tc.wantExit)
			}
		})
	}
}

func TestScheduler_CoalescedWaiterReclaimsAbandonedLease(t *testing.T) {
	root := t.TempDir()
	owner := store.NewCacheManager(store.NewLocalStore(root))
	waiter := store.NewCacheManager(store.NewLocalStore(root))
	contender := store.NewCacheManager(store.NewLocalStore(root))
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	won, abandon := owner.TryClaimTaskEntry(hash, time.Second)
	if !won {
		t.Fatal("initial owner did not claim cold key")
	}
	t.Cleanup(abandon)

	scheduler := &Scheduler{cache: waiter, cfg: SchedulerConfig{}}
	job := &ScheduledJob{ExpectedWallMs: 1000, JobDef: &extension.JobDefinition{TimeoutMs: 2000}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		abandon()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, release := scheduler.coalesceMiss(ctx, job, hash, schedulerLeaseTestCoalescer{cache: waiter})
	if result != nil {
		t.Fatalf("abandoned lease restored result = %+v, want local execution fallback", result)
	}
	defer release()

	if won, contenderRelease := contender.TryClaimTaskEntry(hash, time.Second); won {
		contenderRelease()
		t.Fatal("waiter returned without owning the replacement lease")
	}
}

func TestScheduler_CoalescedWaiterFallsBackAfterJobTimeout(t *testing.T) {
	root := t.TempDir()
	owner := store.NewCacheManager(store.NewLocalStore(root))
	waiter := store.NewCacheManager(store.NewLocalStore(root))
	const hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	won, releaseOwner := owner.TryClaimTaskEntry(hash, time.Second)
	if !won {
		t.Fatal("initial owner did not claim cold key")
	}
	defer releaseOwner()

	scheduler := &Scheduler{cache: waiter, cfg: SchedulerConfig{}}
	job := &ScheduledJob{ExpectedWallMs: 1000, JobDef: &extension.JobDefinition{TimeoutMs: 40}}
	started := time.Now()
	result, release := scheduler.coalesceMiss(context.Background(), job, hash, schedulerLeaseTestCoalescer{cache: waiter})
	defer release()
	if result != nil {
		t.Fatalf("timed-out waiter restored result = %+v, want local execution fallback", result)
	}
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("wait elapsed = %s, want bounded fallback near the 40ms job timeout", elapsed)
	}
}

func TestScheduler_CoalescingHonorsBreakEvenFloor(t *testing.T) {
	root := t.TempDir()
	owner := store.NewCacheManager(store.NewLocalStore(root))
	cheap := store.NewCacheManager(store.NewLocalStore(root))
	const hash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	won, releaseOwner := owner.TryClaimTaskEntry(hash, time.Second)
	if !won {
		t.Fatal("initial owner did not claim cold key")
	}
	defer releaseOwner()

	scheduler := &Scheduler{cache: cheap, cfg: SchedulerConfig{}}
	job := &ScheduledJob{
		ExpectedWallMs: cache.DefaultBreakEven.MinDurationMs - 1,
		JobDef:         &extension.JobDefinition{TimeoutMs: 2000},
	}
	started := time.Now()
	result, release := scheduler.coalesceMiss(context.Background(), job, hash, schedulerLeaseTestCoalescer{cache: cheap})
	defer release()
	if result != nil {
		t.Fatalf("cheap job restored result = %+v, want independent execution", result)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("cheap job waited %s despite the %dms break-even floor", elapsed, cache.DefaultBreakEven.MinDurationMs)
	}
}

func TestScheduler_EmitsEventlessReuseSessionOutcomeOnce(t *testing.T) {
	tests := []struct {
		name      string
		result    *JobResult
		outcome   string
		cache     bool
		coalesced bool
	}{
		{
			name:    "cached",
			result:  &JobResult{Status: "success", CacheHit: true, TaskWall: 80 * time.Millisecond},
			outcome: JobOutcomeCached,
			cache:   true,
		},
		{
			name:      "coalesced",
			result:    &JobResult{Status: "success", Coalesced: true, TaskWall: 80 * time.Millisecond},
			outcome:   JobOutcomeCoalesced,
			coalesced: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var eventCount int
			var eventType string
			var data map[string]any
			var task *TaskResult
			scheduler := &Scheduler{onSessionEvent: func(record SessionRecord) {
				eventCount++
				eventType = record.Type
				data = record.Data
				task = record.Task
			}}
			job := &ScheduledJob{
				Project:   &workspace.Project{ID: "/app", Name: "app"},
				Extension: &extension.ExtensionDescription{Name: "@putnami/test"},
				JobDef:    &extension.JobDefinition{Name: "build~generate"},
				Step:      &extension.PipelineStep{Task: "build-generate"},
			}
			scheduler.emitSessionJobEnd(job, tt.result)
			scheduler.emitSessionJobEnd(job, tt.result)

			if eventCount != 1 || eventType != "job:end" {
				t.Fatalf("terminal events = %d type %q, want one job:end", eventCount, eventType)
			}
			if data["status"] != "success" || data["outcome"] != tt.outcome || data["cache"] != tt.cache || data["coalesced"] != tt.coalesced {
				t.Fatalf("reuse session event = %#v", data)
			}
			if data["taskKind"] != "build-generate" || data["taskWallMs"] != int64(80) {
				t.Fatalf("task economics session event = %#v", data)
			}
			if _, exists := data["spawnToFirstEventMs"]; exists {
				t.Fatalf("eventless reuse fabricated startup latency: %#v", data)
			}
			// The typed half of the record must classify the same reuse the
			// untyped half encodes as two booleans, or the profiler and the
			// recorded session would disagree about what this task was.
			if task == nil || task.Reuse.CacheHit() != tt.cache ||
				(task.Reuse == ReuseCoalesced) != tt.coalesced {
				t.Fatalf("typed task reuse = %+v, payload cache=%t coalesced=%t", task, tt.cache, tt.coalesced)
			}
		})
	}
}

func TestSchedulerResult_SuccessWhenAllSucceed(t *testing.T) {
	result := &SchedulerResult{
		Results: map[string]*JobResult{
			"a:build": {Status: "success"},
			"b:build": {Status: "success"},
		},
		Success:  true,
		Duration: 100 * time.Millisecond,
	}
	if !result.Success {
		t.Error("expected success")
	}
}

func TestSchedulerResult_FailureWhenAnyFails(t *testing.T) {
	result := &SchedulerResult{
		Results: map[string]*JobResult{
			"a:build": {Status: "success"},
			"b:build": {Status: "failed"},
		},
		Success: false,
	}
	if result.Success {
		t.Error("expected failure")
	}
}

func TestScheduler_SessionEventHandler(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	planned := []*ScheduledJob{
		makeScheduledJob("proj", "a", nil),
	}

	renderer := &mockRenderer{}

	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, renderer, nil)

	var eventCount atomic.Int32
	scheduler.setSessionEventHandler(func(SessionRecord) {
		eventCount.Add(1)
	})

	// The scheduler will try to run a subprocess that doesn't exist,
	// so the job will fail, but we can still verify session events fire
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)
	assertResourcePoolIdle(t, scheduler)

	// At minimum we should get a job:end event
	if eventCount.Load() == 0 {
		t.Error("expected at least one session event")
	}

	if len(result.Results) != 1 {
		t.Errorf("expected 1 result, got %d", len(result.Results))
	}

	// Renderer should have been called
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if !renderer.started {
		t.Error("renderer Start not called")
	}
	if !renderer.finished {
		t.Error("renderer Finish not called")
	}
}

func TestScheduler_EmitsParallelSummaryAndTuning(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "opening-session-identity", "sessionless-scheduler-events-omit-session-identity")
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	planned := []*ScheduledJob{
		makeScheduledJob("p1", "build", nil),
		makeScheduledJob("p2", "build", nil),
	}

	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel: 2,
		NoCache:     true,
	}, &mockRenderer{}, nil)

	var mu sync.Mutex
	eventTypes := map[string]int{}
	var parallelData map[string]any
	scheduler.setSessionEventHandler(func(record SessionRecord) {
		mu.Lock()
		defer mu.Unlock()
		eventTypes[record.Type]++
		if record.Type == "scheduler:parallel" {
			parallelData = record.Data
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	if result.Tuning == nil {
		t.Fatal("expected a tuning report on the result")
	}
	if result.Tuning.Parallel.Mode != parallelModeNumeric {
		t.Errorf("tuning mode = %q, want %q", result.Tuning.Parallel.Mode, parallelModeNumeric)
	}
	if result.Tuning.Parallel.Workers != 2 {
		t.Errorf("tuning workers = %d, want 2", result.Tuning.Parallel.Workers)
	}
	if result.Tuning.Parallel.PlannedJobs != 2 {
		t.Errorf("tuning plannedJobs = %d, want 2", result.Tuning.Parallel.PlannedJobs)
	}

	mu.Lock()
	defer mu.Unlock()
	if eventTypes["scheduler:parallel"] != 1 {
		t.Fatalf("scheduler:parallel events = %d, want exactly 1", eventTypes["scheduler:parallel"])
	}
	if _, present := parallelData["sessionId"]; present {
		t.Errorf("sessionless scheduler emitted sessionId: %v", parallelData)
	}
	if parallelData["mode"] != parallelModeNumeric {
		t.Errorf("parallel event mode = %v, want %q", parallelData["mode"], parallelModeNumeric)
	}
	if parallelData["workers"] != 2 {
		t.Errorf("parallel event workers = %v, want 2", parallelData["workers"])
	}
	if capacity, ok := parallelData["cpuCapacity"].(int); !ok || capacity < 1 {
		t.Errorf("parallel event cpuCapacity = %v, want positive stable capacity", parallelData["cpuCapacity"])
	}
	if _, ok := parallelData["memoryUsableMiB"]; !ok {
		t.Errorf("parallel event missing memoryUsableMiB: %v", parallelData)
	}
}

func TestScheduler_SkipDependencyFailed(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	jobA := makeScheduledJob("proj", "a", nil)
	jobB := makeScheduledJob("proj", "b", []string{"/proj:a"})

	renderer := &mockRenderer{}

	scheduler := newScheduler(ws, []*ScheduledJob{jobA, jobB}, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, renderer, nil)
	terminalEvents := map[string]int{}
	scheduler.setSessionEventHandler(func(record SessionRecord) {
		if record.Type == SessionRecordJobEnd {
			terminalEvents[record.JobKey]++
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	// A should fail (no command), B should be canceled (session aborted)
	rA := result.Results["/proj:a"]
	if rA == nil || rA.Status != "failed" {
		t.Errorf("expected A to fail, got %v", rA)
	}

	rB := result.Results["/proj:b"]
	if rB == nil || rB.Status != "canceled" {
		t.Errorf("expected B to be canceled, got %v", rB)
	}
	for _, job := range []*ScheduledJob{jobA, jobB} {
		if terminalEvents[job.Key()] != 1 {
			t.Errorf("%s terminal events = %d, want exactly one", job.Key(), terminalEvents[job.Key()])
		}
	}
}

func TestScheduler_StuckDAGFailsWithDependencyMessage(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	job := makeScheduledJob("proj", "build", []string{"/missing:build"})
	sibling := makeScheduledJob("zother", "test", []string{"/missing:test"})
	renderer := &mockRenderer{}

	scheduler := newScheduler(ws, []*ScheduledJob{job, sibling}, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, renderer, nil)
	terminalEvents := map[string]int{}
	scheduler.setSessionEventHandler(func(record SessionRecord) {
		if record.Type == SessionRecordJobEnd {
			terminalEvents[record.JobKey]++
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	if result.Success {
		t.Fatal("stuck DAG should fail the scheduler result")
	}
	got := result.Results["/proj:build"]
	if got == nil || got.Status != "failed" {
		t.Fatalf("expected failed stuck job, got %+v", got)
	}
	if got.Error == nil || !strings.Contains(got.Error.Message, "/missing:build") {
		t.Fatalf("expected dependency message to name missing blocker, got %+v", got.Error)
	}
	if got := result.Results[sibling.Key()]; got == nil || got.Status != "canceled" {
		t.Fatalf("stuck sibling result = %+v, want canceled", got)
	}
	for _, planned := range []*ScheduledJob{job, sibling} {
		if terminalEvents[planned.Key()] != 1 {
			t.Errorf("%s terminal events = %d, want exactly one", planned.Key(), terminalEvents[planned.Key()])
		}
	}
}

func TestScheduler_FailureDrainEmitsEveryTerminalEventOnce(t *testing.T) {
	wsRoot := t.TempDir()
	// Programs, not shell scripts: Windows cannot start a script as a command,
	// so both jobs failed at once there and either one could be the failure.
	failCommand := fixtureproc.Write(t, filepath.Join(wsRoot, "fail"), fixtureproc.Program{Sleep: 50 * time.Millisecond, Exit: 7})
	slowCommand := fixtureproc.Write(t, filepath.Join(wsRoot, "slow"), fixtureproc.Program{Sleep: 5 * time.Second})
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	ext := &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot}
	commandJob := func(name, command string) *ScheduledJob {
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			JobDef: &extension.JobDefinition{
				Name:          name,
				ExtensionName: ext.Name,
				Command:       command,
				TimeoutMs:     unboundedJobTimeoutMs,
			},
		}
	}
	failedJob := commandJob("build~fail", failCommand)
	drainedJob := commandJob("build~slow", slowCommand)
	ws := workspace.NewWorkspace(wsRoot, nil, []*workspace.Project{project})
	ws.Name = "test-ws"

	scheduler := newScheduler(
		ws,
		[]*ScheduledJob{failedJob, drainedJob},
		nil,
		SchedulerConfig{MaxParallel: 2, NoCache: true},
		&mockRenderer{},
		nil,
	)
	terminalEvents := map[string]int{}
	scheduler.setSessionEventHandler(func(record SessionRecord) {
		if record.Type == SessionRecordJobEnd {
			terminalEvents[record.JobKey]++
		}
	})

	result := scheduler.Run(t.Context())
	if got := result.Results[failedJob.Key()]; got == nil || got.Status != "failed" {
		t.Fatalf("failed result = %+v", got)
	}
	if got := result.Results[drainedJob.Key()]; got == nil || got.Status != "canceled" {
		t.Fatalf("drained result = %+v, want canceled", got)
	}
	for _, job := range []*ScheduledJob{failedJob, drainedJob} {
		if terminalEvents[job.Key()] != 1 {
			t.Errorf("%s terminal events = %d, want exactly one", job.Key(), terminalEvents[job.Key()])
		}
	}
}

func TestScheduler_RetryPreservesFirstStartupSampleAndMeasuresTaskWall(t *testing.T) {
	wsRoot := t.TempDir()
	attemptMarker := filepath.Join(wsRoot, "attempted")
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	ws := workspace.NewWorkspace(wsRoot, nil, []*workspace.Project{project})
	ws.Name = "test-ws"
	job := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			Name:          "test~retry",
			ExtensionName: "@putnami/test",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	// The first attempt fails after a startup delay; the retry succeeds.
	fixtureTask(t, job.JobDef, fixtureScript{
		{"sleep", "0.02"},
		{"fail-once", attemptMarker, `{"v":2,"type":"result","data":{"status":"failed","error":{"message":"retry"}}}`, "1"},
	})
	var terminalData map[string]any
	scheduler := newScheduler(
		ws,
		[]*ScheduledJob{job},
		nil,
		SchedulerConfig{MaxParallel: 1, Retry: 1, NoCache: true},
		&mockRenderer{},
		nil,
	)
	scheduler.setSessionEventHandler(func(record SessionRecord) {
		if record.Type == SessionRecordJobEnd {
			terminalData = record.Data
		}
	})

	run := scheduler.Run(context.Background())
	assertResourcePoolIdle(t, scheduler)
	result := run.Results[job.Key()]
	if result == nil || result.Status != "success" {
		t.Fatalf("retry result = %+v, want success", result)
	}
	if !result.FirstEventObserved || result.SpawnToFirstEvent < 20*time.Millisecond {
		t.Fatalf("first-attempt startup sample = %s observed=%v", result.SpawnToFirstEvent, result.FirstEventObserved)
	}
	if result.TaskWall <= result.Duration || result.TaskWall < result.SpawnToFirstEvent {
		t.Fatalf("task wall = %s, final attempt = %s, startup = %s", result.TaskWall, result.Duration, result.SpawnToFirstEvent)
	}
	taskWallMs, _ := terminalData["taskWallMs"].(int64)
	spawnMs, _ := terminalData["spawnToFirstEventMs"].(int64)
	if taskWallMs == 0 || taskWallMs < spawnMs {
		t.Fatalf("terminal task economics = %#v", terminalData)
	}
}

func TestScheduler_ContinueOnError(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	// Two independent jobs
	jobA := makeScheduledJob("p1", "build", nil)
	jobB := makeScheduledJob("p2", "build", nil)

	renderer := &mockRenderer{}

	scheduler := newScheduler(ws, []*ScheduledJob{jobA, jobB}, nil, SchedulerConfig{
		MaxParallel:     1,
		ContinueOnError: true,
		NoCache:         true,
	}, renderer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	// Both should run even if first fails
	if len(result.Results) != 2 {
		t.Errorf("expected 2 results with continue-on-error, got %d", len(result.Results))
	}
}

func TestScheduler_PublishSubtaskNonZeroExitFailsSession(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	project := &workspace.Project{ID: "/auth/server", Name: "auth/server", Path: "."}
	ws := workspace.NewWorkspace(wsRoot, nil, []*workspace.Project{project})
	ws.Name = "test-ws"

	failScript := filepath.Join(wsRoot, "fake-cloud-publish-config.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"log\",\"level\":\"error\",\"message\":\"Invalid or expired refresh token. Run `putnami cloud login`.\"}'\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"SKIP\"}}'\n" +
		"exit 7\n"
	if err := os.WriteFile(failScript, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake publish script: %v", err)
	}

	ext := &extension.ExtensionDescription{Name: "@putnami/cloud", Path: wsRoot}
	publishJob := func(name, command string, deps []string) *ScheduledJob {
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			JobDef: &extension.JobDefinition{
				Name:          name,
				ExtensionName: "@putnami/cloud",
				Command:       command,
				TimeoutMs:     unboundedJobTimeoutMs,
			},
			DependsOn: deps,
		}
	}
	cloudConfig := publishJob("publish~cloud-publish-config", failScript, nil)
	config := publishJob("publish~config", "/bin/true", []string{cloudConfig.Key()})

	renderer := &mockRenderer{}
	scheduler := newScheduler(ws, []*ScheduledJob{cloudConfig, config}, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, renderer, nil)

	result := scheduler.Run(t.Context())

	if result.Success {
		t.Fatal("expected failed publish session")
	}
	cloudResult := result.Results[cloudConfig.Key()]
	if cloudResult == nil || cloudResult.Status != "failed" || cloudResult.ExitCode != 7 {
		t.Fatalf("cloud publish result = %+v, want failed exit 7", cloudResult)
	}
	if len(cloudResult.Events) == 0 || !strings.Contains(cloudResult.Events[0].Message, "Invalid or expired refresh token") {
		t.Fatalf("auth failure event not retained: %+v", cloudResult.Events)
	}
	configResult := result.Results[config.Key()]
	if configResult == nil || configResult.Status != "canceled" {
		t.Fatalf("dependent publish result = %+v, want canceled", configResult)
	}

	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if !renderer.finished {
		t.Fatal("renderer Finish not called")
	}
	if renderer.results[cloudConfig.Key()].Status != "failed" {
		t.Fatalf("renderer saw cloud publish status %q, want failed", renderer.results[cloudConfig.Key()].Status)
	}
}

// TestScheduler_DiamondDAGParallel exercises the worker pool with
// MaxParallel > 1 over the classic diamond — A → {B, C} → D — and asserts
// that the DAG is still respected: A completes before B/C, B and C both
// complete before D. The jobs themselves fail (no real command available
// in the temp project dir), but the scheduler must still record results
// for all four in the correct partial order. Run under `-race` this also
// catches data races on the shared results map and worker mutex.
func TestScheduler_DiamondDAGParallel(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	jobA := makeScheduledJob("proj", "a", nil)
	jobB := makeScheduledJob("proj", "b", []string{"/proj:a"})
	jobC := makeScheduledJob("proj", "c", []string{"/proj:a"})
	jobD := makeScheduledJob("proj", "d", []string{"/proj:b", "/proj:c"})

	renderer := &mockRenderer{}
	scheduler := newScheduler(ws, []*ScheduledJob{jobA, jobB, jobC, jobD}, nil, SchedulerConfig{
		MaxParallel:     4,
		ContinueOnError: true,
		NoCache:         true,
	}, renderer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	if len(result.Results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(result.Results))
	}

	// Verify DAG ordering on the renderer's jobEnds (records completion order).
	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if len(renderer.jobEnds) != 4 {
		t.Fatalf("expected 4 jobEnds, got %d: %v", len(renderer.jobEnds), renderer.jobEnds)
	}
	posOf := map[string]int{}
	for i, k := range renderer.jobEnds {
		posOf[k] = i
	}
	if posOf["/proj:a"] >= posOf["/proj:b"] || posOf["/proj:a"] >= posOf["/proj:c"] {
		t.Errorf("A must complete before B and C; got order %v", renderer.jobEnds)
	}
	if posOf["/proj:b"] >= posOf["/proj:d"] || posOf["/proj:c"] >= posOf["/proj:d"] {
		t.Errorf("B and C must both complete before D; got order %v", renderer.jobEnds)
	}
}

// TestScheduler_SerializeAfterPreventsConcurrentWriters runs two
// functionally-independent jobs that share a write resource (modeled by a
// SerializeAfter edge) through a 2-worker pool. Each job grabs an exclusive
// mkdir lock; if the two ever overlapped the second would fail to acquire it
// and exit non-zero. Both succeeding proves the serialize edge held them apart,
// and the completion order confirms the writer ran before its successor.
func TestScheduler_SerializeAfterPreventsConcurrentWriters(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	ws := workspace.NewWorkspace(wsRoot, nil, []*workspace.Project{project})
	ws.Name = "test-ws"

	lockDir := filepath.Join(wsRoot, "writer.lock")
	script := filepath.Join(wsRoot, "exclusive-writer.sh")
	body := "#!/bin/sh\n" +
		"if mkdir '" + lockDir + "' 2>/dev/null; then\n" +
		"  sleep 0.3\n" +
		"  rmdir '" + lockDir + "'\n" +
		"  exit 0\n" +
		"fi\n" +
		"exit 17\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write writer script: %v", err)
	}

	ext := &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot}
	writer := func(name string) *ScheduledJob {
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			// No wall-clock budget: in the full gate this package shares 10
			// cores with race+coverage suites and 5s of spawn delay fired a
			// budget spuriously; 60s only moved that load point. The
			// proof is the mkdir lock and the completion order, not the wall.
			JobDef: &extension.JobDefinition{Name: name, ExtensionName: "@putnami/test", Command: script, TimeoutMs: unboundedJobTimeoutMs},
		}
	}
	a := writer("build~generate")
	b := writer("test~generate")
	b.SerializeAfter = []string{a.Key()} // shared .gen resource conflict

	renderer := &mockRenderer{}
	scheduler := newScheduler(ws, []*ScheduledJob{a, b}, nil, SchedulerConfig{
		MaxParallel: 2,
		NoCache:     true,
	}, renderer, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	for _, job := range []*ScheduledJob{a, b} {
		r := result.Results[job.Key()]
		if r == nil || r.Status != "success" {
			t.Fatalf("%s = %+v, want success (a non-zero exit means the writers overlapped)", job.Key(), r)
		}
	}

	renderer.mu.Lock()
	defer renderer.mu.Unlock()
	if len(renderer.jobEnds) != 2 || renderer.jobEnds[0] != a.Key() {
		t.Errorf("serialized writer must complete first; completion order = %v", renderer.jobEnds)
	}
}

// TestScheduler_ManyParallelJobsNoRace runs many independent jobs through
// the parallel worker pool. Its purpose is not to assert ordering (there
// is none across independent jobs) but to give the race detector real
// concurrent dispatch to inspect under `-race`.
func TestScheduler_ManyParallelJobsNoRace(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	const n = 24
	planned := make([]*ScheduledJob, 0, n)
	for i := range n {
		planned = append(planned, makeScheduledJob("p"+string(rune('a'+i)), "build", nil))
	}

	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel:     8,
		ContinueOnError: true,
		NoCache:         true,
	}, &mockRenderer{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	if len(result.Results) != n {
		t.Errorf("expected %d results, got %d", n, len(result.Results))
	}
}

func TestScheduler_ContextCancellation(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}

	// Create many jobs to ensure some don't run
	var jobs []*ScheduledJob
	for i := 0; i < 10; i++ {
		jobs = append(jobs, makeScheduledJob("p"+string(rune('0'+i)), "build", nil))
	}

	renderer := &mockRenderer{}

	scheduler := newScheduler(ws, jobs, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, renderer, nil)

	// Cancel quickly
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := scheduler.Run(ctx)
	assertResourcePoolIdle(t, scheduler)

	// All jobs should have a result (some may be skipped due to cancellation)
	if len(result.Results) != 10 {
		t.Errorf("expected 10 results, got %d", len(result.Results))
	}
}

// A run a signal cut short has no verdict to report: its remaining jobs were
// killed before they could pass or fail. Calling it a success would let CI
// green-light a killed build and would publish a successful-build marker for a
// plan that never ran.
func TestScheduler_AbortedRunIsNotSuccess(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}
	planned := []*ScheduledJob{makeScheduledJob("proj", "build", nil)}
	renderer := &mockRenderer{}
	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, renderer, nil)

	abort.Reset()
	abort.Record(syscall.SIGINT)
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := scheduler.Run(ctx)

	if result.Success {
		t.Error("Success = true for a run stopped by a signal, want false")
	}
	if !result.Outcome.Aborted {
		t.Error("Outcome.Aborted = false after the parent context was canceled")
	}
	if result.Outcome.AbortedBy != AbortUser {
		t.Errorf("Outcome.AbortedBy = %q, want %q", result.Outcome.AbortedBy, AbortUser)
	}
	// The renderer must be told, or its summary reports the killed work as an
	// ordinary skip and prints "Session completed".
	renderer.mu.Lock()
	got := renderer.outcome
	renderer.mu.Unlock()
	if !got.Aborted || got.AbortedBy != AbortUser {
		t.Errorf("renderer received outcome %+v, want aborted by %q", got, AbortUser)
	}
}

// Watch and serve cancel each iteration routinely — a file change restarts the
// server that way. With no signal on record that is a normal end, not an abort;
// classifying it otherwise reported every serve restart as a stopped session.
func TestScheduler_CancelWithoutSignalIsNotAnAbort(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}
	planned := []*ScheduledJob{makeScheduledJob("proj", "build", nil)}
	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, &mockRenderer{}, nil)

	abort.Reset()
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := scheduler.Run(ctx)

	if result.Outcome.Aborted {
		t.Error("Outcome.Aborted = true for a cancellation with no signal on record")
	}
	if result.Outcome.AbortedBy != "" {
		t.Errorf("Outcome.AbortedBy = %q with no signal recorded, want empty", result.Outcome.AbortedBy)
	}
	// Success is what the watch renderer reads to label the iteration. A serve
	// restart cancels jobs without failing any, so it must stay true or every
	// restart prints as "[watch] failed".
	if !result.Success {
		t.Error("Success = false for a restart-style cancellation; the iteration failed nothing")
	}
}

// A job failure cancels the scheduler's own child context. That must not be
// mistaken for an abort: the run reached a verdict, and it is "failed".
func TestScheduler_JobFailureIsNotAnAbort(t *testing.T) {
	ws := &workspace.Workspace{
		Name:  "test-ws",
		Root:  t.TempDir(),
		Graph: workspace.BuildGraph(nil),
	}
	failing := makeScheduledJob("proj", "build", nil)
	failing.JobDef.Command = "definitely-not-a-real-binary-xyz"
	planned := []*ScheduledJob{failing}
	scheduler := newScheduler(ws, planned, nil, SchedulerConfig{
		MaxParallel: 1,
		NoCache:     true,
	}, &mockRenderer{}, nil)

	abort.Reset()
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := scheduler.Run(ctx)

	if result.Success {
		t.Error("Success = true for a failing job, want false")
	}
	if result.Outcome.Aborted {
		t.Error("Outcome.Aborted = true for a plain job failure, want false")
	}
}

// finishHookRenderer wraps mockRenderer so a test can observe the results map
// AT THE MOMENT Finish is called, rather than after the run has settled. The
// map is shared and mutated in place, so a saved reference proves nothing about
// ordering; a hook does.
type finishHookRenderer struct {
	*mockRenderer
	onFinish func(results map[string]*JobResult, outcome SessionOutcome)
}

func (r *finishHookRenderer) Finish(results map[string]*JobResult, outcome SessionOutcome) {
	r.onFinish(results, outcome)
	r.mockRenderer.Finish(results, outcome)
}

func statusOfJob(results map[string]*JobResult, key string) string {
	if r := results[key]; r != nil {
		return r.Status
	}
	return "<missing>"
}

// TestFinalizeRun_TerminalPhaseOrder pins the order of the run's terminal
// phase, which is the highest-risk sequence an earlier split moved out of Run.
//
// Three steps must happen in this order, and each boundary is proved by what
// the NEXT step can see:
//
//  1. synthesizeCanceledResults gives every planned job that never reached a
//     terminal state its row, so
//  2. cfg.FinalizeResults — the caller's last chance to amend — observes a
//     COMPLETE map, and its amendments are what
//  3. renderer.Finish and the canonical reduction report.
//
// Reordering these looks harmless and is not: finalizing before synthesizing
// hides rows from the caller's amendment, and reducing before finalizing makes
// the run's verdict disagree with what the user was shown. Neither shows up in
// an end-to-end assertion on a run where every job completed.
func TestFinalizeRun_TerminalPhaseOrder(t *testing.T) {
	ran := makeScheduledJob("ran", "build", nil)
	neverRan := makeScheduledJob("never", "build", nil)

	var order []string
	var finalizeSaw, finishSaw string

	renderer := &finishHookRenderer{
		mockRenderer: &mockRenderer{},
		onFinish: func(results map[string]*JobResult, _ SessionOutcome) {
			order = append(order, "renderer.Finish")
			finishSaw = statusOfJob(results, neverRan.Key())
		},
	}

	s := &Scheduler{
		planned:  []*ScheduledJob{ran, neverRan},
		renderer: renderer,
		metrics:  newSchedulerMetrics(),
		cpuAlloc: newCPUAllocator(1, ""),
		cfg: SchedulerConfig{FinalizeResults: func(results map[string]*JobResult) {
			order = append(order, "cfg.FinalizeResults")
			finalizeSaw = statusOfJob(results, neverRan.Key())
			// Amend the synthesized row. Everything after this must report the
			// amendment, not the synthesized value.
			results[neverRan.Key()] = &JobResult{Status: "skipped"}
		}},
	}

	results := map[string]*JobResult{ran.Key(): {Status: "success"}}
	var mu sync.Mutex
	got := s.finalizeRun(context.Background(), ParallelDecision{Workers: 1}, time.Now(), results, &mu)

	if joined := strings.Join(order, ","); joined != "cfg.FinalizeResults,renderer.Finish" {
		t.Errorf("terminal phase order = %q, want cfg.FinalizeResults before renderer.Finish", joined)
	}
	if finalizeSaw != "canceled" {
		t.Errorf("cfg.FinalizeResults saw %q for the job that never ran, want \"canceled\" — "+
			"synthesizeCanceledResults must run BEFORE the caller's finalizer", finalizeSaw)
	}
	if finishSaw != "skipped" {
		t.Errorf("renderer.Finish saw %q, want \"skipped\" — the renderer must report the "+
			"finalizer's amendment", finishSaw)
	}
	if got.Session == nil {
		t.Fatal("finalizeRun returned no canonical session reduction")
	}
	if got.Session.Tasks != 2 {
		t.Errorf("session Tasks = %d, want 2 — every planned job must have a row by reduction time",
			got.Session.Tasks)
	}
	if got.Session.Status.Skipped != 1 || got.Session.Status.Canceled != 0 {
		t.Errorf("session status histogram = %+v, want the AMENDED row (1 skipped, 0 canceled) — "+
			"the reduction must run after cfg.FinalizeResults", got.Session.Status)
	}
	if got.Tuning == nil {
		t.Error("finalizeRun returned no tuning report")
	}
	if !renderer.mockRenderer.finished {
		t.Error("renderer.Finish was never reached")
	}
}

// TestCoordinatorStateTransitions_CompletionThenDispatch pins the two pure
// state transitions an earlier change lifted out of the coordinator's inner body:
// recordCompletedGroup (apply a finished dispatch group) and queueReady (admit
// what it unblocked). Neither owns a channel or a goroutine, which is why they
// could leave the loop at all — and this test exercises them without one.
func TestCoordinatorStateTransitions_CompletionThenDispatch(t *testing.T) {
	newFixture := func(continueOnError bool) (*Scheduler, *dagState, []*ScheduledJob) {
		producer := makeScheduledJob("producer", "build", nil)
		consumer := makeScheduledJob("consumer", "build", []string{producer.Key()})
		jobs := []*ScheduledJob{producer, consumer}
		s := &Scheduler{
			planned:  jobs,
			renderer: &mockRenderer{},
			metrics:  newSchedulerMetrics(),
			cfg:      SchedulerConfig{ContinueOnError: continueOnError},
		}
		return s, newDAGState(jobs), jobs
	}

	t.Run("a failed group is recorded and releases its dependents", func(t *testing.T) {
		s, state, jobs := newFixture(false)
		producer, consumer := jobs[0], jobs[1]
		results := map[string]*JobResult{}
		var mu sync.Mutex
		totalCompleted := 0

		failed := &JobResult{Status: "failed"}
		groupFailed, newlyReady := s.recordCompletedGroup(
			jobGroupDone{jobs: []jobDone{{job: producer, result: failed}}},
			state, results, &mu, &totalCompleted)

		if !groupFailed {
			t.Error("groupFailed = false for a group containing a failed task")
		}
		if totalCompleted != 1 {
			t.Errorf("totalCompleted = %d, want 1", totalCompleted)
		}
		if results[producer.Key()] != failed {
			t.Errorf("results[%s] = %v, want the completed group's own result", producer.Key(), results[producer.Key()])
		}
		if len(newlyReady) != 1 || newlyReady[0] != consumer {
			t.Errorf("newlyReady = %v, want exactly the dependent the completion unblocked", newlyReady)
		}
	})

	t.Run("a dependent of a failed producer is skipped, not queued", func(t *testing.T) {
		s, state, jobs := newFixture(false)
		producer, consumer := jobs[0], jobs[1]
		results := map[string]*JobResult{producer.Key(): {Status: "failed"}}
		var mu sync.Mutex
		totalCompleted := 1

		pending := s.queueReady(nil, []*ScheduledJob{consumer}, state, results, &mu, &totalCompleted)

		if len(pending) != 0 {
			t.Errorf("pending = %v, want empty — a dependent of a failed producer must not dispatch", pending)
		}
		if r := results[consumer.Key()]; r == nil || r.Status != "skipped" {
			t.Errorf("results[%s] = %v, want a skipped row", consumer.Key(), r)
		}
		// The skip counts as a completion, or the coordinator's
		// `totalCompleted < state.size()` loop can never terminate.
		if totalCompleted != 2 {
			t.Errorf("totalCompleted = %d, want 2 — a skip must count as a completion", totalCompleted)
		}
	})

	t.Run("--continue-on-error queues the dependent instead", func(t *testing.T) {
		s, state, jobs := newFixture(true)
		producer, consumer := jobs[0], jobs[1]
		results := map[string]*JobResult{producer.Key(): {Status: "failed"}}
		var mu sync.Mutex
		totalCompleted := 1

		pending := s.queueReady(nil, []*ScheduledJob{consumer}, state, results, &mu, &totalCompleted)

		if len(pending) != 1 || pending[0] != consumer {
			t.Errorf("pending = %v, want the dependent queued under --continue-on-error", pending)
		}
		if _, recorded := results[consumer.Key()]; recorded {
			t.Error("a queued dependent must not have a terminal row yet")
		}
		if totalCompleted != 1 {
			t.Errorf("totalCompleted = %d, want 1 — queueing is not completing", totalCompleted)
		}
	})
}
