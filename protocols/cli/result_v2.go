package cli

// Machine result contract, version 2 — the ONLY machine contract, now that the
// version-1 emitters are deleted. See doc/02-result-v2.md.
//
// Version 1 had four machine surfaces and none of them said which contract it
// spoke: the --output=json envelope (Result), the --output=jsonl session stream
// (protocols/runtime's job:*/session:end envelopes), the MCP run_jobs/plan_jobs
// results, and the recorded session files. Version 2 stamps every one of them
// with ProtocolVersion, so a consumer reads the contract off the document
// instead of guessing from the CLI version — a document WITHOUT the field is
// version 1, and can now only come from a build published before B1b.
//
// This file is the contract's types. The non-job envelope's constructor and
// writer are in result_v2_envelope.go; the CLI's projections onto the other
// documents live in tooling/cli/internal/machine. What v2 fixes is recorded in
// doc/02-result-v2.md — including the v1→v2 migration — and machine-checked by
// ValidateDocument against the cross-language corpus in
// conformance/manifest.json:
//
//   - UNIFIED SUCCESS is strict. A run succeeds only when every selected task
//     succeeded, reuse included: a cache hit carries the verdict it stored, so
//     a reused success counts as a success and a reused failure fails the run.
//     v1's lenient accounting (jobs.SessionResult.SuccessIgnoringReuse, which
//     the JSONL session summary read) has no v2 spelling.
//   - ABORT WINS over failure, and the envelope agrees with the process. v1's
//     JSON envelope ordered failure first while the process exit code ordered
//     abort first; v2 reports "aborted" on every surface with exit code 130 and
//     keeps the failures visible in counts.failed and failures[], so nothing is
//     hidden by the precedence.
//   - IDENTITY IS TYPED. project/task/provider are structured values; the
//     string key stays as an explicitly DERIVED view of them (B2a).

// ResultProtocolVersion is the version stamped on every v2 machine document.
// A document without a protocolVersion field is version 1.
const ResultProtocolVersion = 2

// ResultV2SchemaID is the $id of schemas/result-v2.json, kept in lock-step with
// the types below by the drift test.
const ResultV2SchemaID = "https://putnami.dev/schemas/putnami-cli-result-v2.json"

// StatusAborted is the third ResultV2.Status value. It is v2-only: the v1
// envelope has two statuses and folds an interrupted run into "failure".
const StatusAborted = "aborted"

// Run outcomes. The precedence is aborted > failure > success.
const (
	RunOutcomeSuccess = "success"
	RunOutcomeFailure = "failure"
	RunOutcomeAborted = "aborted"
)

// Abort sources.
const (
	// AbortedByUser is an interactive Ctrl-C.
	AbortedByUser = "user"
	// AbortedBySignal is a SIGTERM from a supervisor.
	AbortedBySignal = "signal"
)

// Task execution verdicts. The spellings match the vocabulary already on the
// wire ("failed", not "failure") so v2 does not silently re-word v1 statuses.
const (
	TaskStatusSuccess  = "success"
	TaskStatusFailed   = "failed"
	TaskStatusCanceled = "canceled"
	TaskStatusSkipped  = "skipped"
)

// Task reuse provenance. Orthogonal to the verdict: reuse says how a result was
// obtained without executing, never whether it passed.
const (
	// TaskReuseNone means the task executed in this session. v2 spells it
	// explicitly instead of v1's empty string, so "did it run?" is answerable
	// without knowing which absence means what.
	TaskReuseNone        = "none"
	TaskReuseLocalCache  = "local-cache"
	TaskReuseRemoteCache = "remote-cache"
	TaskReuseCoalesced   = "coalesced"
)

// CPU allocation sources — where RunCPU.AllocatedMillicores came from. The two
// are not the same claim: a quota THROTTLES a run that exceeds it, while a core
// count is merely the point past which there is nothing left to run on.
const (
	// CPUAllocationCgroupQuota means a cgroup v2 cpu.max or cgroup v1
	// cpu.cfs_quota_us bandwidth limit was in force and bounds the run.
	CPUAllocationCgroupQuota = "cgroup-quota"
	// CPUAllocationLogicalCPUs means nothing bounded the run below the CPU count
	// visible to the process — no cgroup, a v2 cpu.max of "max", or a v1
	// cpu.cfs_quota_us of -1.
	CPUAllocationLogicalCPUs = "logical-cpus"
)

// Memory capacity provenance. Physical capacity is host RAM; cgroup sources
// are controller limits. Effective capacity names the smaller bound the
// scheduler already admits against, without changing that policy.
const (
	MemoryCapacityPhysical    = "physical"
	MemoryCapacityCgroupLimit = "cgroup-limit"
	CgroupMemoryV1            = "cgroup-v1"
	CgroupMemoryV2            = "cgroup-v2"
)

// Memory pressure scopes. Host PSI and cgroup PSI carry the same units but
// describe different populations and therefore are never interchangeable.
const (
	MemoryPressureHost   = "host"
	MemoryPressureCgroup = "cgroup"
)

// Task identity scopes.
const (
	// TaskScopeProject is a task that runs once per selected project.
	TaskScopeProject = "project"
	// TaskScopeWorkspace is a task that runs once for the whole workspace.
	TaskScopeWorkspace = "workspace"
)

// Session-stream record discriminators. v1 spelled these job:start, job:event
// and job:end; v2 uses the epic's task vocabulary. RecordTestCase is additive:
// a reader that does not know it skips the line (doc/02-result-v2.md).
const (
	RecordTaskStart  = "task:start"
	RecordTaskEvent  = "task:event"
	RecordTaskEnd    = "task:end"
	RecordTestCase   = "test:case"
	RecordPlanEnd    = "plan:end"
	RecordSessionEnd = "session:end"
)

// Machine-output modes. Normal suppresses debug-level task detail from the live
// stream; verbose admits it under a larger fixed budget. The complete sanitized
// sequence remains in the session artifact, and the mode never changes task
// selection, task verdicts, the run verdict or the process exit code.
const (
	MachineOutputModeNormal  = "normal"
	MachineOutputModeVerbose = "verbose"
)

// Machine-output sanitization and session-artifact vocabulary.
const (
	// MachineOutputSanitizationV1 names the deterministic sanitizer applied to
	// every string value and sensitive-member value before persistence,
	// measurement or live emission. Member names are retained. See
	// doc/04-machine-output.md.
	MachineOutputSanitizationV1 = "terminal-safe-redacted-v1"
	// MachineOutputArtifactPath is relative to the session directory named by
	// MachineOutputArtifact.SessionID.
	MachineOutputArtifactPath = "events.jsonl"
	// MachineOutputArtifactRetentionSession means the complete event stream is
	// retained and pruned with its owning session, never by the live-stream cap.
	MachineOutputArtifactRetentionSession = "session"
)

// Live machine-output budgets. MaxBytes measures the complete UTF-8 JSONL
// stream, including one LF after every compact record and the mandatory final
// session:end record. MaxRecords likewise includes session:end.
//
// The failure reserve is a hard partition within the total: ordinary records
// can never consume it, and unused failure capacity is not reclaimed. That
// keeps failure evidence admissible even after a noisy successful workload has
// exhausted the ordinary partition. The final reserve is also never reclaimed;
// it guarantees that the bounded terminal verdict and exact elision accounting
// can always close the stream.
const (
	MachineOutputNormalMaxBytes              = 1 << 20 // 1 MiB
	MachineOutputNormalMaxRecords            = 1024
	MachineOutputNormalFailureReserveBytes   = 256 << 10 // 256 KiB
	MachineOutputNormalFailureReserveRecords = 256

	MachineOutputVerboseMaxBytes              = 8 << 20 // 8 MiB
	MachineOutputVerboseMaxRecords            = 8192
	MachineOutputVerboseFailureReserveBytes   = 2 << 20 // 2 MiB
	MachineOutputVerboseFailureReserveRecords = 2048

	MachineOutputFinalReserveBytes   = 16 << 10 // 16 KiB
	MachineOutputFinalReserveRecords = 1
)

// MCP tools that return a v2 result document.
const (
	MCPToolRunJobs  = "run_jobs"
	MCPToolPlanJobs = "plan_jobs"
)

// DocumentKind names one of the v2 machine documents. It is the discriminator
// ValidateDocument takes, because the four surfaces write different documents
// to different places and a reader always knows which one it opened.
type DocumentKind string

// The v2 machine documents, one per surface (session files contribute three:
// the metadata document, the plan snapshot and the synthesis report; the session
// event log reuses the session-stream record).
const (
	// DocumentResultEnvelope is the --output=json document.
	DocumentResultEnvelope DocumentKind = "resultEnvelope"
	// DocumentSessionStreamRecord is one --output=jsonl line. Task/session
	// variants are recorded; the plan:end preview variant is live-only.
	DocumentSessionStreamRecord DocumentKind = "sessionStreamRecord"
	// DocumentMCPResult is an MCP run_jobs/plan_jobs result.
	DocumentMCPResult DocumentKind = "mcpResult"
	// DocumentSessionFile is the recorded session.json.
	DocumentSessionFile DocumentKind = "sessionFile"
	// DocumentSessionPlanFile is the recorded plan.json.
	DocumentSessionPlanFile DocumentKind = "sessionPlanFile"
	// DocumentReportFile is the recorded report.json — the run's BOUNDED
	// synthesis, contracted so a consumer picks it up instead of scraping the
	// session (doc/03-report.md).
	DocumentReportFile DocumentKind = "reportFile"
)

// Report origins — which surface drove the run a report describes. The two are
// not cosmetic: an MCP run is an agent's, and a consumer aggregating build
// economics over time has to keep agent traffic apart from a human's or CI's
// rather than average them together.
const (
	// ReportOriginCLI is a run driven by the command line.
	ReportOriginCLI = "cli"
	// ReportOriginMCP is a run driven by an MCP tool call.
	ReportOriginMCP = "mcp"
)

// Coverage granularities. Languages measure coverage differently (Go counts
// statements, TypeScript lines), so a percentage travels with the granularity it
// was measured at — aggregating percentages across granularities is the reader's
// decision to make knowingly, not one this contract hides.
const (
	CoverageStatements = "statements"
	CoverageLines      = "lines"
	CoverageFunctions  = "functions"
	CoverageBranches   = "branches"
)

