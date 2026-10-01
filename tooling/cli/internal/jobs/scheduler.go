package jobs

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// SchedulerConfig holds configuration for the DAG scheduler.
type SchedulerConfig struct {
	MaxParallel     int
	MaxParallelMode string
	// ResourceBudgets is what the run declares EXISTS of each named external
	// resource (--resource <name>=<units>): the second admission dimension
	// beside the worker count, so a task kind's scarce dependency caps only the
	// tasks that claim it. A name absent here is unlimited, which is why
	// a run that declares none admits exactly as it did before. Names are opaque
	// and never a cache key. See resource_budgets.go.
	ResourceBudgets map[string]int
	// CPUBudgetPolicy selects how a task's exported CPU ceiling is derived:
	// "critical-path" (default) sizes it by the task's share of the plan's
	// makespan, floored by measured occupancy; "measured" uses occupancy alone.
	// Empty means the default. It is an execution hint and never a cache key.
	CPUBudgetPolicy string
	ContinueOnError bool
	Retry           int
	NoCache         bool
	// NoCacheProjects is the RESOLVED id set of --no-cache-projects: the
	// projects whose own tasks refuse cache reuse while the rest of the run
	// keeps it. Empty means the run-wide policy alone applies, and
	// NoCache subsumes it.
	//
	// Like NoCache it is execution policy: the scheduler resolves it over the
	// plan into a CacheBypass and consults that when deciding whether to look a
	// task up. Neither the ids nor the closure reaches a cache key, a run
	// marker, or the task params a job's identity is computed from.
	NoCacheProjects map[string]bool
	// RetryFailed forces re-execution of tasks whose last failure is recorded
	// in the local failure cache (--retry-failed). It is execution policy, not
	// content identity: it never reaches a cache key, a run marker, or the task
	// params a job's identity is computed from, exactly like --max-parallel.
	// --no-cache already covers the blunt case, because it disables the whole
	// cache-enabled path the negative entry hangs off.
	RetryFailed bool
	Debug       bool
	// CacheVerification enables observation-only cache checks. Verification
	// forces singleton execution at the engine boundary and records each task's
	// project-tree writes; it never changes cache identity or entry formats.
	CacheVerification bool
	VersionInfo       RunVersions
	FinalizeResults   func(results map[string]*JobResult)
	// SessionID is the id of the session this run records, or "" for a run that
	// records none (every lifecycle run). The scheduler exports it into each
	// task subprocess as protocolcli.ParentSessionEnv so a CLI a task spawns can
	// record which run spawned it, and into the opening scheduler:parallel event
	// so a reader can locate the engine's recorded plan before tasks start.
	//
	// It is EXECUTION-ONLY, like ResourceBudgets and CPUBudgetPolicy above, and
	// for a sharper reason: a session id is unique per run, so if anything
	// derived from it reached a cache key, a run marker or the task params a
	// job's identity is computed from, every cacheable task in the workspace
	// would miss on every run forever. It reaches only cmd.Env through
	// jobProcessEnv() and the session audit event.
	SessionID string
	// Preparation is the dependency-preparation stage's phase attribution,
	// accumulated by the composer BEFORE this scheduler existed (runtime
	// synchronization runs in engine phase 1c/1d, ahead of
	// planning, so it can never be a scheduler measurement). The scheduler only
	// carries it onto the session so the recorded document holds the whole run;
	// it neither reads nor adds to it. Nil for a caller that recorded none.
	Preparation *PreparationReport
}

// SchedulerResult holds the outcome of a full scheduler run.
type SchedulerResult struct {
	Results map[string]*JobResult
	Success bool
	// Outcome reports how the session itself ended — notably whether a signal
	// cut it short. Success is never true for an aborted run.
	Outcome  SessionOutcome
	Duration time.Duration
	// Tuning summarizes the auto-tuning decision and DAG wait behavior for the
	// run. Always set by Run; nil only on a zero-value SchedulerResult.
	Tuning *TuningReport
	// Cache summarizes remote build cache activity (hits, bytes, time saved vs
	// spent). Nil when no remote cache was active for the run.
	Cache *CacheStatsSnapshot
	// Session is the canonical reduction of this run: task and session counts,
	// diagnostics, failures, artifacts, timings, aborted state, and the cache
	// summary above. Success above is Session.Success(), and a later change
	// pointed the session stats writer and the exit-code derivation here too, so
	// no consumer tallies Results itself any more.
	Session *SessionResult
}

