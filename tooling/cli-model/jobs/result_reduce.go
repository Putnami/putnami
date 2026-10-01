package jobs

import (
	"sort"
	"time"

	"go.putnami.dev/cli/model/extension"
)

// The one reducer.
//
// Nine tally loops walked the same result map and each invented its own
// bucketing (internal/output/json.go, internal/output/jsonl.go,
// internal/mcp/adapter.go, internal/cli/jobs_helpers.go,
// internal/watch/session_iteration.go, jobs.runSucceeded, and the display-side
// rollups). SessionReducer is the single fold they collapse onto; every count
// it produces is pinned against those loops in result_reduce_test.go, which is
// the contract a later migration relies on when it deletes them.
//
// Cache accounting is NOT re-derived here. CacheStats is already a correct
// run-scoped reducer with union-interval restore timing, so its snapshot is
// folded in as-is.

// TaskCounts is a status histogram. Which tasks it counts depends on where it
// sits in SessionResult.
type TaskCounts struct {
	Succeeded int
	Failed    int
	Canceled  int
	Skipped   int
}

// Total is the sum of the four buckets.
func (c TaskCounts) Total() int {
	return c.Succeeded + c.Failed + c.Canceled + c.Skipped
}

func (c *TaskCounts) add(status TaskStatus) {
	switch status {
	case TaskStatusSuccess:
		c.Succeeded++
	case TaskStatusFailed:
		c.Failed++
	case TaskStatusCanceled:
		c.Canceled++
	case TaskStatusSkipped:
		c.Skipped++
	}
}

// ReuseCounts is a provenance histogram over reused tasks.
type ReuseCounts struct {
	LocalCache  int
	RemoteCache int
	Coalesced   int
}

// Cached is the number of ordinary cache hits, local and remote together — the
// figure every existing summary calls "cached".
func (c ReuseCounts) Cached() int { return c.LocalCache + c.RemoteCache }

// Total is the number of tasks whose result was reused.
func (c ReuseCounts) Total() int { return c.Cached() + c.Coalesced }

func (c *ReuseCounts) add(kind ReuseKind) {
	switch kind {
	case ReuseLocalCache:
		c.LocalCache++
	case ReuseRemoteCache:
		c.RemoteCache++
	case ReuseCoalesced:
		c.Coalesced++
	}
}

// TaskFailure is one failed task, with the diagnostics it reported.
type TaskFailure struct {
	Key         string
	Project     string
	Job         string
	Error       string
	Diagnostics []TaskDiagnostic
}

// SessionResult is the canonical outcome of a whole run.
//
// Status and Fresh are deliberately separate histograms over the same tasks:
//
//   - Status counts EVERY task by execution verdict, reuse included. It is what
//     decides whether the run succeeded — a reused failure is still a failure.
//   - Fresh counts only tasks that actually executed. Together with Reuse it
//     forms the six mutually-exclusive buckets every renderer reports, where a
//     cache hit is presented as "cached" instead of as "success".
//
// Keeping both is what lets one reduction serve loops that disagreed about
// whether a cached task counts as succeeded.
type SessionResult struct {
	// Tasks is every observed task, however it ended.
	Tasks int
	// Status is the verdict histogram over all tasks.
	Status TaskCounts
	// Fresh is the verdict histogram over tasks that executed (Reuse == none).
	Fresh TaskCounts
	// Reuse is the provenance histogram over tasks that did not execute.
	Reuse ReuseCounts

	// Aborted reports that a signal cut the run short; the counts then describe
	// only the part of the plan that got to run.
	Aborted   bool
	AbortedBy string

	// Executions is the run's PHYSICAL ledger: every subprocess the run spawned,
	// listed once, INCLUDING those no surviving task record references (a
	// superseded retry attempt, or a batch leader whose members all re-executed
	// solo). It is accumulated by the scheduler as executions finish and
	// attached to this reduction; nothing derives it from the task list, which
	// holds only each task's final attempt. Empty for a reduction built by a
	// consumer that did not run the plan (ReduceRun).
	Executions []Execution

	// Environment is the runner this run executed on, with every cumulative
	// counter windowed to the run itself. Like Executions it is ATTACHED by the
	// scheduler rather than reduced from results — nothing in a task result
	// knows what the machine's quota was — and it is nil for a reduction built
	// by a consumer that did not run the plan (ReduceRun).
	Environment *RunnerEnvironment

	// Preparation is the dependency-preparation stage decomposed into ownership
	// phases. It is ATTACHED like the two above, and for
	// a stronger reason: the stage runs before planning, so no task and no
	// execution in this reduction can account for it. Nil when the run prepared
	// no runtime, or when the composer recorded no attribution.
	Preparation *PreparationSummary

	// Duration is the run's wall time.
	Duration time.Duration
	// ExecutedDurationMs is the sum of per-task subprocess durations over
	// executed tasks, accumulated in milliseconds (truncating each task the way
	// the session-stats loops already do, so the total is bit-identical).
	ExecutedDurationMs int64

	Failures    []TaskFailure
	Diagnostics []TaskDiagnostic
	Artifacts   []TaskArtifact

	// Cache is the remote build cache summary for the run, folded in from
	// CacheStats unchanged. Nil for a local-only run.
	Cache *CacheStatsSnapshot
}

