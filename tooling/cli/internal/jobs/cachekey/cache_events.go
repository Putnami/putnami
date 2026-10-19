package cachekey

import (
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/store"
)

// sourceMutationDataKey is cache-private metadata carried in ActionResult.Data
// so both local and remote cache entries retain it without widening the cache
// protocol. It is removed before results reach renderers or callers.
const sourceMutationDataKey = "__putnami_source_mutation_v1"

func JobResultFromEntryResult(result *store.EntryResult) *jobmodel.JobResult {
	// Reuse provenance is intentionally applied by the caller: an identical
	// stored result can be an ordinary warm hit or a coalesced cold miss.
	out := &jobmodel.JobResult{}
	if result == nil {
		return out
	}
	out.Status = result.Status
	out.Data, out.SourceMutated = resultDataWithoutSourceMutation(result.Data)
	out.Events = rawEventsFromCache(result.Events)
	if result.Error != nil {
		out.Error = &jobmodel.JobError{Message: result.Error.Message, Code: result.Error.Code}
	}
	return out
}

func EntryResultFromJobResult(result *jobmodel.JobResult) *store.EntryResult {
	out := &store.EntryResult{}
	if result == nil {
		return out
	}
	out.Status = result.Status
	out.Data = resultDataWithSourceMutation(result.Data, result.SourceMutated)
	out.Events = cacheEventsFromRaw(result.Events)
	if result.Error != nil {
		out.Error = &store.EntryError{Message: result.Error.Message, Code: result.Error.Code}
	}
	return out
}

// FailureEntryResultFromJobResult encodes a FAILED result for a negative entry.
// It differs from EntryResultFromJobResult only in the event allow-list: the
// source-mutation envelope is not applied, because only a successful
// source-writing task can claim it.
func FailureEntryResultFromJobResult(result *jobmodel.JobResult) *store.EntryResult {
	out := &store.EntryResult{}
	if result == nil {
		return out
	}
	out.Status = result.Status
	out.Data = result.Data
	out.Events = failureCacheEventsFromRaw(result.Events)
	if result.Error != nil {
		out.Error = &store.EntryError{Message: result.Error.Message, Code: result.Error.Code}
	}
	return out
}

func JobResultFromActionResult(result *cache.ActionResult) *jobmodel.JobResult {
	out := &jobmodel.JobResult{}
	if result == nil {
		return out
	}
	out.Status = result.Status
	out.Data, out.SourceMutated = resultDataWithoutSourceMutation(result.Data)
	out.Events = rawEventsFromCache(result.Events)
	if result.Error != nil {
		out.Error = &jobmodel.JobError{Message: result.Error.Message, Code: result.Error.Code}
	}
	return out
}

func EntryResultFromActionResult(result *cache.ActionResult) *store.EntryResult {
	out := &store.EntryResult{}
	if result == nil {
		return out
	}
	out.Status = result.Status
	out.Data = result.Data
	out.Events = result.Events
	if result.Error != nil {
		out.Error = &store.EntryError{Message: result.Error.Message, Code: result.Error.Code}
	}
	return out
}

// resultDataWithSourceMutation adds the cache-private marker without mutating
// extension-owned result data. The inverse helper keeps the marker out of
// renderer output and replayed result events.
func resultDataWithSourceMutation(data map[string]any, sourceMutated bool) map[string]any {
	if !sourceMutated {
		return data
	}
	out := make(map[string]any, len(data)+1)
	for key, value := range data {
		out[key] = value
	}
	out[sourceMutationDataKey] = true
	return out
}

// resultDataWithoutSourceMutation returns public extension data and the
// cache-private mutation bit. Entries without that bit retain the existing
// allocation behavior.
func resultDataWithoutSourceMutation(data map[string]any) (map[string]any, bool) {
	if data == nil {
		return nil, false
	}
	mutated, _ := data[sourceMutationDataKey].(bool)
	if !mutated {
		return data, false
	}
	out := make(map[string]any, len(data)-1)
	for key, value := range data {
		if key != sourceMutationDataKey {
			out[key] = value
		}
	}
	return out, true
}

