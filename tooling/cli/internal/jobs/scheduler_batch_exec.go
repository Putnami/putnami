// The batch INVOCATION: what sharing one subprocess adds, and nothing else.
//
// An earlier refactor moved the lifecycle out of this file. Cache lookup,
// leasing, hooks, capture, finalization and retry live in scheduler_exec.go and
// are the same code for one work item or twelve; what remains here is the part
// that is genuinely batch-specific:
//
//   - the LEADER: one plan node carrying every member's project as its
//     selection, with a deadline scaled to the group it must cover;
//   - the SPLIT: the typed batch wire (`batchResults`, unchanged by this move)
//     attributed back onto the members by TYPED IDENTITY;
//   - the CONVERSION: that wire read exactly once, into canonical TaskResults;
//   - the PROJECTION: a display/persistence event stream derived from those
//     canonical records for members that had no live stream of their own.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// maxDurationDeadlineMs is the largest positive millisecond count that can be
// represented as a time.Duration. Deadline arithmetic saturates there rather
// than overflowing into a negative duration, which every caller would interpret
// as an unbounded job.
const maxDurationDeadlineMs = int64(1<<63-1) / int64(time.Millisecond)

// boundedDeadlineMs scales a positive deadline without permitting duration
// overflow. The zero/default and negative/unbounded sentinels pass through.
func boundedDeadlineMs(perTaskMs, n int) int {
	if perTaskMs <= 0 {
		return perTaskMs
	}
	if n < 1 {
		n = 1
	}
	limit := min(maxDurationDeadlineMs, int64(^uint(0)>>1))
	if int64(perTaskMs) > limit/int64(n) {
		return int(limit)
	}
	return perTaskMs * n
}

// batchLeaderTimeoutMs scales a per-task subprocess timeout to cover a batch of
// n suites sharing one process. The manifest timeout is a per-suite budget (in
// the singleton path one project is one suite); a group of n runs up to n× that,
// so the shared process deadline is scaled to n× to avoid failing every
// batch-mate when a couple of legitimately slow suites exceed the single-suite
// budget. Sentinels are preserved: a negative timeout stays "no timeout", and a
// zero (use-default) is resolved to the default before scaling. n<=1 is a no-op.
func batchLeaderTimeoutMs(perTaskMs, n int) int {
	if perTaskMs < 0 || n <= 1 {
		return perTaskMs
	}
	effective := perTaskMs
	if effective == 0 {
		effective = DefaultTimeoutMs
	}
	return boundedDeadlineMs(effective, n)
}

// batchGroupTimeoutMs reduces per-project effective deadlines to the one
// budget a shared subprocess runs under. A mixed group takes the largest member
// allowance before applying N× scaling; if any member is manifest-unbounded the
// group is unbounded too, because no finite group deadline can respect it.
func batchGroupTimeoutMs(work []taskWork) int {
	if len(work) == 0 {
		return 0
	}
	if len(work) == 1 {
		return work[0].job.EffectiveTimeoutMs()
	}

	maxPerTaskMs := 0
	for _, member := range work {
		perTaskMs := member.job.EffectiveTimeoutMs()
		if perTaskMs < 0 {
			return -1
		}
		if perTaskMs == 0 {
			perTaskMs = DefaultTimeoutMs
		}
		maxPerTaskMs = max(maxPerTaskMs, perTaskMs)
	}
	return batchLeaderTimeoutMs(maxPerTaskMs, len(work))
}

type batchWireResult struct {
	ProjectID   string                `json:"projectId"`
	Status      string                `json:"status"`
	Data        map[string]any        `json:"data,omitempty"`
	Diagnostics []batchWireDiagnostic `json:"diagnostics,omitempty"`
	Artifacts   []batchWireArtifact   `json:"artifacts,omitempty"`
	Summary     batchWireSummary      `json:"summary,omitempty"`
	// Metrics carries per-project counters that cannot ride the (often nil)
	// result Data — e.g. the build producers' transpiled-files/type-declarations/
	// generate-hash/compiled-executables. omitempty keeps this backward-compatible:
	// lint/test/Python emit none, so their reconstructed rows are unchanged. The
	// field name and json tags MUST mirror the extension's buildBatchMetric.
	Metrics []batchWireMetric `json:"metrics,omitempty"`
}

