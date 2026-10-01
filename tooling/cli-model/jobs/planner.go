package jobs

import (
	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	protocoljob "go.putnami.dev/protocol/job"
)

// ScheduledJob is a job ready for execution with resolved dependencies.
type ScheduledJob struct {
	Project   *workspace.Project
	Extension *extension.ExtensionDescription
	JobDef    *extension.JobDefinition
	Step      *extension.PipelineStep // nil for non-pipeline jobs
	// SelectedProjects carries the resolved command selection for jobs that run
	// once for the workspace. Per-project jobs leave this nil so their execution
	// context and cache key stay scoped to the single project.
	SelectedProjects []*workspace.Project
	// Selection is HOW this run's projects were chosen — the mode, whether the
	// run is narrowed, the baseline `--impacted` resolved to, and the resolved
	// project ids. Unlike SelectedProjects it is stamped on EVERY node of the
	// plan, because a project-scoped task reporting on the workspace needs to
	// know the run was narrowed just as much as a workspace-scoped one does.
	//
	// It is run state, not task identity: nothing derived from it may reach a
	// cache key. A task's verdict is a function of the files it read, and
	// folding the selection in would give the same task a different key per
	// baseline — a permanent miss on every `--impacted` run.
	Selection *protocoljob.Selection
	// UserScope is set only when an interactive subcommand runs outside any
	// workspace from an extension pinned in the user scope. It carries the
	// directory the command was invoked from: the job runs there, and its
	// context reports it as userScope.callerDir.
	//
	// It is run state, not task identity: nothing derived from it may reach a
	// cache key or a session record.
	UserScope *protocoljob.UserScope
	// SessionPrerequisiteNoopGates records dependent-owned gate commands that
	// the planner proved have zero contributors across this prerequisite's
	// active selection. It is an execution-only authorization fact, stamped on
	// every job of the prerequisite command; it is not manifest input, task
	// identity, task context, or cache-key material.
	SessionPrerequisiteNoopGates []string
	DependsOn                    []string // functional dependency keys (data/correctness)
	// SerializeAfter holds write-serialization edges: predecessors this job must
	// not run concurrently with because their resource accesses conflict. These
	// order execution like DependsOn but are NOT functional dependencies — they
	// never contribute to cache keys and a serialize predecessor failing does not
	// skip this job.
	SerializeAfter []string
	// CPUBudget is the per-job CPU allowance the scheduler assigns when the job
	// starts executing, deterministically from this task's machine-local history
	// (cache hits never consume budget). It bounds internal tool parallelism
	// (e.g. GOMAXPROCS) and does not affect cache keys. Zero means "unset" — no
	// budget is applied.
	CPUBudget int
	// CPUBudgetHistoryEligible is true when this singleton's own recommendation
	// came from history, so CPUBudgetUnweightedCeiling may be retained. A fixed
	// compatible-class ceiling can inflate CPUBudget without changing that own
	// pre-weight signal. Cold fallbacks and multi-member batch grants remain
	// ineligible.
	CPUBudgetHistoryEligible bool
	// CPUBudgetUnweightedCeiling is the history-derived singleton ceiling before
	// configured and learned weights are applied. It is persisted only when
	// CPUBudgetHistoryEligible is true, keeping future grants responsive to a
	// changed weight while retaining known unweighted long-pole demand.
	CPUBudgetUnweightedCeiling int
	// CPUWeight is the configured relative multiplier applied to the task's
	// history-derived CPU demand, resolved from the project's putnami.json tasks
	// section (step name first, then command name) or the pipeline step's
	// cpuWeight. Zero means unconfigured (treated as 1).
	CPUWeight float64
	// LearnedCPUWeight boosts tasks whose historical CPU consumption is far
	// above same-named tasks of other projects, derived from .putnami/stats.
	// Values <= 1 mean no boost.
	LearnedCPUWeight float64
	// ExpectedWallMs is the job's historical wall-clock duration (EMA from past
	// runs), used to dispatch long-pole jobs first. Zero means unknown.
	ExpectedWallMs int64
	// ExpectedCPUWorkMs is the job's historical subprocess CPU time (EMA from
	// past runs). Together with ExpectedWallMs it records observed parallelism;
	// unlike wall time alone, it does not make a throttled run look cheap. Zero
	// means unknown.
	ExpectedCPUWorkMs float64
	// CriticalPathMs is ExpectedWallMs plus the longest expected-wall chain
	// among this job's transitive scheduling dependents, stamped once per plan.
	// Dispatch sorts on it so a cheap link that unblocks a long pole outranks
	// unrelated medium work; own duration alone would sort it last. Zero means
	// unknown (cold history), which preserves plain longest-first ordering.
	CriticalPathMs int64
	// HistoricalCPUCeiling is the largest successful history-backed singleton
	// ceiling recorded before configured or learned weight is applied. It is a
	// floor for the next deterministic grant so occupancy measured under a
	// smaller prior ceiling cannot starve a long pole without freezing a prior
	// weight into later runs. Zero means the history predates concurrency
	// recording or is absent; cold and batch grants are excluded.
	HistoricalCPUCeiling int
	// ResourceProfile is the bounded set of measured resource observations for
	// this task. Each observation retains the subprocess ceiling that produced
	// its CPU/wall/RSS measurements, so admission never compares a peak measured
	// at one concurrency with a recommendation for another as if they were the
	// same experiment. It is an execution hint only and never enters cache keys.
	ResourceProfile TaskResourceProfile
	// TimeoutOverrideMs, when non-nil, is this synthesized node's effective
	// scheduler deadline. Batch leaders use it for their group-wide budget so
	// they never mutate the JobDefinition shared by singleton members and retries.
	TimeoutOverrideMs *int

	// Identity is the typed identity Plan stamps once after the fixpoint
	// (AttachIdentities). Immutable from then on;
	// TypedIdentity derives the same value lazily for nodes assembled outside
	// Plan. Never write it after planning.
	Identity *protocolcli.TaskIdentity

	// SharedExecutionID names the plan-level shared node this job belongs to:
	// the set of content-identical nodes several commands scheduled over one
	// manifest task, which the scheduler runs ONCE physically (plan_shared.go).
	// Empty for every node with no peer.
	//
	// It is an EXECUTION grouping, not an identity: the members keep their own
	// keys, their own cache entries, their own dependents and their own session
	// rows. Nothing derived from it reaches a cache key.
	SharedExecutionID string

	// InvocationProducer is the plan node whose invocation-scoped output this
	// node consumes, for a member of a `finalizes` relation's consumer frontier.
	// It is stamped by resolveInvocationRelations and is
	// what lets a consumer's cache key fold in the PRODUCING ACTION's digest
	// instead of anything the producer wrote — a secret's content must never
	// reach a cache key.
	InvocationProducer *ScheduledJob
	// Invocation is the non-secret locator (id + private artifact root) for the
	// relation this node participates in. It is written ONCE, when the producer
	// starts, and is delivered only to that relation's producer, listed
	// consumers, and finalizer; every other node keeps it nil and receives
	// neither the context member nor the {invocationArtifactRoot} token.
	Invocation *protocoljob.Invocation
}