// Report bounds. They are CONTRACT clauses, not producer preferences: the
// report is a bounded synthesis a consumer picks up WHOLE, so its worst case
// has to be stated rather than discovered in production. Each one is enforced by
// both validators and pinned against schemas/result-v2.json by the drift test,
// so the Go producer, the TypeScript mirror and the schema cannot disagree about
// what "bounded" means.
//
// The numbers are sized on measured sessions — 21 recorded lint,test,build runs
// of this workspace: 745 selected tasks per run, 121 diagnostic-bearing task
// records with a median of 26 diagnostics and a worst case of 139, and
// diagnostic messages of 31 bytes at p50, 108 at p99 and 8215 at the worst.
// Projecting those bounds over a real gate session yields a 26 KB report against
// a 539 KB session. See doc/03-report.md.
const (
	// ReportMaxJobs bounds ReportFile.Jobs. A gate run selects hundreds of tasks;
	// the report carries the most informative ReportMaxJobs of them and counts
	// every other selected task in ElidedJobs, so the total stays recoverable.
	ReportMaxJobs = 64
	// ReportMaxJobDiagnostics bounds ONE job's diagnostics. Past it the producer
	// truncates and states how many it dropped in ReportJob.TruncatedCount, while
	// ReportCommand.Errors and .Warnings keep the totals the list no longer holds.
	ReportMaxJobDiagnostics = 16
	// ReportMaxMessageBytes bounds one diagnostic message, in UTF-8 BYTES. A
	// producer truncates on a rune boundary so the message stays valid UTF-8; the
	// untruncated text remains in the session's own task record.
	ReportMaxMessageBytes = 1024
)

const (
	// TestFailureDetailsTruncatedCode is the cross-process accounting marker
	// emitted when a test producer cannot carry every causal failure diagnostic.
	// Producers and the report reducer share this spelling because the reducer
	// selects the marker ahead of ordinary diagnostics so the omission count
	// cannot itself be dropped.
	TestFailureDetailsTruncatedCode = "TEST_FAILURE_DETAILS_TRUNCATED"
	// BatchProjectLogContextKey attributes one runtime log event to one project
	// in a shared extension invocation. The CLI removes it after routing.
	BatchProjectLogContextKey = "batchProjectId"
	// BatchProjectLogsContextKey attributes one shared runtime log event to
	// several affected projects without making the producer emit it once per
	// project. The CLI removes it after routing.
	BatchProjectLogsContextKey = "batchProjectIds"
)

// ProjectIdentity identifies the owning project structurally.
type ProjectIdentity struct {
	// ID is the workspace project id ("/tooling/cli"). A workspace-scoped task
	// carries the workspace's own id.
	ID string `json:"id"`
	// Name is the declared project name ("@putnami/cli").
	Name string `json:"name"`
}

// TaskRef identifies the task within its project structurally.
type TaskRef struct {
	// Name is the canonical plan name — the scheduler/DAG name ("build~compile"),
	// never a display name.
	Name string `json:"name"`
	// Command is the root command the task belongs to ("build").
	Command string `json:"command"`
	// Step is the pipeline step id when the task is an expanded step.
	Step string `json:"step,omitempty"`
	// Kind is the task kind: the step's declared task when there is one,
	// otherwise the task name. It groups same-shaped work across projects.
	Kind string `json:"kind"`
}

// ProviderIdentity identifies the extension providing the task.
type ProviderIdentity struct {
	// Extension is the extension name ("@putnami/go").
	Extension string `json:"extension"`
	// Version is the resolved extension version when the surface knows it.
	Version string `json:"version,omitempty"`
}

// TaskIdentity is the immutable typed identity of one planned task — the
// identity diagnostics, events, session records and cache keys join on.
type TaskIdentity struct {
	// Key is a DERIVED view: exactly Project.ID + ":" + Task.Name. It exists so
	// v2 consumers keep one stable join string, and B2a keeps it byte-identical
	// to the v1 plan key. A key that disagrees with the structured fields is a
	// contract violation, not an alternative spelling.
	Key string `json:"key"`
	// Scope is TaskScopeProject or TaskScopeWorkspace.
	Scope string `json:"scope"`
	// Project is the owning project. A workspace-scoped task names the workspace.
	Project ProjectIdentity `json:"project"`
	// Task is the task within that project.
	Task TaskRef `json:"task"`
	// Provider is the extension that supplies the task. It is identity, not
	// provenance: it says who declared the work, not who executed this attempt.
	Provider ProviderIdentity `json:"provider"`
}

// DerivedKey returns the key the identity's structured fields imply.
func (i TaskIdentity) DerivedKey() string { return i.Project.ID + ":" + i.Task.Name }

// Diagnostic is one diagnostic attributed to a task.
type Diagnostic struct {
	// Severity is the diagnostic's level as the producing extension reported it.
	Severity string `json:"severity"`
	// Message is the human-facing text.
	Message string `json:"message"`
	// Code is the producer's stable identifier for this class of diagnostic,
	// when it has one.
	Code string `json:"code,omitempty"`
	// File is workspace-relative when the producer could make it so.
	File string `json:"file,omitempty"`
	// Line is 1-based; absent when the diagnostic is not anchored to a line.
	Line int `json:"line,omitempty"`
	// Column is 1-based and meaningful only alongside Line.
	Column int `json:"column,omitempty"`
}

// ExecutionRecord is one PHYSICAL execution — one subprocess the CLI spawned —
// and the resources that process tree actually consumed.
//
// It exists because the task list is LOGICAL and fans out: a batched dispatch
// runs n projects in ONE subprocess, so n task records describe work that was
// measured once. Summing those records double-counts cost (a measured run showed
// 796 logical task records against 57.1 de-duplicated physical task-minutes).
// A consumer that wants "what did this run actually cost the machine" sums
// THESE records, each of which appears exactly once; a consumer that wants
// per-project attribution keeps reading the task list. Reuse contributes no
// execution at all, which is the point: a cache hit spends nothing here.
//
// The list is the run's COMPLETE spawn ledger, so it also carries executions no
// task record references. A retried task names only its final attempt, and a
// batch whose split failed is superseded by every member re-executing solo —
// the superseded subprocess still burned wall, CPU and memory, and omitting it
// would under-state the machine's cost.
//
// Every member is measured, never derived. A platform that does not expose a
// counter omits it rather than reporting a zero that reads as "measured zero".
type ExecutionRecord struct {
	// ID is the execution's handle within this document. Task records reference
	// it; it is unique among this document's executions and is not meaningful
	// outside it.
	ID string `json:"id"`
	// WallMs is the subprocess's measured wall time, from immediately before
	// fork/exec until wait returned.
	WallMs int64 `json:"wallMs"`
	// UserCPUMs is user-mode CPU time of the process tree, waited children
	// included.
	UserCPUMs int64 `json:"userCpuMs"`
	// SystemCPUMs is kernel-mode CPU time of the same tree.
	SystemCPUMs int64 `json:"systemCpuMs"`
	// MaxRSSBytes is peak resident set size in BYTES, normalized by the producer
	// because the underlying rusage member's unit is platform-dependent (bytes on
	// darwin, kilobytes on linux). Absent where the platform reports no peak.
	MaxRSSBytes int64 `json:"maxRssBytes,omitempty"`
	// IOInBlocks is the rusage block-input operation count. Absent where the
	// platform does not account it (darwin reports 0 for most processes).
	IOInBlocks int64 `json:"ioInBlocks,omitempty"`
	// IOOutBlocks is the rusage block-output operation count, with the same
	// platform caveat as IOInBlocks.
	IOOutBlocks int64 `json:"ioOutBlocks,omitempty"`
	// Concurrency is the tool-native parallelism this execution was granted —
	// the CPU budget the CLI exported to the subprocess. Absent when the
	// scheduler granted none, so CPU time cannot be read against a bound that
	// was never stated.
	Concurrency int `json:"concurrency,omitempty"`
	// Tasks is how many logical task records in this document reference this
	// execution: 1 for an ordinary spawn, n for a batch, and 0 for an execution
	// every record superseded. It states the fan-out on the physical record
	// itself, so the double-count is visible without a join — and a 0 marks work
	// the machine paid for that no task claims.
	Tasks int `json:"tasks"`
}

// TaskRecord is one task's terminal result.
type TaskRecord struct {
	// Identity is the task this record is the terminal result of.
	Identity TaskIdentity `json:"identity"`
	// ExecutionID names the physical execution that produced this record, and
	// every logical record of a batch carries the SAME id. It is absent for a
	// task that spawned nothing in this run — a cache hit, a coalesced result, a
	// skipped task — which is exactly how a reader tells reuse from work.
	ExecutionID string `json:"executionId,omitempty"`
	// InputDigest is the cache key the scheduler computed for this task, spelled
	// `sha256:` followed by 64 lowercase hex characters. It is the SAME value the
	// cache is addressed by, so it moves exactly when the key does: two records
	// with one digest were keyed on the same inputs, and the reuse beside it says
	// what the run did with that. Absent on a skipped record, which never looked
	// its key up, and on a task that has no cache identity at all.
	InputDigest string `json:"inputDigest,omitempty"`
	// Status is the execution verdict, carried through reuse unchanged.
	Status string `json:"status"`
	// Reuse is the provenance: TaskReuseNone when the task executed.
	Reuse string `json:"reuse"`
	// ExitCode is the task process's exit code, or the reused result's recorded
	// code. It is 0 for a skipped task, which never ran.
	ExitCode int `json:"exitCode"`
	// DurationMs is the final attempt's wall time, or the reused result's
	// recorded duration.
	DurationMs int64 `json:"durationMs"`
	// TaskWallMs is the scheduler-level wall around the whole task.
	TaskWallMs int64 `json:"taskWallMs,omitempty"`
	// SpawnToFirstEventMs is absent when no subprocess event was observed.
	SpawnToFirstEventMs int64 `json:"spawnToFirstEventMs,omitempty"`
	// Error is present only when the task did not succeed.
	Error *ResultError `json:"error,omitempty"`
	// Diagnostics are the task's diagnostics. They are orthogonal to Status: a
	// succeeding task may still report warnings.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// TestCasesDropped counts the test cases this task ran that the stream
	// carries no test:case record for, because the producer or the CLI applied
	// TestCaseMaxPerTask or a byte budget, or could not describe the case.
	// Absent when every reported case has its record. It rides on every task
	// record: task:end and the session file's tasks.
	TestCasesDropped int `json:"testCasesDropped,omitempty"`
}