type batchWireMetric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

type batchWireArtifact struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

type batchWireDiagnostic struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"`
	Column      int    `json:"column,omitempty"`
}

type batchWireSummary struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos"`
}

// runBatchAttempt performs ONE shared invocation for n > 1 work items and
// returns a result per member.
//
// It is the n > 1 arm of runSharedAttempt and does nothing outside the
// invocation itself: no lookup, no lease, no hook, no capture, no retry. A
// member's result leaves here as an ordinary *JobResult carrying its canonical
// TaskResult, and the lifecycle publishes it exactly as it publishes a
// singleton's.
func (s *Scheduler) runBatchAttempt(ctx context.Context, work []taskWork) map[string]*JobResult {
	leader := batchLeader(work)
	ctx = s.processCapabilities.contextForJob(ctx, leader)

	// The leader's stream is one interleaved stream for n projects and nothing in
	// it says which member a line belongs to. A session-aware renderer records it
	// immediately under a stable batch identity, before RunJob's bounded result
	// retention can omit noisy detail. Other renderers keep the historical member
	// projection below and receive no unattributed live lines.
	//
	// Test producers additionally attach a batchProjectId context hint to their
	// transcript log events. The split below projects those onto the owning
	// member once the typed wire arrives; every other batch event is projected
	// from that wire.
	var aggregateEvents EventHandler
	if recorder, ok := s.renderer.(interface {
		BatchJobEvent(*ScheduledJob, RawJobEvent)
	}); ok {
		aggregateEvents = func(event RawJobEvent) { recorder.BatchJobEvent(leader, event) }
	}
	aggregate, err := runJob(
		ctx,
		s.ws,
		leader,
		s.taskParams,
		s.commandDefaults(leader),
		s.cfg.VersionInfo,
		s.jobProcessEnv(),
		aggregateEvents,
	)
	if err != nil {
		aggregate = spawnFailureResult(ctx, err)
	}

	results, ok := splitBatchResult(s.ws.Root, work, aggregate)
	if !ok {
		results = sharedFailureResults(work, aggregate)
	}
	// One process ran on behalf of n members, and it is recorded ONCE: every
	// member points at that physical execution, and the wall and CPU each member
	// reports become its share of it. Before an earlier fix a member
	// claimed the leader's whole wall and an unreconcilable equal slice of its
	// CPU, so summing the members multiplied the batch's real cost by n
	// (attributeSharedExecution, execution.go).
	attributeSharedExecution(work, results, aggregate)

	// Replay each member's projected stream now, before any per-task retry, so a
	// batch renders in the same order a group of singletons would: every member
	// starts, the work happens, then each member's records arrive. A member that
	// goes on to retry solo streams that attempt live on top, exactly as a
	// singleton's second attempt does.
	for i := range work {
		result := results[work[i].job.Key()]
		if result == nil {
			continue
		}
		for _, event := range result.Events {
			if batchRenderer, ok := s.renderer.(interface {
				BatchMemberJobEvent(*ScheduledJob, RawJobEvent)
			}); ok {
				batchRenderer.BatchMemberJobEvent(work[i].job, event)
			} else {
				s.renderer.JobEvent(work[i].job, event)
			}
		}
	}
	return results
}

// batchLeader builds the ONE plan node the shared subprocess runs: the first
// member's node, carrying every member's project as its selection.
//
// The shared process runs every grouped suite (extension-loop batches such as
// test-run run them sequentially), so the single-task subprocess deadline is
// scaled to cover the whole group — otherwise two slow-but-valid suites trip the
// one-suite timeout and fail every batch-mate. Mixed project overrides use the
// largest member allowance; an unbounded member leaves the group unbounded.
// The widened value is stored on the copied node, so singleton members and
// retries retain their own deadlines.
func batchLeader(work []taskWork) *ScheduledJob {
	leader := *work[0].job
	// The copied node's stamped identity is work[0]'s PROJECT-scoped one, but
	// this leader is about to gain SelectedProjects — the shape whose derived
	// identity is workspace-scoped. Clear the stamp so TypedIdentity falls back
	// to the pure derivation and B2a's stamped≡derived invariant holds on the
	// one synthesized node in the system (wave-4 integration finding F1).
	leader.Identity = nil
	leader.SelectedProjects = make([]*workspace.Project, 0, len(work))
	for i := range work {
		leader.SelectedProjects = append(leader.SelectedProjects, work[i].job.Project)
	}
	if scaled := batchGroupTimeoutMs(work); scaled != leader.EffectiveTimeoutMs() {
		leader.TimeoutOverrideMs = &scaled
	}
	return &leader
}

