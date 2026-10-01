package jobs

// This file re-exports the pure job/result data model that
// go.putnami.dev/cli/model/jobs owns. Every symbol below is declared in the
// model package and named here under its exact original name, so the planner,
// scheduler, executor, cache and remote code that stays in this package — and
// every CLI consumer of it — keeps a single spelling for one concept. It is the
// same shape internal/workspace and internal/extension use for their models.

import (
	model "go.putnami.dev/cli/model/jobs"
)

// --- Plan node ---

// ScheduledJob is a job ready for execution with resolved dependencies.
type ScheduledJob = model.ScheduledJob

// PlanMetrics summarizes the shape of a planned scheduling DAG.
type PlanMetrics = model.PlanMetrics

// ComputePlanMetrics derives metrics from a planned job list.
var ComputePlanMetrics = model.ComputePlanMetrics

// CanUseCache reports whether a planned job's task definition permits cache
// reuse.
var CanUseCache = model.CanUseCache

// ProducesVerificationReport reports whether a planned job writes its
// project's reserved feature verification report.
var ProducesVerificationReport = model.ProducesVerificationReport

// --- Typed identity ---

// TaskIdentityOfKey is the fallback for a result the plan does not name.
var TaskIdentityOfKey = model.TaskIdentityOfKey

// --- Legacy job result ---

// JobResult holds the outcome of a job execution.
type JobResult = model.JobResult

// JobError is the structured error from a failed job.
type JobError = model.JobError

// ReplayedFailure is the provenance of a failure served from the local failure
// cache instead of executed.
type ReplayedFailure = model.ReplayedFailure

// RawJobEvent is the CLI's rendering projection of a runtime protocol event.
type RawJobEvent = model.RawJobEvent

// BoundedJobEvents applies the strict JobResult event-retention budget.
var BoundedJobEvents = model.BoundedJobEvents

// EventHandler receives parsed JSONL events in real-time as they arrive.
type EventHandler = model.EventHandler

// Renderer is the interface for rendering job execution output.
type Renderer = model.Renderer

// SessionOutcome describes how the run ended, beyond the per-job results.
type SessionOutcome = model.SessionOutcome

// Re-exported outcome vocabulary.
const (
	JobOutcomeCached    = model.JobOutcomeCached
	JobOutcomeCoalesced = model.JobOutcomeCoalesced
)

// Abort sources reported in SessionOutcome.AbortedBy.
const (
	AbortUser   = model.AbortUser
	AbortSignal = model.AbortSignal
)

// Event type constants, sourced from the runtime event protocol.
const (
	EventTypeLog        = model.EventTypeLog
	EventTypeProgress   = model.EventTypeProgress
	EventTypeArtifact   = model.EventTypeArtifact
	EventTypeDiagnostic = model.EventTypeDiagnostic
	EventTypeMetric     = model.EventTypeMetric
	EventTypePhase      = model.EventTypePhase
	EventTypeSummary    = model.EventTypeSummary
	EventTypeResult     = model.EventTypeResult
	EventTypeMeta       = model.EventTypeMeta
	EventTypeReady      = model.EventTypeReady
)

// BatchResultsDataKey is the result-data key of a batch subprocess's
// per-member entries.
const BatchResultsDataKey = model.BatchResultsDataKey

// --- Canonical task result ---

// TaskStatus is the execution verdict of a single task.
type TaskStatus = model.TaskStatus

// ReuseKind classifies how a task's result was obtained without executing it.
type ReuseKind = model.ReuseKind

// Re-exported task verdicts and reuse provenance.
const (
	TaskStatusUnknown  = model.TaskStatusUnknown
	TaskStatusSuccess  = model.TaskStatusSuccess
	TaskStatusFailed   = model.TaskStatusFailed
	TaskStatusCanceled = model.TaskStatusCanceled
	TaskStatusSkipped  = model.TaskStatusSkipped

	ReuseNone        = model.ReuseNone
	ReuseLocalCache  = model.ReuseLocalCache
	ReuseRemoteCache = model.ReuseRemoteCache
	ReuseCoalesced   = model.ReuseCoalesced
)

// TaskTiming holds one task's measured durations.
type TaskTiming = model.TaskTiming

// TaskDiagnostic is one diagnostic attributed to a task.
type TaskDiagnostic = model.TaskDiagnostic

// TaskArtifact is one artifact a task produced.
type TaskArtifact = model.TaskArtifact

// TaskMetric is one metric a task reported.
type TaskMetric = model.TaskMetric

// Publication is the typed Docker publication fact an extension emits.
type Publication = model.Publication

// TaskResult is the canonical outcome of one scheduled task.
type TaskResult = model.TaskResult

// TaskResultOf projects a scheduled job and its legacy JobResult onto the
// canonical model.
var TaskResultOf = model.TaskResultOf

// ExpectsVerifiedImagePublication reports whether a job owns the established
// immutable image-publication result contract.
var ExpectsVerifiedImagePublication = model.ExpectsVerifiedImagePublication

// TaskSummaryOf projects a job and its result WITHOUT the structured records.
var TaskSummaryOf = model.TaskSummaryOf

// SessionEventKind discriminates the canonical session stream.
type SessionEventKind = model.SessionEventKind

// SessionEvent is one record of the canonical session stream.
type SessionEvent = model.SessionEvent

// Re-exported session event kinds.
const (
	SessionEventTaskEnd = model.SessionEventTaskEnd
	SessionEventAborted = model.SessionEventAborted
)

// TaskEndEvent wraps a task result as a session event.
var TaskEndEvent = model.TaskEndEvent

// SessionEventHandler receives records for session recording.
type SessionEventHandler = model.SessionEventHandler

// SessionRecord is one scheduler/session audit record.
type SessionRecord = model.SessionRecord