// TaskFailure is one failed task in a run summary. It carries the full typed
// identity — v1's JSONL summary repeated the plan key into both "package" and
// "job", which no consumer could take apart.
type TaskFailure struct {
	// Identity is the failed task, in the same typed form every other surface
	// uses, so a failure joins to its plan entry and its events.
	Identity TaskIdentity `json:"identity"`
	// Error is required here: a task listed as a failure always carries one.
	Error ResultError `json:"error"`
	// Diagnostics are the failed task's diagnostics, repeated from its
	// TaskRecord so a summary-only consumer need not read the task list.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// RunCounts is the verdict histogram over EVERY selected task, reuse included.
// There is no "cached" bucket: reuse is counted apart in RunReuse, and that
// separation is what removes v1's strict/lenient fork.
type RunCounts struct {
	// Total is every selected task, and equals Sum of the four buckets below.
	Total int `json:"total"`
	// Succeeded counts tasks whose verdict is TaskStatusSuccess, including
	// successes served from reuse.
	Succeeded int `json:"succeeded"`
	// Failed counts tasks whose verdict is TaskStatusFailed. A reused failure
	// counts here, which is what makes the run verdict strict.
	Failed int `json:"failed"`
	// Canceled counts tasks the run abandoned before they reached a verdict.
	Canceled int `json:"canceled"`
	// Skipped counts tasks the plan selected but never scheduled.
	Skipped int `json:"skipped"`
}

// Sum is the sum of the four verdict buckets, which must equal Total.
func (c RunCounts) Sum() int { return c.Succeeded + c.Failed + c.Canceled + c.Skipped }

// RunReuse is the provenance histogram over the same tasks RunCounts spans.
type RunReuse struct {
	// LocalCache counts results restored from this machine's action cache.
	LocalCache int `json:"localCache"`
	// RemoteCache counts results restored from the remote cache.
	RemoteCache int `json:"remoteCache"`
	// Coalesced counts tasks that adopted the result of an identical task
	// already executing in this same run.
	Coalesced int `json:"coalesced"`
}

// Sum is the number of tasks whose result was reused instead of executed.
func (r RunReuse) Sum() int { return r.LocalCache + r.RemoteCache + r.Coalesced }

// DockerPublishTimings is the measured, per-workload Docker publication
// breakdown. BuildMs is the package task's wall time; the remaining fields are
// measured by the Docker publisher around its registry operations.
type DockerPublishTimings struct {
	// BuildMs is the package task's wall time — image construction, not publication.
	BuildMs int64 `json:"buildMs"`
	// CacheLookupMs is time spent asking whether the content already exists.
	CacheLookupMs int64 `json:"cacheLookupMs"`
	// CacheTransferMs is time moving cached layers. It is 0 on a digest-travel
	// publish, where nothing had to move.
	CacheTransferMs int64 `json:"cacheTransferMs"`
	// RegistryPushMs is time uploading layers to the registry.
	RegistryPushMs int64 `json:"registryPushMs"`
	// ReferencePublishMs is time attaching tags to the published digest.
	ReferencePublishMs int64 `json:"referencePublishMs"`
	// DigestResolveMs is time resolving the final immutable image digest.
	DigestResolveMs int64 `json:"digestResolveMs"`
}

// DockerPublishConcurrency separates the configured scheduler cap from the
// maximum registry-publication concurrency actually observed during the run.
// ConfiguredCap is positive; Effective may be zero when all Docker publication
// work was satisfied without a registry-facing phase.
type DockerPublishConcurrency struct {
	// ConfiguredCap is the scheduler's ceiling on concurrent publications.
	ConfiguredCap int `json:"configuredCap"`
	// Effective is the peak concurrency actually reached. It never exceeds
	// ConfiguredCap, and is 0 when no registry-facing phase ran.
	Effective int `json:"effective"`
}

// DockerPublication is one selected workload's verified Docker publication.
// Identity names its project/task, Session is the publish session's resolved
// release identity, and ImageDigest is always a SHA-256 OCI digest — never a
// mutable tag. The record is additive and appears only for Docker publishes
// that could verify immutable provenance.
type DockerPublication struct {
	// Identity is the workload whose publication this record describes.
	Identity TaskIdentity `json:"identity"`
	// Session is the publish session's resolved release identity — the version
	// every artifact of this run shares.
	Session string `json:"session"`
	// Registry is the host the image was published to.
	Registry string `json:"registry"`
	// Image is the repository path within Registry, without a tag or digest.
	Image string `json:"image"`
	// ImmutableRef is the exact repository@digest reference verified by the
	// publisher. It must equal Image + "@" + ImageDigest.
	ImmutableRef string `json:"immutableRef"`
	// Tags are the mutable references attached to ImageDigest. They are
	// provenance-free by design: the digest is what identifies the content.
	Tags []string `json:"tags,omitempty"`
	// ImageDigest is always a SHA-256 OCI digest, never a mutable tag. It is the
	// field a consumer pins on.
	ImageDigest string `json:"imageDigest"`
	// ContentStatus says whether this run pushed content, retagged existing
	// content, or reused the exact immutable content without moving a tag.
	ContentStatus string `json:"contentStatus"`
	// CacheOutcome says how the publisher's own content lookup resolved.
	CacheOutcome string `json:"cacheOutcome"`
	// DigestReused reports a digest-travel publish: the content was already
	// published under this digest, so only references had to move.
	DigestReused bool `json:"digestReused"`
	// Timings is the measured phase breakdown.
	Timings DockerPublishTimings `json:"timings"`
	// Concurrency is the configured and observed publication parallelism.
	Concurrency DockerPublishConcurrency `json:"concurrency"`
}

// RunCPU is the run's CPU balance sheet: what the runner was ENTITLED to over
// this run's wall, against what the run actually burned.
//
// Actual is summed from the PHYSICAL execution ledger, so each subprocess
// contributes exactly once however many logical task records it produced. That
// is the whole reason it is stated here rather than left to a consumer: summing
// tasks[].durationMs multiplies a batch's cost by its fan-out, and the
// baseline (120.9 allocated vCPU-min against 80.7 actual) is only meaningful
// when the second number counts physical work once.
//
// The block is absent when the run spawned nothing, and absent when no runner
// environment was captured — allocation cannot be stated without knowing what
// the machine granted, and a guessed denominator is worse than none.
type RunCPU struct {
	// AllocatedMillicores is the CPU the runner may use, in thousandths of a
	// core: a cgroup quota of 800000us per 100000us period is 8000.
	AllocatedMillicores int `json:"allocatedMillicores"`
	// AllocatedSource says WHERE that number came from —
	// CPUAllocationCgroupQuota when a cgroup v1 or v2 bandwidth limit is in
	// force, CPUAllocationLogicalCPUs when nothing bounds the run below the
	// visible CPU count. The two are not interchangeable: a quota throttles, a
	// core count merely runs out.
	AllocatedSource string `json:"allocatedSource"`
	// AllocatedMs is AllocatedMillicores applied over DurationMs — the CPU-time
	// budget this run could have spent. It is stated against the SAME durationMs
	// this summary reports, so the two always divide.
	AllocatedMs int64 `json:"allocatedMs"`
	// ActualMs is the CPU time the run's subprocesses actually consumed, user
	// plus system, summed over the physical ledger with each execution counted
	// once. It excludes the CLI's own process, which the cgroup usage counter in
	// the environment block does include.
	ActualMs int64 `json:"actualMs"`
	// Executions is how many physical executions ActualMs was summed from, so a
	// reader can see the denominator of the average without joining the ledger.
	Executions int `json:"executions"`
}

// RunLocalCache attributes the cache-serving work performed by the CLI itself.
// ServedMs is the union of all measured local cache intervals, so concurrent
// tasks do not multiply elapsed time. Each segment of that union is assigned
// once, by stable priority (bindings, keys, restore-verify), making the phase
// walls additive apart from independent millisecond truncation.
type RunLocalCache struct {
	Hits             int64 `json:"hits"`
	Misses           int64 `json:"misses"`
	ServedMs         int64 `json:"servedMs"`
	KeysMs           int64 `json:"keysMs"`
	BindingsMs       int64 `json:"bindingsMs"`
	RestoreVerifyMs  int64 `json:"restoreVerifyMs"`
	SpawnedProcesses int64 `json:"spawnedProcesses"`
}

// RunCache is the typed cache attribution carried by every run surface. Local
// is present when core's local cache leg performed measurable work.
type RunCache struct {
	Local *RunLocalCache `json:"local,omitempty"`
}

// RunSummary is the canonical verdict of one run, identical on all four
// surfaces. Outcome is the single settled verdict: strict success, and abort
// ahead of failure. See doc/02-result-v2.md.
type RunSummary struct {
	// Outcome is RunOutcomeSuccess, RunOutcomeFailure or RunOutcomeAborted, with
	// abort taking precedence over failure.
	Outcome string `json:"outcome"`
	// AbortedBy is present exactly when Outcome is RunOutcomeAborted.
	AbortedBy string `json:"abortedBy,omitempty"`
	// ExitCode is the process exit code this run produces: 0 for success, 130
	// for aborted, any other non-zero code for failure.
	ExitCode int `json:"exitCode"`
	// Counts is the verdict histogram over every selected task.
	Counts RunCounts `json:"counts"`
	// Reuse is the provenance histogram over that same task set.
	Reuse RunReuse `json:"reuse"`
	// DurationMs is the run's wall time, which is less than the sum of the task
	// durations whenever tasks ran in parallel.
	DurationMs int64 `json:"durationMs"`
	// Failures, when present, lists every failed task: its length equals
	// Counts.Failed, so a reused failure cannot be counted and then omitted.
	Failures []TaskFailure `json:"failures,omitempty"`
	// Publications carries additive, per-workload Docker phase facts for
	// consumers such as a cloud deployment workspace. It is omitted when no
	// Docker workload produced a verified immutable digest.
	Publications []DockerPublication `json:"publications,omitempty"`
	// CPU is the run's actual-vs-allocated CPU balance, absent when the run
	// spawned nothing or the producer captured no runner environment.
	CPU *RunCPU `json:"cpu,omitempty"`
	// Cache attributes the cache-serving work that occurs outside task spans.
	// It is additive and absent when caching performed no measurable work.
	Cache *RunCache `json:"cache,omitempty"`
}

// StreamRunSummary is the bounded terminal verdict carried by session:end.
//
// RunSummary's failures and publications are intentionally absent: either list
// can grow without bound, so merely asking a producer to omit those optional
// members would not make the wire type bounded. Counts.Failed preserves the
// exact number of failures, failure-priority task records use their dedicated
// live reserve, and the complete sanitized detail remains in the session
// artifact named by MachineOutputSummary.Artifact.
type StreamRunSummary struct {
	Outcome    string    `json:"outcome"`
	AbortedBy  string    `json:"abortedBy,omitempty"`
	ExitCode   int       `json:"exitCode"`
	Counts     RunCounts `json:"counts"`
	Reuse      RunReuse  `json:"reuse"`
	DurationMs int64     `json:"durationMs"`
	CPU        *RunCPU   `json:"cpu,omitempty"`
}

// Succeeded reports the strict unified verdict: not aborted and nothing failed.
func (r RunSummary) Succeeded() bool {
	return r.Outcome != RunOutcomeAborted && r.Counts.Failed == 0
}

// PlanMetrics summarizes the shape of a plan.
type PlanMetrics struct {
	// Tasks is the number of planned tasks, and equals len(PlanSummary.Tasks).
	Tasks int `json:"tasks"`
	// Edges is the number of dependency edges across those tasks.
	Edges int `json:"edges"`
	// Projects is the number of distinct projects the plan spans.
	Projects int `json:"projects"`
	// ByCommand is the task count per root command ("build", "test"). Its values
	// sum to Tasks.
	ByCommand map[string]int `json:"byCommand,omitempty"`
}

// PlannedTask is one task in a plan. Edges reference other tasks by their
// derived identity key.
type PlannedTask struct {
	// Identity is the task, in the same typed form the run surfaces report.
	Identity TaskIdentity `json:"identity"`
	// DependsOn lists the identity keys this task consumes output from.
	DependsOn []string `json:"dependsOn,omitempty"`
	// After lists identity keys this task merely orders behind, without
	// consuming their output.
	After []string `json:"after,omitempty"`
	// Cache reports whether the task is cacheable, which is a property of the
	// plan — not a claim that this run will hit the cache.
	Cache bool `json:"cache"`
}

// PlanSummary is the plan a dry run or plan_jobs reports.
type PlanSummary struct {
	// DryRun reports that the plan was computed without executing it.
	DryRun bool `json:"dryRun"`
	// Metrics is the plan's shape.
	Metrics PlanMetrics `json:"metrics"`
	// Tasks is the plan in topological order.
	Tasks []PlannedTask `json:"tasks"`
}

// ResultV2 is the --output=json document. Status and ExitCode are the
// document's verdict and always agree with each other and with the process exit
// code; when Run is present its Outcome and ExitCode equal them.
type ResultV2 struct {
	// ProtocolVersion is ResultProtocolVersion. Its absence means version 1.
	ProtocolVersion int `json:"protocolVersion"`
	// Command is the invoked command the document reports on.
	Command string `json:"command"`
	// Status is StatusSuccess, StatusFailure or StatusAborted.
	Status string `json:"status"`
	// ExitCode is the process exit code, which always agrees with Status.
	ExitCode int `json:"exitCode"`
	// Data is the payload of commands that are not job runs. Job runs report
	// the typed Run member instead of stuffing a summary in here.
	Data any `json:"data,omitempty"`
	// Run is the typed run summary for job-running commands.
	Run *RunSummary `json:"run,omitempty"`
	// Plan is the typed plan for a successful plan-only preview. It is mutually
	// exclusive with Run, Data, and Error: no session executed when present.
	Plan *PlanSummary `json:"plan,omitempty"`
	// Error is present exactly when Status is not StatusSuccess. An aborted run
	// carries code "signal".
	Error *ResultError `json:"error,omitempty"`
}

// SessionStreamRecord is one --output=jsonl line. Task/session variants are
// also recorded session events; plan:end is a live-only preview terminal.
type SessionStreamRecord struct {
	// ProtocolVersion is ResultProtocolVersion, stamped on every record so a
	// reader can tell the contract from a single line.
	ProtocolVersion int `json:"protocolVersion"`
	// Record is RecordTaskStart, RecordTaskEvent, RecordTaskEnd, RecordTestCase,
	// RecordPlanEnd or RecordSessionEnd. RecordPlanEnd is a terminal preview
	// record, not a session event and never claims a run or session artifact.
	Record string `json:"record"`
	// Time is when the record was emitted, RFC 3339 with a timezone offset.
	Time string `json:"time"`
	// Identity is required on every task:* and test:case record and forbidden on
	// plan:end and session:end.
	Identity *TaskIdentity `json:"identity,omitempty"`
	// Event is the subprocess runtime event, versioned by protocols/runtime
	// (B0c). Required on task:event, forbidden otherwise.
	Event map[string]any `json:"event,omitempty"`
	// Task is required on task:end, forbidden otherwise.
	Task *TaskRecord `json:"task,omitempty"`
	// TestCase is required on test:case, forbidden otherwise.
	TestCase *TestCase `json:"testCase,omitempty"`
	// Run is required on session:end and forbidden otherwise. Legacy live
	// producers use the canonical RunSummary; the opt-in bounded profile uses
	// BoundedSessionEndRecord until producer adoption lands.
	Run *RunSummary `json:"run,omitempty"`
	// Plan is required on plan:end and forbidden otherwise.
	Plan *PlanSummary `json:"plan,omitempty"`
	// MachineOutput opts a session:end record into the bounded-stream profile.
	// It remains optional here so the contract slice can land before the
	// producer migration; ValidateSessionStream requires it.
	MachineOutput *MachineOutputSummary `json:"machineOutput,omitempty"`
}

// BoundedSessionEndRecord is the opt-in terminal wire type implemented by the
// bounded-stream producer. Keeping it distinct lets this contract land before
// the current SessionStreamRecord producer adopts elision.
type BoundedSessionEndRecord struct {
	ProtocolVersion int                  `json:"protocolVersion"`
	Record          string               `json:"record"`
	Time            string               `json:"time"`
	Run             StreamRunSummary     `json:"run"`
	MachineOutput   MachineOutputSummary `json:"machineOutput"`
}

// MachineOutputBudget is the self-described budget selected by Mode. All
// values are fixed contract constants; validators reject a producer-selected
// widening or narrowing.
type MachineOutputBudget struct {
	MaxBytes              int64 `json:"maxBytes"`
	MaxRecords            int   `json:"maxRecords"`
	FailureReserveBytes   int64 `json:"failureReserveBytes"`
	FailureReserveRecords int   `json:"failureReserveRecords"`
	FinalReserveBytes     int64 `json:"finalReserveBytes"`
	FinalReserveRecords   int   `json:"finalReserveRecords"`
}

// MachineOutputElision accounts for whole sanitized records omitted from the
// live stream and their compact UTF-8 JSON plus LF bytes.
type MachineOutputElision struct {
	Records int   `json:"records"`
	Bytes   int64 `json:"bytes"`
}

// MachineOutputElisions keeps ordinary detail separate from failure-priority
// evidence, so a consumer can see when a bounded stream omitted failures rather
// than inferring from one aggregate number.
type MachineOutputElisions struct {
	Ordinary MachineOutputElision `json:"ordinary"`
	Failure  MachineOutputElision `json:"failure"`
}

// MachineOutputArtifact names the complete sanitized stream retained beside
// session.json. Path is a bounded relative constant, never a producer-provided
// filesystem path.
type MachineOutputArtifact struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Retention string `json:"retention"`
}