// Scheduler executes a DAG of scheduled jobs with parallel goroutines.
type Scheduler struct {
	ws            *workspace.Workspace
	planned       []*ScheduledJob
	commandParams map[string]any
	// taskParams is the parameter bag serialized for extension execution. It is
	// commandParams plus execution-only framework globals such as max-parallel
	// and no-cache. Cache keys and run markers continue to read commandParams so
	// resource policy cannot perturb content identity.
	taskParams extension.ParamMap
	cfg        SchedulerConfig
	// bypass is cfg.NoCache and cfg.NoCacheProjects resolved over this exact
	// plan, once, at construction: every cache decision of the run reads it
	// instead of re-deriving the closure per job.
	bypass   CacheBypass
	renderer Renderer
	cache    *store.CacheManager
	// cacheStats is present for every cache-enabled run, including local-only
	// runs. A configured remote cache shares this exact accumulator so the final
	// snapshot cannot split one session across two independently-timed views.
	cacheStats *CacheStats
	// onCacheLeaseWait observes a genuine miss after this scheduler lost the
	// per-key lease and immediately before it waits for the owner. Tests install
	// it before Run; production leaves it nil.
	onCacheLeaseWait func(*ScheduledJob)
	// outputLocks excludes other sessions of this workspace from the outputs a
	// task writes (task_output_lock.go). Nil takes no lock.
	outputLocks *taskOutputLocks
	// onOutputLockWait receives the one line a contended task-output lock
	// prints. Tests install it before Run; production leaves it nil and the
	// line goes to stderr.
	onOutputLockWait func(message string)
	onSessionEvent   SessionEventHandler
	// openingSessionEvents are RunRequest.OpeningSessionEvents, recorded once
	// after the opening scheduler summary.
	openingSessionEvents []SessionRecord
	// sessionEnded de-duplicates terminal job events across coordinator paths
	// (normal completion, drain, stuck DAG, and final cancellation synthesis).
	sessionEnded   map[string]bool
	sessionEndedMu sync.Mutex

	// remote, when set, negotiates and shares cache results with a remote build
	// cache. It is best-effort and degrades to local-only caching on any error.
	remote *RemoteCache

	// storeBudget is the in-run half of the store byte budget, or nil
	// when caching is off. The coordinator is its only caller: AfterBatch at each
	// completed dispatch group, Close once the workers are gone.
	storeBudget *store.RunBudget

	// hookSlots tracks the in-flight or completed state of each preBuild hook
	// keyed by extension+project, so each hook runs at most once per pair AND
	// concurrent callers with the same key wait for the result instead of
	// racing past with stale/missing hook outputs.
	hookSlots map[string]*hookSlot
	hooksMu   sync.Mutex

	// failureRecords is the set of cache keys this run already recorded a
	// failure for, so one run counts one attempt per key however many plan
	// nodes finalize against it (a plan-level shared node's leader and its
	// followers all do). See task_failure.go.
	failureRecords   map[string]bool
	failureRecordsMu sync.Mutex

	// replayedFailures is the read-side twin: the negative entry this run
	// already replayed at each key. The lookup that consumes it runs BEFORE
	// enterSharedExecution, so every member of a plan-level shared node reaches
	// it; without the memo a three-member node would count three observations
	// of one failure this run observed once. See task_failure.go.
	replayedFailures   map[string]*store.TaskFailure
	replayedFailuresMu sync.Mutex

	// commandOutputs serializes the first detach of each shared command-output
	// directory. Task-owned restores and executions share the directory, so a
	// stale symlink must become a writable tree exactly once before either writes.
	commandOutputs   map[string]*commandOutputState
	commandOutputsMu sync.Mutex

	// sharedSlots holds one rendezvous per plan-level shared node — the set of
	// content-identical nodes several commands scheduled over one manifest task.
	// The first member to miss its cache takes the
	// slot and executes; the rest adopt its result instead of spawning a second
	// identical subprocess. See scheduler_shared.go.
	sharedSlots map[string]*sharedExecutionSlot
	sharedMu    sync.Mutex

	// executions is the run's PHYSICAL ledger: every subprocess this scheduler
	// spawned, appended as each attempt finishes. It is deliberately NOT derived
	// from the surviving results — those keep only their final attempt, so a
	// superseded retry (and a whole batch leader whose members all retried solo)
	// would otherwise disappear from the run's measured cost. See execution.go.
	executions executionLedger

	// environmentBegin is the opening reading of the runner's cumulative
	// counters — cgroup throttling, PSI, steal — taken before any job is
	// dispatched. Every counter it holds is cumulative since boot, so it is
	// useless alone: finalizeRun takes the closing reading and publishes only
	// the delta, which is this session's own window. See environment.go.
	environmentBegin environmentSample

	// versionBuildTime is shared by every real execution in this scheduler run.
	// versionFiles tracks projects already refreshed after their first cache
	// miss, avoiding a timestamp rewrite before each sibling step. The source
	// binding memo shares this mutex: refreshes/restamps are its readers, while
	// completed executions and source-visible restores invalidate entries.
	versionBuildTime         string
	versionFiles             map[string]bool
	capabilitySourceBindings *capabilitySourceBindingMemo
	versionFilesMu           sync.Mutex

	// invocations owns the plan's `finalizes` relations: the private
	// invocation scratch each producer writes into, the non-secret lease that
	// survives a kill, the exactly-once finalizer, and the fail-closed guard
	// that keeps a sensitive artifact's path and bytes off every published
	// surface. Nil for every plan with no finalizer.
	//
	// It is the ONLY provisioning machinery left in the scheduler, and it is
	// generic: an earlier version deleted the database test-infra planner that
	// used to sit beside it — a core component that walked each test project's
	// requirements closure, shelled out to docker, synthesized a connection
	// string and injected it through a private env seam. Database test
	// environments are one PROVIDER of this shape now
	// (go.putnami.dev/sdk/extension/dbtestenv), not a second implementation of
	// it.
	invocations *invocationRuntime

	// metrics records per-job ready/start timing so Run can report ready wait
	// and the critical path. Initialized per Run.
	metrics *schedulerMetrics

	// processCapabilities is the runtime half of an opaque authorization the
	// engine derived from the final plan. Manifests cannot construct it, and the
	// scheduler grants it only after the required functional leaves succeeded.
	processCapabilities *processCapabilityRuntime
	// internalJobs are framework-owned DAG nodes. Their executable behavior is
	// supplied in-memory by the engine and therefore cannot be authored by an
	// extension manifest or serialized into a plan/session. They still
	// participate in the ordinary dependency, rendering, and failure paths.
	internalJobs map[string]InternalJobRunner

	// cpuAlloc derives deterministic per-task ceilings from persisted execution
	// history after a cache miss. Initialized per Run.
	cpuAlloc *cpuAllocator
	// criticalityCeilingByJob is each planned job's share of the machine by
	// expected wall time within THIS plan. Plan-scoped and never persisted —
	// see planCriticalityCeilings. Empty under the measured policy's inputs or
	// when no job in the plan has wall history.
	criticalityCeilingByJob map[string]int
	// batchCPUBudgetByJob fixes one physical ceiling for every member of a
	// readyBatchKey-compatible planned class. A member receives the same ceiling
	// whether dispatch timing runs it alone, in a partial group, or with the full
	// class. Initialized once before workers start.
	batchCPUBudgetByJob map[string]int
	// resourcePlanByJob fixes each task's subprocess ceiling and admission
	// request before workers exist. resources is coordinator-owned and accounts
	// only physical jobGroup dispatches, never logical batch members.
	resourcePlanByJob map[string]scheduledResourcePlan
	resources         *resourcePool

	// taskStats is the machine-local execution history (CPU and wall time per
	// project+job). Loaded per Run, updated by real executions, saved after
	// the run. Drives learned CPU weights and longest-first dispatch.
	taskStats *taskStatsStore
	// workerCount is the resolved scheduler concurrency for this run. Optional
	// batch policies use it to avoid tool-specific batching envelopes that
	// would replace useful cross-project parallelism.
	workerCount int
}

// hookSlot coordinates concurrent runPreBuildHook calls for the same
// extension+project pair. The first caller (leader) runs the hook and closes
// done; later callers (followers) block on done and read err.
type hookSlot struct {
	done chan struct{}
	err  error
}

// commandOutputState is the per-(project, command) synchronization state for a
// shared .putnami/out/<project>/<command> directory.
type commandOutputState struct {
	mu           sync.Mutex
	materialized bool
	project      string
	command      string
}

type coordinatorWaitResult struct {
	completed jobGroupDone
	pending   []*ScheduledJob
	sent      bool
	canceled  bool
}