// batchMemberID is the identity a batch member is attributed by: its TYPED
// identity's project id, not an ad hoc field read.
//
// Both sides of the join are then the plan's own identity — the CLI puts these
// ids in the leader's selection, and the extension answers with them — so a
// member cannot be matched by a string one layer spells differently. It is also
// total where the field read was not: a node without a project answers
// "unknown", which no wire entry can claim (the split below rejects an empty or
// duplicated projectId), so an unattributable member fails the group closed
// instead of panicking.
func batchMemberID(job *ScheduledJob) string {
	return job.TypedIdentity().Project.ID
}

// splitBatchResult attributes the typed batch wire back onto the work items.
// It returns false — and the caller fails the whole group — for any aggregate
// it cannot read as a complete, unambiguous answer for every member.
//
// wsRoot is the workspace root the members' reported paths are displayed
// against (batchDisplayPath); "" disables that normalization.
func splitBatchResult(wsRoot string, work []taskWork, aggregate *JobResult) (map[string]*JobResult, bool) {
	if aggregate == nil || aggregate.Status != "success" || aggregate.Data == nil {
		return nil, false
	}
	raw, ok := aggregate.Data[BatchResultsDataKey]
	if !ok {
		return nil, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var wire []batchWireResult
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, false
	}

	byProject := make(map[string]batchWireResult, len(wire))
	for _, item := range wire {
		if item.ProjectID == "" {
			return nil, false
		}
		if _, exists := byProject[item.ProjectID]; exists {
			return nil, false
		}
		byProject[item.ProjectID] = item
	}

	results := make(map[string]*JobResult, len(work))
	logsByProject := batchProjectLogs(aggregate)
	for i := range work {
		item, ok := byProject[batchMemberID(work[i].job)]
		if !ok {
			return nil, false
		}
		results[work[i].job.Key()] = jobResultFromBatchWire(
			wsRoot,
			work[i].job,
			item,
			aggregate,
			logsByProject[item.ProjectID],
		)
	}
	return results, true
}

// jobResultFromBatchWire converts one member's slice of the batch wire schema.
//
// This is the ONLY reader of that wire. It produces the canonical TaskResult —
// already tagged with the member's identity, so a diagnostic carries its origin
// from the moment it exists — and the canonical model reads that
// (JobResult.Canonical); no consumer re-parses anything to recover records the
// conversion already holds.
//
// Events are then PROJECTED from those same canonical records
// (canonicalTaskEvents) and passed through the same strict per-task retention
// budget as a live subprocess stream. They are not a second representation of the wire: the
// projection cannot see it. They exist because RawJobEvent is still the
// Renderer interface's alphabet and the shape a cache entry persists for a warm
// replay, so a member that had no live stream of its own must be given one or
// batching would silently cost diagnostics, metrics and summaries that a
// singleton keeps — in the terminal, in --output=jsonl, and in every entry the
// member writes. They are deletable the day renderers and cache entries read
// canonical records instead, not before.
func jobResultFromBatchWire(
	wsRoot string,
	job *ScheduledJob,
	wire batchWireResult,
	aggregate *JobResult,
	logs []RawJobEvent,
) *JobResult {
	status, valid := parseTaskStatus(wire.Status)
	if !valid {
		status = TaskStatusFailed
	}
	task := taskResultFromBatchWire(wsRoot, job, wire, status)
	events := canonicalTaskEvents(job, task, wire.Data)
	events = insertBatchProjectLogs(events, logs)
	result := &JobResult{
		Status: string(status),
		Data:   wire.Data,
		// Bound AFTER the transcript is attached: the retained-event budget is
		// what keeps a noisy member's logs out of memory, and its failure reserve
		// keeps the verdict-bearing records regardless of transcript volume.
		Events:             BoundedJobEvents(events),
		Duration:           aggregate.Duration,
		SpawnToFirstEvent:  aggregate.SpawnToFirstEvent,
		FirstEventObserved: aggregate.FirstEventObserved,
		ExitCode:           aggregate.ExitCode,
		Canonical:          &task,
	}
	if !valid {
		result.Error = &JobError{Message: fmt.Sprintf(
			"batch result for project %s returned invalid status %q",
			wire.ProjectID,
			wire.Status,
		)}
	}
	return result
}

