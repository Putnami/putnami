# Errors

`go.putnami.dev/errors` provides a structured error model with typed codes, categories, attributes, stack traces, and HTTP mapping. It implements the standard `error` interface and integrates with observability systems.

## Creating errors

### Basic errors

```go
import "go.putnami.dev/errors"

// New error with code and message (captures stack trace)
err := errors.New("user.not_found", "user not found")

// Formatted message
err := errors.Newf("user.not_found", "user %s not found", userID)
```

### Wrapping errors

```go
// Wrap an existing error with a code
result, err := db.Query(ctx, query)
if err != nil {
    return errors.Wrap(err, "db.query")
}

// Wrap with a custom message
return errors.Wrapf(err, "db.query", "failed to fetch user %s", userID)
```

### Convenience constructors

```go
// User-facing error (no stack trace)
err := errors.User("validation.email", "invalid email address")

// Bug — invariant violation (captures stack trace, category=bug)
err := errors.Bug(err)

// Common HTTP errors
err := errors.BadRequest("invalid input")     // 400
err := errors.Unauthorized("not authenticated") // 401
err := errors.Forbidden("access denied")       // 403
err := errors.NotFound("user not found")        // 404
```

## Error codes

Codes use a dotted namespace convention:

```go
type Code string

// Framework codes
const (
    CodeInternal     Code = "internal"
    CodeNotFound     Code = "not_found"
    CodeValidation   Code = "validation"
    CodeTimeout      Code = "timeout"
    CodeUnauthorized Code = "unauthorized"
    CodeForbidden    Code = "forbidden"
    CodeConflict     Code = "conflict"
    CodeUnavailable  Code = "unavailable"
    CodeCancelled    Code = "canceled"
    CodeRateLimit    Code = "rate_limit"

    // HTTP-specific codes use the http namespace.
    CodeBadRequest          Code = "http.bad_request"
    CodeUnprocessableEntity Code = "http.unprocessable_entity"
    CodeTooManyRequests     Code = "http.too_many_requests"
)
```

Define domain-specific codes:

```go
const (
    CodeUserNotFound  errors.Code = "user.not_found"
    CodeEmailTaken    errors.Code = "user.email_taken"
    CodePaymentFailed errors.Code = "payment.failed"
)
```

## Categories

Categories classify errors for operational handling:

```go
type Category string

const (
    CategoryInfra    Category = "infra"     // Infrastructure failures (DB, network)
    CategoryUser     Category = "user"      // User input errors
    CategoryTransient Category = "transient" // Retryable failures
    CategoryBug      Category = "bug"       // Invariant violations
    CategorySecurity Category = "security"  // Auth/authz failures
)
```

Set a category:

```go
err := errors.New("db.connection", "connection refused").
    WithCategory(errors.CategoryInfra)
```

## Attributes

Attach structured metadata to errors:

```go
err := errors.New("order.failed", "order processing failed",
    errors.String("orderId", orderID),
    errors.Int("amount", 4999),
    errors.Float64("retryAfter", 2.5),
    errors.Bool("retriable", true),
    errors.Any("cause", originalErr),
)
```

| Function | Type | Description |
|----------|------|-------------|
| `errors.String(key, val)` | `string` | String attribute |
| `errors.Int(key, val)` | `int` | Integer attribute |
| `errors.Int64(key, val)` | `int64` | 64-bit integer attribute |
| `errors.Float64(key, val)` | `float64` | Float attribute |
| `errors.Bool(key, val)` | `bool` | Boolean attribute |
| `errors.Any(key, val)` | `any` | Arbitrary attribute |
| `errors.Duration(key, val)` | `time.Duration` | Duration attribute |

## Inspecting errors

### Error methods

```go
var err *errors.Error

err.Code()       // errors.Code
err.Message()    // string
err.Category()   // errors.Category
err.Cause()      // error (wrapped error)
err.Stack()      // string (call stack, if captured)
err.Attrs()      // []errors.Attr
err.IsRetryable() // bool
err.Error()      // string (implements error interface)
```