// newScheduler creates a DAG scheduler for the given execution plan.
//
// It is UNEXPORTED on purpose. ADR 0001 §3 says no adapter
// constructs or configures a jobs.Scheduler: construction, remote-cache wiring
// and session-event wiring are one indivisible step that only RunPlan performs,
// so a caller cannot assemble a run that is missing one of them. Every direct
// construction left in the tree is in this package's own tests, which is where
// the scheduler is legitimately exercised as a unit; the seam is a package-
// private function, so no production package outside internal/jobs can reach it
// even by accident. internal/cli/engine_boundary_test.go pins the rest.
func newScheduler(
	ws *workspace.Workspace,
	planned []*ScheduledJob,
	commandParams map[string]any,
	cfg SchedulerConfig,
	renderer Renderer,
	cache *store.CacheManager,
) *Scheduler {
	s := &Scheduler{
		ws:                       ws,
		planned:                  planned,
		commandParams:            commandParams,
		taskParams:               commandParams,
		cfg:                      cfg,
		bypass:                   NewCacheBypass(planned, cfg.NoCache, cfg.NoCacheProjects),
		renderer:                 renderer,
		cache:                    cache,
		hookSlots:                make(map[string]*hookSlot),
		commandOutputs:           make(map[string]*commandOutputState),
		versionFiles:             make(map[string]bool),
		capabilitySourceBindings: newCapabilitySourceBindingMemo(),
	}
	if cache != nil && !cfg.NoCache {
		s.cacheStats = &CacheStats{}
	}
	if cache != nil {
		// The stamp's marker reads the answer every execution key of the run
		// reads, so an output stamped with no source claim is never stored
		// under a key computed as inside a repository.
		s.capabilitySourceBindings.isUnmanaged = func(repoRoot string) bool {
			return workspaceSourceState(cache, repoRoot) == store.SourceStateUnmanaged
		}
	}
	if ws != nil {
		s.outputLocks = newTaskOutputLocks(
			ws.Root,
			outputLockHolder{Session: cfg.SessionID, PID: os.Getpid()},
			os.Getenv(heldOutputLocksEnv),
			s.announceOutputLockWait,
		)
	}
	return s
}

// RunRequest is a complete description of one job execution: everything the
// scheduler needs, stated once, by the engine.
//
// It exists so the scheduler has no configurable half-built state. Before it,
// a run was "construct, then maybe SetRemoteCache, then maybe
// SetSessionEventHandler, then Run" — four steps each adapter re-implemented a
// different subset of, which is how five hand-assembled run loops appeared.
type RunRequest struct {
	// Workspace is the resolved workspace the plan belongs to.
	Workspace *workspace.Workspace
	// Plan is the scheduled job DAG to execute.
	Plan []*ScheduledJob
	// CommandParams are the run's command params. Values declared as task inputs
	// retain their Go TYPES in cache keys (store.hashParams), so this bag travels
	// as the caller built it before each task projects its own semantic subset.
	CommandParams map[string]any
	// ExtensionParams are execution-only framework globals delivered to the
	// extension task context. They are overlaid after CommandParams but never
	// participate in cache keys or run-marker identity.
	ExtensionParams extension.ParamMap
	// Config is the scheduler tuning for this run.
	Config SchedulerConfig
	// Renderer receives the run's job events.
	Renderer Renderer
	// Cache is the local cache manager, or nil when caching is off.
	Cache *store.CacheManager
	// Remote enables the remote build cache. A nil Remote (or a nil Cache)
	// leaves the run local-only.
	Remote *RemoteCache
	// StoreBudget holds the store's byte budget while the run is in flight: the
	// coordinator checks it each time a dispatch group completes, and a due
	// collection pass runs in the background. The coordinator never waits for
	// it, but the jobs do, in short sections: while the pass removes entries it
	// holds the store's exclusive lock, and their lookups, publishes and
	// restores wait. Nil leaves the budget to the post-build pass alone.
	StoreBudget *store.RunBudget
	// SessionEvents receives the run's session records, or nil when neither a
	// session file nor the profiler is recording.
	SessionEvents SessionEventHandler
	// OpeningSessionEvents are session records the engine resolved before the
	// run — the --impacted selection trace — delivered to SessionEvents in order
	// right after the opening scheduler:parallel event and before any task
	// starts. They wait for the scheduler because only then has the renderer
	// initialized the bounded machine-output state its final record summarizes.
	OpeningSessionEvents []SessionRecord
	// ProcessCapabilityAuthorization is the engine-produced, non-serializable
	// proof for this exact final plan. Nil keeps direct adapters unarmed.
	ProcessCapabilityAuthorization *ProcessCapabilityAuthorization
	// InternalJobs binds framework-owned plan keys to their in-process runners.
	// The map is execution-only: the corresponding ScheduledJob is the complete
	// auditable DAG shape, while the callback never reaches task context, cache
	// identity, remote negotiation, or session serialization.
	InternalJobs map[string]InternalJobRunner
}

// InternalJobRunner executes one framework-owned DAG node against an immutable
// snapshot of already-completed results. Only the engine can supply these
// runners; repository manifests can schedule no function value.
type InternalJobRunner func(context.Context, map[string]*JobResult) *JobResult

// RunPlan is the ONE entry point into job execution (ADR 0001 §3).
//
// It constructs, configures and runs the scheduler in a single call so no caller
// can hold a partially configured *Scheduler. internal/engine is its only
// permitted caller; internal/cli/engine_boundary_test.go fails when a second
// package reaches for it.
func RunPlan(ctx context.Context, req RunRequest) *SchedulerResult {
	// Direct adapters do not gain an authorization object, but they still must
	// not leak an inherited transport to remote cache negotiation or jobs.
	ctx = CaptureProcessCapabilities(ctx)
	return req.scheduler().Run(ctx)
}

// scheduler builds the fully configured scheduler for the request: RunPlan's
// entire body minus Run. It is split out only so the wiring can be asserted
// without executing a plan — in particular that CommandParams reaches the
// scheduler as the SAME map, since a param value's Go type is part of every
// cache key (store.hashParams) and a copy through an intermediate shape is how
// an int becomes a string and every key changes.
func (req RunRequest) scheduler() *Scheduler {
	s := newScheduler(req.Workspace, req.Plan, req.CommandParams, req.Config, req.Renderer, req.Cache)
	if len(req.ExtensionParams) > 0 {
		s.taskParams = make(extension.ParamMap, len(req.CommandParams)+len(req.ExtensionParams))
		for name, value := range req.CommandParams {
			s.taskParams[name] = value
		}
		for name, value := range req.ExtensionParams {
			s.taskParams[name] = value
		}
	}
	s.setRemoteCache(req.Remote)
	s.storeBudget = req.StoreBudget
	s.setSessionEventHandler(req.SessionEvents)
	s.openingSessionEvents = req.OpeningSessionEvents
	s.processCapabilities = newProcessCapabilityRuntime(req.ProcessCapabilityAuthorization, req.Plan)
	s.internalJobs = req.InternalJobs
	return s
}

// setSessionEventHandler sets the callback for session event recording.
func (s *Scheduler) setSessionEventHandler(handler SessionEventHandler) {
	s.onSessionEvent = handler
}