// batchProjectLogs groups transcript events for every member in one pass. A
// shared event may name several projects, which avoids producer-side
// encode/parse/redaction amplification for an unattributed group failure. The
// routing hints are removed from the projected events so every member stream
// has the same shape as its corresponding solo stream.
func batchProjectLogs(aggregate *JobResult) map[string][]RawJobEvent {
	if aggregate == nil {
		return nil
	}
	logs := map[string][]RawJobEvent{}
	for _, event := range aggregate.Events {
		if event.Type != EventTypeLog || event.Data == nil {
			continue
		}
		contextData, ok := event.Data["context"].(map[string]any)
		if !ok {
			continue
		}
		var projectIDs []string
		if projectID, ok := contextData[protocolcli.BatchProjectLogContextKey].(string); ok && projectID != "" {
			projectIDs = append(projectIDs, projectID)
		}
		switch shared := contextData[protocolcli.BatchProjectLogsContextKey].(type) {
		case []string:
			for _, projectID := range shared {
				if projectID != "" {
					projectIDs = append(projectIDs, projectID)
				}
			}
		case []any:
			for _, value := range shared {
				if projectID, ok := value.(string); ok && projectID != "" {
					projectIDs = append(projectIDs, projectID)
				}
			}
		}
		if len(projectIDs) == 0 {
			continue
		}

		projected := event
		projected.Data = make(map[string]any, len(event.Data))
		for key, value := range event.Data {
			projected.Data[key] = value
		}
		contextCopy := make(map[string]any, len(contextData))
		for key, value := range contextData {
			if key != protocolcli.BatchProjectLogContextKey && key != protocolcli.BatchProjectLogsContextKey {
				contextCopy[key] = value
			}
		}
		if len(contextCopy) == 0 {
			delete(projected.Data, "context")
		} else {
			projected.Data["context"] = contextCopy
		}
		if len(projected.Data) == 0 {
			projected.Data = nil
		}
		if len(projectIDs) == 1 {
			logs[projectIDs[0]] = append(logs[projectIDs[0]], projected)
			continue
		}
		seen := map[string]struct{}{}
		for _, projectID := range projectIDs {
			if _, duplicate := seen[projectID]; duplicate {
				continue
			}
			seen[projectID] = struct{}{}
			owned := projected
			owned.Data = cloneEventData(projected.Data)
			logs[projectID] = append(logs[projectID], owned)
		}
	}
	return logs
}

// insertBatchProjectLogs places the retained transcript before the synthetic
// phase end, matching the lifecycle order of a solo test invocation.
func insertBatchProjectLogs(events, logs []RawJobEvent) []RawJobEvent {
	if len(logs) == 0 {
		return events
	}
	insertAt := len(events)
	for i, event := range events {
		if event.Type == EventTypePhase && event.Data["action"] == "end" {
			insertAt = i
			break
		}
	}
	out := make([]RawJobEvent, 0, len(events)+len(logs))
	out = append(out, events[:insertAt]...)
	out = append(out, logs...)
	out = append(out, events[insertAt:]...)
	return out
}

// taskResultFromBatchWire builds the canonical records straight from the typed
// wire — no event round trip — normalizes the paths they report the way a solo
// run's stream is normalized, and stamps the member's typed identity onto the
// task and each of its records. canonicalTaskEvents renders these same records,
// so the two representations agree by construction rather than by parallel
// maintenance (pinned by TestBatchWire_CanonicalMatchesSyntheticEvents).
func taskResultFromBatchWire(wsRoot string, job *ScheduledJob, wire batchWireResult, status TaskStatus) TaskResult {
	task := TaskResult{
		Status:      status,
		Diagnostics: batchWireDiagnostics(wire),
		Artifacts:   batchWireArtifacts(wire),
		Metrics:     batchWireMetrics(job, wire),
	}
	normalizeBatchRecordPaths(wsRoot, job, &task)
	applyTaskIdentity(&task, job)
	return task
}

