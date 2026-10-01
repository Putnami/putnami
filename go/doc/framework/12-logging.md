# Logging

`go.putnami.dev/logger` provides structured logging with pluggable sinks, context-aware propagation, and output formats compatible with Google Cloud Logging.

## Default logger

```go
import "go.putnami.dev/logger"

log := logger.Default()
log.Info("server started")
log.Error("connection failed", err)
```

The default logger reads configuration from environment variables:

| Env var | Description |
|---------|-------------|
| `LOG_LEVEL` | Minimum log level: `debug`, `info`, `warn`, `error` (default: `info`) |
| `K_SERVICE` | When set (Cloud Run), uses JSON sink automatically |

## Creating loggers

```go
log := logger.New("my-service", logger.LevelInfo,
    logger.NewJSONSink(),
)

// With console output
log := logger.New("my-service", logger.LevelDebug,
    logger.NewConsoleSink(),
)
```

## Log levels

```go
type Level int

const (
    LevelDebug Level = iota
    LevelInfo
    LevelWarn
    LevelError
)
```

Parse from strings:

```go
level := logger.ParseLevel("debug") // LevelDebug
level := logger.ParseLevel("info")  // LevelInfo
level := logger.ParseLevel("warn")  // LevelWarn
level := logger.ParseLevel("error") // LevelError
```

## Logging methods

```go
log.Debug("processing request")
log.Info("user created")
log.Warn("deprecated API called")
log.Error("query failed", err)
```

## Structured context

Add persistent fields to a logger:

```go
// Named child logger
requestLog := log.Named("http")
// Output: [my-service.http] ...

// Add key-value context
userLog := log.With("userId", "user-123", "requestId", "req-456")
userLog.Info("processing order")
// Output includes userId=user-123 requestId=req-456
```

Child loggers inherit the parent's sinks and level but add their own context fields.

## Sinks

### JSON sink

Structured JSON output compatible with Google Cloud Logging:

```go
sink := logger.NewJSONSink()
```

Output format:

```json
{"severity":"INFO","message":"server started","timestamp":"2024-01-15T10:30:00Z","logger":"my-service"}
```

### Console sink

Human-readable text output for local development:

```go
sink := logger.NewConsoleSink()
```

Output format:

```
2024-01-15 10:30:00 INFO  [my-service] server started
```

### Memory sink

In-memory sink for testing:

```go
sink := logger.NewMemorySink()

log := logger.New("test", logger.LevelDebug, sink)
log.Info("test message")

// Inspect entries
fmt.Println(sink.Len())            // 1
fmt.Println(sink.Last().Message)   // "test message"

// All entries
for _, entry := range sink.Entries {
    fmt.Println(entry.Level, entry.Message)
}

// Clear
sink.Clear()
```

## Context integration

Attach a logger to `context.Context` for propagation through the call chain:

```go
// Attach to context
ctx := logger.WithLogger(ctx, log.With("requestId", requestID))

// Retrieve from context
func processRequest(ctx context.Context) {
    log := logger.FromContext(ctx)
    if log != nil {
        log.Info("processing")
    }

    // Or with fallback to default
    log = logger.FromContextOrDefault(ctx)
    log.Info("processing")
}
```

## Patterns

### Per-request logging

```go
func requestLogger() fhttp.Middleware {
    return func(ctx *fhttp.Context, next func() *fhttp.Response) *fhttp.Response {
        log := logger.Default().With(
            "method", ctx.Method,
            "path", ctx.Path,
            "requestId", ctx.Header("X-Request-ID"),
        )

        // Attach to context for downstream use
        goCtx := logger.WithLogger(ctx.Context(), log)
        ctx = ctx.WithContext(goCtx)

        start := time.Now()
        resp := next()
        log.Info(fmt.Sprintf("completed in %dms", time.Since(start).Milliseconds()))
        return resp
    }
}
```

### Service-level logging

```go
type UserService struct {
    log  *logger.Logger
    repo *UserRepository
}

func NewUserService(repo *UserRepository) *UserService {
    return &UserService{
        log:  logger.Default().Named("UserService"),
        repo: repo,
    }
}

func (s *UserService) Create(ctx context.Context, input CreateInput) (*User, error) {
    s.log.Info("creating user")
    user, err := s.repo.Create(ctx, input)
    if err != nil {
        s.log.Error("failed to create user", err)
        return nil, err
    }
    s.log.Info("user created")
    return user, nil
}
```

## Related guides

- [Telemetry](/docs/frameworks/go/telemetry) — trace-correlated logging
- [Errors](/docs/frameworks/go/errors) — structured error logging
- [HTTP & Middleware](/docs/frameworks/go/http) — request logging middleware

## Support and compatibility

`go.putnami.dev/logger` is a public, documented, maintained package classified
`stable`. Its structured-logging specification and accepted record-precedence
ADR live next to the package source. Before v1.0.0, minor `0.x` releases may
still contain documented breaking changes; strict compatibility across every
pre-1.0 minor or blanket cross-runtime parity is not promised.
