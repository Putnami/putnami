package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

type panicMarshaler struct{}

func (panicMarshaler) MarshalJSON() ([]byte, error) { panic("boom in MarshalJSON") }

func TestLoggerIsolatesMarshalPanic(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "failure-isolation", "sink-write-panic-isolated")
	// A logged value whose MarshalJSON panics must not propagate out of the log
	// call and crash the caller — observability must never take down what it
	// observes (encoding/json does not recover marshaler panics).
	var buf bytes.Buffer
	log := New("panic-test", LevelInfo, NewJSONSinkWriter(&buf))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("log call leaked a marshaler panic to the caller: %v", r)
		}
	}()

	log.With("resp", panicMarshaler{}).Info("handling request")
	// The logger is still usable afterwards.
	log.Info("still alive")
}

func TestLoggerLevels(t *testing.T) {
	sink := NewMemorySink()
	log := New("test", LevelInfo, sink)

	log.Debug("debug msg")
	log.Info("info msg")
	log.Warn("warn msg")
	log.Error("error msg", nil)

	// Debug should be filtered out at Info level
	if sink.Len() != 3 {
		t.Errorf("expected 3 entries (debug filtered), got %d", sink.Len())
	}
	if sink.Entries[0].Message != "info msg" {
		t.Errorf("expected 'info msg', got %q", sink.Entries[0].Message)
	}
}

func TestLoggerNamed(t *testing.T) {
	sink := NewMemorySink()
	log := New("app", LevelDebug, sink)
	child := log.Named("http")

	child.Info("request")

	if sink.Last().Logger != "app.http" {
		t.Errorf("expected 'app.http', got %q", sink.Last().Logger)
	}
}

func TestLoggerWith(t *testing.T) {
	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)
	logWithCtx := log.With("requestId", "abc-123")

	logWithCtx.Info("hello")

	entry := sink.Last()
	if entry.Context["requestId"] != "abc-123" {
		t.Errorf("expected context requestId='abc-123', got %v", entry.Context["requestId"])
	}
}

func TestLoggerError(t *testing.T) {
	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	log.Error("failed", errors.New("connection refused"))

	entry := sink.Last()
	if entry.Error == nil {
		t.Fatal("expected error info")
	}
	if entry.Error.Message != "connection refused" {
		t.Errorf("expected 'connection refused', got %q", entry.Error.Message)
	}
}

func TestLoggerErrorStructured(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "record", "record-structured-error")
	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	err := perrors.New(perrors.CodeConnection, "database unreachable", perrors.String("host", "localhost")).
		WithCategory(perrors.CategoryInfra).
		WithSource("registry.bootstrap").
		WithRetryable(true)

	log.Error("failed to bootstrap", err)

	entry := sink.Last()
	if entry.Error == nil {
		t.Fatal("expected error info")
	}
	if entry.Error.Code != string(perrors.CodeConnection) {
		t.Errorf("expected code %q, got %q", perrors.CodeConnection, entry.Error.Code)
	}
	if entry.Error.Category != string(perrors.CategoryInfra) {
		t.Errorf("expected category %q, got %q", perrors.CategoryInfra, entry.Error.Category)
	}
	if entry.Error.Source != "registry.bootstrap" {
		t.Errorf("expected source %q, got %q", "registry.bootstrap", entry.Error.Source)
	}
	if !entry.Error.Retryable {
		t.Error("expected retryable error")
	}
	if entry.Error.Attrs["host"] != "localhost" {
		t.Errorf("expected attrs.host=%q, got %v", "localhost", entry.Error.Attrs["host"])
	}
	if entry.Error.Stack == "" {
		t.Error("expected structured stack trace")
	}
	if entry.Error.Name != string(perrors.CodeConnection) {
		t.Errorf("expected name to reflect perrors code %q, got %q", perrors.CodeConnection, entry.Error.Name)
	}
}

type namedTestError struct{ msg string }

func (e *namedTestError) Error() string { return e.msg }