// normalizeBatchRecordPaths maps the file paths a batch member reported onto
// the workspace-relative display form the SOLO event stream would have carried
// for the same records (workspacePathEventMapper). Without it the same finding
// names its file differently depending on whether the task happened to be
// batched — a solo diagnostic says "a/src.ts" where a batched one says
// "src.ts" — which is exactly the solo↔batch divergence this slice removes.
func normalizeBatchRecordPaths(wsRoot string, job *ScheduledJob, task *TaskResult) {
	if wsRoot == "" || job == nil || job.Project == nil {
		return
	}
	projRoot := filepath.Join(wsRoot, job.Project.Path)
	for i := range task.Diagnostics {
		task.Diagnostics[i].File = batchDisplayPath(wsRoot, projRoot, task.Diagnostics[i].File)
	}
	for i := range task.Artifacts {
		task.Artifacts[i].Path = batchDisplayPath(wsRoot, projRoot, task.Artifacts[i].Path)
	}
}

// batchDisplayPath is the member-scoped half of the solo path mapper
// (event_paths.go), applied to a canonical record instead of an event.
//
// It normalizes only what it can RESOLVE — an absolute path inside the
// workspace, or a project-relative path that exists under this member's project
// root — and returns everything else verbatim. The solo mapper additionally
// re-roots unresolvable paths LEXICALLY against the job's cwd; a batch answer
// must not take that fallback, because the shared process ran from one member's
// directory on behalf of all of them, so re-rooting a path nobody can resolve
// would turn an unhelpful path into a confidently wrong one.
func batchDisplayPath(wsRoot, projRoot, path string) string {
	if path == "" || shouldKeepRawPath(path) {
		return path
	}
	native := filepath.Clean(filepath.FromSlash(path))
	if filepath.IsAbs(native) {
		return workspaceRelOrClean(wsRoot, native)
	}
	// Already workspace-relative (what the first-party batch producers emit, so
	// they can attribute a diagnostic to a project at all): keep it.
	if relativePathExists(wsRoot, native) {
		return cleanDisplayPath(native)
	}
	if rel, ok := existingPathRelativeToWorkspace(wsRoot, projRoot, native); ok {
		return rel
	}
	return path
}

func batchWireDiagnostics(wire batchWireResult) []TaskDiagnostic {
	if len(wire.Diagnostics) == 0 {
		return nil
	}
	diagnostics := make([]TaskDiagnostic, 0, len(wire.Diagnostics))
	for _, diagnostic := range wire.Diagnostics {
		record := TaskDiagnostic{
			Severity: diagnostic.Severity,
			Message:  diagnostic.Description,
			Code:     diagnostic.Category,
		}
		// A position without a file names nothing, so it is dropped — which is
		// also exactly what the rendered event has always carried.
		if diagnostic.File != "" {
			record.File = diagnostic.File
			record.Line = diagnostic.Line
			record.Column = diagnostic.Column
		}
		diagnostics = append(diagnostics, record)
	}
	return diagnostics
}

func batchWireArtifacts(wire batchWireResult) []TaskArtifact {
	if len(wire.Artifacts) == 0 {
		return nil
	}
	artifacts := make([]TaskArtifact, 0, len(wire.Artifacts))
	for _, artifact := range wire.Artifacts {
		artifacts = append(artifacts, TaskArtifact{
			ID:   artifact.ID,
			Name: artifact.Name,
			Kind: artifact.Kind,
			Path: artifact.Path,
		})
	}
	return artifacts
}

