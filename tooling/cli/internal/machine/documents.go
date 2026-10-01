package machine

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"time"

	"go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/distribution"
	protocoljob "go.putnami.dev/protocol/job"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// The two classes of protocols/cli's closed error vocabulary a v2 machine
// document can carry. The protocol exports the sentinel ERRORS (ErrSignal, …)
// and derives the class from them in ErrorCode; these spellings are named here
// so the projections below read like the contract, and machine_test.go pins
// both against protocolcli.ErrorCode so a rename there cannot pass silently.
const (
	errorClassFailure = "failure"
	errorClassSignal  = "signal"
)

// eventPayload is the opaque runtime-event object a task:event record carries.
// protocols/runtime owns its members and versions them; the result
// contract treats it as an object it does not inspect. The alias exists so this
// package names the untyped payload exactly once.
type eventPayload = map[string]any

// noEventPayload is what a producer that supplied no payload contributes. The
// record's event member is required to be an OBJECT, so an absent payload is an
// empty one, never null.
var noEventPayload = eventPayload{}

// Envelope is the --output=json document: one object per invocation.
//
// Unlike v1 it reports the run as the typed run member instead of a free-form
// data payload, and an interrupted run as status "aborted" with exit code 130 —
// the process ordering, which v1's envelope deliberately inverted.
func (r Run) Envelope(command string, durationMs int64) protocolcli.ResultV2 {
	run := r.Summary(durationMs)
	envelope := protocolcli.ResultV2{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Command:         orUnknown(command),
		Status:          run.Outcome,
		ExitCode:        run.ExitCode,
		Run:             &run,
	}
	switch run.Outcome {
	case protocolcli.RunOutcomeAborted:
		// Classified as a signal so the envelope's error class and its verdict
		// agree; the failures the run did collect stay visible in run.counts and
		// run.failures, which is what makes abort-first cost the reader nothing.
		envelope.Error = &protocolcli.ResultError{
			Code:    errorClassSignal,
			Message: abortMessage(r.abortedBy()),
		}
	case protocolcli.RunOutcomeFailure:
		envelope.Error = &protocolcli.ResultError{
			Code:    errorClassFailure,
			Message: failureMessage(run.Counts.Failed),
		}
	}
	return envelope
}

// TaskStart is the task:start stream record.
func TaskStart(job *jobs.ScheduledJob, at time.Time) protocolcli.SessionStreamRecord {
	identity := Identity(job)
	return protocolcli.SessionStreamRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordTaskStart,
		Time:            stamp(at),
		Identity:        &identity,
	}
}

// TaskEvent is the task:event stream record. The record's own time is the
// event's, so the stream keeps the producer's clock rather than restamping it
// with the moment the CLI happened to forward it.
//
// A result event's payload loses its data.testCases: every case travels once,
// in its own test:case record (TaskEndRecords).
func TaskEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent, at time.Time) protocolcli.SessionStreamRecord {
	identity := Identity(job)
	event.Data = withoutResultTestCases(event)
	return taskEvent(identity, event, at)
}

// BatchTaskEvent records one event emitted by a shared batch subprocess before
// the runner's bounded per-task retention. The producer has not attributed the
// line to a member, so a stable workspace-scoped batch identity is truthful;
// pretending it belonged to the leader project would not be.
//
// A result event's payload loses the testCases of every member entry: each
// member's cases travel once, in test:case records under the member's own
// identity (TaskEndRecords).
func BatchTaskEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent, at time.Time) protocolcli.SessionStreamRecord {
	identity := Identity(job)
	identity.Scope = protocolcli.TaskScopeWorkspace
	identity.Project = protocolcli.ProjectIdentity{ID: "/workspace", Name: "workspace"}
	identity.Task.Name += "-batch"
	identity.Task.Kind += "-batch"
	identity.Key = identity.DerivedKey()
	event.Data = withoutBatchResultTestCases(event)
	return taskEvent(identity, event, at)
}

