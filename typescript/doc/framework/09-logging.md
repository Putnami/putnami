# Logging

Putnami provides structured, context-aware logging via `@putnami/runtime`. Logs automatically include trace IDs, request context, and structured data — no manual wiring required.

## Basic usage

```typescript
import { useLogger } from '@putnami/runtime';

const logger = useLogger('orders');

logger.info('Order created', { orderId: '123' });
logger.warn('Low stock', { sku: 'WIDGET-01', remaining: 3 });
logger.error('Payment failed', error);
```

`useLogger()` returns a context-aware logger. When called inside an HTTP request, the logger automatically includes the request's trace ID.

## Log levels

Four levels, in order of severity:

| Level | Use for |
|---|---|
| `debug` | Development-only detail: variable values, branching decisions |
| `info` | Normal operations: request handled, job completed, record created |
| `warn` | Recoverable problems: deprecated usage, slow query, retry triggered |
| `error` | Failures: unhandled exceptions, external service down, data corruption |

The default level is `info`. Messages below the configured level are filtered out.

## Named loggers

Create child loggers to organize output by concern:

```typescript
const logger = useLogger('app');
const authLogger = logger.named('auth');
const dbLogger = logger.named('db');

authLogger.info('Token validated');  // logger: "app.auth"
dbLogger.warn('Slow query');         // logger: "app.db"
```

## Adding context

Use `with()` to attach structured data to log entries.

Inside a request scope, `with()` writes to the request-scoped context and every
logger in that request sees the field:

```typescript
const logger = useLogger('api');
logger.with('userId', 'usr_123');
logger.with('tenantId', 'acme');

logger.info('Request handled');
// → includes userId and tenantId in the log entry
```

Outside a request scope, `with()` is immutable: it returns a new logger carrying
the field rather than mutating the shared one (this prevents fields from leaking
into unrelated log lines across the process). Chain or reassign to accumulate:

```typescript
const scoped = logger.with('userId', 'usr_123').with('tenantId', 'acme');
scoped.info('Request handled');
// → includes userId and tenantId; the original `logger` is unchanged
```

**Always use the return value.** The two paths look the same at the call site but
behave differently: dropping the result is harmless inside a request and a silent
no-op outside one. `const logger = useLogger('x').with('k', v)` is correct on both
paths.

## Accumulating context: `append` and `increment`

`with()` overwrites, which loses information when the same work happens several
times in one request (two publishes, three retries). Use `append(path, value)`
and `increment(path, delta)` — both address **nested groups** through a
dot-separated path, and both follow the same dual-path rule as `with()`:

```typescript
// Inside @putnami/events' publisher: every publish contributes to the caller's
// single terminal record instead of overwriting the previous one.
useLogger('events.publish')
  .append('event.publishes', { topic: topicDef.name, messageId: envelope.id })
  .increment('event.publishCount')
  .debug('message published', { event: { topic: topicDef.name, messageId: envelope.id } });
```

A request that publishes twice then ends with an HTTP terminal record like:

```json
{"severity":"INFO","message":"[POST] /orders","logger":"http","traceId":"…",
 "http":{"method":"POST","routePath":"/orders","status":200,"outcome":"success","durationMs":12},
 "event":{"publishCount":2,"publishes":[{"topic":"orders","messageId":"m-1"},{"topic":"orders","messageId":"m-2"}]}}
```

Semantics:

- `append` — absent leaf becomes `[value]`; an existing array is pushed to; any
  other existing value is promoted to `[existing, value]`, so an earlier value is
  never lost.
- `increment` — a numeric leaf is incremented; anything else (or an absent leaf)
  is set to `delta` (default `1`).
- The list is capped at `MAX_APPENDED_FIELD_VALUES` (100), after which appends
  are dropped **silently** — so always pair an `append` with an `increment` of a
  sibling counter, and read the true total from the counter, not the list length.
  Go's `logger.MaxAppendedFieldValues` holds the same value.

## Error logging

Pass `Error` objects directly — stack traces are extracted automatically:

```typescript
try {
  await processPayment(order);
} catch (error) {
  logger.error('Payment processing failed', error);
}
```

The log entry includes `error.name`, `error.message`, and `error.stack` as structured fields.

## HTTP integration

The `logger()` plugin adds automatic request logging with trace ID propagation:

```typescript
import { application, http, logger } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(logger({ exclude: ['/health'] }));
```

Every request emits one completion log. Method, route, status code, and duration
are stored under its `http` context, together with fields accumulated through
`with()` while handling the request. Trace IDs are extracted from
`X-Cloud-Trace-Context` or `X-Correlation-ID` headers, or generated
automatically.

## The framework log-record contract

Framework-emitted records (HTTP, events, database, migrations) are a **public
contract**: dashboards, alerts, and log-based metrics are written against these
key names, severities, and messages. It is normative in
[`protocols/logging/conformance`](../../../protocols/logging/conformance/README.md),
and both runtimes are held to it by executable golden tests, so the Go and
TypeScript frameworks emit equivalent records for the same boundary event.

**Naming and shape**

- camelCase keys everywhere: `messageId`, `durationMs`, `durationUs`,
  `thresholdMs`, `rollbackCause`, `dlqTopic`.
- Boundary data nests under exactly one top-level group: `http`, `event`,
  `database`, or `migration`.
- Framework-reserved top-level keys — `severity`, `message`, `timestamp`,
  `logger`, `traceId` (or `logging.googleapis.com/trace` under a configured GCP
  project), and `error` — are written last and cannot be forged by a user field
  of the same name.
- Durations are integers. Every terminal record carries `durationMs`; `database`
  records also carry `durationUs`, because sub-millisecond queries are the norm.

**Pinned logger names**: `http`, `events.handler`, `events.publish`,
`events.broker`, `database`, `database.migration` (dots, never colons).