// MachineOutputSummary is required on the opt-in bounded profile's final
// session:end record and optional on the legacy SessionStreamRecord shape.
type MachineOutputSummary struct {
	Mode         string                `json:"mode"`
	Sanitization string                `json:"sanitization"`
	Budget       MachineOutputBudget   `json:"budget"`
	Elided       MachineOutputElisions `json:"elided"`
	Artifact     MachineOutputArtifact `json:"artifact"`
}

// MachineOutputBudgetFor returns the one contract budget for mode.
func MachineOutputBudgetFor(mode string) (MachineOutputBudget, bool) {
	switch mode {
	case MachineOutputModeNormal:
		return MachineOutputBudget{
			MaxBytes:              MachineOutputNormalMaxBytes,
			MaxRecords:            MachineOutputNormalMaxRecords,
			FailureReserveBytes:   MachineOutputNormalFailureReserveBytes,
			FailureReserveRecords: MachineOutputNormalFailureReserveRecords,
			FinalReserveBytes:     MachineOutputFinalReserveBytes,
			FinalReserveRecords:   MachineOutputFinalReserveRecords,
		}, true
	case MachineOutputModeVerbose:
		return MachineOutputBudget{
			MaxBytes:              MachineOutputVerboseMaxBytes,
			MaxRecords:            MachineOutputVerboseMaxRecords,
			FailureReserveBytes:   MachineOutputVerboseFailureReserveBytes,
			FailureReserveRecords: MachineOutputVerboseFailureReserveRecords,
			FinalReserveBytes:     MachineOutputFinalReserveBytes,
			FinalReserveRecords:   MachineOutputFinalReserveRecords,
		}, true
	default:
		return MachineOutputBudget{}, false
	}
}