// withoutResultTestCases returns a result event's payload without its
// data.testCases, and any other event's payload unchanged. The payload's data
// object is the task's result data, which the canonical projection and the
// cache entry read after this record is written, so the strip copies every
// object it changes and never writes to the payload it was given.
func withoutResultTestCases(event jobs.RawJobEvent) eventPayload {
	payload := event.Data
	if !isResultEvent(event) {
		return payload
	}
	data, _ := payload["data"].(eventPayload)
	stripped, changed := withoutTestCasesKey(data)
	if !changed {
		return payload
	}
	out := maps.Clone(payload)
	out["data"] = stripped
	return out
}

// withoutBatchResultTestCases returns a batch result event's payload without
// the testCases of each data.batchResults[i].data, and any other event's
// payload unchanged. Like withoutResultTestCases, it copies every object and
// array it changes.
func withoutBatchResultTestCases(event jobs.RawJobEvent) eventPayload {
	payload := event.Data
	if !isResultEvent(event) {
		return payload
	}
	data, _ := payload["data"].(eventPayload)
	members, _ := data[jobs.BatchResultsDataKey].([]any)
	var strippedMembers []any
	for i, member := range members {
		entry, _ := member.(eventPayload)
		memberData, _ := entry["data"].(eventPayload)
		strippedData, changed := withoutTestCasesKey(memberData)
		if !changed {
			continue
		}
		if strippedMembers == nil {
			strippedMembers = slices.Clone(members)
		}
		strippedEntry := maps.Clone(entry)
		strippedEntry["data"] = strippedData
		strippedMembers[i] = strippedEntry
	}
	if strippedMembers == nil {
		return payload
	}
	strippedBatch := maps.Clone(data)
	strippedBatch[jobs.BatchResultsDataKey] = strippedMembers
	out := maps.Clone(payload)
	out["data"] = strippedBatch
	return out
}

// isResultEvent reports a runtime result event. A parsed subprocess line
// carries its type in both places; a result the CLI synthesizes (a cache
// replay, a batch member's projection) carries it only on the event.
func isResultEvent(event jobs.RawJobEvent) bool {
	return event.Type == jobs.EventTypeResult || event.Data["type"] == jobs.EventTypeResult
}

// withoutTestCasesKey returns a copy of data without its testCases member, and
// whether data had one.
func withoutTestCasesKey(data eventPayload) (eventPayload, bool) {
	if _, present := data[runtimeproto.TestCasesResultDataKey]; !present {
		return data, false
	}
	out := maps.Clone(data)
	delete(out, runtimeproto.TestCasesResultDataKey)
	return out, true
}

func taskEvent(identity protocolcli.TaskIdentity, event jobs.RawJobEvent, at time.Time) protocolcli.SessionStreamRecord {
	record := protocolcli.SessionStreamRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordTaskEvent,
		Time:            event.Time,
		Identity:        &identity,
		Event:           event.Data,
	}
	if record.Time == "" {
		record.Time = stamp(at)
	}
	if record.Event == nil {
		record.Event = noEventPayload
	}
	return record
}

