# Errors

`go.putnami.dev/errors` is the structured error handling library for the Putnami Go framework. Every error carries a typed code, a human-readable message, optional structured attributes, an operational classification (category, retryable), and an auto-captured stack trace. The package has zero external dependencies.

## Import

```go
import "go.putnami.dev/errors"
```

This package re-exports `errors.As`, `errors.Unwrap`, and `errors.Join` from the standard library, so you can use a single import for all error operations.

## Core Types

### Error

`*errors.Error` is the canonical error type. It implements the `error` interface and carries structured metadata:

| Field       | Type       | Description                                      |
|-------------|------------|--------------------------------------------------|
| Code        | `Code`     | Machine-readable error identifier                |
| Message     | `string`   | Human-readable description                       |
| Cause       | `error`    | Underlying error (for wrapping)                  |
| Stack       | `Stack`    | Captured call stack (only on creation, not wrap)  |
| Attrs       | `[]Attr`   | Structured key-value context                     |
| Category    | `Category` | Operational classification                       |
| Retryable   | `bool`     | Whether the caller should retry                  |
| Source      | `string`   | Component/module that produced the error         |
| Time        | `time.Time`| When the error was created                       |

### Code

`Code` is a typed string for stable, machine-readable error identification. Codes use dotted namespaces for domain-specific errors (e.g., `inject.not_registered`, `db.connection`).

Built-in codes:

| Code                | Description                    |
|---------------------|--------------------------------|
| `unknown`           | Unknown or unclassified error  |
| `internal`          | Internal / server error        |
| `unavailable`       | Service unavailable            |
| `canceled`          | Operation cancelled            |
| `timeout`           | Operation timed out            |
| `not_found`         | Resource not found             |
| `not_implemented`   | Not implemented                |
| `validation`        | Validation failure             |
| `invalid_argument`  | Invalid argument               |
| `unauthorized`      | Authentication required        |
| `forbidden`         | Permission denied              |
| `conflict`          | State conflict                 |
| `precondition_failed` | Precondition not met         |
| `already_exists`    | Resource already exists        |
| `connection`        | Connection failure             |
| `rate_limit`        | Rate limit exceeded            |

Define your own domain codes as constants:

```go
const (
    CodeUserNotFound   errors.Code = "user.not_found"
    CodeQuotaExceeded  errors.Code = "billing.quota_exceeded"
)
```

### Category

`Category` classifies errors for operational decisions (log level, alerting, retry policy, user visibility):

| Category      | Description                                    |
|---------------|------------------------------------------------|
| `CategoryInfra`     | Infrastructure failures (DB, network, disk) |
| `CategoryUser`      | User input / request errors                |
| `CategoryTransient` | Temporary, retryable failures              |
| `CategoryBug`       | Invariant violations, programmer errors    |
| `CategorySecurity`  | Authentication / authorization failures    |

### Attr

`Attr` is a typed key-value pair for attaching structured context to errors. Constructor functions ensure type safety:

```go
errors.String("user_id", "abc-123")
errors.Int("attempt", 3)
errors.Int64("bytes", 1048576)
errors.Float64("latency_ms", 12.5)
errors.Bool("cached", false)
errors.Duration("elapsed", 2*time.Second)
errors.Any("metadata", map[string]string{"region": "us-east"})
```

## Creating Errors

### New / Newf

`New` creates a structured error with automatic stack capture:

```go
err := errors.New(errors.CodeNotFound, "user not found",
    errors.String("user_id", "abc-123"),
)
```

`Newf` uses `fmt.Sprintf`-style formatting:

```go
err := errors.Newf(errors.CodeTimeout, "timed out after %ds", 30)
```

Both `New` and `Newf` capture the call stack at the point of creation and fire registered hooks.

### Wrap / Wrapf

`Wrap` wraps an existing error with a code and optional attributes. It does NOT capture a new stack trace -- the original stack (if any) is preserved via `Unwrap`. Returns `nil` if `err` is `nil`.

```go
data, err := os.ReadFile(path)
if err != nil {
    return errors.Wrap(err, errors.CodeInternal,
        errors.String("path", path),
    )
}
```

`Wrapf` wraps with a custom message instead of using the cause's message:

```go
return errors.Wrapf(err, errors.CodeConnection, "database unreachable")
```

### Bug / Bugf

`Bug` marks an error as an invariant violation (programmer error). It captures a stack trace and sets `CategoryBug`:

```go
if user == nil {
    return errors.Bug(fmt.Errorf("user must not be nil after auth"))
}

// Or with formatted message:
return errors.Bugf("invariant violated: expected %d items, got %d", expected, actual)
```

### User