func TestLoggerErrorName(t *testing.T) {
	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	// Plain (non-perrors) error: name should reflect the concrete Go type,
	// not the constant "error".
	log.Error("boom", &namedTestError{msg: "boom"})
	entry := sink.Last()
	if entry.Error == nil {
		t.Fatal("expected error info")
	}
	if entry.Error.Name == "error" {
		t.Errorf("expected name to reflect the real error type, got constant %q", entry.Error.Name)
	}
	if want := "*logger.namedTestError"; entry.Error.Name != want {
		t.Errorf("expected name %q, got %q", want, entry.Error.Name)
	}

	// Structured perrors error: name should be the code name.
	log.Error("connect", perrors.New(perrors.CodeConnection, "down"))
	entry = sink.Last()
	if entry.Error == nil {
		t.Fatal("expected error info")
	}
	if entry.Error.Name != string(perrors.CodeConnection) {
		t.Errorf("expected name %q, got %q", perrors.CodeConnection, entry.Error.Name)
	}
}

func TestConsoleSink(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sink := newConsoleSinkWriters(&stdout, &stderr)

	sink.Write(LogEntry{
		Level:     LevelInfo,
		Message:   "hello world",
		Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Logger:    "app",
	})

	output := stdout.String()
	if !strings.Contains(output, "[INFO]") {
		t.Errorf("expected [INFO] level tag in output: %s", output)
	}
	if !strings.Contains(output, "hello world") {
		t.Errorf("expected message in output: %s", output)
	}
	if !strings.Contains(output, "[app]") {
		t.Errorf("expected logger name in output: %s", output)
	}
}

func TestConsoleSinkFormat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sink := newConsoleSinkWriters(&stdout, &stderr)

	// Info with traceId should go to stdout with TS format: [traceId] [LEVEL] [logger] message
	sink.Write(LogEntry{
		Level:   LevelInfo,
		Message: "request handled",
		Logger:  "http",
		TraceID: "trace-123",
	})

	output := stdout.String()
	expected := "[trace-123] [INFO] [http] request handled\n"
	if output != expected {
		t.Errorf("expected %q, got %q", expected, output)
	}

	// Warn should go to stderr
	stdout.Reset()
	stderr.Reset()
	sink.Write(LogEntry{
		Level:   LevelWarn,
		Message: "high latency",
		Logger:  "http",
	})

	if stdout.Len() != 0 {
		t.Errorf("warn should not write to stdout, got: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[WARN]") {
		t.Errorf("expected [WARN] on stderr: %s", stderr.String())
	}
}

func TestConsoleSinkContext(t *testing.T) {
	var stdout bytes.Buffer
	sink := newConsoleSinkWriters(&stdout, &bytes.Buffer{})

	sink.Write(LogEntry{
		Level:   LevelInfo,
		Message: "hello",
		Context: map[string]any{"userId": "u-1"},
	})

	output := stdout.String()
	if !strings.Contains(output, `"userId":"u-1"`) {
		t.Errorf("expected context JSON in output: %s", output)
	}
}

