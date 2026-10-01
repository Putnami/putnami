// Package logger provides structured logging for the Putnami Go framework.
// It wraps log/slog with pluggable sinks, context-aware logging, and
// support for console, JSON, and buffered output modes.
//
// The default output is JSON (matching the TypeScript runtime).
// Configuration is read from environment variables:
//   - LOG_LEVEL: debug, info, warn, error (default: info)
package logger

import (
	"context"
	stderrors "errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	perrors "go.putnami.dev/errors"
)

// TraceIDFromContext is a pluggable function that extracts a trace ID from a
// context.Context. It defaults to nil (no extraction). Telemetry packages
// should set this at init time to enable trace correlation in logs.
//
// Example (in go.putnami.dev/telemetry):
//
//	func init() {
//	    logger.TraceIDFromContext = func(ctx context.Context) string {
//	        sc := trace.SpanContextFromContext(ctx)
//	        if sc.HasTraceID() { return sc.TraceID().String() }
//	        return ""
//	    }
//	}
var TraceIDFromContext func(ctx context.Context) string

// Level represents a log severity level.
type Level = slog.Level

// Log severity levels.
const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// LogEntry represents a single log record.
type LogEntry struct {
	Level     Level
	Message   string
	Timestamp time.Time
	Logger    string
	TraceID   string
	Context   map[string]any
	Error     *ErrorInfo
	Attrs     []slog.Attr
}

// ErrorInfo holds structured error information.
type ErrorInfo struct {
	Name      string         `json:"name"`
	Message   string         `json:"message"`
	Stack     string         `json:"stack,omitempty"`
	Code      string         `json:"code,omitempty"`
	Category  string         `json:"category,omitempty"`
	Source    string         `json:"source,omitempty"`
	Retryable bool           `json:"retryable,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty"`
}

// Sink is the output destination for log entries.
type Sink interface {
	// Write outputs a single log entry.
	Write(entry LogEntry)
	// Flush ensures all buffered entries are written.
	Flush() error
	// Close releases resources held by the sink.
	Close() error
}

// Logger provides structured logging with pluggable sinks and context support.
type Logger struct {
	name    string
	level   Level
	sinks   []Sink
	context map[string]any
	mu      sync.RWMutex
}

// New creates a new logger with the given name and sinks.
// If no sinks are provided, it uses the default sink (JSON to stdout).
func New(name string, level Level, sinks ...Sink) *Logger {
	if len(sinks) == 0 {
		sinks = []Sink{NewJSONSink()}
	}
	return &Logger{
		name:    name,
		level:   level,
		sinks:   sinks,
		context: make(map[string]any),
	}
}

// Named creates a child logger with an appended name.
func (l *Logger) Named(name string) *Logger {
	fullName := name
	if l.name != "" {
		fullName = l.name + "." + name
	}
	child := &Logger{
		name:    fullName,
		level:   l.level,
		sinks:   l.sinks,
		context: make(map[string]any),
	}
	// Copy parent context
	l.mu.RLock()
	for k, v := range l.context {
		child.context[k] = v
	}
	l.mu.RUnlock()
	return child
}

// With returns a new logger with additional context fields.
func (l *Logger) With(key string, value any) *Logger {
	child := l.Named("")
	child.name = l.name
	child.context[key] = value
	return child
}

// Debug logs a debug message.
func (l *Logger) Debug(msg string, attrs ...slog.Attr) {
	l.emit(LevelDebug, msg, nil, attrs)
}

// Info logs an informational message.
func (l *Logger) Info(msg string, attrs ...slog.Attr) {
	l.emit(LevelInfo, msg, nil, attrs)
}

// Warn logs a warning message.
func (l *Logger) Warn(msg string, attrs ...slog.Attr) {
	l.emit(LevelWarn, msg, nil, attrs)
}

// Error logs an error message with an optional error value.
func (l *Logger) Error(msg string, err error, attrs ...slog.Attr) {
	l.emit(LevelError, msg, err, attrs)
}

// DebugCtx logs a debug message with trace context extraction.
func (l *Logger) DebugCtx(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emitCtx(ctx, LevelDebug, msg, nil, attrs)
}

// InfoCtx logs an informational message with trace context extraction.
func (l *Logger) InfoCtx(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emitCtx(ctx, LevelInfo, msg, nil, attrs)
}