// setRemoteCache enables the remote build cache for this run. A nil remote (or
// nil local cache) leaves the scheduler local-only.
func (s *Scheduler) setRemoteCache(remote *RemoteCache) {
	if s.cache == nil {
		return
	}
	s.remote = remote
	if remote == nil || s.cfg.NoCache {
		return
	}
	// Preserve setup time recorded while the engine resolved the provider before
	// constructing the scheduler. Tests may hand-build a remote without stats;
	// in that case give it the local accumulator rather than leaving a nil trap.
	if remote.stats != nil {
		s.cacheStats = remote.stats
	} else {
		if s.cacheStats == nil {
			s.cacheStats = &CacheStats{}
		}
		remote.stats = s.cacheStats
	}
}

// prepareRun performs the pre-execution setup that must complete before any
// cache key is computed and workers start: it generates each project's
// version.json.
//
// It used to do a second thing — export a per-project fixture digest into the
// cache-key environment for every test job about to receive a provisioned
// database — and the ordering constraint was the whole reason this function
// exists. That is gone with the test-infra planner: a
// consumer of an invocation-scoped resource folds in the PRODUCING ACTION's
// digest (executor.go), which is derived from the producer's declared inputs
// and needs no process-global side effect to be visible to the key.
func (s *Scheduler) prepareRun() {
	s.versionBuildTime = time.Now().UTC().Format(time.RFC3339)
	started := time.Now()
	versionedPlan := s.planned
	if len(s.internalJobs) > 0 {
		versionedPlan = make([]*ScheduledJob, 0, len(s.planned)-len(s.internalJobs))
		for _, job := range s.planned {
			if job != nil && s.internalJobs[job.Key()] != nil {
				continue
			}
			versionedPlan = append(versionedPlan, job)
		}
	}
	spawned := generateVersionFilesAtWithSourceBindings(
		s.ws, versionedPlan, s.cfg.VersionInfo, s.versionBuildTime, true, s.capabilitySourceBindings,
	)
	s.cacheStats.recordLocalBindings(started, time.Now(), spawned)
}

// prepareScheduling resolves learned execution tuning and performs every setup
// step that must precede cache-key computation.
func (s *Scheduler) prepareScheduling(decision ParallelDecision) {
	s.taskStats = loadTaskStats(s.ws.Root)
	applyTaskTuning(s.planned, s.taskStats)
	applyCriticalPathTuning(s.planned)
	// Size the allocator by the parallelism the host will actually deliver.
	// LogicalCPU counts the CPUs the machine advertises and ignores a cgroup CPU
	// bandwidth limit, so on a quota-limited runner it over-reports — and
	// concentrating the machine on one job on the strength of that number buys
	// throttling, not throughput. The opening environment sample already read
	// the v2 cpu.max or v1 cpu.cfs quota for the session report; this is the same
	// number, used to decide rather than only to describe. Unbounded hosts report
	// 0 and keep LogicalCPU.
	machineCPU := decision.LogicalCPU
	if quota := s.environmentBegin.cpuBandwidthCores(); quota > 0 {
		machineCPU = min(machineCPU, quota)
	}
	s.cpuAlloc = newCPUAllocator(machineCPU, cpuBudgetPolicy(s.cfg.CPUBudgetPolicy))
	s.criticalityCeilingByJob = planCriticalityCeilings(s.planned, machineCPU, decision.Workers)
	memoryCapacity := uint64(max(decision.MemoryTotalMiB, 0)) * 1024 * 1024
	usableMemory := usableMemoryBytes(memoryCapacity)
	s.resources = newResourcePool(machineCPU, usableMemory, s.cfg.ResourceBudgets)
	s.prepareRun()

	// Fix one ceiling for each planned compatibility class before dispatch
	// timing can choose its runtime membership. A shared tool has one thread pool
	// whether it receives one project or many, and extension-loop batches run
	// projects sequentially, so use the maximum member recommendation rather
	// than the sum. Summing the whole class would make large classes converge on
	// the machine cap and overgrant every singleton merely because peers exist.
	s.batchCPUBudgetByJob = make(map[string]int)
	s.resourcePlanByJob = make(map[string]scheduledResourcePlan, len(s.planned))
	classBudget := make(map[string]int)
	classMembers := make(map[string][]*ScheduledJob)
	for _, job := range s.planned {
		recommendation := s.cpuAlloc.recommend(
			job.EffectiveCPUWeight(),
			job.ExpectedCPUWorkMs,
			float64(job.ExpectedWallMs),
			job.HistoricalCPUCeiling,
			s.criticalityCeilingByJob[job.Key()],
		)
		fallback := bootstrapResourceRequest(job, machineCPU, usableMemory)
		admissionCPU := fallback.cpu
		if recommendation.fromHistory {
			admissionCPU = profiledCPURequest(
				job.ResourceProfile,
				recommendation.budget,
				job.ExpectedCPUWorkMs,
				float64(job.ExpectedWallMs),
			)
		}
		s.resourcePlanByJob[job.Key()] = scheduledResourcePlan{
			recommendation: recommendation,
			ceiling:        recommendation.budget,
			cpu:            admissionCPU,
			memoryFallback: fallback.memoryBytes,
		}

		key := s.readyBatchKey(job)
		if key == "" {
			continue
		}
		classBudget[key] = max(classBudget[key], recommendation.budget)
		classMembers[key] = append(classMembers[key], job)
	}
	for key, members := range classMembers {
		for _, job := range members {
			jobKey := job.Key()
			s.batchCPUBudgetByJob[jobKey] = classBudget[key]
			plan := s.resourcePlanByJob[jobKey]
			plan.ceiling = classBudget[key]
			if plan.recommendation.fromHistory {
				plan.cpu = profiledCPURequest(
					job.ResourceProfile,
					plan.ceiling,
					job.ExpectedCPUWorkMs,
					float64(job.ExpectedWallMs),
				)
			}
			s.resourcePlanByJob[jobKey] = plan
		}
	}
	// A `runOn: finally` node is planned but never DISPATCHED: its trigger is
	// producer start and its frontier is the consumers' terminal states, neither
	// of which a dependency DAG can express. The coordinator runs over the
	// schedulable plan (schedulableJobs) and this runtime owns the finalizers.
	s.invocations = newInvocationRuntime(s.ws, s.planned)
	// Negotiate the finalizes relations' consumer frontiers BEFORE any producer
	// can materialize a resource: an all-hit frontier prunes the producer and
	// its finalizer together.
	s.invocations.prune(
		s.ws, s.planned, s.commandParams, s.cfg.VersionInfo,
		s.cache, s.bypass, s.cacheStats,
	)
}

