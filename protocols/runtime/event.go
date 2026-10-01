// Package runtime provides typed models, JSONL parsing, and emitter helpers
// for the Putnami runtime event protocol (v1).
//
// Every job subprocess emits structured JSONL events on stdout. Each line
// is a self-contained JSON object conforming to the event schema.
package runtime

import "encoding/json"

// ProtocolVersion is the current protocol version.
const ProtocolVersion = 1

// EventType enumerates the discriminator values for runtime events.
type EventType string

// EventType values identify the kind of runtime event being emitted.
const (
	EventLog        EventType = "log"
	EventProgress   EventType = "progress"
	EventArtifact   EventType = "artifact"
	EventDiagnostic EventType = "diagnostic"
	EventMetric     EventType = "metric"
	EventPhase      EventType = "phase"
	EventSummary    EventType = "summary"
	EventResult     EventType = "result"
	EventMeta       EventType = "meta"
)

// LogLevel enumerates log severity levels.
type LogLevel string

// LogLevel values classify the severity of a log event.
const (
	LevelDebug LogLevel = "debug"
	LevelInfo  LogLevel = "info"
	LevelWarn  LogLevel = "warn"
	LevelError LogLevel = "error"
)

// PhaseAction describes whether a phase is starting or ending.
type PhaseAction string

// PhaseAction values indicate whether a phase event opens or closes a phase.
const (
	PhaseStart PhaseAction = "start"
	PhaseEnd   PhaseAction = "end"
)

// PhaseStatus describes the outcome of a phase.
type PhaseStatus string

// PhaseStatus values describe the terminal state of a phase.
const (
	PhaseSuccess PhaseStatus = "success"
	PhaseFailed  PhaseStatus = "failed"
	PhaseSkipped PhaseStatus = "skipped"
)

// ResultStatus describes the final outcome of a job.
type ResultStatus string

// ResultStatus values describe the final job result reported by the runtime.
const (
	ResultOK     ResultStatus = "OK"
	ResultFailed ResultStatus = "FAILED"
	ResultSkip   ResultStatus = "SKIP"
)

// DiagnosticSeverity enumerates diagnostic severity levels.
type DiagnosticSeverity string

// DiagnosticSeverity values classify emitted diagnostics by severity.
const (
	SeverityError   DiagnosticSeverity = "error"
	SeverityWarning DiagnosticSeverity = "warning"
	SeverityInfo    DiagnosticSeverity = "info"
	SeverityHint    DiagnosticSeverity = "hint"
)

