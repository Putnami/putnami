// CONFINEMENT, second half: no needle reaches a surface that publishes.
//
// invocation_needles.go decides what the secrets ARE. The invariant this file
// owns is that none of them leaves the process — not on the live event stream,
// not in a stored result, a cache entry, a session record or on the terminal —
// and that a participant which emitted one is FAILED rather than reported
// green.
//
// Three properties are what make that checkable in one place:
//
//   - ONE MATCHER. redactSensitive is used by the event path and by the result
//     path, so "what counts as a leak" cannot differ between what a user sees
//     live and what is persisted. The one exemption — a declared, non-sensitive
//     output's own display path — is granted at an EXACT spelling the
//     declaration owns, never by suffix, so nothing can be smuggled in beside
//     it.
//   - SCRUB FIRST, FAIL SECOND. Scrubbing keeps the value out of the surfaces
//     that already hold a reference to the result; failing is what the contract
//     requires, because a task that emitted a secret has already lost control of
//     it and reporting success would publish it under a reusable cache key.
//   - THE PRODUCER'S WINDOW. Every other participant runs after the needles
//     exist, so its events are scrubbed as they arrive. The producer emits
//     BEFORE anything has been written, so there is no set to scrub it with: its
//     stream is withheld and released, redacted, once its artifacts have been
//     validated. That hold is the one piece of confinement that is a LIFETIME
//     rather than a filter, which is why it lives beside the filter it hands
//     off to instead of with the lease lifecycle.
package jobs

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// redactedMarker replaces a leaked value on the live event stream. The task is
// failed anyway; redacting first is what keeps the value off the terminal and
// out of any renderer that persists what it displayed.
const redactedMarker = "[redacted: sensitive invocation artifact]"

// producerEventSpool is the producer's pre-needle event stream. Its file lives
// in the owner-only invocation root, beside (not inside) the ArtifactRoot
// exposed to the subprocess. A pointer and a small encoder are the only
// per-stream memory: noisy producers grow private disk usage, not the
// scheduler's heap.
type producerEventSpool struct {
	file   *os.File
	path   string
	failed bool
}

// redactSensitive replaces every needle in text and reports whether it found
// one. It is the single matcher used by the event stream and the result guard,
// so "what counts as a leak" cannot differ between them.
func redactSensitive(text string, needles []string) (string, bool) {
	if text == "" || len(needles) == 0 {
		return text, false
	}
	leaked := false
	for _, needle := range needles {
		if needle == "" || !strings.Contains(text, needle) {
			continue
		}
		leaked = true
		text = strings.ReplaceAll(text, needle, redactedMarker)
	}
	return text, leaked
}

// redactEvent scrubs one runtime event in place and reports whether it carried a
// sensitive value.
func redactEvent(job *ScheduledJob, event *RawJobEvent, needles []string) bool {
	if event == nil || len(needles) == 0 {
		return false
	}
	leaked := false
	if scrubbed, hit := redactSensitive(event.Message, needles); hit {
		event.Message = scrubbed
		leaked = true
	}
	path, declared := declaredArtifactPath(job, event)
	if declared {
		delete(event.Data, "path")
	}
	if redactData(event.Data, needles) {
		leaked = true
	}
	if declared {
		event.Data["path"] = path
	}
	return leaked
}

// declaredArtifactPath identifies the one event field that is trusted as a
// non-sensitive output locator. A byte-derived needle can legitimately be a
// segment of a declared report's path, but only at the exact display path the
// declaration owns. All other event fields, including an undeclared, sensitive,
// or mismatched artifact path, remain subject to the leak guard.
func declaredArtifactPath(job *ScheduledJob, event *RawJobEvent) (string, bool) {
	if event == nil || event.Type != EventTypeArtifact || event.Data == nil {
		return "", false
	}
	artifactPath, ok := event.Data["path"].(string)
	if !ok {
		return "", false
	}
	id, ok := event.Data["id"].(string)
	if !ok {
		return "", false
	}
	declaration := taskDeclarationOf(job)
	if declaration == nil {
		return "", false
	}
	output, ok := declaration.Outputs[id]
	if !ok || output.Sensitive || output.PathFrom != "" {
		return "", false
	}
	return artifactPath, matchesDeclaredArtifactPath(job, output, artifactPath)
}

// matchesDeclaredArtifactPath accepts the normalized display spellings the
// runner can emit for a literal declared output. The short command form is
// retained for the pre-v3 event shape; the runner's current mapper
// emits the .putnami/out form. A suffix match would let a task smuggle a secret
// before a declared filename, so every accepted spelling is exact.
func matchesDeclaredArtifactPath(job *ScheduledJob, output extension.DeclaredOutput, artifactPath string) bool {
	outputPath, err := extension.NormalizeOutputPath(output.Path)
	if err != nil {
		return false
	}
	actual := path.Clean(strings.ReplaceAll(artifactPath, "\\", "/"))
	expected := []string{outputPath}
	switch output.EffectiveRoot() {
	case extension.OutputRootCommandOutput:
		commandPath := path.Join(projectRelPath(job), job.CommandName(), outputPath)
		expected = append(expected, commandPath, path.Join(".putnami", "out", commandPath))
	case extension.OutputRootProject:
		expected = append(expected, path.Join(projectRelPath(job), outputPath))
	case extension.OutputRootWorkspace:
		// outputPath already names the workspace-relative display form.
	default:
		return false
	}
	for _, candidate := range expected {
		if actual == candidate {
			return true
		}
	}
	return false
}