### Checking error codes

```go
if errors.Is(err, "user.not_found") {
    // handle not found
}

// Extract the structured error
if e := errors.GetError(err); e != nil {
    fmt.Println(e.Code(), e.Message())
}

// Get the code (returns CodeUnknown for non-structured errors)
code := errors.GetCode(err)

// Check retryability
if errors.IsRetryable(err) {
    // retry the operation
}
```

## HTTP mapping

Errors automatically map to HTTP status codes:

```go
// In an HTTP handler
func handler(ctx *fhttp.Context) *fhttp.Response {
    user, err := userService.Find(ctx.Context(), ctx.Param("id"))
    if err != nil {
        // WriteHTTPError maps error code → HTTP status
        errors.WriteHTTPError(ctx.Writer, err)
        return nil
    }
    return fhttp.JSON(user)
}
```

### Code → HTTP status mapping

| Error Code | HTTP Status |
|-----------|-------------|
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

`WriteHTTPError` exposes messages and `details` only for `user` and `security`
categories. Other categories receive a generic message; causes and stack traces
are never serialized to clients.

## Stack traces

Stack traces are captured by `New`, `Newf`, `Bug`, and `Bugf`. They are omitted
by `User`, `Wrap`, and `Wrapf` to avoid noise for expected errors:

```go
// Stack trace captured
err := errors.New("bug.nil_pointer", "unexpected nil")
fmt.Println(err.Stack())
// go.putnami.dev/myservice.ProcessOrder
//     /app/service.go:42
// go.putnami.dev/myservice.Handler
//     /app/handler.go:15

// No stack trace (user-facing error)
err := errors.User("validation.email", "invalid email")
fmt.Println(err.Stack()) // ""
```

## Framework error codes

Each framework package defines its own error codes:

| Package | Codes |
|---------|-------|
| `inject` | `inject.not_registered`, `inject.circular_dependency`, `inject.scope_violation`, `inject.container_closed`, `inject.duplicate_provider`, `inject.requirement_not_met`, `inject.type_mismatch`, `inject.factory_failed` |
| `app` | `app.already_running`, `app.configure`, `app.register`, `app.start`, `app.stop`, `app.invoke`, `app.runner` |
| `sql` | `db.connection`, `db.query`, `db.migration`, `db.transaction` |
| `http` | `http.listen`, `http.body`, `http.scope`, `http.no_server`, `http.ambiguous_server` |
| `config` | `config.source`, `config.path`, `config.mapping` |

## Support and compatibility

`go.putnami.dev/errors` is a public, documented, maintained package classified
`stable`. Its structured-error specification and accepted disclosure ADR live
next to the package source. Before v1.0.0, minor `0.x` releases may still
contain documented breaking changes; strict compatibility across every pre-1.0
minor is not promised.

## Patterns

### Service layer errors

```go
func (s *UserService) Create(ctx context.Context, input CreateUserInput) (*User, error) {
    existing, err := s.repo.FindByEmail(ctx, input.Email)
    if err != nil && !errors.Is(err, "not_found") {
        return nil, errors.Wrap(err, "db.query")
    }
    if existing != nil {
        return nil, errors.User("user.email_taken", "email already registered",
            errors.String("email", input.Email),
        )
    }

    user, err := s.repo.Create(ctx, input)
    if err != nil {
        return nil, errors.Wrapf(err, "user.create", "failed to create user %s", input.Email)
    }
    return user, nil
}
```

### Error handling in handlers

```go
func getUser(ctx *fhttp.Context) *fhttp.Response {
    user, err := userService.Find(ctx.Context(), ctx.Param("id"))
    if err != nil {
        if errors.Is(err, "not_found") {
            return fhttp.NotFound()
        }
        return fhttp.InternalError(err.Error())
    }
    return fhttp.JSON(user)
}
```

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — error responses
- [Telemetry](/docs/frameworks/go/telemetry) — error metrics
- [Logging](/docs/frameworks/go/logging) — error logging