// Cached is the number of tasks served from a cache hit.
func (r *SessionResult) Cached() int {
	if r == nil {
		return 0
	}
	return r.Reuse.Cached()
}

// BucketTotal is the sum of the six mutually-exclusive presentation buckets.
// It equals Tasks unless a task carried a status outside the canonical
// vocabulary, which the bucket loops silently drop and Tasks does not.
func (r *SessionResult) BucketTotal() int {
	if r == nil {
		return 0
	}
	return r.Fresh.Total() + r.Reuse.Total()
}

// Success is the ONE success predicate — the unified strict rule.
//
// History: an earlier migration shipped two predicates reading this reduction —
// strict Success (exit code, recorded session, gates) and lenient
// SuccessIgnoringReuse (the v1 JSONL session:end, whose legacy tally skipped
// reused results before its failure branch). They disagreed on exactly one
// input, a reused failure. A later change made the recorded
// wire change that collapses them: every surface now reports the strict rule,
// and the lenient spelling is gone.

// Success reports whether the run reached a clean verdict. An aborted run never
// has one: its unfinished tasks were killed before they could produce it.
// Reused failures count, which is why this reads Status and not Fresh.
func (r *SessionResult) Success() bool {
	if r == nil {
		return false
	}
	return !r.Aborted && r.Status.Failed == 0
}

// SessionReducer folds the canonical session stream into a SessionResult.
//
// Observe takes a value and appends only for tasks that produced records, so a
// warm all-hit run allocates nothing beyond the reducer itself.
type SessionReducer struct {
	result SessionResult
}

// Observe folds one session event in.
func (r *SessionReducer) Observe(event SessionEvent) {
	switch event.Kind {
	case SessionEventAborted:
		r.result.Aborted = true
		if event.AbortedBy != "" {
			r.result.AbortedBy = event.AbortedBy
		}
	case SessionEventTaskEnd:
		r.observeTask(event.Task)
	}
}