// ReleaseSetResultEvent projects the finalizer's one successful typed result
// back onto the canonical runtime v1 wire nested inside the CLI v2 session
// stream. The finalizer runs after ordinary task-event callbacks have ended, so
// Finish is the only point where renderers can record the deployment handoff.
//
// Every rejection is deliberately silent. Machine output must never promote a
// malformed, duplicate, unsuccessful, or partial coordination result into
// deployment evidence merely because it occupied the well-known data key.
func ReleaseSetResultEvent(
	run Run,
	results map[string]*jobs.JobResult,
	at time.Time,
) (protocolcli.SessionStreamRecord, bool) {
	if run.Summary(0).Outcome != protocolcli.RunOutcomeSuccess {
		return protocolcli.SessionStreamRecord{}, false
	}

	var candidateKey string
	var candidate *jobs.JobResult
	candidates := 0
	for key, result := range results {
		if result == nil || result.Data == nil {
			continue
		}
		if _, present := result.Data[runtimeproto.ReleaseSetResultDataKey]; !present {
			continue
		}
		candidates++
		candidateKey = key
		candidate = result
	}
	if candidates != 1 || candidate == nil || candidate.Status != string(jobs.TaskStatusSuccess) || len(candidate.Data) != 1 {
		return protocolcli.SessionStreamRecord{}, false
	}

	rawOutcome, diagnostics := runtimeproto.ExtractReleaseSetPublishOutcome(candidate.Data)
	if len(rawOutcome) == 0 || diagnostic.HasErrors(diagnostics) {
		return protocolcli.SessionStreamRecord{}, false
	}
	outcome, diagnostics := distribution.ParseAndValidateReleaseSetPublishOutcome(rawOutcome)
	if outcome == nil || diagnostic.HasErrors(diagnostics) {
		return protocolcli.SessionStreamRecord{}, false
	}

	resultValues := maps.Clone(candidate.Data)
	resultValues[runtimeproto.ReleaseSetResultDataKey] = outcome
	resultData, err := json.Marshal(runtimeproto.ResultData{Status: runtimeproto.ResultOK, Data: resultValues})
	if err != nil {
		return protocolcli.SessionStreamRecord{}, false
	}
	runtimeEvent, err := json.Marshal(runtimeproto.Event{
		V:    runtimeproto.ProtocolVersion,
		Type: runtimeproto.EventResult,
		Data: resultData,
	})
	if err != nil {
		return protocolcli.SessionStreamRecord{}, false
	}
	var event eventPayload
	if err := json.Unmarshal(runtimeEvent, &event); err != nil {
		return protocolcli.SessionStreamRecord{}, false
	}

	return taskEvent(
		jobs.TaskIdentityOfKey(candidateKey),
		jobs.RawJobEvent{
			Version: runtimeproto.ProtocolVersion,
			Type:    string(runtimeproto.EventResult),
			Data:    event,
		},
		at,
	), true
}

// TaskEndRecords are the records that close a task in the session stream: one
// test:case record per test case the task reported, in runner order, then its
// task:end. A renderer emits them as one group that no other record splits, so
// a reader meets every case of a task right before the task's verdict.
func TaskEndRecords(task Task, at time.Time) []protocolcli.SessionStreamRecord {
	records := make([]protocolcli.SessionStreamRecord, 0, len(task.Result.TestCases)+1)
	for _, testCase := range task.Result.TestCases {
		identity := task.Identity
		records = append(records, protocolcli.SessionStreamRecord{
			ProtocolVersion: protocolcli.ResultProtocolVersion,
			Record:          protocolcli.RecordTestCase,
			Time:            stamp(at),
			Identity:        &identity,
			TestCase:        &testCase,
		})
	}
	return append(records, TaskEnd(task, at))
}

// TaskEnd is the task:end stream record.
func TaskEnd(task Task, at time.Time) protocolcli.SessionStreamRecord {
	identity := task.Identity
	record := TaskRecord(task)
	return protocolcli.SessionStreamRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordTaskEnd,
		Time:            stamp(at),
		Identity:        &identity,
		Task:            &record,
	}
}

// SessionEnd is the session:end stream record, closing --output=jsonl with the
// same run summary the envelope reports.
func (r Run) SessionEnd(at time.Time, durationMs int64) protocolcli.SessionStreamRecord {
	run := r.Summary(durationMs)
	return protocolcli.SessionStreamRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordSessionEnd,
		Time:            stamp(at),
		Run:             &run,
	}
}

// MCPRun is the run_jobs result document.
func (r Run) MCPRun(commands []string) *protocolcli.MCPResult {
	run := r.Summary(millis(r.duration()))
	return &protocolcli.MCPResult{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Tool:            protocolcli.MCPToolRunJobs,
		Commands:        commandList(commands),
		Run:             &run,
	}
}

// MCPPlan is the plan_jobs result document.
func MCPPlan(commands []string, planned []*jobs.ScheduledJob) *protocolcli.MCPResult {
	plan := PlanSummary(planned)
	return &protocolcli.MCPResult{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Tool:            protocolcli.MCPToolPlanJobs,
		Commands:        commandList(commands),
		Plan:            &plan,
	}
}