`User` creates a safe, user-facing error. No stack trace is captured:

```go
return errors.User(errors.CodeBadRequest, "email address is invalid",
    errors.String("field", "email"),
)
```

### Retryable

`Retryable` marks an error as retryable. If the error is already an `*errors.Error`, it sets the flag directly; otherwise it wraps it:

```go
result, err := callExternalAPI()
if err != nil {
    return errors.Retryable(err)
}
```

## Chaining

`*Error` methods return the error for fluent chaining:

```go
err := errors.New(errors.CodeInternal, "storage write failed").
    WithSource("storage").
    WithCategory(errors.CategoryInfra).
    WithRetryable(true).
    WithAttr(errors.String("bucket", "uploads"))
```

## Inspecting Errors

### Code Matching

`Is` checks if any error in the unwrap chain matches a given code:

```go
if errors.Is(err, errors.CodeNotFound) {
    // handle not found
}
```

### Extracting Metadata

```go
// Get the code from the first *Error in the chain
code := errors.GetCode(err) // returns CodeUnknown for plain errors

// Get the full *Error
if e := errors.GetError(err); e != nil {
    log.Printf("source=%s category=%s", e.Source(), e.Category())
}

// Get the category
cat := errors.GetCategory(err)

// Collect all attributes across the entire error chain
attrs := errors.GetAttrs(err)

// Check retryability anywhere in the chain
if errors.IsRetryable(err) {
    // schedule retry
}
```

### Standard Library Compatibility

The package re-exports stdlib functions so you can use a single import:

```go
// errors.As — extract a typed error from the chain
var target *errors.Error
if errors.As(err, &target) {
    fmt.Println(target.Code())
}

// errors.Unwrap — get the direct cause
cause := errors.Unwrap(err)

// errors.Join — combine multiple errors
combined := errors.Join(err1, err2)
```

`*Error` implements `Unwrap() error`, so standard `errors.Is` and `errors.As` work through the entire chain.

## HTTP Integration

### HTTP Error Codes

The package defines HTTP-specific codes and maps all codes to HTTP status codes:

| Constructor            | Code                         | Status | Category    | Retryable |
|------------------------|------------------------------|--------|-------------|-----------|
| `BadRequest`           | `http.bad_request`           | 400    | user        | no        |
| `Unauthorized`         | `unauthorized`               | 401    | security    | no        |
| `Forbidden`            | `forbidden`                  | 403    | security    | no        |
| `NotFound`             | `not_found`                  | 404    | user        | no        |
| `MethodNotAllowed`     | `http.method_not_allowed`    | 405    | user        | no        |
| `Conflict`             | `conflict`                   | 409    | user        | no        |
| `UnprocessableEntity`  | `http.unprocessable_entity`  | 422    | user        | no        |
| `TooManyRequests`      | `http.too_many_requests`     | 429    | user        | yes       |
| `InternalServerError`  | `http.internal_server`       | 500    | infra       | no        |
| `BadGateway`           | `http.bad_gateway`           | 502    | infra       | no        |
| `ServiceUnavailable`   | `http.service_unavailable`   | 503    | transient   | yes       |
| `GatewayTimeout`       | `http.gateway_timeout`       | 504    | transient   | yes       |

```go
// Create HTTP errors with convenience constructors
return errors.NotFound("user not found",
    errors.String("user_id", id),
)

return errors.Forbidden("insufficient permissions")
```

### HTTPStatus

`HTTPStatus` resolves an error to its HTTP status code. Unknown codes and plain errors default to 500:

```go
status := errors.HTTPStatus(err) // e.g., 404
```

### WriteHTTPError

`WriteHTTPError` writes a structured JSON error response. Internal errors have their messages hidden; only `user` and `security` category errors expose messages to the client:

```go
func handler(w http.ResponseWriter, r *http.Request) {
    user, err := findUser(r.Context(), userID)
    if err != nil {
        errors.WriteHTTPError(w, err)
        return
    }
}
```

Response body format:

```json
{
    "code": "not_found",
    "error": "Not Found",
    "message": "user not found",
    "details": null
}
```

For internal errors, the message is replaced with `"An internal error occurred"` to prevent leaking implementation details.

### RegisterHTTPStatus

Register custom code-to-status mappings:

```go
errors.RegisterHTTPStatus("billing.quota_exceeded", http.StatusPaymentRequired)
```

### FromStatus

Create an error from an HTTP status code:

```go
err := errors.FromStatus(resp.StatusCode, "upstream service returned error")
```

## Validation Errors

`ValidationErrors` collects field-level validation failures and converts them to structured HTTP errors.