// Run executes all planned jobs respecting the dependency DAG.
// It returns when all jobs are complete or the context is canceled.
//
// The body below is the run's PHASE ORDER and little else. Each phase is a
// named helper, except the two things that cannot leave without taking
// ownership with them — the channel/WaitGroup wiring and the inline
// coordinator loop:
//
//	beginRun         parallelism decision, per-run metrics, learned tuning,
//	                 version stamps — everything a cache key may depend on
//	prepareRemote    one-round-trip remote negotiation plus background prefetch
//	(inline)         DAG state, the results map and its mutex, readyCh/doneCh,
//	                 the cancellable child context
//	startWorkers     the fixed worker pool that drains readyCh
//	(inline)         the coordinator: dispatch, completion, cancellation
//	close + wg.Wait  the ONLY close of readyCh and the ONLY join of the pool
//	closeInvocations invocation finalizers, once no worker can run again
//	drainRemote      background uploads drained, then prefetching stopped
//	finalizeRun      canceled-result synthesis, rendering, persistence, reduction
//
// Run owns readyCh (it is the sole closer), doneCh (never closed — the
// coordinator counts completions instead of ranging over it), the WaitGroup,
// the cancel func, and the mutex-guarded results/hashes maps. The helpers
// receive those as parameters, with DIRECTIONAL channel types, so the compiler
// rather than a convention keeps the close and the join here.
func (s *Scheduler) Run(ctx context.Context) *SchedulerResult {
	// Retain the caller's context. Run shadows ctx with its own cancellable
	// child below, which it also cancels on a job failure; only the parent
	// being done means the run was cut short from outside, by a signal.
	parentCtx := ctx

	start := time.Now()
	decision := s.beginRun()
	maxWorkers := decision.Workers

	// Still the CALLER's context here, on purpose — see prepareRemote.
	s.prepareRemote(ctx)

	// Build dependency state
	state := newDAGState(schedulableJobs(s.planned))

	results := make(map[string]*JobResult, len(s.planned))
	hashes := make(map[string]string) // jobKey → cache hash
	var mu sync.Mutex

	// beginRun applies task tuning before live progress snapshots planned work.
	// Once Start has captured that settled plan, emit the opening scheduler audit
	// event before channels or workers can produce any task records. Machine
	// renderers have now initialized the exact bounded state the final summarizes.
	s.renderer.Start(s.planned)
	s.emitSchedulerSummary(decision)

	// Channels
	readyCh := make(chan jobGroup, maxWorkers)
	doneCh := make(chan jobGroupDone, maxWorkers)

	// Use a cancellable context for clean abort
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Worker goroutines. wg stays declared HERE: Run adds nothing to it and
	// waits on it exactly once, at `finish:`, after closing readyCh.
	var wg sync.WaitGroup
	s.startWorkers(ctx, &wg, maxWorkers, readyCh, doneCh, &mu, hashes, results)

	// Coordinator: runs in-line (not a goroutine) to avoid orphaning goroutines.
	// Uses a pending queue to avoid blocking on readyCh while doneCh has results.
	// The queue dispatches historical long poles first so the task that bounds
	// the makespan starts immediately instead of behind the short tail.
	pending := state.ready()
	sortPendingLongestFirst(pending)
	for _, job := range pending {
		s.metrics.markReady(job.Key())
	}
	inFlight := 0
	totalCompleted := 0

	for totalCompleted < state.size() {
		// Try to send pending jobs to workers without blocking
		for len(pending) > 0 && inFlight < maxWorkers {
			group, rest, admitted := s.takeAdmissiblePendingGroup(pending)
			if !admitted {
				goto waitForDone
			}
			select {
			case readyCh <- group:
				s.resources.record(group)
				pending = rest
				inFlight++
			default:
				s.resources.release(group.reservation)
				goto waitForDone
			}
		}

	waitForDone:
		if inFlight == 0 && len(pending) == 0 {
			if totalCompleted < state.size() {
				s.failStuckDAG(state, results, &mu)
			}
			break
		}

		// If we have pending jobs but all workers are busy, or no pending jobs
		// and we need to wait for completions — use a blocking select.
		var completed jobGroupDone
		if len(pending) > 0 && inFlight > 0 {
			wait := s.waitForWorker(ctx, pending, inFlight, maxWorkers, readyCh, doneCh)
			if wait.canceled {
				s.drainInFlight(doneCh, inFlight, results, &mu)
				goto finish
			}
			if wait.sent {
				pending = wait.pending
				inFlight++
				continue
			}
			completed = wait.completed
		} else if len(pending) > 0 {
			// Workers haven't started yet, send to channel
			group, rest, admitted := s.takeAdmissiblePendingGroup(pending)
			if !admitted {
				// Every request is clamped to the empty pool's capacity, so this
				// can only mean corrupt internal accounting. Fail closed instead
				// of waiting on an empty done channel forever.
				s.failStuckDAG(state, results, &mu)
				goto finish
			}
			select {
			case readyCh <- group:
				s.resources.record(group)
				pending = rest
				inFlight++
				continue
			case <-ctx.Done():
				s.resources.release(group.reservation)
				goto finish
			}
		} else {
			// No pending, just wait for completions
			select {
			case completed = <-doneCh:
			case <-ctx.Done():
				s.drainInFlight(doneCh, inFlight, results, &mu)
				goto finish
			}
		}

		// Process completion
		s.releaseGroupResources(completed)
		inFlight--
		groupFailed, newlyReady := s.recordCompletedGroup(completed, state, results, &mu, &totalCompleted)

		// A batch boundary: the cheap in-run budget check. It never blocks this
		// loop; a due collection pass runs beside the work still in flight, which
		// waits on the store while the pass holds its exclusive lock (in
		// sections of about 200 ms; the session's cache record has the totals).
		s.storeBudget.AfterBatch()

		// Tear down every invocation-scoped resource whose consumer frontier just
		// became terminal, before dispatching more work.
		s.runReadyFinalizers(ctx, results, &mu)

		if groupFailed && !s.cfg.ContinueOnError {
			cancel() // cancel in-flight jobs
			s.drainInFlight(doneCh, inFlight, results, &mu)
			break
		}

		pending = s.queueReady(pending, newlyReady, state, results, &mu, &totalCompleted)
	}

finish:
	// The terminal sequence. Every step depends on the one before it, and the
	// order is the contract — see drainRemote and finalizeRun for the two that
	// have a stated reason to be where they are.
	//
	// close(readyCh) is what ends each worker's range loop, and Run is its only
	// closer; wg.Wait is the only join. Neither may move into a helper without
	// moving the pool's lifetime with it.
	close(readyCh)
	wg.Wait()

	// Workers can no longer run, so every armed finalizer that never saw its
	// frontier close runs here, and its terminal row lands in results BEFORE
	// finalizeRun would otherwise synthesize a bare "canceled" over it.
	s.closeInvocations(ctx, results, &mu)

	s.drainRemote()

	// No boundary can start another budget pass now, so join the one that may
	// still be evicting (the process must never exit in the middle of it), let
	// the budget record the growth no pass measured, and keep what it did for
	// the session record, before finalizeRun snapshots the cache stats.
	s.cacheStats.recordStoreBudget(s.storeBudget.Close())

	return s.finalizeRun(parentCtx, decision, start, results, &mu)
}