// PlanSummary projects a scheduled plan onto the v2 plan document.
func PlanSummary(planned []*jobs.ScheduledJob) protocolcli.PlanSummary {
	metrics := jobs.ComputePlanMetrics(planned)
	return protocolcli.PlanSummary{
		DryRun: true,
		Metrics: protocolcli.PlanMetrics{
			Tasks:     metrics.Jobs,
			Edges:     metrics.Edges,
			Projects:  metrics.Projects,
			ByCommand: metrics.ByCommand,
		},
		Tasks: PlannedTasks(planned),
	}
}

// PlannedTasks projects the plan's nodes onto v2 planned tasks. Edges keep
// referencing other tasks by their derived identity key, which is the v1 plan
// key unchanged.
func PlannedTasks(planned []*jobs.ScheduledJob) []protocolcli.PlannedTask {
	tasks := make([]protocolcli.PlannedTask, 0, len(planned))
	for _, job := range planned {
		task := protocolcli.PlannedTask{
			Identity:  Identity(job),
			DependsOn: append([]string(nil), job.DependsOn...),
			After:     append([]string(nil), job.SerializeAfter...),
		}
		if job.JobDef != nil {
			task.Cache = job.JobDef.Cache
		}
		tasks = append(tasks, task)
	}
	return tasks
}

// SessionPlanFile is the recorded plan.json document.
func SessionPlanFile(sessionID string, commands []string, planned []*jobs.ScheduledJob) *protocolcli.SessionPlanFile {
	return &protocolcli.SessionPlanFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		SessionID:       orUnknown(sessionID),
		Commands:        commandList(commands),
		Tasks:           PlannedTasks(planned),
	}
}

// SessionFile is the recorded session.json document, minus the two members the
// writer stamps when it closes the session (endTime, and the run's wall
// duration) — see workspace_state.Session.FinalizeV2. The run's CPU allocation
// is derived from the duration stated here, so the writer re-derives it over
// the wall it stamps.
func (r Run) SessionFile(commands []string, scheduler, cache any) *protocolcli.SessionFile {
	return &protocolcli.SessionFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Commands:        commandList(commands),
		Run:             r.Summary(millis(r.duration())),
		Tasks:           r.Records(),
		Executions:      r.Executions(),
		Environment:     r.Environment(),
		Preparation:     r.Preparation(),
		Scheduler:       scheduler,
		Cache:           cache,
	}
}

// SessionSelection projects the run's resolved selection onto the recorded
// session document's own selection block.
//
// The two types are declared separately because protocols/cli must stay
// readable without protocols/job — a recorded session is read by consumers that
// know nothing about job contexts — so the mode vocabulary is repeated by value
// there and pinned equal by TestSessionSelectionModesMatchTheJobContract.
//
// The baseline is deliberately NOT repeated here. The session's git block
// already records the ref selection was computed against, and the same value in
// two members is how the two start disagreeing; mode answers the question the
// git block cannot, which is whether the projects were named or derived.
//
// The release-set half IS repeated, because nothing else in the record carries
// it: a session that published beside its gate planned the union of two
// selections, and a reader that cannot subtract one from the other reads a
// project the run only verified as one it published.
func SessionSelection(selection *protocoljob.Selection) *protocolcli.SessionSelection {
	if selection == nil || selection.Mode == "" {
		return nil
	}
	return &protocolcli.SessionSelection{
		Mode:               selection.Mode,
		Scoped:             selection.Scoped,
		Projects:           selection.ProjectIDs,
		ReleaseSetProjects: selection.ReleaseSetProjects,
	}
}

func (r Run) duration() time.Duration {
	if r.Session == nil {
		return 0
	}
	return r.Session.Duration
}

func (r Run) abortedBy() string {
	if r.Session == nil {
		return ""
	}
	return r.Session.AbortedBy
}

// commandList never returns nil: commands is a required array member.
func commandList(commands []string) []string {
	if commands == nil {
		return []string{}
	}
	return commands
}

func failureMessage(failed int) string {
	if failed == 1 {
		return "1 task failed"
	}
	return strconv.Itoa(failed) + " tasks failed"
}

func stamp(at time.Time) string { return at.Format(time.RFC3339Nano) }
