# go.putnami.dev/logger

Structured logging with pluggable sinks (console, JSON/Cloud Logging, memory).

## Quick Start

```go
import (
	"log/slog"

	"go.putnami.dev/logger"
)

log := logger.New("app", logger.LevelInfo, logger.NewConsoleSink())

log.Info("server started", slog.Int("port", 8080))
log.Error("request failed", err, slog.String("path", "/api/users"))
log.Warn("deprecated endpoint", slog.String("path", "/v1/old"))
log.Debug("cache hit", slog.String("key", cacheKey))
```

Log methods accept `...slog.Attr` (build attrs with `slog.String`, `slog.Int`,
etc.). `Error` takes the error as its second argument. When `New` is called with
no sinks it defaults to a JSON sink on stdout.

## Log Levels

Pass a level to `New` (`logger.LevelDebug`, `LevelInfo`, `LevelWarn`,
`LevelError`). To honor the `LOG_LEVEL` env var, parse it with
`logger.ParseLevel`:

```go
log := logger.New("app", logger.ParseLevel(os.Getenv("LOG_LEVEL")), logger.NewJSONSink())
```

## Sinks

```go
// Console (human-readable plain text; debug/info to stdout, warn/error to stderr)
logger.New("app", logger.LevelInfo, logger.NewConsoleSink())

// JSON (Cloud Logging format) -- used by default when no sink is provided
logger.New("app", logger.LevelInfo, logger.NewJSONSink())

// Memory (testing)
sink := logger.NewMemorySink()
log := logger.New("app", logger.LevelInfo, sink)
// sink.Entries holds captured entries; sink.Len() and sink.Last() are also available
```

## With Context

```go
log = log.With("request_id", reqID).With("user_id", userID)
log.Info("processing request") // includes request_id and user_id
```

`With` adds a single key/value pair and returns a child logger; chain it to add
several fields.

## Default Logger and the Live Level

`logger.Default()` returns a shared JSON logger. It and every logger derived
from it (`Named`, `With`) read one process-wide level on each record:

- The config key `logger.level` (`debug`, `info`, `warn`, `error`) sets it.
  Without the key, `LOG_LEVEL` does; otherwise it is `info`.
- A started `go.putnami.dev/app` application re-reads `logger.level` from its
  conf files and `CONFIG_DATA` every `logger.refreshSeconds` (default 30, 0
  turns it off) and applies a changed value without a restart.
- `logger.SetLevel(level)` changes it at runtime; `logger.CurrentLevel()`
  reads it. The last change wins.
- A logger `New` builds keeps its own level.

A record at error or above is emitted whatever the level.
`logger.RecordCounts()` returns the cumulative records by level and outcome
(`emitted`, `dropped`); the telemetry plugin exports them as `log.records`.

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the
[structured logging specification](specs/structured-logging.json) and [record
precedence ADR](doc/adr/0001-record-precedence-and-failure-isolation.md) and
[live level ADR](doc/adr/0002-the-log-level-is-live.md). Before
v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor or blanket cross-runtime parity.
