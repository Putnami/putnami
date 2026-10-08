package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The runtime half of a plan-level shared node, exercised through the REAL
// scheduler and REAL subprocesses: the invocation log is the only honest answer
// to "did this work run twice", which is the whole claim being tested.

// sharedRuntimeFixture plans two nodes over one project — `build~describe` and
// `test~describe` — stamped as one shared execution, ordered the way the real
// plan orders them (the test node after the build node, as the `clients`
// write-serialization edge does). Each invocation of the probe script appends a
// line to logPath and emits a diagnostic plus a result event, so a test can see
// both how many subprocesses ran and what each logical row carried.
func sharedRuntimeFixture(t *testing.T, logPath, exitCode string) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app", "main.go"), "package main\n")

	command := filepath.Join(root, "describe.sh")
	writeExecutable(t, command,
		"#!/bin/sh\n"+
			// The leader's cost assertion below reads getrusage; burnCPUShell
			// (test_helpers_test.go) owns the tick-granularity rationale.
			burnCPUShell()+
			"printf 'run:%s\\n' \"$PUTNAMI_JOB_NAME\" >> "+shellQuote(logPath)+"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"diagnostic\",\"data\":{\"severity\":\"warning\",\"message\":\"probe\"}}'\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
			"exit "+exitCode+"\n")

	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	ws.Name = "shared-ws"

	ext := &extension.ExtensionDescription{
		Name:    "@test/probe",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ext"),
		Tasks: map[string]extension.TaskDefinition{
			"probe-describe": {Declares: &extension.TaskDeclaration{}},
		},
	}

	planned := make([]*ScheduledJob, 0, 2)
	for _, name := range []string{"build~describe", "test~describe"} {
		command, step := jobCommandAndStep(name)
		planned = append(planned, &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: step, Task: "probe-describe"},
			JobDef: &extension.JobDefinition{
				ExtensionName: "@test/probe",
				Name:          name,
				CommandName:   command,
				StepID:        step,
				Command:       filepath.Join(root, "describe.sh"),
				Cwd:           "{projectRoot}",
				Cache:         true,
				FilePatterns:  []string{"main.go"},
				TaskCachePolicy: &extension.TaskCachePolicy{
					Deterministic: true,
					NoOutput:      true,
				},
			},
			// The shared node the planner would have stamped. Sharing is decided
			// by plan_shared.go and asserted there; this fixture states the
			// grouping directly so the runtime behavior is the only variable.
			SharedExecutionID: "shared-1",
		})
	}
	// The `clients` write-serialization edge the real plan carries: the test
	// node runs after the build node, so the leader is deterministic here.
	planned[1].SerializeAfter = []string{planned[0].Key()}
	return ws, planned
}

func sharedRuntimeCache(t *testing.T) *store.CacheManager {
	t.Helper()
	return store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))
}

// runSharedScheduler is the ONE scheduler construction these tests share, for
// the same reason runBatchScheduler is: every property here — leader election,
// the physical ledger, the cache boundary a follower still crosses — is a
// property of the real coordinator and of nothing smaller.
func runSharedScheduler(
	ctx context.Context,
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	cfg SchedulerConfig,
	cache *store.CacheManager,
) *SchedulerResult {
	return newScheduler(ws, planned, nil, cfg, &mockRenderer{}, cache).Run(ctx)
}

func requireShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable")
	}
}

func TestGoEmbedMixedBuildTestKeepsEditedOwnedOutput(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "go-describe-cached-old-bundle-cannot-restore-after-embedded-asset-edit")
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := sharedRuntimeFixture(t, logPath, "0")
	project := filepath.Join(ws.Root, "app")
	writeTestFile(t, filepath.Join(project, "embed.go"), "package main\nimport _ \"embed\"\n//go:embed migration.sql\nvar migration string\n")
	asset := filepath.Join(project, "migration.sql")
	writeTestFile(t, asset, "SELECT 'A';\n")
	writeExecutable(t, filepath.Join(ws.Root, "describe.sh"),
		"#!/bin/sh\nmkdir -p .gen\ncat migration.sql > .gen/bundle.sql\n"+
			"printf 'run:%s\\n' \"$PUTNAMI_JOB_NAME\" >> "+shellQuote(logPath)+"\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"success\"}}'\n")
	for _, job := range planned {
		job.JobDef.FilePatterns = []string{"**/*.go", "go-embed:build"}
		job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true}
		job.Extension.Tasks["probe-describe"] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
			"bundle": dirDeclaration(extension.OutputRootProject, ".gen", false),
		}}}
	}
	storeRoot := filepath.Join(t.TempDir(), "store")
	cache := func() *store.CacheManager {
		// The on-disk entries persist across invocations; only the per-run
		// file-hash memo is new after the asset changes.
		return store.NewCacheManager(store.NewLocalStore(storeRoot))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seed := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, cache())
	if !seed.Success {
		t.Fatalf("seed A failed: %+v", seed.Results)
	}
	for _, job := range planned {
		if result := seed.Results[job.Key()]; result == nil || result.Status != "success" || result.ReuseKind() == ReuseLocalCache {
			t.Fatalf("%s did not seed its A entry: %+v", job.Key(), result)
		}
	}
	output := filepath.Join(project, ".gen", "bundle.sql")
	if got := readFileAt(t, output); got != "SELECT 'A';\n" {
		t.Fatalf("seed output = %q", got)
	}
	writeTestFile(t, asset, "SELECT 'B';\n")
	mixed := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, cache())
	if !mixed.Success {
		t.Fatalf("mixed B run failed: %+v", mixed.Results)
	}
	for _, job := range planned {
		if result := mixed.Results[job.Key()]; result == nil || result.Status != "success" || result.ReuseKind() == ReuseLocalCache {
			t.Fatalf("%s reused the old A bundle: %+v", job.Key(), result)
		}
	}
	if got := readFileAt(t, output); got != "SELECT 'B';\n" {
		t.Fatalf("mixed B output = %q", got)
	}
	warm := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, cache())
	if !warm.Success {
		t.Fatalf("warm B run failed: %+v", warm.Results)
	}
	for _, job := range planned {
		if got := warm.Results[job.Key()].ReuseKind(); got != ReuseLocalCache {
			t.Fatalf("%s warm B reuse = %q, want owned-output restoration", job.Key(), got)
		}
	}
	if got := readFileAt(t, output); got != "SELECT 'B';\n" {
		t.Fatalf("restored B output = %q", got)
	}
	if got := invocationLog(t, logPath); len(got) != 2 {
		t.Fatalf("invocations = %v, want seed A and edited B only", got)
	}
}