func ActionResultFromEntryResult(result *store.EntryResult, durationMs, sizeBytes int64, includeEvents bool) *cache.ActionResult {
	out := &cache.ActionResult{DurationMs: durationMs, SizeBytes: sizeBytes}
	if result == nil {
		return out
	}
	out.Status = result.Status
	out.Data = result.Data
	if includeEvents {
		out.Events = result.Events
	}
	if result.Error != nil {
		out.Error = &cache.ActionError{Message: result.Error.Message, Code: result.Error.Code}
	}
	return out
}

func cacheEventsFromRaw(events []jobmodel.RawJobEvent) []cache.ActionEvent {
	return filteredCacheEvents(events, func(ev jobmodel.RawJobEvent) bool { return cacheResultEventType(ev.Type) })
}

// failureCacheEventsFromRaw is the same projection for a NEGATIVE entry, with
// one deliberate difference: it retains error and warning log events.
//
// A positive entry drops logs because a cache hit only has to replay
// report-relevant records. A replayed FAILURE has to reproduce what the user
// saw, and output/failure_details.go rebuilds that detail from exactly those
// log events — so dropping them would make a replayed failure quieter than a
// fresh one. Info and debug logs stay out: they are progress narration, not
// failure detail, and they are what would make the record large.
func failureCacheEventsFromRaw(events []jobmodel.RawJobEvent) []cache.ActionEvent {
	return filteredCacheEvents(events, failureResultEventType)
}

func filteredCacheEvents(events []jobmodel.RawJobEvent, keep func(jobmodel.RawJobEvent) bool) []cache.ActionEvent {
	var out []cache.ActionEvent
	for _, ev := range events {
		if !keep(ev) {
			continue
		}
		out = append(out, cache.ActionEvent{
			Version: ev.Version,
			Type:    ev.Type,
			Time:    ev.Time,
			Level:   ev.Level,
			Message: ev.Message,
			Data:    CloneEventData(ev.Data),
		})
	}
	return out
}

func rawEventsFromCache(events []cache.ActionEvent) []jobmodel.RawJobEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]jobmodel.RawJobEvent, 0, len(events))
	for _, ev := range events {
		if ev.Type == "" {
			continue
		}
		out = append(out, jobmodel.RawJobEvent{
			Version: ev.Version,
			Type:    ev.Type,
			Time:    ev.Time,
			Level:   ev.Level,
			Message: ev.Message,
			Data:    CloneEventData(ev.Data),
		})
	}
	return out
}

// RestoreReservedDeclaredArtifactEvents recovers the structural artifact
// records a task-owned cache entry can prove even when its ActionResult predates
// event persistence or came from a provider without the action-events
// capability. The entry descriptor — not a scan of the live output tree — is
// authoritative: only a PRESENT, exact, command-output file can be recovered,
// so an optional EMPTY output never resurrects a stale file left by another
// session.
//
// Most artifacts cannot be reconstructed from a filesystem declaration alone:
// its output id, display name and kind are runtime result data. These two are
// different because protocols/features reserves their identity and filename,
// and the producer rule (jobmodel.ProducesVerificationReport,
// jobmodel.ProducesSpecCriteriaProjection) names the task that writes each: a
// task of a verification or criteria command that declares the file. They are
// also correctness inputs: the spec gate reads them after the scheduler
// finishes, and losing either on a warm hit can fail closed on valid evidence
// (or silently omit the criteria to enforce).
func RestoreReservedDeclaredArtifactEvents(job *jobmodel.ScheduledJob, entry *store.TaskEntry, result *jobmodel.JobResult) {
	if job == nil || entry == nil || result == nil {
		return
	}
	for _, output := range entry.Outputs {
		if !output.Present() || output.Kind != extensionproto.OutputKindFile ||
			output.Root != extensionproto.OutputRootCommandOutput {
			continue
		}

		var id, name string
		switch {
		case output.Path == features.VerificationReportFilename && jobmodel.ProducesVerificationReport(job):
			id = features.VerificationReportArtifactID
			name = "Feature Verification Report"
		case output.Path == features.SpecCriteriaProjectionFilename && jobmodel.ProducesSpecCriteriaProjection(job):
			id = features.SpecCriteriaProjectionArtifactID
			name = "Executable criteria projection"
		default:
			continue
		}
		if resultHasArtifact(result, id) {
			continue
		}
		result.Events = append(result.Events, TaskArtifactEvent(jobmodel.TaskArtifact{
			ID: id, Name: name, Kind: "report", Path: output.Path,
		}))
	}
}