// MCPResult is the document an MCP tool call returns.
type MCPResult struct {
	// ProtocolVersion is ResultProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Tool is MCPToolRunJobs or MCPToolPlanJobs, and decides which of Run and
	// Plan is present.
	Tool string `json:"tool"`
	// Commands are the commands the tool call ran or planned.
	Commands []string `json:"commands"`
	// Run is present exactly for MCPToolRunJobs.
	Run *RunSummary `json:"run,omitempty"`
	// Plan is present exactly for MCPToolPlanJobs.
	Plan *PlanSummary `json:"plan,omitempty"`
}

// SessionGit captures the git state recorded with a session.
type SessionGit struct {
	// Branch is the checked-out branch, absent on a detached HEAD.
	Branch string `json:"branch,omitempty"`
	// Baseline is the ref selection was computed against, absent when the run
	// did not select by impact.
	Baseline string `json:"baseline,omitempty"`
}

// SessionTree identifies the exact worktree a session ran against, by content.
//
// It exists because "the same files are dirty" is not "the same bytes are on
// disk". SessionGit records a branch and a baseline, and ReportGit records a
// commit; none of them says anything about the uncommitted state, so a consumer
// that wants to know WHICH tree a recorded run measured has to go and compute a
// digest itself — against a worktree that may have moved since, or may not exist
// any more. The session states it instead, and every later reader compares
// strings.
//
// The digest is defined once, in doc/02-result-v2.md § Gated tree fingerprint,
// and computed once, by `putnami tree fingerprint`. Two agents comparing
// fingerprints produced by two implementations compare nothing, which is why the
// definition is part of this contract rather than a convention.
type SessionTree struct {
	// Fingerprint is the lowercase hex sha256 of the canonical byte stream:
	// HEAD, the bytes at every tracked path that differs from it — recursively,
	// through submodules — and the content of every untracked, non-ignored file.
	// Always 64 hex characters — the digest is sha256 whatever hash the
	// repository itself uses for objects.
	Fingerprint string `json:"fingerprint"`
	// Dirty reports whether that worktree differed from HeadSHA. It is a plain
	// bool, not the *bool ReportGit.Dirty uses, because the two carry their
	// "unknown" differently: a report exists whether or not its producer could
	// inspect the tree, so its Dirty needs an absent state, while this whole
	// object is written only when the fingerprint was computed — and computing
	// it IS the measurement that decides dirtiness. Absence of SessionTree is
	// the unknown; a present object always knows.
	Dirty bool `json:"dirty"`
	// HeadSHA is the full object id of the commit the worktree sat on, 40 hex
	// characters in a sha1 repository and 64 in a sha256 one. It is folded into
	// Fingerprint as well as reported here: a delta is only meaningful against
	// the commit it was taken from, so a moved HEAD must read as a different
	// tree instead of canceling out against a coincidentally identical diff.
	HeadSHA string `json:"headSHA"`
}

// SessionPlacement records requested and actual execution locations. It is
// observational metadata, not part of task identity or cache keys. Omit the
// whole object when placement is unknown, including records from older CLIs.
type SessionPlacement struct {
	// Requested is the caller's choice: local or remote.
	Requested string `json:"requested"`
	// Actual is where execution took place: local or remote. A remote request
	// may execute locally when no execution provider is available.
	Actual string `json:"actual"`
	// Provenance identifies the bound execution request a remote session ran
	// for. Present exactly when the recording engine executed a bound request
	// and absent otherwise — a local run, a fallback, an older producer — so a
	// reader never mistakes an absent block for a local one or a zero one.
	Provenance *SessionProvenance `json:"provenance,omitempty"`
}

// SessionProvenance is the identity of the bound execution request whose
// execution a session records, stated by the EXECUTING engine from the request
// it was handed. It lets the submitting side prove that an imported session is
// the one it submitted — the same source bytes, the same execution inputs, the
// same submission — without trusting anything the transport asserts about
// itself. It names no provider, no attempt and no machine: those are the
// transport's observations and live beside the session, never inside it. Like
// SessionTree, a present block always knows all three members; absence is the
// only unknown.
type SessionProvenance struct {
	// SourceDigest is the canonical source-manifest digest of the snapshot the
	// session executed, `sha256:` followed by 64 lowercase hex characters.
	SourceDigest string `json:"sourceDigest"`
	// InputDigest is the execution-input digest of the bound request: source,
	// invocation, frozen selection and pinned environment. It is applicability
	// identity, never a task cache key.
	InputDigest string `json:"inputDigest"`
	// Submission is the request's idempotency key, 32 lowercase hex characters:
	// the one submission this session answers, however many times the
	// submitting side had to resolve it.
	Submission string `json:"submission"`
}

// CgroupThrottle is cgroup v2 CPU bandwidth accounting, WINDOWED to the
// session: each member is the delta between the sample taken when the session
// started and the one taken when it ended, so it describes this session's own
// throttling rather than the runner's lifetime.
//
// It is a nested object rather than three optional members because a ZERO here
// is a measurement, and an important one: "the quota never throttled us" is the
// finding that separates contention from quota starvation. omitempty cannot
// carry that distinction, so presence of the object carries it instead — the
// object exists exactly when cpu.stat exposed the bandwidth counters.
type CgroupThrottle struct {
	// Periods is how many bandwidth periods elapsed during the session.
	Periods int64 `json:"periods"`
	// ThrottledPeriods is how many of those periods ended with the cgroup
	// suspended for running out of quota.
	ThrottledPeriods int64 `json:"throttledPeriods"`
	// ThrottledUs is the total time the cgroup spent suspended, in microseconds.
	ThrottledUs int64 `json:"throttledUs"`
}

// CgroupCPU is the cgroup v1 or v2 CPU controller as the session saw it.
// Absent entirely when neither v2 cpu.max nor v1 cpu.cfs_quota_us and
// cpu.cfs_period_us are readable.
type CgroupCPU struct {
	// PeriodUs is the bandwidth period from v2 cpu.max or v1
	// cpu.cfs_period_us, in microseconds.
	PeriodUs int64 `json:"periodUs"`
	// QuotaUs is the CPU time the cgroup may consume per period. It is ABSENT
	// when v2 cpu.max says "max" or v1 cpu.cfs_quota_us says -1: there is no
	// quota, and reporting one would invent a ceiling. A quota is always
	// positive, so omitempty is unambiguous here.
	QuotaUs int64 `json:"quotaUs,omitempty"`
	// UsageUs is the CPU time the WHOLE cgroup burned during the session, from
	// v2 cpu.stat's cumulative usage_usec or v1 cpuacct.usage converted from
	// nanoseconds, windowed as a delta. It is wider than RunCPU.ActualMs because
	// it includes the CLI itself and anything else sharing the cgroup; the two
	// together say how much of the cgroup's cost the run's subprocesses account
	// for. Absent when neither controller exposed usage.
	UsageUs int64 `json:"usageUs,omitempty"`
	// Throttle is the v2 session-windowed bandwidth accounting, absent on v1
	// and whenever cpu.stat exposed no bandwidth counters.
	Throttle *CgroupThrottle `json:"throttle,omitempty"`
}

// CPUPressure is Linux PSI for CPU (`/proc/pressure/cpu`), windowed to the
// session. It answers the shared-host half of the question: a runner whose own
// quota is never exceeded can still be slow because the HOST is oversubscribed.
//
// Only the `some` line is reported, and only its cumulative `total`, delta'd
// over the session. The avg10/avg60/avg300 columns are decaying averages over
// windows that are not this session's, so they cannot be reconciled with
// anything else in this document; the delta can, against
// SessionEnvironment.WindowMs.
type CPUPressure struct {
	// SomeStalledUs is the microseconds at least one task spent stalled on CPU
	// during the session window.
	SomeStalledUs int64 `json:"someStalledUs"`
}

// HostCPUTime is the aggregate CPU accounting from `/proc/stat`, windowed to
// the session. It is the hypervisor's view: steal is time the host gave to
// somebody else while this vCPU was runnable, and iowait is time it had nothing
// to run because a device had not answered.
//
// The counters are reported in USER_HZ TICKS, exactly as the kernel states
// them, because converting to milliseconds requires a USER_HZ this process
// cannot read. TotalTicks is the sum of every column over the same window, so
// the ratios — the only reading this block is for — are unit-free.
type HostCPUTime struct {
	// StealTicks is involuntary wait imposed by the hypervisor.
	StealTicks int64 `json:"stealTicks"`
	// IOWaitTicks is idle time attributed to outstanding IO.
	IOWaitTicks int64 `json:"ioWaitTicks"`
	// TotalTicks is every CPU-state column summed, the denominator for the two
	// above.
	TotalTicks int64 `json:"totalTicks"`
}

// CgroupMemoryLimit is a finite memory-controller limit and its provenance.
// An unlimited controller has no limit and is represented by an absent object.
type CgroupMemoryLimit struct {
	// Bytes is the finite limit in bytes.
	Bytes int64 `json:"bytes"`
	// Source identifies the controller layout that supplied the limit.
	Source string `json:"source"`
}

// MemoryCapacity states the stable memory bounds available to the session.
// PhysicalBytes and CgroupLimit are independently optional measurements;
// EffectiveBytes is their smaller available bound and EffectiveSource names
// which input supplied it. This block is observe-only and does not redefine
// scheduler admission.
type MemoryCapacity struct {
	// PhysicalBytes is host physical RAM, absent when it could not be measured.
	PhysicalBytes int64 `json:"physicalBytes,omitempty"`
	// CgroupLimit is the finite cgroup limit, absent for an unlimited or
	// unreadable controller.
	CgroupLimit *CgroupMemoryLimit `json:"cgroupLimit,omitempty"`
	// EffectiveBytes is the smaller available stable bound.
	EffectiveBytes int64 `json:"effectiveBytes"`
	// EffectiveSource is MemoryCapacityPhysical or MemoryCapacityCgroupLimit.
	EffectiveSource string `json:"effectiveSource"`
}

// CgroupMemoryComposition is the closing cgroup v2 memory.stat composition.
// FileBytes INCLUDES ShmemBytes under the kernel contract; the two MUST NOT be
// summed. The object is absent on cgroup v1, whose similarly named counters do
// not have these exact v2 semantics.
type CgroupMemoryComposition struct {
	// AnonBytes is anonymous memory charged at session close.
	AnonBytes int64 `json:"anonBytes"`
	// FileBytes is file-backed memory charged at session close and includes
	// ShmemBytes.
	FileBytes int64 `json:"fileBytes"`
	// ShmemBytes is shared-memory-backed content already included in FileBytes.
	ShmemBytes int64 `json:"shmemBytes"`
}