func (r *SessionReducer) observeTask(task *TaskResult) {
	if task == nil {
		return
	}
	r.result.Tasks++
	r.result.Status.add(task.Status)
	if task.Reuse.Reused() {
		r.result.Reuse.add(task.Reuse)
	} else {
		r.result.Fresh.add(task.Status)
		r.result.ExecutedDurationMs += task.Timing.Duration.Milliseconds()
	}

	r.result.Diagnostics = append(r.result.Diagnostics, task.Diagnostics...)
	r.result.Artifacts = append(r.result.Artifacts, task.Artifacts...)

	// Failures are reported for every failed task except a COALESCED one whose
	// failure is its leader's: the leader in this run already carries it under
	// its own key, and listing both would report one subprocess twice. A
	// declared-output drift verdict is never the leader's — it is this
	// checkout's, reached when the entry (cache hit, lease-coalesced or remote)
	// was swapped in here (jobs/task_drift.go) — so it is listed whatever the
	// provenance, because nothing else in this run reports it.
	if task.Status != TaskStatusFailed {
		return
	}
	if task.Reuse == ReuseCoalesced && (task.Error == nil || task.Error.Code != extension.OutputDriftDiagnosticCode) {
		return
	}
	failure := TaskFailure{
		Key:         task.Key,
		Project:     task.Project,
		Job:         task.Job,
		Diagnostics: task.Diagnostics,
	}
	if task.Error != nil {
		failure.Error = task.Error.Message
	}
	r.result.Failures = append(r.result.Failures, failure)
}

// SetOutcome folds the session's own fate in.
func (r *SessionReducer) SetOutcome(outcome SessionOutcome) {
	if !outcome.Aborted {
		return
	}
	r.Observe(SessionEvent{Kind: SessionEventAborted, AbortedBy: outcome.AbortedBy})
}

// Result returns the reduction. The reducer stays usable afterwards.
func (r *SessionReducer) Result() *SessionResult {
	reduced := r.result
	return &reduced
}

// ScheduledTask pairs one canonical task result with the plan node that
// produced it. Job is nil for a result key the plan does not name.
type ScheduledTask struct {
	Job  *ScheduledJob
	Task TaskResult
}

// RunTasks projects a completed run onto the canonical task list, in the same
// order ReduceRun folds it: plan order first, then result keys the plan does
// not name, sorted.
//
// It exists for consumers that must NAME every task rather than only count it —
// the v2 machine surfaces carry a typed identity per task, which only
// the plan node holds. The list and the reduction come from one walk, so a
// count and the task list printed beside it cannot describe different task sets.
//
// It projects WITH the structured records (TaskResultOf), not the counting-only
// summary. Its consumers report a task, and a reported failure that dropped its
// diagnostics is a regression this would otherwise have shipped when it
// deleted the v1 MCP result — that answer listed each failure's diagnostics, and
// the v2 taskFailure/taskRecord members exist to carry exactly the same thing.
// The cost is nil where it would matter: a scheduler-produced result carries its
// canonical task already (JobResult.Canonical), so this is a copy, and the event
// walk only happens for results assembled by hand.
func RunTasks(planned []*ScheduledJob, results map[string]*JobResult) []ScheduledTask {
	return sessionTasks(planned, results, TaskResultOf)
}

// reduceSession folds a run's tasks and session outcome into a SessionResult.
//
// A finalizer's row is projected like any other task — it is listed, rendered
// and recorded — but it does NOT vote, so every surface that reduces a run
// agrees with the exit code. See votesOnTheVerdict.
func reduceSession(
	tasks []ScheduledTask,
	outcome SessionOutcome,
	duration time.Duration,
	cache *CacheStatsSnapshot,
) *SessionResult {
	var reducer SessionReducer
	for i := range tasks {
		if !votesOnTheVerdict(tasks[i].Job) {
			continue
		}
		reducer.Observe(TaskEndEvent(&tasks[i].Task))
	}
	reducer.SetOutcome(outcome)
	result := reducer.Result()
	result.Duration = duration
	result.Cache = cache
	return result
}

