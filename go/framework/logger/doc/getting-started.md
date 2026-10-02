# Logger

Structured logging for the Putnami Go framework. The `go.putnami.dev/logger` module wraps `log/slog` with pluggable sinks, context-aware logging, and support for console, JSON, and in-memory output modes. It has zero external dependencies (stdlib only).

## Installation

```bash
go get go.putnami.dev/logger
```

## Creating a Logger

Create a logger with `New`, providing a name, minimum log level, and one or more sinks. If no sinks are provided, the logger defaults to JSON output on stdout.

```go
import "go.putnami.dev/logger"

// Default JSON sink (writes to stdout)
log := logger.New("app", logger.LevelInfo)

// Explicit console sink for human-readable output
log := logger.New("app", logger.LevelDebug, logger.NewConsoleSink())

// Multiple sinks
log := logger.New("app", logger.LevelInfo,
    logger.NewJSONSink(),
    logger.NewConsoleSink(),
)
```

A shared default logger is available via `logger.Default()`. It reads configuration from environment variables:

- `LOG_LEVEL` -- sets the minimum log level (default: `info`)

```go
log := logger.Default()
log.Info("application started")
```

## Log Levels

Four severity levels are available, mapped to `log/slog` levels:

| Level | Constant | Description |
|-------|----------|-------------|
| Debug | `logger.LevelDebug` | Verbose output for development and troubleshooting |
| Info | `logger.LevelInfo` | Normal operational messages |
| Warn | `logger.LevelWarn` | Unexpected conditions that are not errors |
| Error | `logger.LevelError` | Failures that need attention |

Messages below the logger's configured level are silently discarded.

```go
log := logger.New("app", logger.LevelWarn)

log.Debug("ignored")  // discarded (below Warn)
log.Info("ignored")   // discarded (below Warn)
log.Warn("visible")   // written
log.Error("visible", nil) // written
```

Parse a level from a string with `ParseLevel`. Unrecognized values default to `LevelInfo`:

```go
level := logger.ParseLevel("debug")   // LevelDebug
level := logger.ParseLevel("WARNING") // LevelWarn (case-insensitive)
level := logger.ParseLevel("unknown") // LevelInfo (fallback)
```

## Structured Fields

### Inline Attributes

Pass `slog.Attr` values to any log method for per-message structured data:

```go
import "log/slog"

log.Info("request handled",
    slog.String("method", "GET"),
    slog.String("path", "/api/users"),
    slog.Int("status", 200),
    slog.Duration("latency", 42*time.Millisecond),
)
```

### Persistent Context with `With`

Use `With` to create a child logger that carries fields across all subsequent log calls:

```go
reqLog := log.With("requestId", "abc-123").With("userId", "u-42")

reqLog.Info("processing request")  // includes requestId and userId
reqLog.Info("request complete")    // includes requestId and userId
```

### Child Loggers with `Named`

Create hierarchical logger names using `Named`. Child loggers inherit their parent's context fields:

```go
log := logger.New("app", logger.LevelInfo, sink)
httpLog := log.Named("http")
dbLog := log.Named("db")

httpLog.Info("listening on :8080")  // logger: "app.http"
dbLog.Info("connected")            // logger: "app.db"
```

## Error Logging

The `Error` method accepts an `error` value alongside the message. The error is captured as structured `ErrorInfo` with `name`, `message`, and optional `stack` fields:

```go
err := connectToDatabase()
if err != nil {
    log.Error("database connection failed", err,
        slog.String("host", "db.example.com"),
    )
}
```

## Context Integration

Attach a logger to a `context.Context` to pass it through the call chain. This is the recommended pattern for request-scoped logging in HTTP handlers and background workers.

```go
import "context"

// Attach a logger to a context
ctx := logger.WithLogger(ctx, log.With("requestId", reqID))

// Retrieve it later
log := logger.FromContext(ctx)
if log != nil {
    log.Info("handling request")
}

// Or get a default if none is attached
log := logger.FromContextOrDefault(ctx)
log.Info("always works")
```

- `FromContext` returns `nil` if no logger is in the context.
- `FromContextOrDefault` falls back to `logger.Default()` when none is found.

## Output Formats (Sinks)

Sinks implement the `Sink` interface:

```go
type Sink interface {
    Write(entry LogEntry)
    Flush() error
    Close() error
}
```

### JSON Sink

`NewJSONSink()` writes one JSON object per line to stdout. Compatible with Google Cloud Logging.

```go
sink := logger.NewJSONSink()
```

Output example:

```json
{"severity":"INFO","message":"request handled","timestamp":"2024-01-15T10:30:00.000Z","logger":"http","method":"GET","status":200}
```

Context fields are spread at the top level (Cloud Logging convention). When the `GOOGLE_CLOUD_PROJECT` or `GCP_PROJECT` environment variable is set, trace IDs are formatted as `projects/{project}/traces/{traceId}` under the `logging.googleapis.com/trace` key.

### Console Sink

`NewConsoleSink()` writes human-readable lines. Debug and info go to stdout; warn and error go to stderr. Error stack traces are printed on a separate line to stderr.

```go
sink := logger.NewConsoleSink()
```

Output format:

```
[traceId] [LEVEL] [logger] message key=value {"contextKey":"contextValue"}
```

Example:

```
[trace-123] [INFO] [http] request handled method=GET status=200
```

### Memory Sink

`NewMemorySink()` stores entries in memory. Designed for testing.

```go
sink := logger.NewMemorySink()
log := logger.New("test", logger.LevelDebug, sink)

log.Info("hello")

fmt.Println(sink.Len())          // 1
fmt.Println(sink.Last().Message) // "hello"

sink.Clear() // reset
```

## Lifecycle

Call `Flush()` to force all sinks to write their buffered data. Call `Close()` to release resources (flushes remaining entries):

```go
log := logger.New("app", logger.LevelInfo, logger.NewJSONSink())
defer log.Close()

// On graceful shutdown
log.Flush()
```

## Best Practices

- **Use `logger.Default()` for application bootstrapping**, then create named child loggers for each subsystem (`log.Named("http")`, `log.Named("db")`).
- **Pass loggers through `context.Context`** in request handlers rather than using package-level globals. This enables per-request fields like trace IDs and user IDs.
- **Prefer `With` for request-scoped fields** (request ID, user ID) and `slog.Attr` for event-specific data (status codes, latencies).
- **Set `LOG_LEVEL` via environment variable** to control verbosity without code changes. Use `debug` during development and `info` or `warn` in production.
- **Always defer `log.Close()`** to flush remaining entries and release sink resources on shutdown.

## Contract and compatibility

See the [structured logging specification](../specs/structured-logging.json),
[record precedence ADR](adr/0001-record-precedence-and-failure-isolation.md),
and [support evidence](../README.md#support-and-contract). The package is stable
and maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor or blanket cross-runtime parity.