// SessionRecordJobEnd is the legacy persisted task terminal record type.
const SessionRecordJobEnd = model.SessionRecordJobEnd

// --- Session reduction ---

// TaskCounts is a status histogram.
type TaskCounts = model.TaskCounts

// ReuseCounts is a provenance histogram over reused tasks.
type ReuseCounts = model.ReuseCounts

// TaskFailure is one failed task, with the diagnostics it reported.
type TaskFailure = model.TaskFailure

// SessionResult is the canonical outcome of a whole run.
type SessionResult = model.SessionResult

// SessionReducer folds the canonical session stream into a SessionResult.
type SessionReducer = model.SessionReducer

// ScheduledTask pairs one canonical task result with the plan node that
// produced it.
type ScheduledTask = model.ScheduledTask

// RunTasks projects a completed run onto the canonical task list.
var RunTasks = model.RunTasks

// ReduceRun folds a completed run into the canonical SessionResult.
var ReduceRun = model.ReduceRun

// VotesOnTheVerdict is the exported spelling of the finalizer rule.
var VotesOnTheVerdict = model.VotesOnTheVerdict

// --- Measured cost ---

// Execution is ONE subprocess the CLI spawned.
type Execution = model.Execution

// CacheStatsSnapshot is the immutable, machine-readable view of a run's cache
// activity.
type CacheStatsSnapshot = model.CacheStatsSnapshot

// StoreBudgetStats is the in-run store budget's outcome in the cache snapshot.
type StoreBudgetStats = model.StoreBudgetStats

// CgroupThrottle is cgroup v2 CPU bandwidth accounting over the session window.
type CgroupThrottle = model.CgroupThrottle

// CgroupCPU is the cgroup CPU controller as this session saw it.
type CgroupCPU = model.CgroupCPU

// CPUPressure is PSI for CPU over the session window.
type CPUPressure = model.CPUPressure

// HostCPUTime is the /proc/stat aggregate over the session window.
type HostCPUTime = model.HostCPUTime

// CgroupMemoryLimit is a finite memory-controller limit and its provenance.
type CgroupMemoryLimit = model.CgroupMemoryLimit

// MemoryCapacity is the stable memory bound used for scheduler admission.
type MemoryCapacity = model.MemoryCapacity

// CgroupMemoryComposition is the complete closing cgroup v2 composition.
type CgroupMemoryComposition = model.CgroupMemoryComposition

// CgroupMemoryLifetimePeak is the cgroup-lifetime peak sampled at close.
type CgroupMemoryLifetimePeak = model.CgroupMemoryLifetimePeak

// CgroupMemoryClosing contains closing memory-controller gauges.
type CgroupMemoryClosing = model.CgroupMemoryClosing

// CgroupMemoryEvents is the complete windowed cgroup v2 event set.
type CgroupMemoryEvents = model.CgroupMemoryEvents

// CgroupMemory carries exact controller facts for one stable cgroup identity.
type CgroupMemory = model.CgroupMemory

// MemoryPressure is complete host- or cgroup-scoped memory PSI.
type MemoryPressure = model.MemoryPressure

const (
	MemoryCapacityPhysical    = model.MemoryCapacityPhysical
	MemoryCapacityCgroupLimit = model.MemoryCapacityCgroupLimit
	CgroupMemoryV1            = model.CgroupMemoryV1
	CgroupMemoryV2            = model.CgroupMemoryV2
	MemoryPressureHost        = model.MemoryPressureHost
	MemoryPressureCgroup      = model.MemoryPressureCgroup
)

// RunnerEnvironment is one session's view of the machine it ran on.
type RunnerEnvironment = model.RunnerEnvironment

// PreparationPhase is one OWNERSHIP CLASS of preparation work.
type PreparationPhase = model.PreparationPhase

// Re-exported preparation ownership classes.
const (
	PreparationNetwork      = model.PreparationNetwork
	PreparationResolution   = model.PreparationResolution
	PreparationVerification = model.PreparationVerification
	PreparationGeneration   = model.PreparationGeneration
	PreparationMutation     = model.PreparationMutation
)

// PreparationPhaseTotals is one ownership class's contribution to the stage.
type PreparationPhaseTotals = model.PreparationPhaseTotals

// PreparationSummary is the whole preparation stage, decomposed.
type PreparationSummary = model.PreparationSummary

// --- Lowercase spellings kept for this package's own call sites ---
//
// These moved with the model and are still used by the planning, scheduling,
// execution, capture and cache halves that stay here. Aliasing them under their
// original lowercase names keeps those call sites untouched.

type (
	eventMapper             = model.EventMapper
	taskResourceProfile     = model.TaskResourceProfile
	taskResourceObservation = model.TaskResourceObservation
)

var (
	applyTaskIdentity       = model.ApplyTaskIdentity
	attachIdentities        = model.AttachIdentities
	batchNumber             = model.BatchNumber
	dedupeSorted            = model.DedupeSorted
	eventField              = model.EventField
	eventFloat              = model.EventFloat
	extractResult           = model.ExtractResult
	isFinalizerJob          = model.IsFinalizerJob
	jobCommandAndStep       = model.JobCommandAndStep
	jobCommandName          = model.JobCommandName
	normalizeStatus         = model.NormalizeStatus
	parseRawEvent           = model.ParseRawEvent
	parseTaskStatus         = model.ParseTaskStatus
	readJSONLEvents         = model.ReadJSONLEvents
	readJSONLEventsObserved = model.ReadJSONLEventsObserved
	recordsFromEvents       = model.RecordsFromEvents
	reduceSchedulerSession  = model.ReduceSchedulerSession
	taskDeclarationOf       = model.TaskDeclarationOf
	usesDeclaredCapture     = model.UsesDeclaredCapture
)