// TestSharedExecution_RunsOncePhysically is acceptance criterion 1: two
// commands' content-identical nodes spawn ONE subprocess, and the second node
// still produces its own successful terminal row.
func TestSharedExecution_RunsOncePhysically(t *testing.T) {
	t.Parallel()
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := sharedRuntimeFixture(t, logPath, "0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	if !result.Success {
		t.Fatalf("shared run failed: %+v", result.Results)
	}
	if log := invocationLog(t, logPath); len(log) != 1 {
		t.Fatalf("invocations = %v, want exactly one physical describe", log)
	}

	leader := result.Results[planned[0].Key()]
	follower := result.Results[planned[1].Key()]
	if leader.Status != "success" || follower.Status != "success" {
		t.Fatalf("statuses = %q/%q, want both success", leader.Status, follower.Status)
	}
	if leader.ReuseKind() != ReuseNone {
		t.Errorf("leader reuse = %q, want none — it did the work", leader.ReuseKind())
	}
	if follower.ReuseKind() != ReuseCoalesced {
		t.Errorf("follower reuse = %q, want coalesced", follower.ReuseKind())
	}
	// Each command's node reports its own diagnostics: from the test command's
	// perspective the task said exactly what it would have said alone.
	if len(follower.Events) != len(leader.Events) {
		t.Errorf("follower carries %d events, leader %d", len(follower.Events), len(leader.Events))
	}
}

// TestSharedExecution_LedgerRecordsOneSubprocess proves the model: one
// physical execution, one ledger row, both logical rows
// naming it, and the follower claiming NO CPU of its own.
func TestSharedExecution_LedgerRecordsOneSubprocess(t *testing.T) {
	t.Parallel()
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := sharedRuntimeFixture(t, logPath, "0")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	if got := len(result.Session.Executions); got != 1 {
		t.Fatalf("ledger holds %d executions, want 1: %+v", got, result.Session.Executions)
	}
	execution := result.Session.Executions[0]
	leader := result.Results[planned[0].Key()]
	follower := result.Results[planned[1].Key()]
	for name, row := range map[string]*JobResult{"leader": leader, "follower": follower} {
		if row.Execution == nil || row.Execution.ID != execution.ID {
			t.Errorf("%s names execution %+v, want %s", name, row.Execution, execution.ID)
		}
	}
	if follower.CPUTime != 0 {
		t.Errorf("follower claims %v of CPU; the shared subprocess's CPU is counted once", follower.CPUTime)
	}
	if leader.CPUTime <= 0 {
		t.Errorf("leader recorded no CPU: %v", leader.CPUTime)
	}
}

// TestSharedExecution_FollowerAdoptsFailure is acceptance criterion 2: a
// failure reaches BOTH commands. Two nodes over identical inputs fail
// identically, so adopting the verdict is what keeps each command's dependents
// behaving exactly as they would have if the node had run for that command
// alone — without paying the failing minute twice.
func TestSharedExecution_FollowerAdoptsFailure(t *testing.T) {
	t.Parallel()
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := sharedRuntimeFixture(t, logPath, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2, NoCache: true, ContinueOnError: true}, nil)

	if result.Success {
		t.Fatal("a failing shared node reported a successful run")
	}
	if log := invocationLog(t, logPath); len(log) != 1 {
		t.Fatalf("invocations = %v, want one physical attempt even on failure", log)
	}
	for name, key := range map[string]string{"leader": planned[0].Key(), "follower": planned[1].Key()} {
		if got := result.Results[key].Status; got != "failed" {
			t.Errorf("%s status = %q, want failed", name, got)
		}
	}
}

