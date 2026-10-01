package jobs

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// The canonical task/session result model.
//
// The CLI grew nine struct families describing one task's outcome and five more
// describing a run's, each with its own tally loop. This file declares the one
// shape they collapse onto. JobResult stays alongside it for now — every
// consumer still reads it, and a later migration moves them — so nothing
// here mutates the legacy encoding; it only derives from it.
//
// Two orthogonal axes, deliberately:
//
//   - TaskStatus is the execution verdict the DAG schedules on. Every task has
//     one, including a task whose result was reused.
//   - ReuseKind says how the result was obtained WITHOUT executing. It is a
//     property of provenance, not of the verdict. JobResult encodes the same
//     thing as two booleans (CacheHit, Coalesced) that are mutually exclusive
//     in fact but not in type; ReuseKind makes that exclusivity structural and
//     additionally separates a local hit from a remote one, which the booleans
//     cannot express at all.

// TaskStatus is the execution verdict of a single task.
//
// The constant VALUES are the strings JobResult.Status already carries and that
// renderers, session events, and cached entries already serialize ("failed",
// not "failure"). Keeping the spelling is what makes this type droppable into
// those paths in A2b without rewriting a compatibility surface.
type TaskStatus string

const (
	// TaskStatusUnknown is the zero value: no verdict was recorded.
	TaskStatusUnknown TaskStatus = ""
	// TaskStatusSuccess is a task that completed and passed.
	TaskStatusSuccess TaskStatus = "success"
	// TaskStatusFailed is a task that ran and failed, including a timeout.
	TaskStatusFailed TaskStatus = "failed"
	// TaskStatusCanceled is a task an abort or a failing sibling cut short.
	TaskStatusCanceled TaskStatus = "canceled"
	// TaskStatusSkipped is a task that never ran because a dependency failed.
	TaskStatusSkipped TaskStatus = "skipped"
)

// ReuseKind classifies how a task's result was obtained without executing it.
// The zero value means the task actually ran.
type ReuseKind string

const (
	// ReuseNone means the task executed in this run.
	ReuseNone ReuseKind = ""
	// ReuseLocalCache means the result came from this machine's local store.
	ReuseLocalCache ReuseKind = "local-cache"
	// ReuseRemoteCache means the result came from the remote build cache
	// (directly, or from the local entry a concurrent remote restore published).
	ReuseRemoteCache ReuseKind = "remote-cache"
	// ReuseCoalesced means this run waited for a sibling to compute the cold
	// miss instead of computing it a second time. Two arrangements produce it,
	// and they are the same statement about provenance:
	//
	//   - cache-lease coalescing: another process (or another node at the SAME
	//     key) held the lease, and this task consumed the entry it published;
	//   - a plan-level SHARED NODE: another command scheduled the same manifest
	//     task over the same project with the same declared inputs, and this
	//     task adopted that node's result rather than spawning an identical
	//     subprocess (jobs/scheduler_shared.go).
	//
	// Both mean "this run did not spend the work, a sibling in this run did",
	// which is exactly what the `coalesced` outcome has always reported, so the
	// shared node needs no new value in this vocabulary or on any wire.
	ReuseCoalesced ReuseKind = "coalesced"
)

// Reused reports whether the task's result was obtained without executing it.
func (k ReuseKind) Reused() bool { return k != ReuseNone }

// CacheHit reports whether the reuse was an ordinary cache hit — the predicate
// JobResult.CacheHit encodes. Coalescing is reuse but not a hit: the entry did
// not exist when this run asked for it.
func (k ReuseKind) CacheHit() bool {
	return k == ReuseLocalCache || k == ReuseRemoteCache
}

// Outcome maps the reuse kind onto the user-visible outcome vocabulary already
// written to sessions and rendered by the CLI. Local and remote hits both
// report "cached": the distinction is real but belongs to the cache summary,
// not to the per-task outcome string, and changing that string would break
// sessions written by an older CLI.
func (k ReuseKind) Outcome() string {
	switch k {
	case ReuseCoalesced:
		return JobOutcomeCoalesced
	case ReuseLocalCache, ReuseRemoteCache:
		return JobOutcomeCached
	default:
		return ""
	}
}

// ParseTaskStatus maps any status spelling an extension may emit onto the
// canonical vocabulary. It is the single status normalizer: NormalizeStatus
// (the JSONL result-event path) delegates to it, so the subprocess stream and
// the batch wire schema cannot disagree about what "OK" means.
func ParseTaskStatus(s string) (TaskStatus, bool) {
	switch s {
	case "success", "succeeded", "OK", "ok":
		return TaskStatusSuccess, true
	case "failed", "failure", "error", "FAILED":
		return TaskStatusFailed, true
	case "skipped", "skip", "SKIP":
		return TaskStatusSkipped, true
	case "canceled":
		return TaskStatusCanceled, true
	default:
		return TaskStatusUnknown, false
	}
}

