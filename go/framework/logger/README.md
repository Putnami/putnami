# Logger

The `logger` package provides structured logging with pluggable sinks, context-aware logging, and support for console, JSON, and in-memory output modes. It wraps `log/slog` for compatibility with the Go standard library.

The Go and TypeScript runtimes share selected conventions: JSON output by
default, `LOG_LEVEL` support, and a checked-in conformance corpus for the
records that explicitly opt into it. This does not claim that every sink or
output shape is identical across runtimes.

## Default Logger

The recommended way to get a logger is through `Default()`, which reads configuration from environment variables:

```go
import "go.putnami.dev/logger"

log := logger.Default().Named("my-service")
defer log.Close()
```

Environment variables:
- `LOG_LEVEL`: `debug`, `info`, `warn`, `error` (default: `info`)

## Creating a Custom Logger

For custom sink configurations:

```go
log := logger.New("my-service", logger.LevelInfo,
    logger.NewJSONSink(),
)
defer log.Close()
```

If no sinks are provided, a JSON sink to stdout is used by default.

## Log Levels

```go
log.Debug("debug details")           // filtered at LevelInfo
log.Info("request handled")
log.Warn("high latency detected")
log.Error("failed to connect", err)  // accepts an error value
```

Levels: `LevelDebug`, `LevelInfo`, `LevelWarn`, `LevelError`.

Parse from string: `logger.ParseLevel("debug")`.

## Child Loggers

### Named

Create a child logger with an appended name:

```go
httpLog := log.Named("http")
httpLog.Info("request") // logger name: "my-service.http"
```

### With Context Fields

Add structured key-value pairs:

```go
reqLog := log.With("requestId", "abc-123")
reqLog.Info("processing") // includes requestId in output
```

## Sinks

### JSONSink

Structured JSON output to stdout (GCP Cloud Logging compatible). This is the default sink.

```go
sink := logger.NewJSONSink()
// Output: {"severity":"WARNING","message":"high latency","duration_ms":1500}
```

The keys `severity`, `message`, `timestamp`, `logger`, and the trace keys
(`traceId` / `logging.googleapis.com/trace`) are reserved by the framework:
a context field or attr with one of those names cannot overwrite or forge
the framework value — colliding user keys are dropped from the output.

TraceID handling matches the TypeScript runtime:
- With `GOOGLE_CLOUD_PROJECT` or `GCP_PROJECT` env var: formats as `logging.googleapis.com/trace`
- Without: uses plain `traceId` field

### ConsoleSink

Human-readable text output matching the TypeScript runtime format:

```go
sink := logger.NewConsoleSink()
// Output: [INFO] [app] hello world
// With trace: [trace-123] [INFO] [app] hello world
```

Routing: debug/info to stdout, warn/error to stderr. Error stack traces are printed separately on stderr.

### MemorySink

In-memory sink for testing:

```go
sink := logger.NewMemorySink()
// After logging:
sink.Len()      // number of entries
sink.Last()     // most recent entry
sink.Entries    // all entries
sink.Clear()    // reset
```

## Request-scoped field accumulation

`With` returns a child logger, which last-write-loses when the same work happens
several times inside one request. For that, a boundary installs a `FieldBag` on
the context and every contributor writes into it; the boundary's single terminal
record then carries the accumulated fields:

```go
// Boundary (e.g. the HTTP logging middleware):
bag := logger.NewFieldBag()
ctx = logger.ContextWithFieldBag(ctx, bag)

// Anywhere inside the boundary — no-ops when ctx carries no bag:
logger.Set(ctx, "event", map[string]any{"topic": topic})       // merge/overwrite
logger.Append(ctx, "event.publishes", map[string]any{"topic": topic, "messageId": id})
logger.Increment(ctx, "event.publishCount", 1)
logger.SetRequestError(ctx, err)                                // the real failure

// The terminal record (a *Ctx call) merges the bag over the logger's With context:
log.InfoCtx(ctx, msg, slog.Any("http", httpGroup))
```

- Paths are dot-separated and address nested groups; `Append` promotes an existing
  scalar to a slice rather than losing it.
- `Append` is capped at `MaxAppendedFieldValues` (100) and then drops **silently**
  — always pair it with `Increment` of a sibling counter and read the true total
  from the counter. `@putnami/runtime` defines the same constant with the same
  value.
- `SetRequestError` uses a dedicated slot, so the terminal record's `error` cannot
  be forged through `Set(ctx, "error", …)`; read it back with
  `logger.RequestError(ctx)`.
- `ContextWithFieldBag` always shadows a parent bag: each boundary owns its own
  terminal record.

## Cross-runtime log conformance (`logtest`)

`go.putnami.dev/logger/logtest` executes the canonical corpus in
`protocols/logging/conformance` against real emitters, capturing the real
`JSONSink` output:

```go
suite := logtest.LoadCases(t, "../../../protocols/logging/conformance/manifest.json", "event")
rec := logtest.NewRecorder(t)                  // captures the real JSON lines
broker.logs = eventLoggersFrom(rec.Root())     // …drive real boundary code…
want := suite.Case(t, "event.terminal.success")
logtest.AssertRecord(t, rec.Record(t, want), want)
```

`Record` requires exactly one record for the case's pinned (logger, message) pair,
so a duplicated terminal record fails. `CompareRecord` is the pure core (tokens,
`$open`, sorted-key 2-space canonical form) and the TypeScript twin is
`@putnami/runtime/testing`'s `log-conformance`.

## Context Integration

Attach a logger to a `context.Context` for propagation through call stacks:

```go
ctx := logger.WithLogger(ctx, log)

// Later, retrieve it:
log := logger.FromContext(ctx)
log.Info("from context")

// Or get a default if none attached:
log := logger.FromContextOrDefault(ctx)
```

## Auto-Detection

The default logger automatically uses JSON output (matching the TypeScript runtime default). Console (text) output is available by explicitly using `NewConsoleSink()`.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/logger` is public, documented, maintained, and classified
`stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [structured logging specification](specs/structured-logging.json) and
[record precedence ADR](doc/adr/0001-record-precedence-and-failure-isolation.md)
define the package contract. Regression evidence covers [logger
behavior](logger_test.go), [field accumulation](fields_test.go), [sink
isolation and lifecycle](coverage_test.go), and the [`logtest` conformance
adapter](logtest/logtest_test.go). The corpus is scoped evidence; it is not a
blanket parity designation.

This project is a pilot of the executable spec gate:
[`putnami.features.json`](putnami.features.json) declares acceptance checks
per requirement, each protecting test binds itself with `spectest.Proves`,
and `options.sdd.verification.specs` is `enforce` in
[`putnami.json`](putnami.json) — deleting, skipping, or renaming a declared
check fails this project's normal `test` gate. `putnami specs verify` replays
the recorded verdict.