// batchWireMetrics derives the per-project metrics a solo run would have
// emitted: the lint summary counters or the test/coverage counters, followed by
// any explicit metrics the extension put on the wire.
func batchWireMetrics(job *ScheduledJob, wire batchWireResult) []TaskMetric {
	var metrics []TaskMetric
	switch batchExtensionJobName(job) {
	case "lint":
		for _, metric := range []TaskMetric{
			{Name: "lint-errors", Value: float64(wire.Summary.Errors), Unit: "count"},
			{Name: "lint-warnings", Value: float64(wire.Summary.Warnings), Unit: "count"},
			{Name: "lint-infos", Value: float64(wire.Summary.Infos), Unit: "count"},
		} {
			if metric.Value == 0 {
				continue
			}
			metrics = append(metrics, metric)
		}
	case "test":
		metrics = append(metrics, testBatchMetrics(wire.Data)...)
	}
	// Replay any explicit per-project metrics (e.g. the build producers'
	// transpiled-files/type-declarations/generate-hash/compiled-executables).
	// Empty for lint/test/Python, so their rows are unchanged.
	for _, metric := range wire.Metrics {
		// The wire metric and the canonical one are field-identical today, so
		// this is a conversion. Should either shape grow a field, it stops
		// compiling and the explicit mapping has to come back — which is the
		// review the divergence deserves.
		metrics = append(metrics, TaskMetric(metric))
	}
	return metrics
}

// canonicalTaskEvents projects a canonical TaskResult onto the event stream a
// solo run of the same task would have emitted.
//
// It reads the CANONICAL RECORDS and the member's own result data — never the
// batch wire, which jobResultFromBatchWire has already consumed. That is the
// whole point of the split: one wire reader, one canonical model, and a display
// projection that can only ever show what the canonical model holds. A record
// the projection does not carry is a record the canonical result never had.
//
// data is the member's own extension result payload (the same map that reaches
// the renderers as JobResult.Data), which the test/summary lines and the
// terminal result event report verbatim.
// canonicalTaskEvents projects a member's canonical result onto the runtime
// event shapes.
//
// All twelve projections stamp MaxKnownProtocolVersion — the version the CLI
// advertises to every subprocess (runner.go) and the only one it accepts back
// (parseRawEvent rejects anything else since the CLI started requiring v2).
// They stamped v1 as a deliberate normalization until an earlier change
// stopped that: harmless, because
// every consumer was traced version-blind and renderers switch on type, but a
// synthesized stream claiming a version the same process refuses to READ is a
// contradiction the "one stream, v2 only" contract cannot keep. Nothing about
// the events' shape changes with the stamp.
func canonicalTaskEvents(job *ScheduledJob, task TaskResult, data map[string]any) []RawJobEvent {
	status := task.Status
	phaseName := batchPhaseName(job)
	jobName := batchExtensionJobName(job)
	events := []RawJobEvent{{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypeMeta,
		Level:   "info",
		Message: "Starting " + jobName,
		Data: map[string]any{
			"extension": job.Extension.Name,
			"job":       jobName,
		},
	}}
	if jobName == "lint" {
		events = append(events, RawJobEvent{
			Version: runtimeproto.MaxKnownProtocolVersion,
			Type:    EventTypeProgress,
			Message: "Formatting and linting code",
			Data: map[string]any{
				"current": float64(1),
				"total":   float64(1),
			},
		})
	} else if jobName == "test" {
		events = append(events, RawJobEvent{
			Version: runtimeproto.MaxKnownProtocolVersion,
			Type:    EventTypeProgress,
			Message: "Running tests...",
			Data: map[string]any{
				"current": float64(1),
				"total":   float64(3),
			},
		})
	}
	events = append(events, RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypePhase,
		Data: map[string]any{
			"name":   phaseName,
			"action": "start",
		},
	})

	for _, diagnostic := range task.Diagnostics {
		data := map[string]any{
			"severity": diagnostic.Severity,
			"message":  diagnostic.Message,
		}
		// An empty code names nothing, so it is dropped — same rule as the
		// position below, and the same shape a SOLO stream carries: a tool that
		// reports no rule id (go test, and every extension that does not fill the
		// batch wire's `category`) emits a diagnostic event with no `code` key at
		// all. Writing `"code": ""` here made the projected member stream — and
		// therefore --output=jsonl and the entry a member caches — differ from the
		// same task's solo stream by one field.
		if diagnostic.Code != "" {
			data["code"] = diagnostic.Code
		}
		if diagnostic.File != "" {
			// float64, like the progress counters above and unlike the canonical
			// record's int: a live stream's Data is JSON-decoded, so every number in
			// it is a float64. A consumer that type-asserts one — the shape a solo
			// stream always had — must not have to special-case a batched member.
			data["location"] = map[string]any{
				"file":   diagnostic.File,
				"line":   float64(diagnostic.Line),
				"column": float64(diagnostic.Column),
			}
		}
		events = append(events, RawJobEvent{
			Version: runtimeproto.MaxKnownProtocolVersion,
			Type:    EventTypeDiagnostic,
			Message: diagnostic.Message,
			Data:    data,
		})
	}
	for _, artifact := range task.Artifacts {
		events = append(events, taskArtifactEvent(artifact))
	}
	for _, metric := range task.Metrics {
		events = append(events, batchMetricEvent(metric.Name, metric.Value, metric.Unit))
	}
	phaseStatus := "success"
	if status == TaskStatusFailed || status == TaskStatusCanceled {
		phaseStatus = "failed"
	} else if status == TaskStatusSkipped {
		phaseStatus = "skipped"
	}
	events = append(events, RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypePhase,
		Data: map[string]any{
			"name":   phaseName,
			"action": "end",
			"status": phaseStatus,
		},
	})
	if jobName == "lint" && status == TaskStatusSuccess {
		events = append(events, RawJobEvent{
			Version: runtimeproto.MaxKnownProtocolVersion,
			Type:    EventTypeLog,
			Level:   "info",
			Message: "Lint completed for " + job.Project.Name,
		})
		if summary := lintSummaryText(task.Metrics); summary != "" {
			events = append(events, RawJobEvent{
				Version: runtimeproto.MaxKnownProtocolVersion,
				Type:    EventTypeSummary,
				Message: summary,
			})
		}
	} else if jobName == "test" {
		if summary := testBatchSummary(data, status == TaskStatusFailed); summary != "" {
			events = append(events, RawJobEvent{
				Version: runtimeproto.MaxKnownProtocolVersion,
				Type:    EventTypeSummary,
				Message: summary,
			})
		}
	}
	resultStatus := "OK"
	if status == TaskStatusFailed || status == TaskStatusCanceled {
		resultStatus = "FAILED"
	} else if status == TaskStatusSkipped {
		resultStatus = "SKIP"
	}
	events = append(events, RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypeResult,
		Level:   "info",
		Message: "Job " + resultStatus,
		Data: map[string]any{
			"status": resultStatus,
			"data":   data,
		},
	})
	return events
}

