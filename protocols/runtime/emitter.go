package runtime

import (
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ErrReadyUnsupportedVersion is returned by Ready on a v1 stream. It is an
// error rather than a silent downgrade because both alternatives are wire
// violations: a `ready` line stamped v1 is an unknown type, and a v2 line in a
// v1 stream is mixed-protocol-version. A caller that cannot emit readiness must
// learn so, not ship an unparsable stream.
var ErrReadyUnsupportedVersion = errors.New(
	"runtime: ready events require protocol version 2; this stream speaks version 1")

// Emitter writes structured JSONL events to a writer.
//
// An Emitter has ONE protocol version, fixed at construction and stamped on
// every line it writes. That is not a convenience: validate.go rejects a stream
// that changes version mid-flight (mixed-protocol-version), so the version is a
// property of the stream, never of the individual event.
type Emitter struct {
	w       io.Writer
	version int
}

// NewEmitter creates an Emitter that writes v1 events to w. v1 is the default
// everywhere so a consumer that never advertised acceptance of a newer
// vocabulary receives the byte-identical stream it has always parsed.
func NewEmitter(w io.Writer) *Emitter {
	return &Emitter{w: w, version: ProtocolVersion}
}

// NewEmitterForVersion creates an Emitter whose stream speaks the given
// protocol version. version is normally the result of NegotiatedVersion, which
// only ever yields a known version; a version this package does not know falls
// back to ProtocolVersion, the fail-closed direction (never emit a vocabulary
// the consumer did not ask for).
func NewEmitterForVersion(w io.Writer, version int) *Emitter {
	if !IsKnownProtocolVersion(version) {
		version = ProtocolVersion
	}
	return &Emitter{w: w, version: version}
}

// Version reports the protocol version this emitter stamps on every event.
func (e *Emitter) Version() int { return e.version }

// SupportsReady reports whether this emitter's stream admits the `ready` event.
func (e *Emitter) SupportsReady() bool { return e.version >= ProtocolVersion2 }

// emit writes a single event as a JSON line.
func (e *Emitter) emit(evt *Event) error {
	if evt.V == 0 {
		evt.V = e.version
	}
	if evt.Time == "" {
		evt.Time = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = e.w.Write(data)
	return err
}

// emitWithExtra writes an event with additional top-level fields merged
// into the JSON object. The envelope schema allows additional
// properties; typed fields win over extras on key collisions.
func (e *Emitter) emitWithExtra(evt *Event, extra map[string]any) error {
	if len(extra) == 0 {
		return e.emit(evt)
	}
	if evt.V == 0 {
		evt.V = e.version
	}
	if evt.Time == "" {
		evt.Time = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	base, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	var obj map[string]any
	if err := json.Unmarshal(base, &obj); err != nil {
		return err
	}
	for k, v := range extra {
		if _, exists := obj[k]; !exists {
			obj[k] = v
		}
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = e.w.Write(data)
	return err
}

// Meta emits a meta event identifying the extension and job.
func (e *Emitter) Meta(extension, job string) error {
	metaData, _ := json.Marshal(MetaData{Extension: extension, Job: job})
	return e.emit(&Event{
		Type:    EventMeta,
		Level:   string(LevelInfo),
		Message: "Starting " + job,
		Data:    metaData,
	})
}

// Log emits a log event.
func (e *Emitter) Log(level LogLevel, message string) error {
	return e.emit(&Event{
		Type:    EventLog,
		Level:   string(level),
		Message: message,
	})
}

// LogContext emits a log event carrying structured context and optional
// error details, used to forward structured log output from child
// processes.
func (e *Emitter) LogContext(level LogLevel, message string, context map[string]any, errInfo *ErrorInfo) error {
	evt := &Event{
		Type:    EventLog,
		Level:   string(level),
		Message: message,
		Error:   errInfo,
	}
	if len(context) > 0 {
		evt.Context = &context
	}
	return e.emit(evt)
}

// Phase emits a phase lifecycle event.
func (e *Emitter) Phase(name string, action PhaseAction, status PhaseStatus) error {
	evt := &Event{
		Type:   EventPhase,
		Name:   name,
		Action: &action,
	}
	if status != "" {
		evt.Status = string(status)
	}
	return e.emit(evt)
}

// Progress emits a progress event.
func (e *Emitter) Progress(current, total int, message string) error {
	return e.emit(&Event{
		Type:    EventProgress,
		Current: &current,
		Total:   &total,
		Message: message,
	})
}

// Metric emits a metric event.
func (e *Emitter) Metric(name string, value float64, unit string) error {
	return e.emit(&Event{
		Type:  EventMetric,
		Name:  name,
		Value: &value,
		Unit:  unit,
	})
}

// Diagnostic emits a diagnostic event.
func (e *Emitter) Diagnostic(severity DiagnosticSeverity, message, code string, loc *SourceLocation) error {
	return e.emit(&Event{
		Type:     EventDiagnostic,
		Severity: &severity,
		Message:  message,
		Code:     code,
		Location: loc,
	})
}

// Artifact emits an artifact event.
func (e *Emitter) Artifact(id, name, kind, path string) error {
	return e.emit(&Event{
		Type: EventArtifact,
		ID:   id,
		Name: name,
		Kind: kind,
		Path: path,
	})
}

// ArtifactData emits an artifact event with additional top-level fields.
func (e *Emitter) ArtifactData(id, name, kind, path string, extra map[string]any) error {
	return e.emitWithExtra(&Event{
		Type: EventArtifact,
		ID:   id,
		Name: name,
		Kind: kind,
		Path: path,
	}, extra)
}

// Summary emits a summary event.
func (e *Emitter) Summary(message string) error {
	return e.emit(&Event{
		Type:    EventSummary,
		Message: message,
	})
}

// SummaryData emits a summary event with additional top-level fields.
func (e *Emitter) SummaryData(message string, extra map[string]any) error {
	return e.emitWithExtra(&Event{
		Type:    EventSummary,
		Message: message,
	}, extra)
}

// Ready emits the typed readiness event. It requires a v2 stream and returns
// ErrReadyUnsupportedVersion on a v1 one — readiness is the whole reason v2
// exists, so an emitter that was not negotiated up to it has nothing valid to
// write. Callers that must tolerate a v1 consumer check SupportsReady first.
//
// The endpoint list is copied and sorted into canonical order before it goes on
// the wire, so a workload that binds its listeners in a race-dependent order
// still emits one deterministic line. No message is set: the payload is the
// contract and renderers format it — readiness must never be read back out of
// human text, which is the substring probe this event replaces.
func (e *Emitter) Ready(data ReadyData) error {
	if !e.SupportsReady() {
		return ErrReadyUnsupportedVersion
	}
	payload, err := json.Marshal(ReadyMarker(data))
	if err != nil {
		return err
	}
	return e.emit(&Event{
		Type: EventReady,
		Data: payload,
	})
}

// Result emits a result event with the given status and optional data/error.
func (e *Emitter) Result(status ResultStatus, data map[string]any, jobErr *JobError) error {
	rd := ResultData{Status: status, Data: data, Error: jobErr}
	payload, _ := json.Marshal(rd)
	return e.emit(&Event{
		Type:    EventResult,
		Level:   string(LevelInfo),
		Message: "Job " + string(status),
		Data:    payload,
	})
}