// redactData scrubs the string leaves of an untyped payload in place, at any
// depth.
func redactData(data map[string]any, needles []string) bool {
	leaked := false
	for key, value := range data {
		scrubbed, hit := redactValue(value, needles)
		if hit {
			data[key] = scrubbed
			leaked = true
		}
	}
	return leaked
}

// redactValue scrubs ONE untyped payload member and returns what its container
// must now hold, plus whether it carried a sensitive value.
//
// Depth is the whole point. A payload's string leaves are not only its direct
// members: `{"records":[{"dsn":"…"}]}` and `{"rows":[["…"]]}` are the shapes a
// task reports rows, findings or connection details in, and a guard that
// descends into maps but reads an array one level deep — and only when the
// element is DIRECTLY a string — neither redacts them nor reports the leak, so
// the value reaches the live stream and the stored result while the task is
// reported green.
//
// Containers are mutated IN PLACE and returned unchanged, because the caller's
// reference (result.Data, an event's Data, an enclosing slice slot) must keep
// pointing at the scrubbed container. A string cannot be mutated through its
// container, so it is returned by value and assigned back by the caller — which
// is why the map and slice branches assign only when something was hit.
//
// The payload comes from JSON, so it is a finite tree: this recursion cannot
// meet a cycle.
func redactValue(value any, needles []string) (any, bool) {
	switch typed := value.(type) {
	case string:
		scrubbed, hit := redactSensitive(typed, needles)
		return scrubbed, hit
	case map[string]any:
		return typed, redactData(typed, needles)
	case []any:
		leaked := false
		for i, element := range typed {
			scrubbed, hit := redactValue(element, needles)
			if hit {
				typed[i] = scrubbed
				leaked = true
			}
		}
		return typed, leaked
	}
	return value, false
}

// guardResult is the fail-closed check applied to a participant's result before
// anything publishes it — the cache entry, the session record, the renderer's
// completion row.
//
// It scrubs first and fails second. Scrubbing is what keeps the value out of the
// surfaces that already hold a reference to the result; failing is what the
// contract requires, because a task that emitted a secret has already lost
// control of it and reporting success would publish it under a reusable key.
func (rt *invocationRuntime) guardResult(job *ScheduledJob, result *JobResult) *JobResult {
	if rt == nil || result == nil {
		return result
	}
	rel := rt.relationFor(job)
	if rel == nil {
		return result
	}
	leaked := rel.jobLeaked(job)
	needles := rel.needleSet()
	if len(needles) == 0 && !leaked {
		return result
	}

	if result.Error != nil {
		if scrubbed, hit := redactSensitive(result.Error.Message, needles); hit {
			result.Error.Message = scrubbed
			leaked = true
		}
	}
	if redactData(result.Data, needles) {
		leaked = true
	}
	for i := range result.Events {
		if redactEvent(job, &result.Events[i], needles) {
			leaked = true
		}
	}
	if !leaked {
		return result
	}
	return failSensitiveLeak(result)
}

func failSensitiveLeak(result *JobResult) *JobResult {
	if result == nil {
		return result
	}
	result.Status = "failed"
	result.SourceMutated = false
	result.Error = &JobError{
		Code: extensionproto.FailureSensitiveLeakDetected,
		Message: "task emitted a sensitive invocation artifact's path or bytes; " +
			"the value was withheld and the task failed rather than publishing it",
	}
	return result
}

func (rel *invocationRelation) recordLeak(job *ScheduledJob) {
	if rel == nil || job == nil {
		return
	}
	rel.mu.Lock()
	if rel.leakedJobs == nil {
		rel.leakedJobs = make(map[string]bool)
	}
	rel.leakedJobs[job.Key()] = true
	rel.mu.Unlock()
}

func (rel *invocationRelation) jobLeaked(job *ScheduledJob) bool {
	if rel == nil || job == nil {
		return false
	}
	rel.mu.Lock()
	leaked := rel.leakedJobs[job.Key()]
	rel.mu.Unlock()
	return leaked
}