// TestSharedExecution_FallsBackWhenNoPeerExecutes pins the property that makes
// the mechanism safe under partial plans: nothing designates a leader in
// advance, and a member that spawns NOTHING must not become one.
//
// The scenario is the ordinary mixed-cache one. `build~describe` is served from
// its own entry, so it takes no slot — a follower parked on a cache hit would
// be waiting for a subprocess that is never going to exist. `test~describe`
// therefore leads and executes exactly as it would have alone.
func TestSharedExecution_FallsBackWhenNoPeerExecutes(t *testing.T) {
	t.Parallel()
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := sharedRuntimeFixture(t, logPath, "0")
	cache := sharedRuntimeCache(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed only the build node's entry.
	seed := runSharedScheduler(ctx, ws, planned[:1], SchedulerConfig{MaxParallel: 1}, cache)
	if !seed.Success {
		t.Fatalf("seeding run failed: %+v", seed.Results)
	}

	result := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, cache)
	if !result.Success {
		t.Fatalf("mixed run failed: %+v", result.Results)
	}
	if got := result.Results[planned[0].Key()].ReuseKind(); got != ReuseLocalCache {
		t.Fatalf("build~describe reuse = %q, want a cache hit", got)
	}
	follower := result.Results[planned[1].Key()]
	if follower.Status != "success" {
		t.Fatalf("test~describe status = %q, want success: %+v", follower.Status, follower.Error)
	}
	if follower.ReuseKind() != ReuseNone {
		t.Errorf("test~describe reuse = %q, want none — it had to do the work itself", follower.ReuseKind())
	}
	if log := invocationLog(t, logPath); len(log) != 2 {
		t.Fatalf("invocations = %v, want the seeding run's and the fallback's", log)
	}
}

// TestSharedExecution_KeepsWarmRunsIdentical is acceptance criterion 3 with
// teeth: the follower publishes its OWN entry at its OWN key, so the second run
// serves both nodes from cache exactly as it did before this slice existed.
// Sharing therefore only ever removes a COLD duplicate; it never moves, merges
// or starves a cached result.
func TestSharedExecution_KeepsWarmRunsIdentical(t *testing.T) {
	t.Parallel()
	requireShell(t)
	logPath := filepath.Join(t.TempDir(), "invocations")
	ws, planned := sharedRuntimeFixture(t, logPath, "0")
	cache := sharedRuntimeCache(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cold := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, cache)
	if !cold.Success {
		t.Fatalf("cold run failed: %+v", cold.Results)
	}
	if log := invocationLog(t, logPath); len(log) != 1 {
		t.Fatalf("cold invocations = %v, want one", log)
	}

	warm := runSharedScheduler(ctx, ws, planned, SchedulerConfig{MaxParallel: 2}, cache)
	if !warm.Success {
		t.Fatalf("warm run failed: %+v", warm.Results)
	}
	if log := invocationLog(t, logPath); len(log) != 1 {
		t.Fatalf("warm run spawned again: invocations = %v", log)
	}
	for name, key := range map[string]string{"leader": planned[0].Key(), "follower": planned[1].Key()} {
		if got := warm.Results[key].ReuseKind(); got != ReuseLocalCache {
			t.Errorf("%s warm reuse = %q, want an ordinary cache hit", name, got)
		}
	}
	if got := len(warm.Session.Executions); got != 0 {
		t.Errorf("warm run recorded %d executions, want none", got)
	}
}

// TestSharedExecutionMembersNeverBatch keeps a follower and its leader out of
// one dispatch group. A batch opens every member's task before running any of
// them, so a follower parked on a leader in the same group would wait for a
// subprocess that cannot start until the follower's own open returns.
func TestSharedExecutionMembersNeverBatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a", "src.txt"), "a")
	project := &workspace.Project{ID: "/a", Name: "a", Path: "a"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	ws.Name = "batch-ws"

	job := &ScheduledJob{
		Project: project,
		Extension: &extension.ExtensionDescription{
			Name: "@test/probe", Version: "1.0.0", Path: filepath.Join(root, "ext"),
			Tasks: map[string]extension.TaskDefinition{"lint-all": {Declares: &extension.TaskDeclaration{}}},
		},
		Step: &extension.PipelineStep{ID: "check", Task: "lint-all"},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@test/probe",
			Name:          "lint~check",
			Command:       "/bin/true",
			Cache:         true,
			FilePatterns:  []string{"src.txt"},
			Batchable:     &extension.TaskBatchPolicy{Tool: "probe"},
		},
	}
	scheduler := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{}, &mockRenderer{}, nil)
	if scheduler.readyBatchKey(job) == "" {
		t.Fatal("fixture is not batchable, so the exclusion below proves nothing")
	}
	job.SharedExecutionID = "shared-1"
	if key := scheduler.readyBatchKey(job); key != "" {
		t.Errorf("a shared-node member resolved batch key %q, want none", key)
	}
}