```go
ve := &errors.ValidationErrors{}
ve.Add("name", "is required", "required", nil)
ve.Add("age", "must be >= 0", "min", -1)

// For sensitive fields (value excluded from output)
ve.AddSensitive("password", "too short", "minlen")

if ve.HasErrors() {
    return ve.ToError() // returns a 400 BadRequest with field details
}
```

`FieldError` fields:

| Field      | Type   | Description                                |
|------------|--------|--------------------------------------------|
| Field      | string | The field name                             |
| Message    | string | Human-readable validation message          |
| Constraint | string | The constraint that failed (e.g., "required", "min") |
| Value      | any    | The rejected value (excluded for sensitive fields)    |

## Aggregate Errors

`NewAggregate` combines multiple errors into a single error. It returns `nil` if the slice is empty. The aggregate is compatible with `errors.Is` and `errors.As` via `Unwrap() []error`:

```go
var errs []error
for _, item := range items {
    if err := process(item); err != nil {
        errs = append(errs, err)
    }
}
if err := errors.NewAggregate("batch processing failed", errs); err != nil {
    return err
}
```

## JSON Serialization

`*Error` implements `json.Marshaler` and `json.Unmarshaler`. Stack traces are intentionally excluded from serialization (they are for local debugging only):

```go
data, _ := json.Marshal(err)
// {"code":"not_found","message":"user not found","category":"user","source":"handler","attrs":[...]}

var decoded errors.Error
json.Unmarshal(data, &decoded)
```

## Stack Traces

Stack traces are captured automatically by `New`, `Newf`, `Bug`, and `Bugf`. They are never captured by `Wrap`, `Wrapf`, or `User` -- the principle is "capture once at the origin."

```go
err := errors.New(errors.CodeInternal, "something failed")
fmt.Println(err.Stack().Format())
// go.putnami.dev/myapp.myFunc
//     /path/to/myapp/handler.go:42
// ...
```

The `Stack` type provides:

- `Frames() *runtime.Frames` -- iterate over stack frames
- `Format() string` -- human-readable multiline output

## Hooks

Register hooks to observe error creation without creating import cycles. Hooks fire on `New`, `Newf`, `Bug`, and `Bugf` -- never on `Wrap`. Use this for telemetry integration (span recording, metrics):

```go
remove := errors.OnError(func(err *errors.Error) {
    span := trace.SpanFromContext(ctx)
    span.RecordError(err)
    metrics.ErrorCounter.Inc(string(err.Code()))
})
```

`OnError` returns a deregister function. Long-lived wiring can ignore it; a hook with a bounded lifetime (a plugin that is set up and torn down, a test) should call `remove()` on teardown so it does not outlive its owner and accumulate across lifecycles. The returned function is idempotent.

Hooks must be goroutine-safe and non-blocking.

## Best Practices

1. **Create errors at the origin, wrap at boundaries.** Use `New`/`Newf` where the error first occurs, `Wrap`/`Wrapf` as it crosses package boundaries. This gives you one stack trace per error.

2. **Use domain-specific codes.** Define package-level `Code` constants with dotted namespaces (`"billing.quota_exceeded"`) rather than reusing generic codes everywhere.

3. **Attach context with attrs.** Instead of string-formatting context into messages, use typed attrs so they can be extracted programmatically:

    ```go
    // Prefer this:
    errors.New(CodeUserNotFound, "user not found", errors.String("user_id", id))

    // Over this:
    errors.Newf(CodeUserNotFound, "user %s not found", id)
    ```

4. **Classify errors with categories.** Set the category to drive operational behavior (alerting, log level, user visibility). Use `CategoryBug` for invariant violations, `CategoryUser` for input errors, `CategoryInfra` for infrastructure failures.

5. **Mark transient failures as retryable.** Callers can check `errors.IsRetryable(err)` to decide whether to retry, without knowing the specific error type.

6. **Use `User` for client-facing errors.** `User` errors have no stack trace (it would be meaningless) and their messages are safe to expose via `WriteHTTPError`.

7. **Use `Bug` for programmer errors.** These always capture a stack trace and are classified as `CategoryBug`, making them easy to filter in monitoring.

8. **Check `Is` by code, not by pointer.** Use `errors.Is(err, errors.CodeNotFound)` to match errors by their semantic code across the unwrap chain, regardless of wrapping depth.

## Contract and compatibility

See the [structured errors specification](../specs/structured-errors.json),
[client-safe disclosure ADR](adr/0001-client-safe-error-disclosure.md), and
[support evidence](../README.md#support-and-contract). The package is stable and
maintained, with the workspace's [pre-1.0 migration
policy](../../../../RELEASE.md), not strict compatibility between every `0.x`
minor.
