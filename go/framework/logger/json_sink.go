package logger

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// JSONSink writes structured JSON log entries, one per line.
// Compatible with Google Cloud Logging format.
// Writes to os.Stdout by default (matching the TypeScript runtime).
type JSONSink struct {
	w   io.Writer
	enc *json.Encoder
	// gcpProject is the Google Cloud project ID resolved once at construction
	// (it is fixed for the process lifetime) so Write does not read the
	// environment on every traced log entry.
	gcpProject string
	mu         sync.Mutex
	lastErr    error
}

// NewJSONSink creates a new JSON sink writing to os.Stdout.
func NewJSONSink() *JSONSink {
	return NewJSONSinkWriter(os.Stdout)
}

// NewJSONSinkWriter creates a JSON sink writing to a custom writer (for tests
// and conformance harnesses that need to capture the real rendered JSON line).
func NewJSONSinkWriter(w io.Writer) *JSONSink {
	// Resolve the GCP project ID once; it is immutable for the process so
	// reading it here avoids an os.Getenv per traced log write.
	gcpProject := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if gcpProject == "" {
		gcpProject = os.Getenv("GCP_PROJECT")
	}
	return &JSONSink{
		w:          w,
		enc:        json.NewEncoder(w),
		gcpProject: gcpProject,
	}
}

func (s *JSONSink) Write(entry LogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record := make(map[string]any, len(entry.Context)+len(entry.Attrs)+5)

	// Spread user-supplied data FIRST (context fields at top level per the
	// Cloud Logging convention, then attrs). The framework-reserved keys
	// are written last so a context field or attr named "severity",
	// "message", "timestamp", "logger", or a trace key can neither
	// overwrite nor forge the real value — a log-forging / field-corruption
	// vector when callers forward user input as a field with a reserved name.
	for k, v := range entry.Context {
		record[k] = v
	}
	for _, attr := range entry.Attrs {
		record[attr.Key] = slogValueToAny(attr.Value)
	}

	// Reserved keys, written last (last-write-wins protects them).
	record["severity"] = gcpSeverity(entry.Level)
	record["message"] = entry.Message
	record["timestamp"] = entry.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")

	// logger is framework-owned: set it when present, otherwise drop any
	// colliding user key so it can't masquerade as the logger name.
	if entry.Logger != "" {
		record["logger"] = entry.Logger
	} else {
		delete(record, "logger")
	}

	// TraceID handling: use GCP format when project is set, otherwise plain
	// traceId. Both trace keys are framework-owned — clear any user-supplied
	// collisions first, then set the applicable one.
	delete(record, "traceId")
	delete(record, "logging.googleapis.com/trace")
	if entry.TraceID != "" {
		if s.gcpProject != "" && !strings.Contains(entry.TraceID, "/") {
			record["logging.googleapis.com/trace"] = "projects/" + s.gcpProject + "/traces/" + entry.TraceID
		} else {
			record["traceId"] = entry.TraceID
		}
	}

	// The framework's structured error wins over a user attr named "error"
	// when present; a user "error" field is left intact when there is none.
	if entry.Error != nil {
		record["error"] = entry.Error
	}

	if err := s.enc.Encode(record); err != nil {
		s.lastErr = err
	}
}

// Flush is a no-op for JSONSink since writes are unbuffered.
func (s *JSONSink) Flush() error { return nil }

// Close is a no-op for JSONSink; the underlying writer is not owned.
func (s *JSONSink) Close() error { return nil }

// Err returns the last write error encountered by the sink, if any.
func (s *JSONSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// slogValueToAny resolves a slog.Value into a JSON-encodable Go value. It
// resolves LogValuer values, recursively renders a group as a map[string]any so
// nested groups (required by the logging contract) serialize as JSON objects
// rather than the opaque []slog.Attr that Value.Any() would return, and
// otherwise returns the concrete underlying value.
func slogValueToAny(v slog.Value) any {
	v = v.Resolve()
	if v.Kind() == slog.KindGroup {
		group := v.Group()
		m := make(map[string]any, len(group))
		for _, a := range group {
			m[a.Key] = slogValueToAny(a.Value)
		}
		return m
	}
	return v.Any()
}

func gcpSeverity(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARNING"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}