// WarnCtx logs a warning message with trace context extraction.
func (l *Logger) WarnCtx(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.emitCtx(ctx, LevelWarn, msg, nil, attrs)
}

// ErrorCtx logs an error message with trace context extraction.
func (l *Logger) ErrorCtx(ctx context.Context, msg string, err error, attrs ...slog.Attr) {
	l.emitCtx(ctx, LevelError, msg, err, attrs)
}

type traceIDKey struct{}

// ContextWithTraceID attaches a trace ID to ctx using the logger-owned key.
// The *Ctx methods resolve this key before the pluggable TraceIDFromContext
// global, so a caller (e.g. event dispatch) can seed a trace ID even when no
// telemetry span is active.
func ContextWithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

func extractTraceID(ctx context.Context) string {
	if ctx != nil {
		if id, ok := ctx.Value(traceIDKey{}).(string); ok && id != "" {
			return id
		}
	}
	if TraceIDFromContext != nil {
		return TraceIDFromContext(ctx)
	}
	return ""
}

// ErrorAttr returns an attr keyed "error" whose value is the structured
// ErrorInfo for err, letting a non-Error-level record (e.g. a Warn) carry a
// structured error object. The JSON sink leaves a user "error" attr intact when
// entry.Error is nil, so the ErrorInfo renders as a proper JSON object.
func ErrorAttr(err error) slog.Attr {
	return slog.Any("error", buildErrorInfo(err))
}