// TaskTiming holds one task's measured durations. They are recorded once, at
// the point each is observed, and never recomputed downstream.
type TaskTiming struct {
	// Duration is the final subprocess attempt's wall time.
	Duration time.Duration
	// TaskWall is the scheduler-level wall around the whole task: lookup and
	// reuse, hooks, every retry attempt, and result publication.
	TaskWall time.Duration
	// CPUTime is the subprocess tree's consumed CPU time (user+system).
	CPUTime time.Duration
	// SpawnToFirstEvent is the latency from immediately before cmd.Start until
	// the first valid runtime event. Meaningful only when FirstEventObserved.
	SpawnToFirstEvent  time.Duration
	FirstEventObserved bool
}

// TaskDiagnostic is one diagnostic attributed to a task.
type TaskDiagnostic struct {
	Project  string
	Job      string
	Severity string
	Code     string
	Message  string
	File     string
	Line     int
	Column   int
}

// TaskArtifact is one artifact a task produced.
type TaskArtifact struct {
	Project string
	Job     string
	ID      string
	Name    string
	Kind    string
	Path    string
}

// TaskMetric is one metric a task reported.
type TaskMetric struct {
	Name  string
	Value float64
	Unit  string
}

// Publication is the typed Docker publication fact an extension emits as a
// published artifact. It keeps the producer's timings and immutable-digest
// provenance separate from the scheduler's task timing; machine projections
// join the package-task timing and bounded concurrency at the terminal edge.
type Publication struct {
	Registry       string
	TargetRegistry string
	Name           string
	Version        string
	Tags           []string
	DryRun         bool
	ContentStatus  string
	CacheOutcome   string
	ImageDigest    string
	ImmutableRef   string
	DigestVerified bool
	DigestReused   bool
	PublishTimings *runtimeproto.PublishTimings
}

// TaskResult is the canonical outcome of one scheduled task: identity, the
// orthogonal status/reuse pair, timings, and the structured records the task
// produced. It replaces JobResult, batchWireResult, jobReadResult,
// output.jobRunEntry, mcp.jobFailure, mcp.diagnostic and
// workspace_state.SessionJobEntry as the thing consumers read (A2b).
type TaskResult struct {
	Key       string
	Project   string
	Job       string
	TaskKind  string
	Extension string

	Status TaskStatus
	Reuse  ReuseKind
	Error  *JobError

	ExitCode int
	Timing   TaskTiming
	// InputDigest is the cache key this task was keyed on, in the `sha256:`
	// spelling every digest on the wire uses. It is empty for a task that reached
	// no verdict of its own (skipped, or a status outside the vocabulary) and for
	// a task with no cache identity. See inputDigestOf.
	InputDigest string
	// Execution is the physical subprocess this task's result came out of, shared
	// with every other member of its batch and nil when nothing was spawned. It
	// is what a cost roll-up counts (once per execution); Timing is what
	// per-project attribution reads. See execution.go.
	Execution *Execution

	Diagnostics  []TaskDiagnostic
	Artifacts    []TaskArtifact
	Metrics      []TaskMetric
	Publications []Publication

	// Tests and Coverage are the typed per-verb measurements the task's result
	// payload carried — protocol/runtime's TestSummary and CoverageSummary, the
	// shapes both extensions already emit under "testSummary"/"coverageSummary".
	// They are RECORDED, never derived: a task that ran uninstrumented leaves
	// both nil rather than reporting a measured zero, because a consumer must be
	// able to tell "0% covered" from "nothing measured".
	Tests    *runtimeproto.TestSummary
	Coverage *runtimeproto.CoverageSummary

	// TestCases are the per-case results the task's result payload carried
	// under "testCases", in runner order, sanitized and bounded by
	// protocols/cli's test-case contract (protocolcli.BoundTestCases). Each one
	// becomes a test:case record of the session stream. Nil when the task
	// reported none.
	TestCases []protocolcli.TestCase
	// TestCasesDropped counts the cases the task ran that TestCases does not
	// carry: the producer's own "testCasesDropped", every entry that is not a
	// test-case object, and every case the contract dropped. See testCasesOf.
	TestCasesDropped int
}

// Outcome is the user-visible classification: reuse when the result was reused,
// otherwise the execution status. Identical to JobResult.Outcome by
// construction — the vocabulary is the compatibility surface.
func (t *TaskResult) Outcome() string {
	if t == nil {
		return ""
	}
	if outcome := t.Reuse.Outcome(); outcome != "" {
		return outcome
	}
	return string(t.Status)
}