// TestConsoleSink_NewlineCannotForgeLogLine guards the log-injection vector:
// an attacker-controlled value containing "\n" placed in the message, an
// attribute value, or a context value must not produce a second physical line
// on stdout. The injected newline must be escaped so the whole entry stays on
// one line (exactly one trailing "\n"). Mirrors the JSON sink's injection test.
func TestConsoleSink_NewlineCannotForgeLogLine(t *testing.T) {
	const forged = "x\n[INFO] [auth] login succeeded"

	cases := []struct {
		name  string
		entry LogEntry
	}{
		{
			name:  "message",
			entry: LogEntry{Level: LevelInfo, Message: forged},
		},
		{
			name: "attr value",
			entry: LogEntry{
				Level:   LevelInfo,
				Message: "ok",
				Attrs:   []slog.Attr{slog.String("user", forged)},
			},
		},
		{
			name: "context string value",
			entry: LogEntry{
				Level:   LevelInfo,
				Message: "ok",
				Context: map[string]any{"user": forged},
			},
		},
		{
			name: "context non-string value",
			entry: LogEntry{
				Level:   LevelInfo,
				Message: "ok",
				// A slice renders via %v (the unescaped default branch).
				Context: map[string]any{"items": []string{forged}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			sink := newConsoleSinkWriters(&stdout, &stderr)
			sink.Write(tc.entry)

			out := stdout.String()
			// Exactly one physical line: a single trailing newline terminator.
			if n := strings.Count(out, "\n"); n != 1 {
				t.Errorf("expected exactly 1 newline (no forged line), got %d in %q", n, out)
			}
			if !strings.HasSuffix(out, "\n") {
				t.Errorf("expected output to end with a newline terminator: %q", out)
			}
			// The literal newline must have been escaped, and the forged
			// "[INFO]" payload must not begin a new line.
			if strings.Contains(strings.TrimSuffix(out, "\n"), "\n") {
				t.Errorf("raw newline leaked into the body: %q", out)
			}
			if !strings.Contains(out, `\n`) {
				t.Errorf("expected the injected newline to be escaped as \\n: %q", out)
			}
		})
	}
}

func TestConsoleSinkErrorStack(t *testing.T) {
	var stdout, stderr bytes.Buffer
	sink := newConsoleSinkWriters(&stdout, &stderr)

	sink.Write(LogEntry{
		Level:   LevelError,
		Message: "crash",
		Error: &ErrorInfo{
			Name:    "Error",
			Message: "boom",
			Stack:   "goroutine 1 [running]:\nmain.main()",
		},
	})

	// Error message goes to stderr
	errOutput := stderr.String()
	if !strings.Contains(errOutput, "[ERROR]") {
		t.Errorf("expected [ERROR] on stderr: %s", errOutput)
	}
	if !strings.Contains(errOutput, "boom") {
		t.Errorf("expected error message on stderr: %s", errOutput)
	}
	// Stack trace should be on stderr too
	if !strings.Contains(errOutput, "goroutine 1") {
		t.Errorf("expected stack trace on stderr: %s", errOutput)
	}
}

func TestJSONSink(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "record", "record-core-fields")
	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)

	sink.Write(LogEntry{
		Level:     LevelWarn,
		Message:   "high latency",
		Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Logger:    "http",
		Context:   map[string]any{"duration_ms": 1500},
	})

	output := buf.String()
	if !strings.Contains(output, `"severity":"WARNING"`) {
		t.Errorf("expected WARNING severity in JSON: %s", output)
	}
	if !strings.Contains(output, `"message":"high latency"`) {
		t.Errorf("expected message in JSON: %s", output)
	}
	if !strings.Contains(output, `"duration_ms":1500`) {
		t.Errorf("expected context field in JSON: %s", output)
	}
}

func TestJSONSink_ReservedKeysSurviveCollidingUserData(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)

	sink.Write(LogEntry{
		Level:     LevelError,
		Message:   "real message",
		Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Logger:    "http",
		TraceID:   "real-trace",
		// Context and attrs deliberately collide with reserved keys.
		Context: map[string]any{
			"severity": "DEBUG",
			"message":  "forged message",
			"keep":     "context-value",
		},
		Attrs: []slog.Attr{
			slog.String("timestamp", "forged-ts"),
			slog.String("logger", "forged-logger"),
			slog.String("traceId", "forged-trace"),
			slog.String("also_keep", "attr-value"),
		},
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (output=%s)", err, buf.String())
	}

	// Reserved keys must hold the framework values, not the forged ones.
	reserved := map[string]any{
		"severity":  "ERROR",
		"message":   "real message",
		"timestamp": "2024-01-15T10:30:00.000Z",
		"logger":    "http",
		"traceId":   "real-trace",
	}
	for k, want := range reserved {
		if got[k] != want {
			t.Errorf("reserved %q = %v, want %v", k, got[k], want)
		}
	}

	// Non-colliding user data must still pass through.
	if got["keep"] != "context-value" {
		t.Errorf("context field dropped: keep = %v", got["keep"])
	}
	if got["also_keep"] != "attr-value" {
		t.Errorf("attr dropped: also_keep = %v", got["also_keep"])
	}
}