// CgroupMemoryLifetimePeak is memory.peak (v2) or
// memory.max_usage_in_bytes (v1), sampled at session close. It is a peak over
// the CGROUP LIFETIME, not over SessionEnvironment.WindowMs.
type CgroupMemoryLifetimePeak struct {
	// Bytes is the cgroup-lifetime peak sampled at session close.
	Bytes int64 `json:"bytes"`
}

// CgroupMemoryClosing is the memory controller's closing sample. CurrentBytes
// is required so a present block preserves a measured zero. Optional measured
// zeros live in nested objects for the same reason.
type CgroupMemoryClosing struct {
	// CurrentBytes is the total memory charged at session close.
	CurrentBytes int64 `json:"currentBytes"`
	// Composition is the optional closing cgroup v2 memory.stat subset.
	Composition *CgroupMemoryComposition `json:"composition,omitempty"`
	// LifetimePeak is the optional cgroup-lifetime peak sampled at close.
	LifetimePeak *CgroupMemoryLifetimePeak `json:"lifetimePeak,omitempty"`
}

// CgroupMemoryEvents is cgroup v2 memory.events, WINDOWED to the session. Each
// member is the closing cumulative count minus the opening one. Cgroup v1's
// failcnt has different semantics and is never relabelled as one of these.
type CgroupMemoryEvents struct {
	// Low is the session delta for memory.events low.
	Low int64 `json:"low"`
	// High is the session delta for memory.events high.
	High int64 `json:"high"`
	// Max is the session delta for memory.events max.
	Max int64 `json:"max"`
	// OOM is the session delta for memory.events oom.
	OOM int64 `json:"oom"`
	// OOMKill is the session delta for memory.events oom_kill.
	OOMKill int64 `json:"oomKill"`
}

// CgroupMemory is the memory controller the session was a member of. Closing
// values are gauges; Events are session-window deltas and are valid only for a
// cgroup v2 source.
type CgroupMemory struct {
	// Source identifies the cgroup v1 or v2 memory-controller layout.
	Source string `json:"source"`
	// Closing contains gauges sampled at session close.
	Closing CgroupMemoryClosing `json:"closing"`
	// Events contains complete cgroup v2 counter deltas over the session.
	Events *CgroupMemoryEvents `json:"events,omitempty"`
}

// MemoryPressure is Linux memory PSI, WINDOWED to the session. Scope is
// explicit because /proc/pressure/memory describes the host while a cgroup v2
// memory.pressure file describes only that cgroup.
type MemoryPressure struct {
	// Scope identifies whether PSI was read from the host or session cgroup.
	Scope string `json:"scope"`
	// SomeStalledUs is the windowed duration with at least one task stalled.
	SomeStalledUs int64 `json:"someStalledUs"`
	// FullStalledUs is the windowed duration with every non-idle task stalled.
	FullStalledUs int64 `json:"fullStalledUs"`
}

// SessionEnvironment is the RUNNER the session executed on, and how much of it
// the session actually got.
//
// It exists because a per-task duration is uninterpretable on its own. CI
// runners are CPU-quota-limited and share a host with neighbors, so a task that
// took twice as long may have hit its cgroup quota, may have been starved by a
// noisy neighbor, or may simply have had more work — three different problems
// with three different fixes, indistinguishable from timings alone.
//
// EVERY MEMBER IS MEASURED. A file the platform does not have yields an ABSENT
// member, never a zero: macOS has no cgroups and no PSI, and recording zeros
// there would silently claim "measured no throttling" for a machine that cannot
// throttle in the first place. The blocks whose zeros ARE meaningful are nested
// objects, so presence of the object is what says "this was read".
//
// Cumulative counters are reported as SESSION DELTAS, taken between a sample at
// session start and a sample at session end. Closing memory gauges are named
// separately and are not delta'd. WindowMs is the wall every delta is read
// against.
type SessionEnvironment struct {
	// OS is the Go GOOS of the recording CLI ("linux", "darwin").
	OS string `json:"os"`
	// Arch is the Go GOARCH ("amd64", "arm64"). Cross-architecture comparison of
	// task durations is meaningless without it.
	Arch string `json:"arch"`
	// LogicalCPUs is the CPU count visible to the process. It is what the
	// scheduler sized its worker pool from, and it is NOT the quota: a container
	// pinned to 2 cores still sees the host's 64.
	LogicalCPUs int `json:"logicalCpus"`
	// CPUModel is the processor's brand string, absent where the platform does
	// not publish one.
	CPUModel string `json:"cpuModel,omitempty"`
	// WindowMs is the wall time between the opening and closing samples — the
	// window every delta below covers.
	WindowMs int64 `json:"windowMs"`
	// CgroupCPU is the cgroup v1 or v2 CPU controller, absent where neither is
	// readable.
	CgroupCPU *CgroupCPU `json:"cgroupCpu,omitempty"`
	// CPUPressure is PSI for CPU, absent where the kernel exposes none.
	CPUPressure *CPUPressure `json:"cpuPressure,omitempty"`
	// HostCPU is the /proc/stat aggregate, absent where it is unreadable.
	HostCPU *HostCPUTime `json:"hostCpu,omitempty"`
	// MemoryCapacity is the stable physical/cgroup/effective capacity and its
	// provenance, absent when no safe capacity could be measured.
	MemoryCapacity *MemoryCapacity `json:"memoryCapacity,omitempty"`
	// CgroupMemory is the session cgroup's closing gauges and windowed events,
	// absent where no exact memory-controller facts are readable.
	CgroupMemory *CgroupMemory `json:"cgroupMemory,omitempty"`
	// MemoryPressure is host- or cgroup-scoped PSI over WindowMs.
	MemoryPressure *MemoryPressure `json:"memoryPressure,omitempty"`
}

// Preparation phase names — the CLOSED ownership vocabulary the
// dependency-preparation stage is decomposed into.
//
// A closed set is the point. "Preparation took 12 seconds" is not actionable;
// "9 of those 12 were generation and 2 were the mutation lock" names both the
// owner and the remedy. A step that fits none of these five is a step whose
// cost nobody can act on, so the answer is to classify it rather than to add a
// sixth bucket.
const (
	// PreparationPhaseNetwork is time talking to a remote: registry metadata,
	// artifact downloads, integrity fetches.
	PreparationPhaseNetwork = "network"
	// PreparationPhaseResolution is deciding WHAT is needed and under which
	// content identity — enumerating declared inputs and hashing them.
	PreparationPhaseResolution = "resolution"
	// PreparationPhaseVerification is proving that what is on disk is what was
	// promised: integrity, identity, ABI handshakes.
	PreparationPhaseVerification = "verification"
	// PreparationPhaseGeneration is running a declared prepare command — the
	// build that turns source into a usable artifact.
	PreparationPhaseGeneration = "generation"
	// PreparationPhaseMutation is the exclusive, ordered part: staging, waiting
	// for a content-addressed store's ownership lock, publishing, reclaiming.
	PreparationPhaseMutation = "mutation"
)

// PreparationPhaseRecord is one ownership class's contribution to the
// dependency-preparation stage.
//
// WallMs is a SUM OF SPANS, not an interval. When preparation.parallelism is
// above 1 the spans overlapped, so the phases can add up to more than
// preparation.wallMs — deliberately: the phase total is the WORK the class
// represents, and the gap between the phase sum and the stage wall is exactly
// what the parallelism bought. Only at parallelism 1 do the phases partition the
// stage, and the validator enforces that case.
type PreparationPhaseRecord struct {
	// Phase is the ownership class, one of the PreparationPhase* constants.
	Phase string `json:"phase"`
	// WallMs is the summed wall of this phase's spans.
	WallMs int64 `json:"wallMs"`
	// Steps is how many spans were summed, so a mean is derivable without
	// publishing one. Always at least 1: a phase with no span is absent.
	Steps int `json:"steps"`
	// CPUMs is MEASURED child CPU (user+system) for the spans of this phase that
	// spawned a subprocess. Absent for a phase that ran entirely in the CLI
	// process — an in-process phase reports no child CPU rather than a
	// fabricated one, so a reader can add cpuMs across phases and across
	// executions[] without counting anything twice.
	CPUMs int64 `json:"cpuMs,omitempty"`
}

// SessionPreparation is the dependency-preparation stage — resolving, fetching,
// building and publishing the artifacts a run needs BEFORE it can plan anything
// — decomposed into ownership phases.
//
// It is separate from executions[] on purpose, and the separation is a
// correctness property rather than a layout choice. An ExecutionRecord is one
// SUBPROCESS, joined to by TaskRecord.executionId and summed into
// run.cpu.actualMs. Most of preparation is in-process work (hashing input
// trees, copying files into an isolated staging view) that no subprocess
// performed and no task record could reference; filing it as an execution would
// put phantom spawns in the ledger and report CLI-process CPU as child CPU.
// Nothing here is in executions[], and nothing in executions[] is here.
type SessionPreparation struct {
	// WallMs is the stage's own wall — the sum of its top-level invocations,
	// which run sequentially within a session. It is what the critical path
	// actually paid, and the denominator every phase is read against.
	WallMs int64 `json:"wallMs"`
	// Parallelism is the widest independent-step fan-out the stage was permitted.
	// 1 means the phase walls partition WallMs; above 1 they may overlap and must
	// not be added up as elapsed time.
	Parallelism int `json:"parallelism"`
	// Phases are the classes the stage ENTERED, in the canonical order network,
	// resolution, verification, generation, mutation. A class the stage never
	// entered is absent, not zero — the same "measured, never derived" rule the
	// execution and environment blocks follow.
	Phases []PreparationPhaseRecord `json:"phases"`
}

