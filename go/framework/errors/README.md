# Structured Errors

The `errors` package provides the canonical error model for the Putnami Go framework. Every framework package uses this model, replacing ad-hoc `fmt.Errorf` with structured, inspectable, and observable errors.

## Error Anatomy

An `*errors.Error` carries:

| Field | Purpose |
|-------|---------|
| **Code** | Dotted namespace string (e.g., `"db.connection"`, `"inject.not_registered"`) |
| **Message** | Human-readable description |
| **Cause** | Wrapped underlying error (optional) |
| **Stack** | Call stack captured at creation (only for `New`, `Newf`, `Bug`, `Bugf`) |
| **Attrs** | Structured key-value pairs for observability |
| **Category** | Operational classification: infra, user, transient, bug, security |
| **Retryable** | Whether the operation can be retried |
| **Source** | Origin identifier (service name, module) |

## Creating Errors

### New — fresh error with stack

```go
err := errors.New(errors.CodeNotFound, "user not found",
    errors.String("user_id", "abc-123"),
)
```

### Wrap — add code and attrs to an existing error (no stack)

```go
row, err := db.Query(ctx, sql)
if err != nil {
    return errors.Wrap(err, CodeQuery, errors.String("sql", sql))
}
```

### Wrapf — same as Wrap, with a custom message

```go
return errors.Wrapf(err, CodeConnection, "connect to primary", errors.String("dsn", dsn))
```

### Bug — invariant violation (captures stack, category=bug)

```go
if slice == nil {
    return errors.Bug(fmt.Errorf("slice must not be nil"))
}
```

### User — user-facing error (no stack, category=user)

```go
return errors.User(errors.CodeBadRequest, "email is required")
```

### Newf — formatted message with stack

```go
return errors.Newf(CodeMigration, "migration %q failed at step %d", name, step)
```

## Error Codes

Every package defines its own codes as `errors.Code` constants:

```go
const (
    CodeConnection  errors.Code = "db.connection"
    CodeQuery       errors.Code = "db.query"
    CodeMigration   errors.Code = "db.migration"
    CodeTransaction errors.Code = "db.transaction"
)
```

### Framework codes

| Package | Codes |
|---------|-------|
| `errors` | `unknown`, `internal`, `unavailable`, `canceled`, `timeout`, `not_found`, `not_implemented`, `validation`, `invalid_argument`, `unauthorized`, `forbidden`, `conflict`, `precondition_failed`, `already_exists`, `connection`, `rate_limit` |
| `inject` | `inject.not_registered`, `inject.circular_dependency`, `inject.scope_violation`, `inject.container_closed`, `inject.duplicate_provider`, `inject.requirement_not_met`, `inject.validation`, `inject.type_mismatch`, `inject.factory_failed`, `inject.illegal_state` |
| `app` | `app.already_running`, `app.configure`, `app.register`, `app.start`, `app.stop`, `app.invoke`, `app.runner` |
| `sql` | `db.connection`, `db.query`, `db.migration`, `db.transaction` |
| `http` | `http.listen`, `http.body`, `http.scope` |
| `grpc` | `grpc.listen`, `grpc.scope`, `grpc.panic` |
| `storage` | `storage.read`, `storage.write`, `storage.delete`, `storage.list`, `storage.not_found`, `storage.request`, `storage.failed` |
| `client` | `client.circuit_open`, `client.request`, `client.response`, `client.transport` |
| `events` | `events.stopped`, `events.drain_timeout`, `events.no_transport` |
| `config` | `config.source`, `config.path`, `config.mapping` |
| `telemetry` | `telemetry.shutdown` |

## Inspecting Errors

```go
// Check error code
if errors.Is(err, errors.CodeNotFound) {
    // handle not found
}

// Extract structured error
if e := errors.GetError(err); e != nil {
    fmt.Println(e.Code(), e.Category(), e.Attrs())
}

// Check retryability
if errors.IsRetryable(err) {
    // retry the operation
}

// Get code from any error
code := errors.GetCode(err) // returns CodeUnknown for non-structured errors
```

## Categories

Categories drive operational decisions — retry policy, log level, alerting, and user visibility:

| Category | Purpose | Example |
|----------|---------|---------|
| `infra` | Infrastructure failures | DB connection lost, disk full |
| `user` | User input/request errors | Invalid email, missing field |
| `transient` | Temporary, retryable failures | Rate limited, temporary unavailable |
| `bug` | Invariant violations | Nil pointer, impossible state |
| `security` | Auth failures | Invalid token, insufficient permissions |

```go
err := errors.New(errors.CodeConnection, "database unreachable").
    WithCategory(errors.CategoryInfra)
```

## HTTP Mapping

Errors map automatically to HTTP status codes:

```go
// In an HTTP handler:
errors.WriteHTTPError(w, err)
```

| Code | HTTP Status |
|------|-------------|
| `http.bad_request` | 400 |
| `unauthorized` | 401 |
| `forbidden` | 403 |
| `not_found` | 404 |
| `conflict` | 409 |
| `validation`, `invalid_argument` | 400 |
| `http.unprocessable_entity` | 422 |
| `rate_limit`, `http.too_many_requests` | 429 |
| `internal` | 500 |
| `unavailable` | 503 |
| `timeout` | 504 |

For `user` and `security` category errors, the error message is exposed in the HTTP response body. Internal errors return a generic message.

Convenience constructors:

```go
errors.BadRequest("email is required")
errors.NotFound("user not found", errors.String("id", userID))
errors.Unauthorized("invalid token")
errors.Forbidden("insufficient permissions")
```

## Telemetry Bridge

The `telemetry` package automatically bridges structured errors to OpenTelemetry:

- **Counter**: `errors_total` with labels `error.code`, `error.category`, `error.source`
- **Spans**: `telemetry.SetSpanErrorStructured(span, err)` records code, category, and retryable as span attributes

The bridge activates via hooks — no import cycle between `errors` and `telemetry`:

```go
// Automatic: errors.OnError hook is registered during telemetry plugin configure.
// Every errors.New() or errors.Bug() call fires the hook.
```

## JSON Serialization

The structured value implements JSON marshaling for diagnostics and local
persistence. Treat it as an implementation format, not a cross-service wire
contract:

```go
data, _ := json.Marshal(err)
// The message is replaced with the generic internal text; attrs, cause, and
// stack are omitted.
```

Only `user` and `security` errors expose their message and attributes. Other
categories use a generic message; causes and stack traces are never included.

## Design Rules

1. **Stack captured once** — only `New`, `Newf`, `Bug`, and `Bugf` capture a stack. `Wrap`/`Wrapf` never do.
2. **Wrap adds signal** — use code + attrs, not text concatenation.
3. **Nil-safe** — `Wrap(nil, ...)` returns nil. `Bug(nil)` returns nil.
4. **stdlib compatible** — implements `error`, `Unwrap()`, and works with `errors.Is`/`errors.As`.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/errors` is public, documented, maintained, and classified
`stable` in the workspace [support catalog](../../../putnami.support.json).
Before v1.0.0, a minor `0.x` release may still contain a breaking change; the
[release policy](../../../RELEASE.md) requires release notes and migration
documentation rather than strict compatibility between every pre-1.0 minor.

The [structured errors specification](specs/structured-errors.json) and
[client-safe disclosure ADR](doc/adr/0001-client-safe-error-disclosure.md) define
the package contract. Regression evidence covers [creation, wrapping, and
inspection](errors_test.go), [HTTP disclosure and error-tree behavior](audit_fixes_test.go),
and [concurrent hooks](concurrent_test.go).
