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

## Default Logger

`logger.Default()` returns a shared JSON logger whose minimum level comes from
`LOG_LEVEL` (default: info).

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the
[structured logging specification](specs/structured-logging.json) and [record
precedence ADR](doc/adr/0001-record-precedence-and-failure-isolation.md). Before
v1.0, follow the workspace [migration-based compatibility
policy](../../../RELEASE.md); do not infer strict compatibility between every
`0.x` minor or blanket cross-runtime parity.