// SessionEventKind discriminates the canonical session stream.
type SessionEventKind string

const (
	// SessionEventTaskEnd carries one task's terminal result.
	SessionEventTaskEnd SessionEventKind = "task:end"
	// SessionEventAborted reports that a signal cut the run short.
	SessionEventAborted SessionEventKind = "session:aborted"
)

// SessionEvent is one record of the canonical session stream — the reducer's
// input alphabet. It is deliberately a value: observing one adds no allocation.
//
// It is NOT the on-disk session event. workspace_state writes events as
// map[string]any and reads sessions written by older CLIs, so that shape is
// frozen; this type is what A2b migrates the scheduler's SessionEventHandler
// seam onto while the untyped projection stays byte-identical.
type SessionEvent struct {
	Kind SessionEventKind
	// Task is set for SessionEventTaskEnd. The reducer copies what it needs, so
	// the pointer need not outlive the call.
	Task *TaskResult
	// AbortedBy names the abort source for SessionEventAborted.
	AbortedBy string
}

// TaskEndEvent wraps a task result as a session event.
func TaskEndEvent(task *TaskResult) SessionEvent {
	return SessionEvent{Kind: SessionEventTaskEnd, Task: task}
}

// EventField reads key out of an event data object, or out of any object nested
// inside one, and returns nil when the container is not an object.
//
// It is the single reach into RawJobEvent's untyped payload: every canonical
// projection goes through it, so a renderer-shaped map is read in exactly one
// place. Callers pass `any` precisely so nested objects need no cast at the
// call site.
func EventField(container any, key string) any {
	object, _ := container.(map[string]any)
	return object[key]
}

func eventString(container any, key string) string {
	text, _ := EventField(container, key).(string)
	return text
}

// eventInt coerces a JSON-decoded number (float64) — or an int from in-process
// callers such as the batch path — into an int, returning 0 for anything else.
func eventInt(container any, key string) int {
	switch number := EventField(container, key).(type) {
	case float64:
		return int(number)
	case int:
		return number
	case int64:
		return int(number)
	default:
		return 0
	}
}

func EventFloat(container any, key string) (float64, bool) {
	return BatchNumber(EventField(container, key))
}

// diagnosticFromEvent projects a diagnostic event onto the canonical shape:
// flat severity/message/code/file/line/column, with the nested "location"
// object (what protocol/runtime's emitter writes) overriding the flat fields.
func diagnosticFromEvent(event RawJobEvent) TaskDiagnostic {
	diagnostic := TaskDiagnostic{
		Severity: eventString(event.Data, "severity"),
		Message:  eventString(event.Data, "message"),
		Code:     eventString(event.Data, "code"),
		File:     eventString(event.Data, "file"),
		Line:     eventInt(event.Data, "line"),
		Column:   eventInt(event.Data, "column"),
	}
	location := EventField(event.Data, "location")
	if location == nil {
		return diagnostic
	}
	if file := eventString(location, "file"); file != "" {
		diagnostic.File = file
	}
	if line := eventInt(location, "line"); line != 0 {
		diagnostic.Line = line
	}
	if column := eventInt(location, "column"); column != 0 {
		diagnostic.Column = column
	}
	return diagnostic
}

func artifactFromEvent(event RawJobEvent) TaskArtifact {
	return TaskArtifact{
		ID:   eventString(event.Data, "id"),
		Name: eventString(event.Data, "name"),
		Kind: eventString(event.Data, "kind"),
		Path: eventString(event.Data, "path"),
	}
}

func metricFromEvent(event RawJobEvent) (TaskMetric, bool) {
	name := eventString(event.Data, "name")
	if name == "" {
		return TaskMetric{}, false
	}
	value, _ := EventFloat(event.Data, "value")
	return TaskMetric{Name: name, Value: value, Unit: eventString(event.Data, "unit")}, true
}