func TestJSONSink_FrameworkOwnedKeysDroppedWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)

	// No Logger / TraceID on the entry, but the user supplies fields with
	// those reserved names — they must not masquerade as framework values.
	sink.Write(LogEntry{
		Level:     LevelInfo,
		Message:   "msg",
		Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Context: map[string]any{
			"logger":  "forged-logger",
			"traceId": "forged-trace",
			"error":   "user-error-field",
		},
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["logger"]; ok {
		t.Errorf("forged logger key should be dropped when framework sets none: %v", got["logger"])
	}
	if _, ok := got["traceId"]; ok {
		t.Errorf("forged traceId key should be dropped when framework sets none: %v", got["traceId"])
	}
	// "error" is a content field, not framework-structural: a user "error"
	// survives when the framework has no structured error of its own.
	if got["error"] != "user-error-field" {
		t.Errorf("user error field should survive when entry.Error is nil, got %v", got["error"])
	}
}

func TestJSONSinkTraceID(t *testing.T) {
	spectest.Proves(t, "go/structured-logging", "record", "record-trace-identity")
	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)

	// Without GCP env vars, traceId should be plain
	sink.Write(LogEntry{
		Level:   LevelInfo,
		Message: "test",
		TraceID: "abc-123",
	})

	output := buf.String()
	if !strings.Contains(output, `"traceId":"abc-123"`) {
		t.Errorf("expected plain traceId field: %s", output)
	}
	if strings.Contains(output, "logging.googleapis.com/trace") {
		t.Errorf("should not use GCP trace format without env var: %s", output)
	}
}

func TestMemorySink(t *testing.T) {
	sink := NewMemorySink()
	sink.Write(LogEntry{Message: "a"})
	sink.Write(LogEntry{Message: "b"})

	if sink.Len() != 2 {
		t.Errorf("expected 2, got %d", sink.Len())
	}
	if sink.Last().Message != "b" {
		t.Errorf("expected 'b', got %q", sink.Last().Message)
	}

	sink.Clear()
	if sink.Len() != 0 {
		t.Error("should be empty after clear")
	}
	if sink.Last() != nil {
		t.Error("Last() should return nil when empty")
	}
}

func TestLoggerContext(t *testing.T) {
	sink := NewMemorySink()
	log := New("ctx-test", LevelDebug, sink)

	ctx := WithLogger(context.Background(), log)
	retrieved := FromContext(ctx)
	if retrieved == nil {
		t.Fatal("expected logger from context")
	}

	retrieved.Info("from context")
	if sink.Last().Message != "from context" {
		t.Error("expected message from context logger")
	}
}

