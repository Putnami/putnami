package jobs

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

type EventMapper func(RawJobEvent) RawJobEvent

// SessionEventHandler receives scheduler records for session recording.
type SessionEventHandler func(record SessionRecord)

// SessionRecordJobEnd is the legacy persisted record type carrying one task's
// terminal result. New session streams project terminal results directly as
// protocol v2 task:end records, but the scheduler and compatibility readers
// still exchange this frozen value.
const SessionRecordJobEnd = "job:end"

// SessionRecord is one scheduler/session audit record.
//
// Data is the legacy untyped projection persisted by v1 session writers. Task
// is the same terminal payload in its canonical typed form and is nil for every
// record type other than SessionRecordJobEnd. Keeping this seam in the pure
// model lets renderers consume audit records without depending back on the
// scheduler package that produces them.
type SessionRecord struct {
	JobKey string
	Type   string
	Data   map[string]any
	Task   *TaskResult
}

// maxJobEventBytes bounds one event line. A longer line is read to its end
// and discarded, never parsed: see readEventLine.
const maxJobEventBytes = 16 * 1024 * 1024

// retainedJobEvents bounds the in-memory detail kept on one JobResult. Every
// valid event still reaches onEvent and result extraction before it is dropped,
// so the complete session artifact and the task verdict are independent of
// this retention budget. We reuse the normal machine-output partitions because
// they are public, fixed, and already reserve failure evidence against an
// arbitrarily noisy successful task.
type retainedJobEvents struct {
	events          []RawJobEvent
	ordinaryBytes   int64
	ordinaryRecords int
	failureBytes    int64
	failureRecords  int
	artifactBytes   int64
	artifactRecords int
}

func newRetainedJobEvents() retainedJobEvents {
	budget, _ := protocolcli.MachineOutputBudgetFor(protocolcli.MachineOutputModeNormal)
	return retainedJobEvents{
		ordinaryBytes:   budget.MaxBytes - budget.FailureReserveBytes - budget.FinalReserveBytes,
		ordinaryRecords: budget.MaxRecords - budget.FailureReserveRecords - budget.FinalReserveRecords,
		failureBytes:    budget.FailureReserveBytes,
		failureRecords:  budget.FailureReserveRecords,
		artifactBytes:   budget.FailureReserveBytes,
		artifactRecords: budget.FailureReserveRecords,
	}
}

func (r *retainedJobEvents) add(event RawJobEvent) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	size := int64(len(encoded) + 1)
	payload := maps.Clone(event.Data)
	if payload == nil {
		payload = make(map[string]any)
	}
	if _, exists := payload["type"]; !exists {
		payload["type"] = event.Type
	}
	if _, exists := payload["level"]; !exists && event.Level != "" {
		payload["level"] = event.Level
	}
	if _, exists := payload["message"]; !exists && event.Message != "" {
		payload["message"] = event.Message
	}
	record := protocolcli.SessionStreamRecord{Record: protocolcli.RecordTaskEvent, Event: payload}
	if event.Type == EventTypeArtifact {
		// An artifact record names a file the task produced. Consumers read it
		// as a task output — the spec gate joins verification reports, publish
		// resolves images, coverage resolves profiles — so it is structural
		// evidence, not display detail, and a noisy transcript must not be able
		// to delete it. It gets its own reserve for the same reason failure
		// evidence does: a task that emits thousands of ordinary log lines
		// would otherwise exhaust the ordinary partition before its late
		// artifact records arrive, and the loss is silent at every consumer.
		if r.artifactRecords == 0 || size > r.artifactBytes {
			return
		}
		r.artifactRecords--
		r.artifactBytes -= size
		r.events = append(r.events, event)
		return
	}
	if protocolcli.IsMachineOutputFailurePriority(record) {
		if r.failureRecords == 0 || size > r.failureBytes {
			return
		}
		r.failureRecords--
		r.failureBytes -= size
	} else {
		if r.ordinaryRecords == 0 || size > r.ordinaryBytes {
			return
		}
		r.ordinaryRecords--
		r.ordinaryBytes -= size
	}
	r.events = append(r.events, event)
}

