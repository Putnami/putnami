package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The physical execution ledger.
//
// Three properties, and they are the ones every later saving claim is read
// against: one finished subprocess yields one measured record, a platform that
// cannot answer degrades to silence instead of a crash or a fabricated zero,
// and a batch's logical rows add up to the ONE wall the batch actually took
// rather than to n copies of it.

// runFinishedProcess spawns a real process that does a little work and returns
// its ProcessState with the wall measured around it. The ledger can only be
// read from a live ProcessState, so nothing below can be exercised on a
// hand-built value.
func runFinishedProcess(t *testing.T) (*exec.Cmd, time.Duration) {
	t.Helper()
	// burnCPUShell (test_helpers_test.go) owns the tick-granularity rationale.
	command, env := fixtureCommand(t, fixtureScript{{"burn"}})
	cmd := exec.Command(command)
	cmd.Env = append(os.Environ(), env...)
	started := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("run probe process: %v", err)
	}
	return cmd, time.Since(started)
}

func TestCaptureExecution_ReadsRusageFromAFinishedProcess(t *testing.T) {
	t.Parallel()
	cmd, wall := runFinishedProcess(t)

	execution := captureExecution(cmd.ProcessState, wall, 4)
	if execution == nil {
		t.Fatal("captureExecution returned nil for a finished process")
	}
	if execution.ID == "" {
		t.Error("execution has no id")
	}
	if execution.Wall != wall {
		t.Errorf("Wall = %v, want the measured %v", execution.Wall, wall)
	}
	if execution.CPUTime() <= 0 {
		t.Errorf("CPUTime = %v, want > 0 (user %v + system %v)",
			execution.CPUTime(), execution.UserCPU, execution.SystemCPU)
	}
	if execution.UserCPU < 0 || execution.SystemCPU < 0 {
		t.Errorf("negative CPU split: user %v, system %v", execution.UserCPU, execution.SystemCPU)
	}
	if execution.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want the granted 4", execution.Concurrency)
	}
	if execution.IOInBlocks < 0 || execution.IOOutBlocks < 0 {
		t.Errorf("negative IO counters: in %d, out %d", execution.IOInBlocks, execution.IOOutBlocks)
	}

	// Peak RSS is normalized to BYTES on every platform that reports it, which
	// is what makes a darwin measurement comparable with a CI one. A process that
	// ran at all occupies far more than a kilobyte, so a linux reading left in
	// kilobytes would land below this bound.
	switch runtime.GOOS {
	case "darwin", "linux":
		const oneMebibyte = 1 << 20
		if execution.MaxRSSBytes < oneMebibyte {
			t.Errorf("MaxRSSBytes = %d, want at least %d — the unit is bytes, not kilobytes",
				execution.MaxRSSBytes, oneMebibyte)
		}
	default:
		if execution.MaxRSSBytes != 0 {
			t.Errorf("MaxRSSBytes = %d on %s, want 0 — an unread counter is absent, not measured",
				execution.MaxRSSBytes, runtime.GOOS)
		}
	}
}

// TestCaptureExecution_DegradesWithoutAProcessState pins the graceful half of
// the contract: no process, no execution, and no panic.
func TestCaptureExecution_DegradesWithoutAProcessState(t *testing.T) {
	if execution := captureExecution(nil, time.Second, 2); execution != nil {
		t.Errorf("captureExecution(nil) = %+v, want nil", execution)
	}
	if (*Execution)(nil).CPUTime() != 0 {
		t.Error("a nil execution reported CPU time")
	}
}

func TestNextExecutionID_IsUnique(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		id := nextExecutionID()
		if id == "" {
			t.Fatal("empty execution id")
		}
		if seen[id] {
			t.Fatalf("duplicate execution id %q", id)
		}
		seen[id] = true
	}
}

// TestExecutionShare_SumsToTheMeasuredTotal is the arithmetic the whole
// arrangement rests on: shares reconstruct the measurement exactly in
// NANOSECONDS, remainder included, for every fan-out. The published records
// truncate to milliseconds independently, so the wire sum is short by up to
// n-1 ms — which is why the docs tell a consumer to read executions[] for cost
// rather than to reconcile task durations against a wall.
func TestExecutionShare_SumsToTheMeasuredTotal(t *testing.T) {
	for _, total := range []time.Duration{0, 1, 7, time.Millisecond, 3_000_000_007} {
		for n := 1; n <= 13; n++ {
			var sum time.Duration
			for i := range n {
				share := executionShare(total, n, i)
				if share < 0 {
					t.Fatalf("negative share for total %v, n %d, index %d", total, n, i)
				}
				sum += share
			}
			if sum != total {
				t.Errorf("shares of %v across %d members sum to %v", total, n, sum)
			}
		}
	}
	if got := executionShare(42*time.Second, 1, 0); got != 42*time.Second {
		t.Errorf("a solo execution's share = %v, want the whole %v", got, 42*time.Second)
	}
}