// eventSink wraps a job's live event handler with the leak guard. A job that
// participates in no relation, or whose relation declares no sensitive
// artifact, gets the handler unchanged and pays nothing.
//
// The PRODUCER's sink is different in kind, and that difference is the whole
// confinement window. Every other participant runs after the producer — the
// consumers are downstream of it by manifest rule, the finalizer is dispatched
// by the coordinator — so by the time they emit, the needles derived from the
// producer's artifacts already exist and each event can be scrubbed as it
// arrives. The producer itself emits BEFORE anything has been written, so there
// is no needle set to scrub it with: its events are withheld and released,
// redacted, once its artifacts have been validated.
//
// This is the ONLY path a producer's events take. A batch replays its members'
// streams straight to the renderer (scheduler_batch_exec.go), which would step
// around the hold — but readyBatchKey admits only cacheable jobs, and a task
// with an invocation-scoped output must set cache:false (the manifest's
// `invocation-cache-conflict` rule), so a producer is always a singleton
// dispatch.
func (rt *invocationRuntime) eventSink(job *ScheduledJob, handler EventHandler) EventHandler {
	if rt == nil {
		return handler
	}
	// The producer is matched through byProducer rather than through the
	// participants map, so the hold cannot be lost to whatever else the plan
	// makes this node: the relation it PRODUCES for is the one whose needles do
	// not exist yet.
	if rel := rt.byProducer[job.Key()]; rel != nil {
		if !rel.sensitive {
			return handler
		}
		return rel.holdEvents(handler)
	}
	rel := rt.relationFor(job)
	if rel == nil || !rel.sensitive {
		return handler
	}
	return func(event RawJobEvent) {
		if needles := rel.needleSet(); len(needles) > 0 {
			if redactEvent(job, &event, needles) {
				rel.recordLeak(job)
			}
		}
		handler(event)
	}
}

// holdEvents is the producer's sink: it spools until releaseProducerEvents has
// run, and scrubs live after it.
//
// JobResult.Events is bounded, so this stream cannot be retained there or in a
// second in-memory slice. The spool lives in the invocation's private root,
// outside the consumer-visible artifact root, and keeps replay complete while
// the heap remains constant. It is called from the runner goroutine while the
// completing worker runs provisioned/releaseProducerEvents; the relation's
// mutex is what orders the two, so an event either lands in the spool the
// release will drain or is scrubbed with the set the release published.
func (rel *invocationRelation) holdEvents(handler EventHandler) EventHandler {
	return func(event RawJobEvent) {
		rel.mu.Lock()
		if !rel.released {
			rel.spoolEventLocked(event)
			rel.mu.Unlock()
			return
		}
		needles := append([]string(nil), rel.needles...)
		rel.mu.Unlock()
		if redactEvent(rel.producer, &event, needles) {
			rel.recordLeak(rel.producer)
		}
		handler(event)
	}
}

func (rel *invocationRelation) spoolEventLocked(event RawJobEvent) {
	if rel.held == nil {
		rel.held = &producerEventSpool{}
		scratch := rel.scratch
		if scratch == nil {
			rel.held.failed = true
			return
		}
		// ArtifactRoot is the child handed to tasks. Its parent is the owner-only
		// invocation root and is deliberately invisible in the job context.
		file, err := os.CreateTemp(filepath.Dir(scratch.ArtifactRoot()), ".held-events-*.jsonl")
		if err != nil {
			rel.held.failed = true
			return
		}
		rel.held.file = file
		rel.held.path = file.Name()
	}
	if rel.held.failed || rel.held.file == nil {
		return
	}
	if err := json.NewEncoder(rel.held.file).Encode(event); err != nil {
		rel.held.failed = true
	}
}

// releaseProducerEvents redacts the producer's withheld events with the needles
// derived from what it produced and hands them to sink, in stream order.
//
// It is called from closeTask AFTER provisioned (which loads the needles). Each
// replayed hit is written to the per-job leak side channel, and closeTask runs
// guardResult again before publication. That ordering is load-bearing because
// bounded JobResult.Events may not contain a late producer leak.
//
// A producer that never reaches closeTask never releases: dropping its output
// is the fail-closed direction, and the only paths that skip closeTask are the
// ones where no subprocess ran.
func (rt *invocationRuntime) releaseProducerEvents(job *ScheduledJob, sink EventHandler) {
	if rt == nil {
		return
	}
	rel := rt.byProducer[job.Key()]
	if rel == nil || !rel.sensitive {
		return
	}
	rel.mu.Lock()
	held := rel.held
	rel.held = nil
	rel.released = true
	needles := append([]string(nil), rel.needles...)
	rel.mu.Unlock()

	if held == nil {
		return
	}
	defer held.discard()
	if held.failed || held.file == nil {
		rel.recordLeak(job)
		return
	}
	if _, err := held.file.Seek(0, io.SeekStart); err != nil {
		rel.recordLeak(job)
		return
	}
	decoder := json.NewDecoder(held.file)
	for {
		var event RawJobEvent
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			rel.recordLeak(job)
			return
		}
		if redactEvent(job, &event, needles) {
			rel.recordLeak(job)
		}
		if sink != nil {
			sink(event)
		}
	}
}

func (held *producerEventSpool) discard() {
	if held == nil {
		return
	}
	if held.file != nil {
		_ = held.file.Close()
	}
	if held.path != "" {
		_ = os.Remove(held.path)
	}
}

func (rel *invocationRelation) discardProducerEvents() {
	if rel == nil {
		return
	}
	rel.mu.Lock()
	held := rel.held
	rel.held = nil
	rel.mu.Unlock()
	held.discard()
}