// BoundedJobEvents applies the same strict per-task retention budget to a
// derived event slice as ReadJSONLEvents applies while parsing a live stream.
// Canonical task records remain the complete source of truth; this bounds only
// the compatibility/display projection retained on JobResult.
func BoundedJobEvents(events []RawJobEvent) []RawJobEvent {
	retained := newRetainedJobEvents()
	for _, event := range events {
		retained.add(event)
	}
	return retained.events
}

// ReadJSONLEvents reads JSONL lines from an io.Reader, parses each as a
// RawJobEvent, applies mapper (when non-nil), and calls onEvent for each valid
// event. Returns all events and the result extracted from the "result" event
// type (if any).
func ReadJSONLEvents(r io.Reader, onEvent EventHandler, mapper EventMapper) ([]RawJobEvent, *JobResult) {
	return ReadJSONLEventsObserved(r, onEvent, mapper, nil)
}

// ReadJSONLEventsObserved is ReadJSONLEvents with an internal observation seam
// invoked immediately after a valid protocol event is parsed, before path
// mapping or renderer callbacks can add work to the startup measurement.
func ReadJSONLEventsObserved(
	r io.Reader,
	onEvent EventHandler,
	mapper EventMapper,
	onValidEvent func(),
) ([]RawJobEvent, *JobResult) {
	reader := bufio.NewReaderSize(r, 64*1024)

	retained := newRetainedJobEvents()
	var result *JobResult
	metaEmitted := false
	discarded := 0

	for {
		raw, oversized, err := readEventLine(reader)
		if err != nil {
			break
		}
		if oversized {
			// Stopping here would leave the rest of the stream unread, and a
			// task still writing would block on the full pipe until its
			// timeout. Say what was lost instead, as failure evidence.
			discarded++
			event := oversizedEventLine()
			if onEvent != nil {
				onEvent(event)
			}
			retained.add(event)
			continue
		}
		line := string(bytes.TrimSpace(raw))
		if line == "" {
			continue
		}

		event, ok := ParseRawEvent(line)
		if !ok {
			continue
		}
		if onValidEvent != nil {
			onValidEvent()
		}
		if mapper != nil {
			event = mapper(event)
		}

		if onEvent != nil {
			onEvent(event)
		}
		if event.Type == EventTypeMeta {
			metaEmitted = true
			if result != nil {
				result.EmittedMeta = true
			}
		}

		// Extract result from "result" event
		if event.Type == EventTypeResult && event.Data != nil {
			result = ExtractResult(event.Data)
			result.EmittedMeta = metaEmitted
		}

		retained.add(event)
	}

	if result == nil && discarded > 0 {
		// The discarded line may have been the result: the task's verdict is
		// unknown, so it fails rather than passing on its exit code alone.
		result = &JobResult{
			Status: string(TaskStatusFailed),
			Error: &JobError{Message: fmt.Sprintf(
				"the task wrote %d event line(s) over %d bytes, which were discarded, and no result event", discarded, maxJobEventBytes,
			)},
			EmittedMeta: metaEmitted,
		}
	}
	return retained.events, result
}

// readEventLine returns the next line of r without its line feed. A line that
// fits r's buffer aliases it, so it is valid only until the next read; a longer
// one is assembled in its own slice. A line longer than maxJobEventBytes is
// read to its end but not kept: oversized reports it, and line is empty. A last
// line without a line feed is returned like any other; err is io.EOF, or the
// read error, once no line remains.
func readEventLine(r *bufio.Reader) (line []byte, oversized bool, err error) {
	for {
		chunk, readErr := r.ReadSlice('\n')
		content := bytes.TrimSuffix(chunk, []byte{'\n'})
		if !oversized && len(line)+len(content) > maxJobEventBytes {
			oversized, line = true, nil
		}
		if !oversized {
			if line == nil && readErr == nil {
				return content, false, nil
			}
			line = append(line, content...)
		}
		switch {
		case errors.Is(readErr, bufio.ErrBufferFull):
			continue
		case readErr == nil, len(line) > 0 || oversized:
			return line, oversized, nil
		default:
			return nil, false, readErr
		}
	}
}