// beginRun is the run's setup phase: it resolves the parallelism decision, arms
// the per-run state derived from it (metrics, CPU allocator, learned task
// stats, invocation runtime) and completes every step that must precede the
// first cache-key computation.
//
// It runs before a single goroutine exists and touches only scheduler fields,
// which is why nothing here is synchronized. The decision it returns is the
// run's: finalizeRun derives the end-of-run tuning report from that same value.
func (s *Scheduler) beginRun() ParallelDecision {
	s.resetSessionJobEnds()

	// The runner's opening counters, read BEFORE any job is dispatched so the
	// window the closing sample closes is the run's and nothing else's.
	s.environmentBegin = captureEnvironmentSample(runnerEnvironmentRoot)

	memoryCapacity, _, _ := s.environmentBegin.memoryCapacity.effectiveBytes()
	decision := resolveParallelDecisionWithHardware(
		s.cfg, s.planned, runtime.NumCPU(), memoryCapacity,
	)
	if quota := s.environmentBegin.cpuBandwidthCores(); quota > 0 {
		decision.CPUCapacity = min(decision.CPUCapacity, quota)
	}
	s.workerCount = decision.Workers
	s.metrics = newSchedulerMetrics()

	// The publish renderer reports the concurrency the user CONFIGURED — an
	// explicit --max-parallel when there is one, the resolved worker count
	// otherwise — which is not always the count the scheduler settled on.
	configuredCap := decision.Workers
	if s.cfg.MaxParallel > 0 {
		configuredCap = s.cfg.MaxParallel
	}
	if reporter, ok := s.renderer.(PublishConcurrencyReporter); ok {
		reporter.SetPublishConcurrency(configuredCap)
	}

	// Resolve per-job CPU weights, expected durations, and historical grants.
	// Budgets themselves are recorded at spawn time after cache lookup, but their
	// value depends only on the task's persisted history and machine capacity.
	s.prepareScheduling(decision)

	return decision
}

// prepareRemote resolves the whole build's key set against the remote cache in
// one round trip before execution, so each job's lookup can consult the result.
// Best-effort: on any failure the index stays empty and the build runs purely
// local.
//
// ctx is the CALLER's context, not the coordinator's cancellable child, and Run
// calls this BEFORE it shadows ctx for exactly that reason: negotiation and the
// background warm-up it starts are scoped to the run as the caller sees it, and
// must not be torn down by the abort switch the coordinator trips on the first
// job failure. drainRemote is this phase's counterpart at the other end of Run.
func (s *Scheduler) prepareRemote(ctx context.Context) {
	if s.remote == nil {
		return
	}
	s.remote.Negotiate(ctx, s.ws, s.planned, s.commandParams, s.cfg.VersionInfo, s.cache, s.bypass)
	// Warm the hit-dependencies of misses in the background so a local
	// build finds its cached inputs already materialized.
	s.remote.Prefetch(ctx, s.planned, s.cache)
}

// startWorkers launches the fixed pool that drains readyCh until Run closes it.
//
// OWNERSHIP, stated explicitly because this is the one extraction that moves a
// goroutine spawn across a function boundary — and nothing else about the pool
// moved with it:
//
//   - wg belongs to Run. This function only Adds before each `go` and Dones on
//     exit; Run declares it, and Run is the only caller of Wait — exactly once,
//     after it closes readyCh.
//   - readyCh arrives RECEIVE-ONLY and doneCh SEND-ONLY, so no worker can close
//     either, by accident or otherwise. Run is readyCh's sole closer. doneCh is
//     never closed at all: the inline coordinator counts completions instead of
//     ranging over it, and drainInFlight relies on that.
//   - ctx is Run's cancellable CHILD, so canceling the run stops in-flight work.
//     The workers never cancel anything themselves.
//   - mu is the run's single mutex. It guards the coordinator's results map and
//     the workers' hashes map alike (lookupRestoreOrClaim and executeJobGroup
//     take this same lock), so passing it in — rather than reaching for a
//     scheduler-owned one — is what keeps that single ownership visible.
func (s *Scheduler) startWorkers(
	ctx context.Context,
	wg *sync.WaitGroup,
	workers int,
	readyCh <-chan jobGroup,
	doneCh chan<- jobGroupDone,
	mu *sync.Mutex,
	hashes map[string]string,
	results map[string]*JobResult,
) {
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for group := range readyCh {
				for _, job := range group.jobs {
					s.metrics.markStart(job.Key())
				}
				doneCh <- jobGroupDone{
					jobs:        s.executeJobGroup(ctx, group.jobs, mu, hashes, results),
					reservation: group.reservation,
				}
			}
		}()
	}
}

// recordCompletedGroup applies one finished dispatch group to the run's shared
// state: it records each task's terminal result, renders and records its
// job:end, tells the invocation runtime what the producer delivered, and
// returns whether the group failed together with the dependents it unblocked.
//
// It is a pure state transition over (results, state): it owns no channel, no
// goroutine and no cancellation, which is why it can sit outside the inline
// coordinator without moving any ownership out of it. The order INSIDE it is
// load-bearing — a producer that did not deliver its invocation-scoped resource
// must be on record with invocations.observe BEFORE the caller considers its
// dependents for dispatch, or shouldSkip cannot block them. This is the
// ordinary completion path, which includes every way a producer fails before
// its subprocess exists; the abort path records the same three things in
// drainInFlight.
func (s *Scheduler) recordCompletedGroup(
	completed jobGroupDone,
	state *dagState,
	results map[string]*JobResult,
	mu *sync.Mutex,
	totalCompleted *int,
) (groupFailed bool, newlyReady []*ScheduledJob) {
	for _, done := range completed.jobs {
		*totalCompleted++

		mu.Lock()
		results[done.job.Key()] = done.result
		mu.Unlock()

		s.renderer.JobComplete(done.job, done.result)
		s.emitSessionJobEnd(done.job, done.result)
		s.invocations.observe(done.job, done.result)
		groupFailed = groupFailed || done.result.Status == "failed"
		newlyReady = append(newlyReady, state.complete(done.job.Key())...)
	}
	return groupFailed, newlyReady
}

// queueReady admits the jobs a completion just unblocked into the pending
// queue, skipping — and cascading the skip through — any whose dependencies, or
// whose invocation-scoped setup, failed. It returns the new queue, re-sorted
// longest-first only when it actually grew, so the historical long pole is
// dispatched ahead of the short tail.
//
// The queue is the coordinator's own slice, passed and returned by value rather
// than captured: this helper decides membership and order, never dispatch, so
// no channel send can hide inside it.
func (s *Scheduler) queueReady(
	pending []*ScheduledJob,
	newlyReady []*ScheduledJob,
	state *dagState,
	results map[string]*JobResult,
	mu *sync.Mutex,
	totalCompleted *int,
) []*ScheduledJob {
	queued := false
	for _, job := range newlyReady {
		if s.shouldSkip(job, results, mu) {
			s.skipJob(job, state, results, mu, totalCompleted)
			continue
		}
		s.metrics.markReady(job.Key())
		pending = append(pending, job)
		queued = true
	}
	if queued {
		sortPendingLongestFirst(pending)
	}
	return pending
}