// Flush flushes all sinks.
func (l *Logger) Flush() error {
	var firstErr error
	for _, sink := range l.sinks {
		if err := sink.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close closes all sinks.
func (l *Logger) Close() error {
	var firstErr error
	for _, sink := range l.sinks {
		if err := sink.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Logger) emit(level Level, msg string, err error, attrs []slog.Attr) {
	if level < l.level {
		return
	}

	entry := LogEntry{
		Level:     level,
		Message:   msg,
		Timestamp: time.Now(),
		Logger:    l.name,
		Attrs:     attrs,
	}

	// Copy context
	l.mu.RLock()
	if len(l.context) > 0 {
		entry.Context = make(map[string]any, len(l.context))
		for k, v := range l.context {
			entry.Context[k] = v
		}
	}
	l.mu.RUnlock()

	// Set error info
	if err != nil {
		entry.Error = buildErrorInfo(err)
	}

	// Write to all sinks. Each write is isolated behind a recover so a logged
	// value with a panicking MarshalJSON/MarshalText (encoding/json does not
	// recover these) cannot propagate out of log.Info/Error and crash the
	// caller — observability must never take down what it observes. Mirrors
	// errors.fireHooks.
	for _, sink := range l.sinks {
		writeToSink(sink, entry)
	}
}

// emitCtx builds and writes a context-aware entry: it resolves the trace ID
// through the logger-owned key then the pluggable global, and merges any
// request-scoped FieldBag over the logger's With context (bag wins per key;
// when both sides are map[string]any they shallow-merge, bag winning per field).
// Call-site attrs and framework-reserved keys still win at the sink, so the
// overall precedence is With < bag < attrs < reserved.
func (l *Logger) emitCtx(ctx context.Context, level Level, msg string, err error, attrs []slog.Attr) {
	if level < l.level {
		return
	}

	entry := LogEntry{
		Level:     level,
		Message:   msg,
		Timestamp: time.Now(),
		Logger:    l.name,
		TraceID:   extractTraceID(ctx),
		Attrs:     attrs,
	}

	// Base context = copy of the logger's With context.
	l.mu.RLock()
	if len(l.context) > 0 {
		entry.Context = make(map[string]any, len(l.context))
		for k, v := range l.context {
			entry.Context[k] = v
		}
	}
	l.mu.RUnlock()

	// Merge the request-scoped field bag over the With context (bag wins).
	if bag := fieldBagFrom(ctx); bag != nil {
		if snap := bag.Snapshot(); len(snap) > 0 {
			if entry.Context == nil {
				entry.Context = make(map[string]any, len(snap))
			}
			mergeContext(entry.Context, snap)
		}
	}

	if err != nil {
		entry.Error = buildErrorInfo(err)
	}

	for _, sink := range l.sinks {
		writeToSink(sink, entry)
	}
}

// mergeContext merges src into dst with src winning per key. When both the
// existing and incoming values are map[string]any, they shallow-merge with src
// winning per field, into a fresh map so neither source map is mutated.
func mergeContext(dst, src map[string]any) {
	for k, v := range src {
		if existing, ok := dst[k].(map[string]any); ok {
			if incoming, ok := v.(map[string]any); ok {
				merged := make(map[string]any, len(existing)+len(incoming))
				for ek, ev := range existing {
					merged[ek] = ev
				}
				for ik, iv := range incoming {
					merged[ik] = iv
				}
				dst[k] = merged
				continue
			}
		}
		dst[k] = v
	}
}

// writeToSink writes one entry to a sink, swallowing any panic raised while
// marshaling/formatting so a bad logged value cannot unwind the caller.
func writeToSink(sink Sink, entry LogEntry) {
	defer func() { _ = recover() }() //nolint:errcheck // swallow sink panics to protect the logging path
	sink.Write(entry)
}

func buildErrorInfo(err error) *ErrorInfo {
	if err == nil {
		return nil
	}

	info := &ErrorInfo{
		Name:    errorName(err),
		Message: err.Error(),
	}

	if code := perrors.GetCode(err); code != perrors.CodeUnknown {
		info.Code = string(code)
	}
	if category := perrors.GetCategory(err); category != "" {
		info.Category = string(category)
	}
	if perrors.IsRetryable(err) {
		info.Retryable = true
	}

	if attrs := perrors.GetAttrs(err); len(attrs) > 0 {
		info.Attrs = make(map[string]any, len(attrs))
		for _, attr := range attrs {
			if attr.Key == "" {
				continue
			}
			if _, exists := info.Attrs[attr.Key]; exists {
				continue
			}
			info.Attrs[attr.Key] = attr.Value
		}
	}

	for current := err; current != nil; current = stderrors.Unwrap(current) {
		var structured *perrors.Error
		if !stderrors.As(current, &structured) || structured == nil {
			continue
		}
		if info.Source == "" && structured.Source() != "" {
			info.Source = structured.Source()
		}
		if info.Stack == "" && len(structured.Stack()) > 0 {
			info.Stack = structured.Stack().Format()
		}
		if info.Source != "" && info.Stack != "" {
			break
		}
	}

	return info
}

// errorName derives a meaningful identifier for an error. It prefers the
// structured perrors code name when present (matching the TS runtime's
// Error.name semantics) and otherwise falls back to the concrete Go type
// (e.g. "*os.PathError"), so the emitted error.name field always carries
// information about the real error rather than a constant.
func errorName(err error) string {
	if code := perrors.GetCode(err); code != perrors.CodeUnknown {
		return string(code)
	}
	if t := reflect.TypeOf(err); t != nil {
		return t.String()
	}
	return "error"
}

// --- Level parsing ---

// ParseLevel converts a string level name to a Level.
// Supported values: "debug", "info", "warn", "error" (case-insensitive).
// Returns LevelInfo for unrecognized values.
func ParseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// --- Context integration ---

type loggerKey struct{}

// WithLogger attaches a logger to a context.
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// FromContext retrieves the logger from a context.
// Returns nil if no logger is attached.
func FromContext(ctx context.Context) *Logger {
	l, ok := ctx.Value(loggerKey{}).(*Logger)
	if !ok {
		return nil
	}
	return l
}

// FromContextOrDefault retrieves the logger from context or returns a default.
func FromContextOrDefault(ctx context.Context) *Logger {
	if l := FromContext(ctx); l != nil {
		return l
	}
	return Default()
}

var (
	defaultLoggerOnce sync.Once
	defaultLoggerInst *Logger
)

// Default returns the shared default logger.
// The logger is configured from environment variables:
//   - LOG_LEVEL: sets the minimum log level (default: info)
//
// The default output format is JSON (matching the TypeScript runtime).
// Console (text) output is used only when explicitly requested.
func Default() *Logger {
	defaultLoggerOnce.Do(func() {
		level := ParseLevel(os.Getenv("LOG_LEVEL"))
		defaultLoggerInst = New("", level)
	})
	return defaultLoggerInst
}