// TaskResultOf projects a scheduled job and its legacy JobResult onto the
// canonical model.
//
// Records come from the batch path's typed conversion when it ran
// (JobResult.Canonical), and are otherwise extracted from the event stream the
// subprocess already parsed once. Either way the stream is never re-parsed: a
// batch member's event stream is a PROJECTION of these very records, kept for
// the renderers and the cache entry, never a source they are recovered from.
//
// The typed measurements are decoded ONCE, after that branch, because all three
// paths converge here carrying the same result data map: the solo run's result
// event, the batch member's slice of the wire (jobResultFromBatchWire keeps
// wire.Data), and the warm entry's persisted result.json. Decoding here rather
// than in applyTaskSummary keeps it off TaskSummaryOf, the scheduler's hot-path
// counting variant, which needs no record at all.
func TaskResultOf(job *ScheduledJob, result *JobResult) TaskResult {
	task := TaskResult{}
	if result != nil && result.Canonical != nil {
		task = *result.Canonical
	} else if result != nil {
		task.Diagnostics, task.Artifacts, task.Metrics, task.Publications = RecordsFromEvents(result.Events)
	}
	if result != nil {
		// A payload that will not decode is simply not a measurement: the decoder
		// yields nils with its error, and a malformed optional block never turns a
		// successful task into a failed one — the rule publicationFromEvent
		// already follows. A recorded figure is as cadence-correct as a fresh one
		// because --enforce-coverage varies the cache key on both extensions,
		// so a warm hit replayed under the enforcing cadence was produced
		// under it.
		task.Tests, task.Coverage, _, _ = runtimeproto.ExtractResultPayloads(result.Data)
		task.TestCases, task.TestCasesDropped = testCasesOf(result.Data)
	}
	applyTaskSummary(&task, job, result)
	return task
}

// testCasesOf decodes the per-case results a result payload carries and
// applies protocols/cli's test-case contract to them.
//
// Like the summaries above, a payload that will not decode never fails the
// task. The rules for what it counts as dropped:
//
//   - a "testCases" value that is not an array yields no case and counts
//     nothing, because nothing in it can be counted per case;
//   - an array entry that does not decode as a test case counts as one
//     dropped case, and so does every case BoundTestCases drops, including
//     one whose name or suite sanitization left empty;
//   - "testCasesDropped" adds the producer's own count when it is an integer
//     from 0 to math.MaxInt32, and counts nothing otherwise.
func testCasesOf(data map[string]any) ([]protocolcli.TestCase, int) {
	if data == nil {
		return nil, 0
	}
	dropped := producerDroppedTestCases(data[runtimeproto.TestCasesDroppedResultDataKey])
	value, present := data[runtimeproto.TestCasesResultDataKey]
	if !present {
		return nil, dropped
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, dropped
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, dropped
	}
	decoded := make([]protocolcli.TestCase, 0, len(entries))
	for _, entry := range entries {
		var testCase protocolcli.TestCase
		if err := json.Unmarshal(entry, &testCase); err != nil {
			dropped++
			continue
		}
		decoded = append(decoded, testCase)
	}
	kept, boundDropped := protocolcli.BoundTestCases(decoded)
	if len(kept) == 0 {
		kept = nil
	}
	return kept, dropped + boundDropped
}

// producerDroppedTestCases reads the producer's dropped-case count. A value
// that is not an integer from 0 to math.MaxInt32 is not a count.
func producerDroppedTestCases(value any) int {
	var count float64
	switch number := value.(type) {
	case float64:
		count = number
	case int:
		count = float64(number)
	case int64:
		count = float64(number)
	default:
		return 0
	}
	if count < 0 || count > math.MaxInt32 || count != math.Trunc(count) {
		return 0
	}
	return int(count)
}

// TaskSummaryOf projects a job and its result onto the canonical model WITHOUT
// the structured records: identity, verdict, reuse and timings only.
//
// It exists because every counting consumer needs exactly that, and each runs
// where walking the event stream would be new per-task work: the session-event
// producer (once per completion, in the scheduler's hot path) and ReduceRun.
// Record extraction stays in TaskResultOf, which is what the scheduler's own
// end-of-run reduction uses — and what the jsonl stream's task:end uses, since
// that record REPORTS a task rather than counting it and the contract requires
// its diagnostics.
func TaskSummaryOf(job *ScheduledJob, result *JobResult) TaskResult {
	var task TaskResult
	applyTaskSummary(&task, job, result)
	return task
}

// applyTaskSummary stamps the verdict, reuse, timings and identity onto task.
// It is the one place the legacy JobResult encoding is read, so the summary and
// the full projection cannot disagree about, say, what reuse a result carried.
func applyTaskSummary(task *TaskResult, job *ScheduledJob, result *JobResult) {
	if result != nil {
		status, ok := ParseTaskStatus(result.Status)
		if !ok {
			status = TaskStatus(result.Status)
		}
		task.Status = status
		task.Reuse = result.ReuseKind()
		task.Error = result.Error
		task.ExitCode = result.ExitCode
		task.InputDigest = inputDigestOf(status, result.CacheKey)
		task.Execution = result.Execution
		task.Timing = TaskTiming{
			Duration:           result.Duration,
			TaskWall:           result.TaskWall,
			CPUTime:            result.CPUTime,
			SpawnToFirstEvent:  result.SpawnToFirstEvent,
			FirstEventObserved: result.FirstEventObserved,
		}
	}
	ApplyTaskIdentity(task, job)
}