// batchWorkFixture builds n minimal work items — enough for attribution, which
// only needs each item's key.
func batchWorkFixture(n int) []taskWork {
	work := make([]taskWork, 0, n)
	for i := range n {
		work = append(work, taskWork{job: &ScheduledJob{
			Project:   &workspace.Project{ID: "/p" + string(rune('a'+i)), Name: "p"},
			JobDef:    &extension.JobDefinition{Name: "lint"},
			Extension: &extension.ExtensionDescription{Name: "@putnami/typescript"},
		}})
	}
	return work
}

// TestAttributeSharedExecution_MembersShareOneExecutionAndSplitItsCost is the
// regression this slice exists for: before it, each member recorded the
// leader's FULL wall and an equal slice of CPU that reconciled with nothing, so
// summing a 12-project batch reported twelve times the cost the machine paid.
func TestAttributeSharedExecution_MembersShareOneExecutionAndSplitItsCost(t *testing.T) {
	const members = 5
	work := batchWorkFixture(members)
	aggregate := &JobResult{
		Status:    "success",
		Duration:  3_000_000_007,
		CPUTime:   11_000_000_003,
		Execution: &Execution{ID: "exec-1", Wall: 2_900_000_000, UserCPU: 9 * time.Second, SystemCPU: 2 * time.Second},
	}
	results := make(map[string]*JobResult, members)
	for i := range work {
		results[work[i].job.Key()] = &JobResult{
			Status:   "success",
			Duration: aggregate.Duration,
			CPUTime:  aggregate.CPUTime,
		}
	}

	attributeSharedExecution(work, results, aggregate)

	var wallSum, cpuSum time.Duration
	for i := range work {
		result := results[work[i].job.Key()]
		if result.Execution != aggregate.Execution {
			t.Fatalf("member %d points at a different execution than the leader", i)
		}
		if result.Duration == aggregate.Duration {
			t.Errorf("member %d still claims the leader's full wall %v", i, aggregate.Duration)
		}
		wallSum += result.Duration
		cpuSum += result.CPUTime
	}
	if wallSum != aggregate.Duration {
		t.Errorf("member walls sum to %v, want the one measured %v", wallSum, aggregate.Duration)
	}
	if cpuSum != aggregate.CPUTime {
		t.Errorf("member CPU sums to %v, want the one measured %v", cpuSum, aggregate.CPUTime)
	}
}

// TestExecutionLedger_RecordsEachSubprocessOnce pins the accumulator itself:
// a batch's n members carry ONE execution and contribute one entry, a repeat
// is a no-op, and a result that spawned nothing adds nothing.
func TestExecutionLedger_RecordsEachSubprocessOnce(t *testing.T) {
	var ledger executionLedger
	shared := &Execution{ID: "exec-1", Wall: time.Second}

	ledger.record(nil)
	ledger.record(&JobResult{Status: "success"})
	for range 5 {
		ledger.record(&JobResult{Status: "success", Execution: shared})
	}
	ledger.record(&JobResult{Status: "success", Execution: &Execution{ID: "exec-2"}})
	ledger.record(&JobResult{Status: "failed", Execution: &Execution{ID: ""}})

	snapshot := ledger.snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("ledger holds %d executions, want 2: %+v", len(snapshot), snapshot)
	}
	if snapshot[0].ID != "exec-1" || snapshot[1].ID != "exec-2" {
		t.Errorf("ledger order = %q, %q; want completion order", snapshot[0].ID, snapshot[1].ID)
	}
	// The snapshot is a copy: a consumer cannot reach back into the ledger.
	snapshot[0].Wall = 0
	if again := ledger.snapshot(); again[0].Wall != time.Second {
		t.Error("snapshot aliases the ledger's storage")
	}
}

