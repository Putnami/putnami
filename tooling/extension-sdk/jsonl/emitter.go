// Package jsonl provides JSONL event emission for Putnami orchestrator integration.
//
// Events follow the Putnami runtime event protocol defined by
// go.putnami.dev/protocol/runtime: each line is a self-contained JSON
// object with a type, timestamp, and type-specific payload. This package
// is a stdout-bound convenience wrapper around the protocol emitter —
// the wire format is owned by the protocol package.
//
// The stream's protocol version is NEGOTIATED once, when the emitter is built:
// New reads the invoking CLI's advertisement (runtime.AcceptedVersionEnv) and
// every line the emitter writes carries that version, because the protocol
// rejects a stream that mixes versions. No advertisement means v1, so an
// extension invoked by an older CLI — or by hand — emits the byte-identical v1
// stream it always did.
package jsonl

import (
	"os"
	"strconv"

	runtime "go.putnami.dev/protocol/runtime"
)

// Emitter writes JSONL events to stdout.
type Emitter struct {
	version int
}

// New creates a JSONL emitter whose stream speaks the protocol version the
// invoking CLI advertised, defaulting to v1.
func New() *Emitter { return NewForVersion(runtime.NegotiatedVersionFromEnv(os.Getenv)) }

// NewForVersion creates a JSONL emitter pinned to an explicit protocol version,
// bypassing the environment. It exists for tests and for callers that already
// resolved the negotiation themselves; an unknown version falls back to v1.
func NewForVersion(version int) *Emitter {
	if !runtime.IsKnownProtocolVersion(version) {
		version = runtime.ProtocolVersion
	}
	return &Emitter{version: version}
}

// Version reports the protocol version this emitter stamps on every event.
func (e *Emitter) Version() int { return e.version }

// SupportsReady reports whether the negotiated stream admits typed readiness.
// A forwarder checks this before building a readiness payload it could not
// emit.
func (e *Emitter) SupportsReady() bool { return e.version >= runtime.ProtocolVersion2 }

// rt binds a protocol emitter to the current os.Stdout at emit time so
// callers (and tests) that swap os.Stdout see events on the new writer.
func (e *Emitter) rt() *runtime.Emitter {
	return runtime.NewEmitterForVersion(os.Stdout, e.version)
}

// Meta emits a meta event identifying the extension and job.
func (e *Emitter) Meta(extension, job string) {
	_ = e.rt().Meta(extension, job)
}

// Log emits a log event. Level: debug, info, warn, error.
func (e *Emitter) Log(level, message string) {
	_ = e.rt().Log(runtime.LogLevel(level), message)
}

// LogEvent emits a log event with optional context and error fields.
// This is used to forward structured log output from child processes.
func (e *Emitter) LogEvent(level, message string, context map[string]any, errInfo map[string]any) {
	var info *runtime.ErrorInfo
	if len(errInfo) > 0 {
		info = &runtime.ErrorInfo{}
		if msg, ok := errInfo["message"].(string); ok {
			info.Message = msg
		}
		if stack, ok := errInfo["stack"].(string); ok {
			info.Stack = stack
		}
	}
	_ = e.rt().LogContext(runtime.LogLevel(level), message, context, info)
}

// Info emits an info-level log event.
func (e *Emitter) Info(message string) { e.Log("info", message) }

// Warn emits a warn-level log event.
func (e *Emitter) Warn(message string) { e.Log("warn", message) }

// Error emits an error-level log event.
func (e *Emitter) Error(message string) { e.Log("error", message) }

// Debug emits a debug-level log event.
func (e *Emitter) Debug(message string) { e.Log("debug", message) }

// PhaseStart emits the start of a named phase.
func (e *Emitter) PhaseStart(name string) {
	_ = e.rt().Phase(name, runtime.PhaseStart, "")
}

// PhaseEnd emits the end of a named phase. Status: success, failed, skipped.
func (e *Emitter) PhaseEnd(name, status string) {
	_ = e.rt().Phase(name, runtime.PhaseEnd, runtime.PhaseStatus(status))
}

// Progress emits a progress event.
func (e *Emitter) Progress(current, total int, message string) {
	_ = e.rt().Progress(current, total, message)
}

// Diagnostic emits a diagnostic event. Severity: error, warning, info, hint.
func (e *Emitter) Diagnostic(severity, message string, file string, line int) {
	var loc *runtime.SourceLocation
	if file != "" {
		loc = &runtime.SourceLocation{File: file, Line: line}
	}
	_ = e.rt().Diagnostic(runtime.DiagnosticSeverity(severity), message, "", loc)
}

// DiagnosticWithCode emits a diagnostic with optional location and code.
func (e *Emitter) DiagnosticWithCode(severity, message string, file string, line, column int, code string) {
	var loc *runtime.SourceLocation
	if file != "" {
		loc = &runtime.SourceLocation{File: file, Line: line, Column: column}
	}
	_ = e.rt().Diagnostic(runtime.DiagnosticSeverity(severity), message, code, loc)
}

// Metric emits a metric event. Unit: ms, bytes, count, percent.
//
// The protocol requires numeric metric values; numeric strings are
// converted, and non-numeric values are dropped with a debug log
// instead of emitting a malformed metric.
func (e *Emitter) Metric(name string, value any, unit string) {
	num, ok := toFloat64(value)
	if !ok {
		e.Debug("dropping metric " + name + ": non-numeric value")
		return
	}
	_ = e.rt().Metric(name, num, unit)
}

func toFloat64(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// Artifact emits an artifact event.
func (e *Emitter) Artifact(id, name, kind, path string) {
	_ = e.rt().Artifact(id, name, kind, path)
}

// ArtifactWithData emits an artifact event with additional data.
func (e *Emitter) ArtifactWithData(id, name, kind, path string, data map[string]any) {
	_ = e.rt().ArtifactData(id, name, kind, path, data)
}

// Summary emits a human-readable summary label for the job.
func (e *Emitter) Summary(message string) {
	_ = e.rt().Summary(message)
}

// SummaryWithData emits a summary event with additional top-level fields.
func (e *Emitter) SummaryWithData(message string, data map[string]any) {
	_ = e.rt().SummaryData(message, data)
}

// Result emits a result event. Status: OK, FAILED, SKIP.
func (e *Emitter) Result(status string, data map[string]any) {
	_ = e.rt().Result(runtime.ResultStatus(status), data, nil)
}

// Ready emits the typed readiness event and reports whether it was written.
//
// On a stream that was not negotiated up to v2 it writes NOTHING and returns
// false: there is no valid v1 spelling of readiness, and downgrading would
// either emit an unknown type or mix versions in one stream. Callers treat
// false as "this consumer does not speak readiness", not as an error — the
// watch adapter simply never arms for that consumer (B6b deleted the probe).
func (e *Emitter) Ready(data runtime.ReadyData) bool {
	if !e.SupportsReady() {
		return false
	}
	return e.rt().Ready(data) == nil
}