// votesOnTheVerdict reports whether a plan node's terminal row contributes to
// the run's counts and verdict.
//
// Every task votes except a `runOn: finally` FINALIZER, and that exception is
// the contract's: "a runOn: finally step failed … the invocation's outcome is
// unchanged" — a finalizer can neither rescue nor condemn the work it tears
// down. The row is NOT swallowed: it stays in SchedulerResult.Results with its
// `sensitive.finalizer_failed` code, it is rendered, it is written to
// events.jsonl, and RunTasks still names it. It simply does not count.
//
// The rule lives HERE, in the one reduction every surface folds through, rather
// than in a filtered result map the scheduler alone passed: the renderers'
// Finish and the machine projection re-reduce the RAW map, so a rule applied
// only at the scheduler's call site let a failed finalizer count Failed in the
// displayed and machine summaries while the exit code reported success — the
// cross-surface drift a dedicated test corpus polices.
//
// A nil plan node votes: a result key the plan does not name cannot be shown to
// be a finalizer, and dropping unattributable rows would understate the run.
func votesOnTheVerdict(job *ScheduledJob) bool {
	return !IsFinalizerJob(job)
}

// VotesOnTheVerdict is the exported spelling of that rule, for a PROJECTION
// that must describe exactly the task set this reduction counted rather than
// the whole task list — the report document's per-command partition and its
// bounded job list, whose accounting clauses tie both back to counts.total
// (internal/machine/report.go). Reading the rule here is what keeps the two
// from drifting: a projection that restated it would silently start counting a
// finalizer the moment the definition moved.
func VotesOnTheVerdict(job *ScheduledJob) bool {
	return votesOnTheVerdict(job)
}

// sessionTasks projects a scheduler run's results onto canonical tasks in a
// deterministic order: the plan's order first, then any result key the plan does
// not name, sorted. Result maps iterate randomly, so the order has to be imposed
// here for the reduction — and every list derived from it — to be reproducible.
//
// project builds one task; passing TaskSummaryOf skips the per-task event walk
// for consumers that only count.
func sessionTasks(
	planned []*ScheduledJob,
	results map[string]*JobResult,
	project func(*ScheduledJob, *JobResult) TaskResult,
) []ScheduledTask {
	tasks := make([]ScheduledTask, 0, len(results))
	seen := make(map[string]bool, len(planned))
	for _, job := range planned {
		key := job.Key()
		if seen[key] {
			continue
		}
		result, ok := results[key]
		if !ok || result == nil {
			continue
		}
		seen[key] = true
		tasks = append(tasks, ScheduledTask{Job: job, Task: project(job, result)})
	}

	var extra []string
	for key, result := range results {
		if !seen[key] && result != nil {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		task := project(nil, results[key])
		task.Key = key
		tasks = append(tasks, ScheduledTask{Task: task})
	}
	return tasks
}

// ReduceSchedulerSession builds the canonical session result for a completed
// scheduler run, records included. Scheduler.Run calls it exactly once and
// publishes the reduction as SchedulerResult.Session.
func ReduceSchedulerSession(
	planned []*ScheduledJob,
	results map[string]*JobResult,
	outcome SessionOutcome,
	duration time.Duration,
	cache *CacheStatsSnapshot,
) *SessionResult {
	return reduceSession(sessionTasks(planned, results, TaskResultOf), outcome, duration, cache)
}

// ReduceRun folds a completed run into the canonical SessionResult for the
// consumers that report COUNTS and FAILURE IDENTITY: every renderer's Finish
// (--output=json, --output=jsonl, text, cloud-logging), the session stats
// writer, and the exit-code derivation. An earlier migration pointed all of them
// here, deleting the tally loop each had grown.
//
// It deliberately does NOT extract the structured records (diagnostics,
// artifacts, metrics): doing so walks every task's event stream, the scheduler
// already did that once for SchedulerResult.Session, and no counting consumer
// reads them. Failures therefore carry identity and message but no diagnostics.
// A consumer that needs the records reads SchedulerResult.Session instead.
//
// planned may be nil. It only fixes the ORDER of the derived lists (plan order,
// then unplanned keys sorted); every count is order-independent.
func ReduceRun(
	planned []*ScheduledJob,
	results map[string]*JobResult,
	outcome SessionOutcome,
) *SessionResult {
	return reduceSession(sessionTasks(planned, results, TaskSummaryOf), outcome, 0, nil)
}