// Event is the envelope for all runtime JSONL events.
type Event struct {
	// V is the protocol version the emitter negotiated for this stream. Every
	// line of one stream carries the same value; see negotiation.go.
	V int `json:"v"`
	// Type is the discriminator that decides which members below are meaningful
	// and what shape Data has. It must belong to the vocabulary the stream's
	// version admits (nine types at v1, plus "ready" at v2).
	Type EventType `json:"type"`
	// Time is the ISO 8601 UTC timestamp the event was emitted at
	// (2006-01-02T15:04:05.000Z). Optional: a consumer that needs ordering uses
	// stream order, which is authoritative, rather than this value.
	Time string `json:"time,omitempty"`
	// Level is the LogLevel of a log event. It is a plain string rather than the
	// LogLevel type because an unknown level must survive decoding far enough to
	// be reported as a validation diagnostic instead of a decode error.
	Level string `json:"level,omitempty"`
	// Message is the human-readable text of the event. Required for log and
	// diagnostic events, optional elsewhere.
	Message string `json:"message,omitempty"`
	// Data is the type-specific structured payload, kept raw so a consumer
	// decodes only the payloads it understands (ResultData, ReadyData, …).
	Data json.RawMessage `json:"data,omitempty"`

	// Typed fields present on specific event types (flattened in JSON).
	// These are populated by ParseEvent for convenience.

	// Context is a log event's optional structured context map. It is a pointer
	// so an emitted empty object stays distinct from an absent one.
	Context *map[string]any `json:"context,omitempty"`
	// Error carries the optional error detail of a log event.
	Error *ErrorInfo `json:"error,omitempty"`

	// Current is a progress event's completed count. A pointer, so 0 of N is
	// distinguishable from an omitted member.
	Current *int `json:"current,omitempty"`
	// Total is a progress event's expected count.
	Total *int `json:"total,omitempty"`

	// Severity is a diagnostic event's DiagnosticSeverity.
	Severity *DiagnosticSeverity `json:"severity,omitempty"`
	// Code is a diagnostic event's machine-readable identifier, as produced by
	// the underlying tool ("TS2304", "lint/no-unused-vars").
	Code string `json:"code,omitempty"`
	// Location is the optional source position a diagnostic points at.
	Location *SourceLocation `json:"location,omitempty"`

	// Name is a metric event's measurement name.
	Name string `json:"name,omitempty"`
	// Value is a metric event's measured value. A pointer, so a measured zero is
	// distinguishable from an omitted member.
	Value *float64 `json:"value,omitempty"`
	// Unit is the unit Value is expressed in ("count", "percent", "ms").
	Unit string `json:"unit,omitempty"`

	// Action is a phase event's PhaseAction: whether the phase opens or closes.
	Action *PhaseAction `json:"action,omitempty"`
	// Status is a closing phase event's PhaseStatus, and a result event's
	// ResultStatus. It is a plain string for the same reason as Level.
	Status string `json:"status,omitempty"`

	// ID is an artifact event's stable identifier within the job.
	ID string `json:"id,omitempty"`
	// Kind classifies an artifact; the kinds tooling understands are the
	// ArtifactKind* constants in payloads.go.
	Kind string `json:"kind,omitempty"`
	// Path is an artifact's location, relative to the job's output path.
	Path string `json:"path,omitempty"`
}

// SourceLocation identifies a position in source code.
type SourceLocation struct {
	// File is the path the diagnostic refers to, as the producing tool reported
	// it.
	File string `json:"file,omitempty"`
	// Line is the 1-based line number; 0 means the tool reported none.
	Line int `json:"line,omitempty"`
	// Column is the 1-based column number; 0 means the tool reported none.
	Column int `json:"column,omitempty"`
}

// ErrorInfo carries optional error details in log events.
type ErrorInfo struct {
	// Message is the error text.
	Message string `json:"message,omitempty"`
	// Stack is the optional stack trace or equivalent backtrace, verbatim from
	// the producing runtime. It is not parsed or normalized by this protocol.
	Stack string `json:"stack,omitempty"`
}

// ResultData is the payload of a result event.
type ResultData struct {
	// Status is the job's terminal verdict.
	Status ResultStatus `json:"status"`
	// Data carries the typed per-verb payloads (testSummary, coverageSummary,
	// lintSummary, and the Distribution-owned releaseSet publish outcome — see
	// payloads.go) plus any producer-specific extras a consumer must ignore when
	// it does not recognize them. Release-set fields are never declared here;
	// their sole authority is go.putnami.dev/protocol/distribution.
	Data map[string]any `json:"data,omitempty"`
	// Error is the structured failure detail, present when Status is
	// ResultFailed.
	Error *JobError `json:"error,omitempty"`
}

// JobError is a structured error from a failed job.
type JobError struct {
	// Message is the human-readable failure text.
	Message string `json:"message,omitempty"`
	// Code is the producer's machine-readable failure identifier, when it has
	// one. This protocol does not define a code vocabulary.
	Code string `json:"code,omitempty"`
}

// MetaData is the payload of a meta event.
type MetaData struct {
	// Extension is the name of the extension running the job
	// ("@putnami/typescript").
	Extension string `json:"extension,omitempty"`
	// Job is the name of the job being run ("build", "test").
	Job string `json:"job,omitempty"`
}