**Outcome vocabulary**: `success` | `failure` | `canceled` | `skipped`, shared by
every group. A committed transaction is `success`; a rolled-back one is `failure`
with the *classified* reason in `rollbackCause` — never raw error text.

**Stable messages.** Error text and dynamic identifiers are never interpolated
into a message, so log-based metrics can match on it: `[<METHOD>] <routePath>`,
`message handled` / `message handling failed` / `message retry scheduled` /
`message dead-lettered` / `message dropped`, `query executed` / `query failed` /
`slow query` / `transaction committed` / `transaction rolled back`,
`migration applied` / `migration failed`.

**Severities**

| Record | Severity | Why |
|---|---|---|
| HTTP terminal | `INFO` < 500, `ERROR` ≥ 500 | the handler error travels in `error` |
| Event terminal | `INFO` handled, `ERROR` failed | one per delivery |
| Event retry | `WARNING` | recoverable; the error travels as a field |
| Event dead-letter / drop | `ERROR` | the message left the happy path or is lost |
| Query executed | `DEBUG` | normal traffic |
| Query failed, slow query, transaction rolled back | `WARNING` | the data layer reports and *propagates*; the boundary owns `ERROR` exactly once, so a retried-and-recovered query is not error-level noise |
| Migration applied / failed | `INFO` / `ERROR` | one terminal record per migration |

**Terminal-record policy.** Exactly one terminal record per request/event
boundary. Other warnings/errors are allowed only as independent operational
signals (a slow query, an advisory-lock release failure). A dead-lettered message
reports one message-level outcome: `message dead-lettered` once the dead-letter
is accepted, or a single `message dropped` when nothing accepted it — never both.

## Output formats

### JSON (default)

Structured JSON compatible with Google Cloud Logging. Each entry is a single JSON line with `severity`, `message`, `timestamp`, and context fields:

```json
{"severity":"INFO","message":"Order created","timestamp":"2025-01-15T10:30:00.000Z","logger":"orders","traceId":"abc-123","orderId":"123"}
```

### Console

Human-readable text for local development:

```
[abc-123] [INFO] [orders] Order created { orderId: '123' }
```

## Configuration

Via environment variables:

```bash
LOG_LEVEL=debug          # debug | info | warn | error (default: info)
LOGGER_JSON=false        # true for JSON, false for console (default: true)
```

Via YAML (`conf/.env.local.yaml`):

```yaml
logger:
  level: debug
  json: false
```

### Buffered logging

Group logs by request and flush together. Useful for high-throughput services:

```yaml
logger:
  buffer: true
  bufferMaxSize: 100       # Entries per context before flush
  bufferFlushInterval: 5000 # ms between flushes
```

Error-level entries flush the entire request context immediately.

## Testing

Use `MemoryLogger` to capture and assert on log output:

```typescript
import { MemoryLogger } from '@putnami/runtime/testing';

const logger = new MemoryLogger('test');
logger.info('hello');
logger.error('failed', new Error('boom'));

expect(logger.entries).toHaveLength(2);
expect(logger.entries[0].message).toBe('hello');
expect(logger.entries[1].error?.name).toBe('Error');
```

### Asserting the framework contract

Framework boundaries are asserted against the shared corpus with the conformance
harness shipped beside `MemoryLogger`. Each captured entry is rendered through
the JSON sink's own `buildJsonRecord` and byte-compared with the golden record
after canonicalization (type-checked tokens for values that vary, `$open` for
runtime-specific extras, sorted keys):

```typescript
import { assertRecord, findCase, findRecord, loadCases, MemoryLogger } from '@putnami/runtime/testing';

const cases = loadCases('event');           // http | event | database | migration
const memory = new MemoryLogger();
setRootLogger(memory);
// … drive real boundary code …
const want = findCase(cases, 'event.terminal.success');
assertRecord(findRecord(memory.entries, want), want); // exactly one matching record
```

`go.putnami.dev/logger/logtest` is the Go twin (`LoadCases` / `NewRecorder` /
`AssertRecord`). A failure names the corpus and the other runtime's test, because
a record change must land in three places at once.

## Global exception handling

Unhandled exceptions and rejected promises are captured automatically when the application starts. They log through the same logger with full context (trace ID, request data) when they occur inside a request.

## Go

The Go logger follows the same patterns and output formats as the TypeScript runtime.

### Basic usage

```go
import "go.putnami.dev/logger"

log := logger.Default().Named("orders")

log.Info("Order created", slog.String("orderId", "123"))
log.Warn("Low stock", slog.String("sku", "WIDGET-01"), slog.Int("remaining", 3))
log.Error("Payment failed", err)
```

`Default()` returns a shared logger configured from environment variables. `Named()` creates a child logger with an appended name.

### Output formats

JSON output by default (same structure as TypeScript):

```json
{"severity":"INFO","message":"Order created","timestamp":"2025-01-15T10:30:00.000Z","logger":"orders","orderId":"123"}
```

Console output (same format as TypeScript):

```
[INFO] [orders] Order created orderId=123
```

### Configuration

Via environment variables:

```bash
LOG_LEVEL=debug          # debug | info | warn | error (default: info)
```

### Context integration

```go
log := logger.Default().Named("api")
reqLog := log.With("userId", "usr_123")
reqLog.Info("Request handled")  // includes userId in log entry

// Attach to context
ctx = logger.WithLogger(ctx, reqLog)
logger.FromContext(ctx).Info("from context")
```

### Testing

```go
sink := logger.NewMemorySink()
log := logger.New("test", logger.LevelDebug, sink)

log.Info("hello")
log.Error("failed", errors.New("boom"))

// sink.Len() == 2
// sink.Last().Message == "failed"
// sink.Last().Error.Message == "boom"
```