func TestFromContextOrDefault(t *testing.T) {
	log := FromContextOrDefault(context.Background())
	if log == nil {
		t.Error("should return default logger")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input    string
		expected Level
	}{
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"info", LevelInfo},
		{"INFO", LevelInfo},
		{"warn", LevelWarn},
		{"WARN", LevelWarn},
		{"warning", LevelWarn},
		{"error", LevelError},
		{"ERROR", LevelError},
		{"unknown", LevelInfo},
		{"", LevelInfo},
	}

	for _, tt := range tests {
		got := ParseLevel(tt.input)
		if got != tt.expected {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

// --- Context-aware methods ---

func TestInfoCtx_WithTraceID(t *testing.T) {
	// Save and restore global TraceIDFromContext.
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()

	TraceIDFromContext = func(_ context.Context) string {
		return "abc123trace"
	}

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	log.InfoCtx(context.Background(), "traced message")

	entry := sink.Last()
	if entry == nil {
		t.Fatal("expected log entry")
	}
	if entry.Message != "traced message" {
		t.Errorf("message = %q, want 'traced message'", entry.Message)
	}
	if entry.TraceID != "abc123trace" {
		t.Errorf("traceID = %q, want 'abc123trace'", entry.TraceID)
	}
	if entry.Level != LevelInfo {
		t.Errorf("level = %v, want Info", entry.Level)
	}
}

func TestDebugCtx_WithTraceID(t *testing.T) {
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()

	TraceIDFromContext = func(_ context.Context) string {
		return "debug-trace-id"
	}

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	log.DebugCtx(context.Background(), "debug traced")

	entry := sink.Last()
	if entry == nil {
		t.Fatal("expected log entry")
	}
	if entry.TraceID != "debug-trace-id" {
		t.Errorf("traceID = %q, want 'debug-trace-id'", entry.TraceID)
	}
	if entry.Level != LevelDebug {
		t.Errorf("level = %v, want Debug", entry.Level)
	}
}

func TestWarnCtx_WithTraceID(t *testing.T) {
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()

	TraceIDFromContext = func(_ context.Context) string {
		return "warn-trace-id"
	}

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	log.WarnCtx(context.Background(), "warn traced")

	entry := sink.Last()
	if entry == nil {
		t.Fatal("expected log entry")
	}
	if entry.TraceID != "warn-trace-id" {
		t.Errorf("traceID = %q, want 'warn-trace-id'", entry.TraceID)
	}
	if entry.Level != LevelWarn {
		t.Errorf("level = %v, want Warn", entry.Level)
	}
}

func TestErrorCtx_WithTraceID(t *testing.T) {
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()

	TraceIDFromContext = func(_ context.Context) string {
		return "error-trace-id"
	}

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	testErr := errors.New("something broke")
	log.ErrorCtx(context.Background(), "error traced", testErr)

	entry := sink.Last()
	if entry == nil {
		t.Fatal("expected log entry")
	}
	if entry.TraceID != "error-trace-id" {
		t.Errorf("traceID = %q, want 'error-trace-id'", entry.TraceID)
	}
	if entry.Level != LevelError {
		t.Errorf("level = %v, want Error", entry.Level)
	}
	if entry.Error == nil {
		t.Fatal("expected error info")
	}
	if entry.Error.Message != "something broke" {
		t.Errorf("error message = %q", entry.Error.Message)
	}
}

func TestCtxMethods_NoTraceExtractor(t *testing.T) {
	// When TraceIDFromContext is nil, trace ID should be empty.
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()
	TraceIDFromContext = nil

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	log.InfoCtx(context.Background(), "no trace")

	entry := sink.Last()
	if entry == nil {
		t.Fatal("expected log entry")
	}
	if entry.TraceID != "" {
		t.Errorf("traceID should be empty, got %q", entry.TraceID)
	}
}

func TestCtxMethods_EmptyTraceID(t *testing.T) {
	// When TraceIDFromContext returns empty string (no active span).
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()

	TraceIDFromContext = func(_ context.Context) string {
		return ""
	}

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	log.InfoCtx(context.Background(), "empty trace")

	entry := sink.Last()
	if entry == nil {
		t.Fatal("expected log entry")
	}
	if entry.TraceID != "" {
		t.Errorf("traceID should be empty, got %q", entry.TraceID)
	}
}

func TestCtxMethods_LevelFiltering(t *testing.T) {
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()
	TraceIDFromContext = func(_ context.Context) string { return "trace" }

	sink := NewMemorySink()
	log := New("test", LevelWarn, sink) // Only warn and above.

	log.DebugCtx(context.Background(), "filtered")
	log.InfoCtx(context.Background(), "filtered")
	log.WarnCtx(context.Background(), "kept")
	log.ErrorCtx(context.Background(), "kept", nil)

	if sink.Len() != 2 {
		t.Errorf("expected 2 entries (debug/info filtered), got %d", sink.Len())
	}
}

func TestJSONSinkResolvesGCPProjectOnce(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "proj-1")

	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)

	// Changing the env after construction must not affect the sink: the
	// project ID is resolved once and cached, not re-read on every write.
	t.Setenv("GOOGLE_CLOUD_PROJECT", "proj-2")

	sink.Write(LogEntry{
		Level:     LevelInfo,
		Message:   "hello",
		Timestamp: time.Now(),
		TraceID:   "abc-123",
	})

	out := buf.String()
	if !strings.Contains(out, "projects/proj-1/traces/abc-123") {
		t.Errorf("expected cached project proj-1 in trace field, got %q", out)
	}
	if strings.Contains(out, "proj-2") {
		t.Errorf("sink re-read env after construction; output leaked proj-2: %q", out)
	}
}

// --- trace seam -------------------------------------------------------------

func TestContextWithTraceID_PrecedesGlobalExtractor(t *testing.T) {
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()
	TraceIDFromContext = func(_ context.Context) string { return "from-global" }

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	// The logger-owned key must win over the pluggable global extractor.
	ctx := ContextWithTraceID(context.Background(), "from-key")
	log.InfoCtx(ctx, "traced")

	if got := sink.Last().TraceID; got != "from-key" {
		t.Errorf("traceID = %q, want from-key (logger-owned key precedes global)", got)
	}
}