// oversizedEventLine is the error log event that stands for a discarded
// event line. It goes through ParseRawEvent, so it has exactly the shape of a
// log event a subprocess writes.
func oversizedEventLine() RawJobEvent {
	line, _ := json.Marshal(struct {
		V       int    `json:"v"`
		Type    string `json:"type"`
		Level   string `json:"level"`
		Message string `json:"message"`
	}{
		V:       runtimeproto.MaxKnownProtocolVersion,
		Type:    EventTypeLog,
		Level:   "error",
		Message: fmt.Sprintf("discarded an event line over %d bytes", maxJobEventBytes),
	})
	event, _ := ParseRawEvent(string(line))
	return event
}

// ParseRawEvent parses a single JSONL line into a RawJobEvent.
// Returns false if the line is not a valid event (its `v` must be the runtime
// event protocol version this CLI requires, and it must have a type).
//
// RUNTIME EVENTS v2 IS REQUIRED. An earlier version of the reader
// accepted v1 too, and had to: the CLI advertised v2 to every subprocess but
// still loaded extensions that predated the advertisement, and since one stream
// speaks ONE version, rejecting on version would have dropped a whole job's
// output — logs and result included — rather than one unsupported event.
//
// That argument does not survive the require-v3 flip, because the manifest
// loader now makes its premise false: an extension reaches this parser only if
// its manifest declares CLI contract 3, and contract 3 requires an SDK that
// reads the advertisement and answers at v2. A v1 line therefore no longer
// means "an older extension"; it means the stream and the manifest disagree
// about which contract the extension implements, and interpreting it would be
// guessing which one is true. Unknown-to-the-renderer event TYPES stay
// tolerated, as before: every renderer switches on type with no default branch.
//
// Subprocess events use flat top-level keys (e.g. "severity", "location",
// "current", "total") rather than nesting them under a "data" key. This
// function captures all non-standard fields into Data so that renderers
// can access them uniformly via event.Data["severity"], etc.
func ParseRawEvent(line string) (RawJobEvent, bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return RawJobEvent{}, false
	}

	var event RawJobEvent

	// Extract known struct fields
	if v, ok := raw["v"].(float64); ok {
		event.Version = int(v)
	}
	if t, ok := raw["type"].(string); ok {
		event.Type = t
	}
	if t, ok := raw["time"].(string); ok {
		event.Time = t
	}
	if l, ok := raw["level"].(string); ok {
		event.Level = l
	}
	if m, ok := raw["message"].(string); ok {
		event.Message = m
	}

	if event.Version != runtimeproto.MaxKnownProtocolVersion || event.Type == "" {
		return event, false
	}

	// Build Data: start from explicit "data" key if present
	if d, ok := raw["data"].(map[string]any); ok {
		event.Data = d
	} else {
		event.Data = make(map[string]any)
	}

	// Merge all top-level fields into Data (except "v" and "data" itself).
	// This ensures renderers can access flat fields like severity, location,
	// name, action, current, total via event.Data[...].
	for k, v := range raw {
		if k == "v" || k == "data" {
			continue
		}
		if _, exists := event.Data[k]; !exists {
			event.Data[k] = v
		}
	}

	return event, true
}

// ExtractResult builds a JobResult from the data payload of a "result" event.
func ExtractResult(data map[string]any) *JobResult {
	result := &JobResult{}

	if s, ok := data["status"].(string); ok {
		result.Status = NormalizeStatus(s)
	}

	if d, ok := data["data"].(map[string]any); ok {
		result.Data = d
	}

	if e, ok := data["error"].(map[string]any); ok {
		result.Error = &JobError{}
		if msg, ok := e["message"].(string); ok {
			result.Error.Message = msg
		}
		if code, ok := e["code"].(string); ok {
			result.Error.Code = code
		}
	}

	return result
}

// NormalizeStatus maps any status string to the canonical values. It delegates
// to ParseTaskStatus so the subprocess stream and the batch wire schema share
// one status vocabulary; an unrecognized spelling is passed through unchanged,
// as it always was.
func NormalizeStatus(s string) string {
	if status, ok := ParseTaskStatus(s); ok {
		return string(status)
	}
	return s
}
