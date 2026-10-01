# go.putnami.dev/errors

Structured errors with typed codes, categories, HTTP mapping, retryable flags, and stack capture.

## Quick Start

```go
import "go.putnami.dev/errors"

// Create with a typed Code
const CodeUserNotFound errors.Code = "user.not_found"

err := errors.New(CodeUserNotFound, "user not found", errors.String("id", userID))

// Wrap existing errors
err = errors.Wrap(originalErr, CodeUserNotFound)
err = errors.Wrapf(originalErr, CodeUserNotFound, "failed to fetch user")

// Check error codes
if errors.Is(err, CodeUserNotFound) { ... }

// Extract code
code := errors.GetCode(err) // errors.Code("user.not_found")
```

## Error Codes

Typed `Code` constants use dotted namespaces. Framework-wide codes:

```go
errors.CodeNotFound       // "not_found"
errors.CodeValidation     // "validation"
errors.CodeUnauthorized   // "unauthorized"
errors.CodeForbidden      // "forbidden"
errors.CodeConflict       // "conflict"
errors.CodeInternal       // "internal"
errors.CodeTimeout        // "timeout"
errors.CodeRateLimit      // "rate_limit"
```

## Categories

Categories classify errors for operational decisions (method on `*Error`):

```go
err := errors.New(myCode, "something failed").WithCategory(errors.CategoryInfra)

// Available categories:
errors.CategoryInfra     // infrastructure failures (DB, network)
errors.CategoryUser      // user input / request errors
errors.CategoryTransient // temporary, retryable failures
errors.CategoryBug       // invariant violations
errors.CategorySecurity  // auth failures

cat := errors.GetCategory(err)
```

## HTTP Status Mapping

Codes map to HTTP statuses automatically:

```go
status := errors.HTTPStatus(err) // e.g., 404 for CodeNotFound

// Convenience constructors:
errors.BadRequest("invalid input")     // 400
errors.Unauthorized("not logged in")   // 401
errors.Forbidden("no access")          // 403
errors.NotFound("user not found")      // 404
errors.Conflict("already exists")      // 409

// Write structured JSON error response:
errors.WriteHTTPError(w, err)
```

## Retryable Errors

```go
err = errors.Retryable(originalErr)          // wrap and mark retryable
err = errors.New(myCode, "msg").WithRetryable(true) // method chaining

if errors.IsRetryable(err) { /* retry */ }
```

## Attributes

Typed key-value pairs for structured context (passed at construction or chained):

```go
// At construction
err := errors.New(myCode, "failed", errors.String("user_id", id), errors.Int("attempt", 3))

// Method chaining
err = errors.New(myCode, "failed").WithAttr(errors.String("user_id", id))

// Inspect
attrs := errors.GetAttrs(err)

// Attr constructors: String, Int, Int64, Float64, Bool, Any, Duration
```

## Stack Traces

Stack is captured automatically on `New`/`Newf`/`Bug`/`Bugf` (never on `Wrap`/`Wrapf`).
Access with `err.Stack()`.

See `doc/getting-started.md` for full reference.

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). Its behavior is defined by the
[structured errors specification](specs/structured-errors.json) and [client-safe
disclosure ADR](doc/adr/0001-client-safe-error-disclosure.md). Before v1.0,
follow the workspace [migration-based compatibility policy](../../../RELEASE.md);
do not infer strict compatibility between every `0.x` minor.