func batchMetricEvent(name string, value float64, unit string) RawJobEvent {
	return RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypeMetric,
		Data: map[string]any{
			"name":  name,
			"value": value,
			"unit":  unit,
		},
	}
}

// testBatchMetrics derives the canonical test/coverage metrics from a batched
// test result's data payload, in the order a solo run emits them.
func testBatchMetrics(data map[string]any) []TaskMetric {
	var metrics []TaskMetric
	summary := eventField(data, "testSummary")
	for _, metric := range []struct {
		field string
		name  string
	}{
		{field: "total", name: "tests-total"},
		{field: "passed", name: "tests-passed"},
		{field: "failed", name: "tests-failed"},
		{field: "skipped", name: "tests-skipped"},
	} {
		if value, ok := eventFloat(summary, metric.field); ok {
			metrics = append(metrics, TaskMetric{Name: metric.name, Value: value, Unit: "count"})
		}
	}
	coverage := eventField(data, "coverageSummary")
	for _, metric := range []struct {
		field string
		name  string
		unit  string
	}{
		{field: "percentage", name: "coverage", unit: "percent"},
		{field: "totalStatements", name: "coverage-statements-total", unit: "count"},
		{field: "coveredStatements", name: "coverage-statements-covered", unit: "count"},
	} {
		if value, ok := eventFloat(coverage, metric.field); ok {
			metrics = append(metrics, TaskMetric{Name: metric.name, Value: value, Unit: metric.unit})
		}
	}
	return metrics
}