// EffectiveCPUWeight combines the configured weight with the learned boost.
// Both default to 1 when unset, so an untuned job has weight 1.
func (s *ScheduledJob) EffectiveCPUWeight() float64 {
	weight := s.CPUWeight
	if weight <= 0 {
		weight = 1
	}
	if s.LearnedCPUWeight > 1 {
		weight *= s.LearnedCPUWeight
	}
	return weight
}

// EffectiveTimeoutMs is the single resolver for a job's scheduler deadline.
// A batch leader has its own calculated deadline; otherwise a positive project
// tasks.<job>.timeoutMs wins by full step name then command name, and the
// extension manifest supplies the fallback. Manifest sentinels retain their
// meaning: zero asks the CLI to use its default and a negative value is
// unbounded.
func (s *ScheduledJob) EffectiveTimeoutMs() int {
	if s == nil {
		return 0
	}
	if s.TimeoutOverrideMs != nil {
		return *s.TimeoutOverrideMs
	}
	if timeoutMs, ok := resolveConfigTimeoutMs(s); ok {
		return timeoutMs
	}
	if s.JobDef == nil {
		return 0
	}
	return s.JobDef.TimeoutMs
}

// SchedulingPredecessors returns the deduplicated union of functional
// dependencies and write-serialization edges — every job that must complete
// before this one may run. The DAG scheduler and plan validator gate readiness
// on this set; cache-key and failure-skip logic use DependsOn alone.
func (s *ScheduledJob) SchedulingPredecessors() []string {
	if len(s.SerializeAfter) == 0 {
		return s.DependsOn
	}
	if len(s.DependsOn) == 0 {
		return s.SerializeAfter
	}
	merged := make([]string, 0, len(s.DependsOn)+len(s.SerializeAfter))
	merged = append(merged, s.DependsOn...)
	merged = append(merged, s.SerializeAfter...)
	return DedupeSorted(merged)
}

// Key returns the unique identifier for this scheduled job.
// Format: {projectID}:{jobName}
func (s *ScheduledJob) Key() string {
	return s.Project.ID + ":" + s.PlanName()
}

// PlanName returns the internal scheduler/DAG name for the job.
func (s *ScheduledJob) PlanName() string {
	if s.JobDef.InternalName != "" {
		return s.JobDef.InternalName
	}
	return s.JobDef.Name
}

// DisplayName returns the user-facing job name.
func (s *ScheduledJob) DisplayName() string {
	return s.JobDef.Name
}

// CommandName returns the root command this job belongs to.
func (s *ScheduledJob) CommandName() string {
	if s.JobDef.CommandName != "" {
		return s.JobDef.CommandName
	}
	command, _ := JobCommandAndStep(s.JobDef.Name)
	return command
}

// SharedExecution names the plan-level shared node this job belongs to, or ""
// when it has no content-identical peer in the plan. Every member of one shared
// node reports the same value, and exactly one of them spawns a subprocess.
func (s *ScheduledJob) SharedExecution() string {
	if s == nil {
		return ""
	}
	return s.SharedExecutionID
}

// StepID returns the pipeline step id, if this job is an expanded step.
func (s *ScheduledJob) StepID() string {
	if s.JobDef.StepID != "" {
		return s.JobDef.StepID
	}
	_, step := JobCommandAndStep(s.JobDef.Name)
	return step
}