// drainRemote ends the run's remote-cache activity, in this order and no other:
// the builds have finished but their post-build stores may still be in flight,
// so drain background UPLOADS first — freshly built entries should be shared
// before the run ends — then cancel and drain any still-running speculative
// prefetches, whose results are no longer needed.
//
// Run calls this only after wg.Wait(), so no worker can schedule another upload
// while the drain runs, and only before finalizeRun, whose cache summary reads
// counters this drain is still incrementing.
func (s *Scheduler) drainRemote() {
	if s.remote == nil {
		return
	}
	s.remote.DrainUploads()
	s.remote.Stop()
}

// finalizeRun is the run's terminal phase: every planned job gets a row, the
// caller's finalizer sees the complete map, the renderer finishes, execution
// history is persisted, and the canonical reduction becomes the result.
//
// The ORDER is the contract, and it is the order of the statements below:
//
//  1. synthesizeCanceledResults first, so cfg.FinalizeResults and every reader
//     after it see a row for every planned job. Run calls this phase after
//     closeInvocations precisely so the finalizer rows recorded there are
//     already present and are NOT overwritten with a bare "canceled".
//  2. cfg.FinalizeResults — the caller's last chance to amend rows.
//  3. resolveSessionOutcome then renderer.Finish, so human and machine output
//     report the amended rows and the abort verdict together.
//  4. taskStats.save, best-effort, once the results it learned from are final.
//  5. the reduction, which is the run's verdict: Session.Success() rather than
//     a second walk of the map. Its cache summary is read after
//     drainRemote, so upload counters are in it.
//
// mu is uncontended by the time this runs — no worker exists any more — but it
// is still taken, because synthesizeCanceledResults is the shared helper the
// coordinator paths use and has no such guarantee.
func (s *Scheduler) finalizeRun(
	parentCtx context.Context,
	decision ParallelDecision,
	start time.Time,
	results map[string]*JobResult,
	mu *sync.Mutex,
) *SchedulerResult {
	// Mark any remaining planned jobs as canceled (if aborted).
	s.synthesizeCanceledResults(results, mu)

	if s.cfg.FinalizeResults != nil {
		s.cfg.FinalizeResults(results)
	}

	outcome := resolveSessionOutcome(parentCtx)
	duration := time.Since(start)
	outcome.Duration = duration
	var cacheStats *CacheStatsSnapshot
	if s.cacheStats != nil {
		cacheStats = s.cacheStats.Snapshot()
		if !cacheStats.HasActivity() {
			cacheStats = nil
		}
	}
	outcome.Cache = cacheStats
	s.renderer.Finish(results, outcome)

	// Persist the run's execution history so the next run's learned weights
	// and dispatch order see it. Best-effort by design.
	s.taskStats.save()

	// The run's verdict is the canonical reduction's, not a second walk of the
	// result map: a later change deleted runSucceeded in favor of Session.Success,
	// which applies the same strict rule (a reused failure still fails the run).
	session := reduceSchedulerSession(s.planned, results, outcome, duration, cacheStats)
	// The physical ledger is ATTACHED, not reduced: it is the one quantity in
	// the session that cannot be recovered from the surviving task results.
	session.Executions = s.executions.snapshot()
	// The runner environment closes its window HERE, after the last subprocess
	// has been waited for, so the throttle, pressure and steal deltas cover the
	// whole of the work they are meant to explain.
	environment := resolveRunnerEnvironment(runnerEnvironmentRoot, s.environmentBegin, captureEnvironmentSample(runnerEnvironmentRoot))
	session.Environment = &environment
	// Preparation is carried through UNCHANGED. It was measured before this
	// scheduler existed and describes in-process work, so it is neither reduced
	// from results nor folded into the execution ledger — see preparation.go.
	session.Preparation = s.cfg.Preparation.Snapshot()
	tuning := buildTuningReport(decision, s.planned, results, s.metrics.readyWaits(), s.cpuAlloc.grantsSnapshot())
	tuning.ResourceReservations = s.resources.grantsSnapshot()
	return &SchedulerResult{
		Results:  results,
		Success:  session.Success(),
		Outcome:  outcome,
		Duration: duration,
		Tuning:   tuning,
		Cache:    cacheStats,
		Session:  session,
	}
}

func (s *Scheduler) waitForWorker(
	ctx context.Context,
	pending []*ScheduledJob,
	inFlight int,
	maxWorkers int,
	readyCh chan<- jobGroup,
	doneCh <-chan jobGroupDone,
) coordinatorWaitResult {
	if inFlight >= maxWorkers {
		select {
		case completed := <-doneCh:
			return coordinatorWaitResult{completed: completed}
		case <-ctx.Done():
			return coordinatorWaitResult{canceled: true}
		}
	}

	group, rest, admitted := s.takeAdmissiblePendingGroup(pending)
	if !admitted {
		select {
		case completed := <-doneCh:
			return coordinatorWaitResult{completed: completed}
		case <-ctx.Done():
			return coordinatorWaitResult{canceled: true}
		}
	}
	select {
	case readyCh <- group:
		s.resources.record(group)
		return coordinatorWaitResult{pending: rest, sent: true}
	case completed := <-doneCh:
		s.resources.release(group.reservation)
		return coordinatorWaitResult{completed: completed}
	case <-ctx.Done():
		s.resources.release(group.reservation)
		return coordinatorWaitResult{canceled: true}
	}
}

func (s *Scheduler) resetSessionJobEnds() {
	s.sessionEndedMu.Lock()
	s.sessionEnded = make(map[string]bool, len(s.planned))
	s.sessionEndedMu.Unlock()
}

// The scheduler no longer prints an end-of-run findings block.
//
// It had two producers and both are gone. Infra AGGREGATION findings left
// because emitting a workload's aggregated requirements is a task
// the language pipelines own, so its findings arrive as that task's own
// warnings. TEST-INFRA findings left for the same
// reason: the extension task that provisions a database test environment
// reports what it could not do, attributed to the project whose test run
// surfaced it, instead of an end-of-run block core assembled by walking a graph
// it should not be interpreting.

// resolveSessionOutcome classifies how the run ended. An abort needs BOTH a
// done parent context and a signal on record.
//
// The context alone is not enough. Watch and serve cancel each iteration
// routinely — every file change restarts the server that way — and those runs
// end exactly as intended; treating a bare cancellation as an abort would
// report each restart as a stopped, unsuccessful session. The recorded signal
// is what separates "someone stopped this" from "this iteration is over".
func resolveSessionOutcome(parentCtx context.Context) SessionOutcome {
	if parentCtx.Err() == nil {
		return SessionOutcome{}
	}
	by := abort.Source()
	if by == "" {
		return SessionOutcome{}
	}
	return SessionOutcome{Aborted: true, AbortedBy: by}
}