func testBatchSummary(data map[string]any, testFailed bool) string {
	var parts []string
	failedCount := float64(0)
	if summary, ok := data["testSummary"].(map[string]any); ok {
		passed, _ := batchNumber(summary["passed"])
		total, _ := batchNumber(summary["total"])
		failed, _ := batchNumber(summary["failed"])
		failedCount = failed
		skipped, _ := batchNumber(summary["skipped"])
		if total > 0 {
			parts = append(parts, fmt.Sprintf("%.0f/%.0f passed", passed, total))
		}
		if failed > 0 {
			parts = append(parts, fmt.Sprintf("%.0f failed", failed))
		}
		if skipped > 0 {
			parts = append(parts, fmt.Sprintf("%.0f skipped", skipped))
		}
		if omitted, ok := batchNumber(summary["failureDetailsTruncated"]); ok && omitted > 0 {
			parts = append(parts, fmt.Sprintf("%.0f failure detail(s) omitted", omitted))
		}
	}
	if testFailed && failedCount == 0 {
		parts = append(parts, "test run failed")
	}
	if coverage, ok := data["coverageSummary"].(map[string]any); ok {
		if percentage, ok := batchNumber(coverage["percentage"]); ok {
			parts = append(parts, fmt.Sprintf("%.1f%% coverage", percentage))
		}
	}
	return strings.Join(parts, ", ")
}

func batchExtensionJobName(job *ScheduledJob) string {
	if job != nil && job.JobDef != nil {
		for _, arg := range job.JobDef.Args {
			switch arg {
			case "lint", "lint-format", "lint-check", "test":
				return arg
			}
		}
	}
	return jobCommandName(job)
}

// lintSummaryText renders the lint summary line from the CANONICAL metrics
// (lint-errors / lint-warnings), not from the wire summary those metrics were
// derived from. Same numbers, one source: a counter the canonical result does
// not carry can no longer appear in the rendered summary.
func lintSummaryText(metrics []TaskMetric) string {
	var parts []string
	for _, item := range []struct {
		metric string
		name   string
	}{
		{metric: "lint-errors", name: "error"},
		{metric: "lint-warnings", name: "warning"},
	} {
		count := int(taskMetricValue(metrics, item.metric))
		if count == 0 {
			continue
		}
		suffix := ""
		if count != 1 {
			suffix = "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s%s", count, item.name, suffix))
	}
	return strings.Join(parts, ", ")
}

// taskMetricValue reads one named metric off a canonical result, or 0.
func taskMetricValue(metrics []TaskMetric, name string) float64 {
	for _, metric := range metrics {
		if metric.Name == name {
			return metric.Value
		}
	}
	return 0
}

func batchPhaseName(job *ScheduledJob) string {
	if job != nil && job.JobDef != nil {
		for _, arg := range job.JobDef.Args {
			switch arg {
			case "lint-format":
				return "format"
			case "lint", "lint-check":
				return "lint"
			// Build producers name their solo phase after the build kind, not the
			// pipeline step id; derive it from the task arg so the reconstructed
			// phase name matches the solo emit.PhaseStart/PhaseEnd exactly.
			case "build-generate":
				return "generate"
			case "build-transpile":
				return "transpile"
			case "build-types":
				return "types"
			case "build-compile":
				return "compile"
			}
		}
	}
	if job != nil && job.StepID() != "" {
		return job.StepID()
	}
	return "batch"
}

// sharedFailureResults is the fail-closed answer for an aggregate that cannot
// be attributed: every member takes the shared invocation's outcome and its
// diagnostics. A member is never silently reported green from an aggregate the
// split could not read, and each failed member is then free to re-execute solo
// under the runner's per-task retry policy.
func sharedFailureResults(work []taskWork, aggregate *JobResult) map[string]*JobResult {
	results := make(map[string]*JobResult, len(work))
	for i := range work {
		result := &JobResult{Status: "failed"}
		if aggregate != nil {
			*result = *aggregate
			result.Events = append([]RawJobEvent(nil), aggregate.Events...)
			if result.Status == "success" {
				result.Status = "failed"
			}
		}
		if result.Status != "canceled" && result.Error == nil {
			result.Error = &JobError{Message: "batch subprocess did not return per-project results"}
		}
		results[work[i].job.Key()] = result
	}
	return results
}