func resultHasArtifact(result *jobmodel.JobResult, id string) bool {
	for _, event := range result.Events {
		artifactID, _ := event.Data["id"].(string)
		if event.Type == jobmodel.EventTypeArtifact && artifactID == id {
			return true
		}
	}
	return false
}

// TaskArtifactEvent is the one projection of a canonical artifact onto the
// runtime event stream. Batch results and descriptor-recovered cache results
// use it so the two synthesized paths cannot drift in version or field shape.
func TaskArtifactEvent(artifact jobmodel.TaskArtifact) jobmodel.RawJobEvent {
	return jobmodel.RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    jobmodel.EventTypeArtifact,
		Data: map[string]any{
			"id":   artifact.ID,
			"name": artifact.Name,
			"kind": artifact.Kind,
			"path": artifact.Path,
		},
	}
}

// cacheResultEventType is the allow-list of event types worth persisting in a
// cache entry so a cache hit can replay the report-relevant records a cold run
// emitted. EventTypeResult carries the job's testSummary/coverageSummary in its
// Data, so it MUST be retained (its omission dropped those summaries from warm
// JSONL streams). Logs/progress/phase are display-only and intentionally
// not cached, per the contract that logs need not be replayed for reporting.
func cacheResultEventType(eventType string) bool {
	switch eventType {
	case jobmodel.EventTypeMeta, jobmodel.EventTypeMetric, jobmodel.EventTypeArtifact, jobmodel.EventTypeDiagnostic, jobmodel.EventTypeSummary, jobmodel.EventTypeResult:
		return true
	default:
		return false
	}
}

// failureResultEventType is the allow-list a NEGATIVE entry persists: whatever
// a positive entry keeps, plus the error/warning log events the human failure
// detail is rebuilt from.
func failureResultEventType(event jobmodel.RawJobEvent) bool {
	if cacheResultEventType(event.Type) {
		return true
	}
	if event.Type != jobmodel.EventTypeLog {
		return false
	}
	switch strings.ToLower(failureEventLevel(event)) {
	case "error", "warn", "warning", "fatal":
		return true
	default:
		return false
	}
}

// failureEventLevel reads a log event's level from the flat field or from the
// folded Data map, the two shapes ParseRawEvent can leave behind.
func failureEventLevel(event jobmodel.RawJobEvent) string {
	if event.Level != "" {
		return event.Level
	}
	if level, _ := event.Data["level"].(string); level != "" {
		return level
	}
	return ""
}

// SynthesizeResultEvent reconstructs the "result" event a cold run streams from
// a restored JobResult's Status/Data/Error. Cache keys do not include the CLI
// version, so entries written before result events were captured stay
// valid across an upgrade: they carry result.Data but no cached result event,
// so a warm replay would emit only start/end and drop testSummary/coverageSummary
// from the JSONL stream until the entry is rebuilt or evicted. Rebuilding an
// equivalent record lets legacy warm streams fold to the same summaries as cold.
//
// The event mirrors the on-wire shape a subprocess emits — Data nests the report
// payload under "data" and the outcome under "status"/"error", stamped with the
// version the CLI advertises and is the only one it accepts back (an earlier
// format stamped v1 until then). A replayed job:event is therefore indistinguishable
// from a freshly captured one, version included. Returns false when there is
// nothing report-relevant to synthesize (no data and no error), leaving
// data-less jobs (e.g. plain builds) untouched.
func SynthesizeResultEvent(result *jobmodel.JobResult) (jobmodel.RawJobEvent, bool) {
	if result == nil || (len(result.Data) == 0 && result.Error == nil) {
		return jobmodel.RawJobEvent{}, false
	}
	data := map[string]any{}
	if result.Status != "" {
		data["status"] = result.Status
	}
	if len(result.Data) > 0 {
		data["data"] = result.Data
	}
	if result.Error != nil {
		errData := map[string]any{}
		if result.Error.Message != "" {
			errData["message"] = result.Error.Message
		}
		if result.Error.Code != "" {
			errData["code"] = result.Error.Code
		}
		data["error"] = errData
	}
	return jobmodel.RawJobEvent{
		Version: runtimeproto.MaxKnownProtocolVersion,
		Type:    jobmodel.EventTypeResult,
		Data:    data,
	}, true
}

func CloneEventData(data map[string]any) map[string]any {
	if len(data) == 0 {
		return nil
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	return out
}