// splitFailureBatchFixture makes the SHARED invocation answer with an aggregate
// that names no per-project results, so the split fails and every member fails
// closed. A solo invocation of the same script succeeds, so with a retry budget
// all n members re-execute alone — the case where nothing that survives the run
// still points at the batch leader.
func splitFailureBatchFixture(t *testing.T, logPath string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	ws, planned := makeBatchSchedulerFixture(t, "")
	command := filepath.Join(ws.Root, "split-failure.sh")
	writeExecutable(t, command, batchShellHeader(logPath)+
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n")
	for _, job := range planned {
		job.JobDef.Command = command
	}
	return ws, planned
}

// unreferencedExecutions returns the ledger entries no surviving result names.
func unreferencedExecutions(result *SchedulerResult) []Execution {
	referenced := make(map[string]bool, len(result.Results))
	for _, job := range result.Results {
		if job != nil && job.Execution != nil {
			referenced[job.Execution.ID] = true
		}
	}
	var orphans []Execution
	for _, execution := range result.Session.Executions {
		if !referenced[execution.ID] {
			orphans = append(orphans, execution)
		}
	}
	return orphans
}

// TestLedger_KeepsASupersededBatchAttempt runs the REAL scheduler: a batch
// where one member fails and retries solo. Both subprocesses are in the run's
// ledger even though the retried member's result now names only the second.
func TestLedger_KeepsASupersededBatchAttempt(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := retryBatchFixture(t, logPath)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := runBatchScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2, Retry: 1}, cache)
	if !result.Success {
		t.Fatalf("retried batch failed: %+v", result.Results)
	}

	log := invocationLog(t, logPath)
	if len(log) != 2 {
		t.Fatalf("invocations = %v, want a batch then one solo retry", log)
	}
	// One ledger entry per SUBPROCESS: the shared attempt and the solo retry.
	if got := len(result.Session.Executions); got != len(log) {
		t.Fatalf("ledger holds %d executions for %d subprocesses: %+v",
			got, len(log), result.Session.Executions)
	}
	retried := result.Results[planned[0].Key()]
	if retried.Execution == nil || retried.Execution.ID != result.Session.Executions[1].ID {
		t.Errorf("retried member names %+v, want the solo attempt", retried.Execution)
	}
	for _, execution := range result.Session.Executions {
		if execution.CPUTime() <= 0 {
			t.Errorf("execution %s recorded no CPU: %+v", execution.ID, execution)
		}
	}
}

// TestLedger_KeepsABatchLeaderNoResultReferences is the harder half: when the
// split cannot be attributed, every member re-executes solo, so NOTHING that
// survives the run points at the batch leader. A ledger derived from the task
// results would lose that whole subprocess; the accumulated one keeps it.
func TestLedger_KeepsABatchLeaderNoResultReferences(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := splitFailureBatchFixture(t, logPath)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := runBatchScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2, Retry: 1}, cache)
	if !result.Success {
		t.Fatalf("every member should have recovered solo: %+v", result.Results)
	}

	log := invocationLog(t, logPath)
	if len(log) != 3 || !strings.HasPrefix(log[0], "batch:") {
		t.Fatalf("invocations = %v, want one batch then a solo retry per member", log)
	}
	if got := len(result.Session.Executions); got != 3 {
		t.Fatalf("ledger holds %d executions for 3 subprocesses: %+v", got, result.Session.Executions)
	}

	orphans := unreferencedExecutions(result)
	if len(orphans) != 1 {
		t.Fatalf("%d executions are unreferenced, want exactly the batch leader: %+v", len(orphans), orphans)
	}
	if orphans[0].ID != result.Session.Executions[0].ID {
		t.Errorf("unreferenced execution = %s, want the first (the batch leader)", orphans[0].ID)
	}
	if orphans[0].CPUTime() <= 0 {
		t.Errorf("the dropped-by-derivation leader recorded no cost: %+v", orphans[0])
	}
}

// TestAttributeSharedExecution_LeavesASingletonAlone keeps the n == 1 dispatch
// byte-identical: there is nothing to share and nothing to split.
func TestAttributeSharedExecution_LeavesASingletonAlone(t *testing.T) {
	work := batchWorkFixture(1)
	execution := &Execution{ID: "exec-1", Wall: time.Second}
	aggregate := &JobResult{Status: "success", Duration: time.Second, CPUTime: 2 * time.Second, Execution: execution}
	results := map[string]*JobResult{work[0].job.Key(): {
		Status:    "success",
		Duration:  time.Second,
		CPUTime:   2 * time.Second,
		Execution: execution,
	}}

	attributeSharedExecution(work, results, aggregate)

	result := results[work[0].job.Key()]
	if result.Duration != time.Second || result.CPUTime != 2*time.Second {
		t.Errorf("singleton timings changed: %v / %v", result.Duration, result.CPUTime)
	}
	if result.Execution != execution {
		t.Error("singleton lost its execution")
	}
}