// Selection modes: the closed vocabulary of how a run chose the projects it
// planned over.
//
// The three values repeat protocol/job's SelectionMode* constants by VALUE
// rather than by import: a recorded session must stay readable without dragging
// the job-context protocol in, exactly as protocol/job itself repeats the CLI's
// resolved selection type rather than importing it. The equality is pinned by a
// drift test in the CLI, which sees both.
const (
	// SessionSelectionModeAll is the unscoped whole-workspace projection.
	SessionSelectionModeAll = "all"
	// SessionSelectionModeImpacted is the `--impacted` projection.
	SessionSelectionModeImpacted = "impacted"
	// SessionSelectionModeProjects is an explicit selector, with or without
	// filters.
	SessionSelectionModeProjects = "projects"
)

// SessionSelectionModes is the closed selection-mode vocabulary in canonical
// (sorted) order.
var SessionSelectionModes = []string{
	SessionSelectionModeAll,
	SessionSelectionModeImpacted,
	SessionSelectionModeProjects,
}

// SessionSelection is how the run chose the projects it planned over.
//
// It exists because the recorded git block cannot answer the question a ledger
// asks. `git.baseline` is present only when the user named a baseline, and the
// selected project ids alone cannot tell an explicit `--projects a,b` from an
// `--impacted` run that happened to resolve to the same two projects — yet the
// two cost and mean entirely different things. Mode states it directly.
//
// The block is optional: every session recorded before this member existed
// carries none, and a producer that cannot state the mode omits the whole
// object rather than guessing one.
type SessionSelection struct {
	// Mode is how the projection was chosen: one of SessionSelectionModes.
	Mode string `json:"mode"`
	// Scoped distinguishes a narrowed run from the whole-workspace default. It
	// is the member a consumer acts on: a session that claims to have covered
	// the workspace may only do so when Scoped is false.
	Scoped bool `json:"scoped"`
	// Projects are the selected projects' canonical ids, sorted. Absent when the
	// run selected nothing — an `--impacted` run over an unchanged tree is the
	// ordinary case, and it is a success, not a failure to find work.
	Projects []string `json:"projects,omitempty"`
	// ReleaseSetProjects are the canonical ids, sorted, of the projects whose
	// publish and package steps this session's release-set plan owned. Absent
	// when the session coordinated no release set, and when its plan selected
	// no member. Always a subset of Projects.
	//
	// A session that names publish beside other commands does two things at
	// once: it publishes what the channel head decided, and it verifies what
	// the caller's own selection asked for. Projects is the union — what ran —
	// so this member is what lets a consumer attribute each half.
	ReleaseSetProjects []string `json:"releaseSetProjects,omitempty"`
}

// SessionFile is the recorded session metadata document (session.json).
type SessionFile struct {
	// ProtocolVersion is ResultProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionID is the recorded session's directory name, and the handle
	// `putnami sessions` commands take.
	SessionID string `json:"sessionId"`
	// ParentSessionID names the session of the run that SPAWNED this one, and is
	// absent for a top-level run.
	//
	// A task may invoke the CLI again (a validation guard that builds what it
	// checks is the canonical case), and the nested run records its own session
	// beside its parent's. Without this member the only discriminator is time
	// containment, which is not one: concurrent worktrees share a store root
	// through symlinks, and two ordinary runs in one worktree overlap. An
	// accounting consumer that counts a nested run as a gate double-counts its
	// CPU, because the parent already charges the spawning task's subprocess
	// tree.
	//
	// The producer learns it from the run-level process environment its parent
	// exported into the task subprocess — an execution-only channel that reaches
	// no cache key, no run marker, and no task parameter.
	ParentSessionID string `json:"parentSessionId,omitempty"`
	// StartTime is when the session began, RFC 3339 with a timezone offset.
	StartTime string `json:"startTime"`
	// EndTime is absent while the session is still running.
	EndTime string `json:"endTime,omitempty"`
	// Commands are the commands the session ran.
	Commands []string `json:"commands"`
	// Selection is how the run chose the projects it planned over, absent when
	// the producer recorded none.
	Selection *SessionSelection `json:"selection,omitempty"`
	// Git is the recorded repository state, absent outside a git worktree.
	Git *SessionGit `json:"git,omitempty"`
	// Tree identifies the worktree this session ran against by content, as it
	// stood when the session opened. Absent when the producer could not compute
	// it — outside a git worktree, in a repository with no commit, or when git
	// failed — because a session that cannot say which tree it measured says
	// nothing rather than claiming a clean one.
	Tree *SessionTree `json:"tree,omitempty"`
	// Placement distinguishes the requested location from actual execution.
	// Absent when the producer did not record placement.
	Placement *SessionPlacement `json:"placement,omitempty"`
	// Run is the session's verdict, identical to the one the other surfaces
	// reported for the same run.
	Run RunSummary `json:"run"`
	// Tasks are the terminal records for every selected task.
	Tasks []TaskRecord `json:"tasks,omitempty"`
	// Executions is the session's complete physical spawn ledger, each execution
	// listed once however many task records it produced — including those every
	// record superseded. Absent when nothing executed.
	Executions []ExecutionRecord `json:"executions,omitempty"`
	// Environment is the runner this session ran on and how much of it the
	// session got. Absent when the producer captured none.
	Environment *SessionEnvironment `json:"environment,omitempty"`
	// Preparation is the dependency-preparation stage decomposed into ownership
	// phases. Absent when the session prepared nothing.
	Preparation *SessionPreparation `json:"preparation,omitempty"`
	// Scheduler is the scheduler tuning report; its shape is the scheduler's.
	Scheduler any `json:"scheduler,omitempty"`
	// Cache is the detailed local/remote end-of-run summary; its shape is owned
	// by the CLI cache implementation. Portable consumers use Run.Cache.
	Cache any `json:"cache,omitempty"`
}

// SessionPlanFile is the recorded plan snapshot document (plan.json).
type SessionPlanFile struct {
	// ProtocolVersion is ResultProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionID is the session this plan belongs to; it matches the sibling
	// SessionFile's.
	SessionID string `json:"sessionId"`
	// Commands are the commands the plan was computed for.
	Commands []string `json:"commands"`
	// Tasks is the plan as it stood when the session started, so a later reader
	// can tell what was selected from what actually ran.
	Tasks []PlannedTask `json:"tasks"`
}

// ReportGit is the repository state a report was produced against.
//
// It is report-owned rather than a reuse of SessionGit: the session's git block
// records what SELECTION was computed from (a branch and a baseline), while a
// report is a durable fact a consumer files against a commit, so Sha is required
// here and optional-by-absence there. Widening SessionGit instead would have
// changed a document that is already on disk in every recorded session.
type ReportGit struct {
	// Branch is the checked-out branch, absent on a detached HEAD.
	Branch string `json:"branch,omitempty"`
	// Sha is the full object id of the commit the run was produced at — 40 hex
	// characters for a sha1 repository, 64 for a sha256 one. It is required
	// because it is the join key a longitudinal consumer files the report under;
	// an abbreviation or a symbolic name is a contract violation, not a shorthand.
	Sha string `json:"sha"`
	// Dirty reports uncommitted changes in the worktree. Absent when the producer
	// did not determine it — a report that cannot say whether the tree matched
	// Sha says nothing rather than claiming a clean tree.
	Dirty *bool `json:"dirty,omitempty"`
	// Baseline is the ref selection was computed against, absent when the run did
	// not select by impact.
	Baseline string `json:"baseline,omitempty"`
}

// ReportRun is the run's verdict as the report states it.
//
// It is deliberately NOT RunSummary. RunSummary carries failures[], whose length
// equals counts.failed and whose members each embed a full typed identity, an
// error and that task's diagnostics — an UNBOUNDED member, and the report's
// whole premise is that its worst case is stated up front. The failed tasks are
// not lost: they sort first into Jobs by the selection priority, so a report
// that elides anything elides successes before failures.
type ReportRun struct {
	// Outcome is RunOutcomeSuccess, RunOutcomeFailure or RunOutcomeAborted, with
	// the same precedence every other v2 surface reports.
	Outcome string `json:"outcome"`
	// ExitCode is the process exit code the run produced, agreeing with Outcome
	// exactly as it does on the other surfaces.
	ExitCode int `json:"exitCode"`
	// Counts is the verdict histogram over every selected task, reuse included.
	Counts RunCounts `json:"counts"`
	// Reuse is the provenance histogram over that same task set.
	Reuse RunReuse `json:"reuse"`
	// DurationMs is the run's wall time.
	DurationMs int64 `json:"durationMs"`
	// CPU is the run's actual-vs-allocated CPU balance, absent when the run
	// spawned nothing or captured no runner environment. It is the same shape the
	// session file reports, so the two are comparable without conversion.
	CPU *RunCPU `json:"cpu,omitempty"`
}

// ReportTests is one command's test outcome, mirroring protocols/runtime's
// TestSummary so a report member and the event payload it was aggregated from
// carry the same counters and omission accounting.
type ReportTests struct {
	// Total is every test the command ran, and equals Passed + Failed + Skipped.
	Total int `json:"total"`
	// Passed counts tests that passed.
	Passed int `json:"passed"`
	// Failed counts tests that failed.
	Failed int `json:"failed"`
	// Skipped counts tests that were not run.
	Skipped int `json:"skipped"`
	// FailureDetailsTruncated counts causal failure diagnostics the command's
	// producers omitted before the report reducer saw them. It is additive and
	// absent when zero so older report documents and readers remain valid.
	FailureDetailsTruncated int `json:"failureDetailsTruncated,omitempty"`
}

// ReportCoverage is a coverage measurement, mirroring protocols/runtime's
// CoverageSummary. It is present only where coverage was actually collected: the
// report DISPLAYS what the run measured and never forces instrumentation, so an
// uninstrumented command omits the block rather than reporting 0%.
type ReportCoverage struct {
	// Percentage is the covered share in [0,100] at the declared granularity.
	Percentage float64 `json:"percentage"`
	// Granularity is what the percentage counts — statements, lines, functions or
	// branches. It travels with the number because languages measure differently
	// and averaging across granularities is the reader's decision to make
	// knowingly.
	Granularity string `json:"granularity"`
	// Covered is the covered unit count at that granularity, when the producer
	// reported one.
	Covered int `json:"covered,omitempty"`
	// Total is the measured unit count at that granularity, when the producer
	// reported one.
	Total int `json:"total,omitempty"`
	// Enforced reports whether a threshold was in force for this measurement —
	// the difference between "coverage was observed" and "coverage could have
	// failed the run". A run without --enforce-coverage still measures.
	Enforced bool `json:"enforced"`
}