// synthesizeCanceledResults gives every planned job that never reached a
// terminal state its row.
//
// A consumer whose invocation-scoped setup failed is reported as SKIPPED with
// the typed causal diagnostic rather than as canceled. The distinction is the
// whole point of the code: "canceled" says the run was cut short and tells the
// reader nothing, while `sensitive.setup_failed` names the provisioning task
// whose failure is the reason this task never ran.
func (s *Scheduler) synthesizeCanceledResults(results map[string]*JobResult, mu *sync.Mutex) {
	var synthesized []jobDone
	mu.Lock()
	for _, job := range s.planned {
		if _, ok := results[job.Key()]; ok {
			continue
		}
		terminal := &JobResult{Status: "canceled"}
		if cause := s.invocations.blockedError(job); cause != nil {
			terminal = &JobResult{Status: "skipped", Error: cause}
		}
		results[job.Key()] = terminal
		synthesized = append(synthesized, jobDone{job: job, result: terminal})
	}
	mu.Unlock()

	for _, done := range synthesized {
		s.emitSessionJobEnd(done.job, done.result)
	}
}

// emitSchedulerSummary records the parallelism decision as a session event so
// it lands in machine-readable JSONL output before any job runs, then the
// engine's opening records (RunRequest.OpeningSessionEvents).
func (s *Scheduler) emitSchedulerSummary(decision ParallelDecision) {
	if s.onSessionEvent == nil {
		return
	}
	data := map[string]any{
		"mode":             decision.Mode,
		"workers":          decision.Workers,
		"logicalCpu":       decision.LogicalCPU,
		"cpuCapacity":      decision.CPUCapacity,
		"memoryTotalMiB":   decision.MemoryTotalMiB,
		"memoryUsableMiB":  decision.MemoryUsableMiB,
		"memoryCapWorkers": decision.MemoryCapWorkers,
		"plannedJobs":      decision.PlannedJobs,
		"heavyJobPermille": decision.HeavyJobPermille,
		"cpuBudget":        string(normalizeCPUBudgetPolicy(s.cfg.CPUBudgetPolicy)),
	}
	// The worker count alone no longer explains why a run held back: a named
	// budget is the other admission dimension, so record it whenever the run
	// declared one. Omitted entirely otherwise, so the record's shape is
	// unchanged for every run that declares none.
	if len(s.cfg.ResourceBudgets) > 0 {
		data["resourceBudgets"] = s.cfg.ResourceBudgets
	}
	if s.cfg.SessionID != "" {
		data["sessionId"] = s.cfg.SessionID
	}
	s.onSessionEvent(SessionRecord{Type: "scheduler:parallel", Data: data})
	// The engine's pre-run records follow the opening event, so it stays the
	// first session record a stream reader binds to.
	for _, record := range s.openingSessionEvents {
		s.onSessionEvent(record)
	}
}

func (s *Scheduler) failStuckDAG(state *dagState, results map[string]*JobResult, mu *sync.Mutex) {
	remainingJobs, remainingCounts := state.remainingSnapshot()
	if len(remainingJobs) == 0 {
		return
	}

	message := stuckDAGMessage(s.planned, remainingCounts)
	failedJob := remainingJobs[0]
	failed := &JobResult{
		Status: "failed",
		Error:  &JobError{Message: message},
	}
	terminal := make([]jobDone, 0, len(remainingJobs))
	terminal = append(terminal, jobDone{job: failedJob, result: failed})

	mu.Lock()
	results[failedJob.Key()] = failed
	for _, job := range remainingJobs[1:] {
		if _, exists := results[job.Key()]; !exists {
			canceled := &JobResult{
				Status: "canceled",
				Error:  &JobError{Message: "dependency graph could not make progress"},
			}
			results[job.Key()] = canceled
			terminal = append(terminal, jobDone{job: job, result: canceled})
		}
	}
	mu.Unlock()

	s.renderer.JobStart(failedJob)
	s.renderer.JobComplete(failedJob, failed)
	for _, done := range terminal {
		s.emitSessionJobEnd(done.job, done.result)
	}
}

// drainInFlight waits for all in-flight jobs to complete and records their results.
func (s *Scheduler) drainInFlight(doneCh <-chan jobGroupDone, count int, results map[string]*JobResult, mu *sync.Mutex) {
	for i := 0; i < count; i++ {
		completed := <-doneCh
		s.releaseGroupResources(completed)
		for _, done := range completed.jobs {
			mu.Lock()
			if _, exists := results[done.job.Key()]; !exists {
				results[done.job.Key()] = done.result
			}
			mu.Unlock()
			s.renderer.JobComplete(done.job, done.result)
			s.emitSessionJobEnd(done.job, done.result)
			s.invocations.observe(done.job, done.result)
		}
	}
}

// skipJob marks a job as skipped and cascades to its dependents.
//
// A consumer blocked by a failed invocation-resource setup gets the typed
// causal diagnostic instead of the generic message: "dependency failed" says
// nothing a caller can act on, while `sensitive.setup_failed` names the
// provisioning task's failure as the reason this task never ran against a
// resource that does not exist.
func (s *Scheduler) skipJob(job *ScheduledJob, state *dagState, results map[string]*JobResult, mu *sync.Mutex, totalCompleted *int) {
	cause := s.invocations.blockedError(job)
	if cause == nil {
		cause = &JobError{Message: "dependency failed"}
	}
	skipped := &JobResult{
		Status: "skipped",
		Error:  cause,
	}
	mu.Lock()
	results[job.Key()] = skipped
	mu.Unlock()
	s.renderer.JobStart(job)
	s.renderer.JobComplete(job, skipped)
	s.emitSessionJobEnd(job, skipped)
	*totalCompleted++

	for _, cascaded := range state.complete(job.Key()) {
		s.skipJob(cascaded, state, results, mu, totalCompleted)
	}
}

// shouldSkip checks if any dependency of the job has failed.
//
// A consumer whose invocation-scoped setup failed is skipped even under
// --continue-on-error. That flag says "keep going past an unrelated failure",
// not "run this task against a resource that does not exist": the producer's
// whole purpose was to provision what this consumer reads, so running it would
// report a failure about a missing database instead of about the setup.
func (s *Scheduler) shouldSkip(job *ScheduledJob, results map[string]*JobResult, mu *sync.Mutex) bool {
	if s.invocations.blockedError(job) != nil {
		return true
	}
	// Capability gates are not weakened by --continue-on-error. Structural
	// dominance was proven before execution; here every functional ancestor must
	// also have reached a successful terminal result before the private grant.
	if !s.processCapabilities.authorizeReady(job, results, mu) {
		return true
	}
	if s.cfg.ContinueOnError {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	for _, depKey := range job.DependsOn {
		if r, ok := results[depKey]; ok && r.Status == "failed" {
			return true
		}
	}
	return false
}