func TestContextWithTraceID_FallsBackToGlobalExtractor(t *testing.T) {
	orig := TraceIDFromContext
	defer func() { TraceIDFromContext = orig }()
	TraceIDFromContext = func(_ context.Context) string { return "from-global" }

	sink := NewMemorySink()
	log := New("test", LevelDebug, sink)

	// No logger-owned key installed: resolution falls back to the global.
	log.InfoCtx(context.Background(), "traced")

	if got := sink.Last().TraceID; got != "from-global" {
		t.Errorf("traceID = %q, want from-global fallback", got)
	}
}

// --- slog.Group / LogValuer rendering ---------------------------------------

type logValuerStub struct{ v string }

func (l logValuerStub) LogValue() slog.Value { return slog.StringValue(l.v) }

func TestSlogValueToAny_GroupAndLogValuer(t *testing.T) {
	group := slog.GroupValue(slog.String("topic", "orders"), slog.Int("partition", 3))
	got, ok := slogValueToAny(group).(map[string]any)
	if !ok {
		t.Fatalf("group did not resolve to a map: %#v", slogValueToAny(group))
	}
	if got["topic"] != "orders" {
		t.Errorf("group topic = %v, want orders", got["topic"])
	}
	if got["partition"] != int64(3) {
		t.Errorf("group partition = %#v, want int64(3)", got["partition"])
	}

	// A LogValuer must be Resolve()d before its concrete value is taken.
	if v := slogValueToAny(slog.AnyValue(logValuerStub{v: "resolved"})); v != "resolved" {
		t.Errorf("LogValuer not resolved: %#v", v)
	}
}

func TestJSONSink_RendersSlogGroupAsObject(t *testing.T) {
	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)
	sink.Write(LogEntry{
		Level:     LevelInfo,
		Message:   "m",
		Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		Attrs:     []slog.Attr{slog.Group("event", slog.String("topic", "orders"), slog.Int("partition", 3))},
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (output=%s)", err, buf.String())
	}
	event, ok := got["event"].(map[string]any)
	if !ok {
		t.Fatalf("slog.Group did not render as a JSON object: %#v", got["event"])
	}
	if event["topic"] != "orders" || event["partition"] != float64(3) {
		t.Errorf("group object = %#v, want topic=orders partition=3", event)
	}
}

// --- ErrorAttr on a non-error record ----------------------------------------

func TestErrorAttr_WarnRendersStructuredObject(t *testing.T) {
	warnErr := perrors.New(perrors.CodeConnection, "timeout")

	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)
	sink.Write(LogEntry{
		Level:     LevelWarn,
		Message:   "query failed",
		Timestamp: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		// No entry.Error — the structured error rides in as an attr instead.
		Attrs: []slog.Attr{ErrorAttr(warnErr)},
	})

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (output=%s)", err, buf.String())
	}
	if got["severity"] != "WARNING" {
		t.Errorf("severity = %v, want WARNING", got["severity"])
	}
	errObj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("ErrorAttr did not render a structured object: %#v", got["error"])
	}
	if errObj["message"] != warnErr.Error() {
		t.Errorf("error.message = %v, want %v", errObj["message"], warnErr.Error())
	}
	if errObj["name"] != string(perrors.CodeConnection) {
		t.Errorf("error.name = %v, want %v", errObj["name"], perrors.CodeConnection)
	}
}

func TestJSONSinkGCPProjectFallback(t *testing.T) {
	// An empty GOOGLE_CLOUD_PROJECT falls back to GCP_PROJECT, also resolved
	// once at construction.
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "fallback-proj")

	var buf bytes.Buffer
	sink := NewJSONSinkWriter(&buf)
	sink.Write(LogEntry{
		Level:     LevelInfo,
		Message:   "m",
		Timestamp: time.Now(),
		TraceID:   "t-9",
	})

	if out := buf.String(); !strings.Contains(out, "projects/fallback-proj/traces/t-9") {
		t.Errorf("expected GCP_PROJECT fallback in trace, got %q", out)
	}
}