// ReportCommand is one root command's synthesis.
//
// The vocabulary is COMMANDS, not phases. A task names the root command it
// belongs to (TaskRef.Command), so this aggregation is a projection of facts the
// run already carries, and "phase" is taken in this contract by
// SessionPreparation's ownership classes, which decompose something else
// entirely.
type ReportCommand struct {
	// Command is the root command ("build", "test", "lint"). Each command appears
	// at most once: this list is a histogram over commands, not a per-task log.
	Command string `json:"command"`
	// Counts is the verdict histogram over this command's selected tasks. The
	// per-command totals sum to the run's own total, because every selected task
	// belongs to exactly one root command.
	Counts RunCounts `json:"counts"`
	// Reuse is the provenance histogram over that same task set.
	Reuse RunReuse `json:"reuse"`
	// FreshWallMs is the summed wall time of this command's tasks that actually
	// EXECUTED. Reused tasks contribute nothing: a cache hit spends no wall, and
	// counting the duration it replayed would make a fully cached command look as
	// expensive as the run that populated the cache.
	FreshWallMs int64 `json:"freshWallMs"`
	// CPUMs is the summed CPU of this command's fresh tasks — each execution's
	// measured CPU divided among the task records that reference it, so a batched
	// dispatch is not multiplied by its fan-out. Summed over commands it never
	// exceeds run.cpu.actualMs: superseded attempts belong to the run's ledger and
	// to no command. Absent when no execution of this command was measured.
	CPUMs int64 `json:"cpuMs,omitempty"`
	// Tests is the command's test outcome and omission accounting, absent when it
	// carries neither executed-test counts nor omitted failure details.
	Tests *ReportTests `json:"tests,omitempty"`
	// Coverage is the command's coverage measurement, absent when none was
	// collected.
	Coverage *ReportCoverage `json:"coverage,omitempty"`
	// Errors counts every real reported error-severity diagnostic, including
	// those the bounded job lists dropped, plus producer-side failure details
	// omitted before reduction. The synthetic accounting marker is not an error.
	Errors int `json:"errors"`
	// Warnings is the same count for warning severity.
	Warnings int `json:"warnings"`
}

// ReportJob is one selected task's line in the report.
//
// Identity is FLATTENED to the three strings a consumer joins on — the derived
// key, the project id and the task name — plus the root command that ties the
// job to its ReportCommand. The full TaskIdentity (display name, provider,
// kind, step) stays in the session: repeating it 64 times would spend the
// report's whole budget on identity a reader can look up.
type ReportJob struct {
	// Key is the identity key every other v2 surface joins on, exactly
	// Project + ":" + Task. A key that disagrees with the two is a violation, not
	// an alternative spelling — the same rule TaskIdentity carries.
	Key string `json:"key"`
	// Project is the workspace project id ("/tooling/cli").
	Project string `json:"project"`
	// Task is the canonical plan name ("build~compile").
	Task string `json:"task"`
	// Command is the root command the task belongs to, so a job joins to its
	// command's synthesis without re-deriving it from the task name.
	Command string `json:"command"`
	// Outcome is the task verdict, carried through reuse unchanged: a reused
	// failure is a failure here too.
	Outcome string `json:"outcome"`
	// Reuse is the provenance: TaskReuseNone when the task executed.
	Reuse string `json:"reuse"`
	// DurationMs is the task's wall time, or the reused result's recorded one.
	DurationMs int64 `json:"durationMs"`
	// CPUMs is this task's share of its execution's measured CPU. Absent for a
	// task that spawned nothing, and absent where the platform measured nothing —
	// never a zero, which would read as "ran and cost nothing".
	CPUMs int64 `json:"cpuMs,omitempty"`
	// Coverage is this task's own coverage measurement, absent when it collected
	// none.
	Coverage *ReportCoverage `json:"coverage,omitempty"`
	// Diagnostics are the task's diagnostics, at most ReportMaxJobDiagnostics of
	// them and each message at most ReportMaxMessageBytes. They are orthogonal to
	// Outcome: a succeeding task may still report warnings.
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// FailureDetailsTruncated is the producer-side part of TruncatedCount. Keeping
	// it on the job lets validators distinguish legitimate early omission from a
	// report reducer that discarded diagnostics while it still had room.
	FailureDetailsTruncated int `json:"failureDetailsTruncated,omitempty"`
	// TruncatedCount is how many diagnostics were omitted before or during the
	// report reduction. It includes FailureDetailsTruncated plus diagnostics the
	// report's own bound dropped. Any report-side remainder requires a full list;
	// absence means the displayed diagnostics are complete.
	TruncatedCount int `json:"truncatedCount,omitempty"`
}

// ReportCache is the run's remote-cache economics: a TYPED subset of the
// scheduler's own cache snapshot, chosen so the report answers "did the cache
// pay off" without inheriting an open shape. The session file's free-form
// `cache: any` is exactly what a contracted document must not repeat — a
// consumer binding to it is binding to whatever the scheduler happened to
// serialize that release.
//
// The block is absent when no remote cache participated in the run. That is the
// absent-is-not-zero rule again: an all-zero block would claim "the cache was
// asked and answered nothing", which is a different fact from "there was no
// cache".
type ReportCache struct {
	// Hits is how many negotiated keys the remote cache held.
	Hits int64 `json:"hits"`
	// Misses is how many it did not.
	Misses int64 `json:"misses"`
	// Restored is how many hits were actually materialized into the local store.
	// It never exceeds Hits: a key cannot be restored without having been hit.
	Restored int64 `json:"restored"`
	// Uploads is how many freshly built results were stored remotely.
	Uploads int64 `json:"uploads"`
	// TimeSavedMs is the build time the restored results avoided — the recorded
	// duration of the work that did not have to run again.
	TimeSavedMs int64 `json:"timeSavedMs"`
	// BytesFetched is what the run downloaded from the cache.
	BytesFetched int64 `json:"bytesFetched"`
	// BytesUploaded is what it pushed back. Byte traffic is stated ONCE, at run
	// level: a per-command split would have to attribute shared blobs, and any
	// split of a deduplicated transfer is a fiction.
	BytesUploaded int64 `json:"bytesUploaded"`
}

// ReportScheduler is the two scheduler facts a budget consumer reads out of the
// session's free-form scheduler report. Absent when the run recorded no
// scheduler report.
type ReportScheduler struct {
	// Parallelism is the worker count the scheduler chose for this run. Without
	// it a wall time cannot be read against the work it covered.
	Parallelism int `json:"parallelism"`
	// CriticalPathMs is the longest dependency chain by cumulative task duration
	// — the makespan floor no worker count can beat. A fully cached plan has no
	// positive-duration chain, and 0 there is a real measurement.
	CriticalPathMs int64 `json:"criticalPathMs"`
}

// ReportFile is the recorded synthesis document (report.json) — the run's
// contracted bilan (doc/03-report.md).
//
// It exists because a consumer that wants "what did this run cost, what failed,
// what did it cover" had to scrape the raw session: cloud's CI runner bound the
// gate session by set-difference and rode CPU facts on an untyped field bag,
// and every new fact meant more scraping. The report is the document that
// handoff should have had — small enough to POST, closed enough to bind to, and
// versioned by the same protocolVersion as every other v2 surface.
//
// Three rules govern its content:
//
//   - ABSENT IS NOT ZERO. A fact the run could not measure is OMITTED. A zero
//     means measured-zero, everywhere in this document.
//   - IT IS A PROJECTION, NEVER AN INPUT. The report is derived from the run's
//     own records after the run settled; nothing in it influences pass/fail, and
//     nothing in it enters a cache key.
//   - IT IS BOUNDED BY CONTRACT. Jobs, per-job diagnostics and message length
//     all have stated ceilings, and what the ceilings drop is COUNTED
//     (ElidedJobs, TruncatedCount, the per-command error/warning totals) rather
//     than silently lost.
type ReportFile struct {
	// ProtocolVersion is ResultProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// SessionID is the session this report synthesizes, so a consumer that wants
	// the full detail knows exactly which session directory to open.
	SessionID string `json:"sessionId"`
	// StartTime is when the run began, RFC 3339 with a timezone offset.
	StartTime string `json:"startTime"`
	// EndTime is when it ended, in the same form. It is required: a report is
	// written from a settled run, so an unfinished one has no report rather than
	// a report missing its end.
	EndTime string `json:"endTime"`
	// Origin is ReportOriginCLI or ReportOriginMCP — which surface drove the run.
	Origin string `json:"origin"`
	// EnforceCoverage records whether a configured coverage-threshold could fail
	// this run. It defaults to true; a run that passed --no-enforce-coverage still
	// measures and still reports percentages, so this is the marker that tells a
	// longitudinal consumer whether a green verdict actually means the thresholds
	// held.
	EnforceCoverage bool `json:"enforceCoverage"`
	// Git is the repository state the run was produced against, absent outside a
	// git worktree.
	Git *ReportGit `json:"git,omitempty"`
	// Run is the run's verdict.
	Run ReportRun `json:"run"`
	// Commands is the per-command synthesis, one entry per root command.
	Commands []ReportCommand `json:"commands"`
	// Jobs is the per-job synthesis, at most ReportMaxJobs entries, selected by
	// the priority in doc/03-report.md: failed first, then diagnostic-bearing,
	// then coverage-bearing, then the longest wall.
	Jobs []ReportJob `json:"jobs"`
	// ElidedJobs is how many selected tasks the bound left out, so
	// len(Jobs) + ElidedJobs is the run's own task total and a reader can always
	// tell a small run from a truncated one.
	ElidedJobs int `json:"elidedJobs"`
	// Cache is the run's remote-cache economics, absent when no remote cache
	// participated.
	Cache *ReportCache `json:"cache,omitempty"`
	// Scheduler is the scheduler's shape, absent when the run recorded none.
	Scheduler *ReportScheduler `json:"scheduler,omitempty"`
}