// inputDigestOf spells the key a task was keyed on as its input digest.
//
// Only a task that reached a verdict carries one. A skipped task never looked
// its key up — the scheduler decided not to run it before any lookup — so a
// digest beside it would name inputs nothing was keyed on, and a reader grouping
// records by digest would count a task that never ran as one more observation of
// that key. A key that is not a lowercase hex SHA-256 is not a key the store
// computed, and publishing it would write a record its own contract rejects.
func inputDigestOf(status TaskStatus, key string) string {
	switch status {
	case TaskStatusSuccess, TaskStatusFailed, TaskStatusCanceled:
	default:
		return ""
	}
	if len(key) != sha256.Size*2 {
		return ""
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return "sha256:" + key
}

// ApplyTaskIdentity stamps the job's identity onto the task and its records, so
// a diagnostic or artifact carries its origin wherever it is aggregated.
func ApplyTaskIdentity(task *TaskResult, job *ScheduledJob) {
	if job == nil {
		return
	}
	task.Key = job.Key()
	if job.Project != nil {
		task.Project = job.Project.Name
	}
	if job.JobDef != nil {
		task.Job = job.JobDef.Name
	}
	task.TaskKind = task.Job
	if job.Step != nil && job.Step.Task != "" {
		task.TaskKind = job.Step.Task
	}
	if job.Extension != nil {
		task.Extension = job.Extension.Name
	}
	for i := range task.Diagnostics {
		task.Diagnostics[i].Project = task.Project
		task.Diagnostics[i].Job = task.Job
	}
	for i := range task.Artifacts {
		task.Artifacts[i].Project = task.Project
		task.Artifacts[i].Job = task.Job
	}
}

// RecordsFromEvents walks an already-parsed event stream once and extracts the
// structured records. Nothing is allocated for a task that emitted none, which
// is the all-hit warm path.
func RecordsFromEvents(events []RawJobEvent) ([]TaskDiagnostic, []TaskArtifact, []TaskMetric, []Publication) {
	var diagnostics []TaskDiagnostic
	var artifacts []TaskArtifact
	var metrics []TaskMetric
	var publications []Publication
	for _, event := range events {
		switch event.Type {
		case EventTypeDiagnostic:
			diagnostics = append(diagnostics, diagnosticFromEvent(event))
		case EventTypeArtifact:
			artifacts = append(artifacts, artifactFromEvent(event))
			if publication, ok := publicationFromEvent(event); ok {
				publications = append(publications, publication)
			}
		case EventTypeMetric:
			if metric, ok := metricFromEvent(event); ok {
				metrics = append(metrics, metric)
			}
		}
	}
	return diagnostics, artifacts, metrics, publications
}

const verifiedImagePublicationStep = "docker"

// ExpectsVerifiedImagePublication reports whether a planned job owns the
// established image-publication result contract. Provider vocabulary remains
// quarantined in this canonical projection instead of leaking into the
// scheduler or release-set coordinator.
func ExpectsVerifiedImagePublication(job *ScheduledJob) bool {
	return job != nil && job.JobDef != nil && job.CommandName() == "publish" && job.StepID() == verifiedImagePublicationStep
}

// publicationFromEvent accepts only the shared Docker publisher's published
// artifact records. Unknown publisher-specific fields remain harmless runtime
// event extensions; a malformed optional payload never turns a successful task
// into a failed one just because it cannot be surfaced in a recap.
func publicationFromEvent(event RawJobEvent) (Publication, bool) {
	kind, _ := event.Data["kind"].(string)
	if kind != runtimeproto.ArtifactKindPublished {
		return Publication{}, false
	}
	record, err := runtimeproto.DecodePublishRecord(event.Data)
	if err != nil || record == nil || record.Registry != verifiedImagePublicationStep || record.ImageDigest == "" {
		return Publication{}, false
	}
	return Publication{
		Registry:       record.Registry,
		TargetRegistry: record.TargetRegistry,
		Name:           record.Name,
		Version:        record.Version,
		Tags:           append([]string(nil), record.Tags...),
		DryRun:         record.DryRun,
		ContentStatus:  record.ContentStatus,
		CacheOutcome:   record.CacheOutcome,
		ImageDigest:    record.ImageDigest,
		ImmutableRef:   record.ImmutableRef,
		DigestVerified: record.DigestVerified,
		DigestReused:   record.DigestReused,
		PublishTimings: record.PublishTimings,
	}, true
}
