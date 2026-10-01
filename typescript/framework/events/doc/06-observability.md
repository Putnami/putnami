# Observability

Each handler invocation runs in its own async context with structured logging, telemetry metrics, and distributed tracing.

## Async Context

Every handler runs inside `runInContext()`, making the event context available via `useContext()` anywhere in the call chain:

```typescript
import { useContext } from '@putnami/runtime';
import type { EventContext } from '@putnami/events';

handler(UserCreated).handle(async (msg) => {
  const ctx = useContext<EventContext>();
  // ctx.traceId       — trace ID for correlation
  // ctx.eventTopic    — 'user.created'
  // ctx.eventMessageId — unique message ID
  // ctx.eventAttempt   — current delivery attempt (1-based)
});
```

This means `useLogger()` automatically prefixes log lines with the trace ID, and any DI services resolved inside the handler (including `.inject()` dependencies) share the same async context.

### Trace Continuity

When an event is published from inside an HTTP request, the publisher automatically captures the request's `traceId` into the message envelope. The handler then receives the same `traceId` in its context, creating an end-to-end trace across the request-event boundary:

```
HTTP Request (traceId: abc-123)
  → publish(UserCreated, ...)          // envelope.traceId = abc-123
    → handler receives message          // useContext().traceId = abc-123
      → useLogger() logs with abc-123
```

### Auth Context on Messages

The publisher also auto-captures authenticated user claims (`sub`, `email`, `azp`, `client_id`) as `auth.*` attributes on the envelope. Handlers can read these to know which user or client triggered the event:

Caller-supplied attributes whose keys start with `auth.` are ignored so these
claims cannot be spoofed by publisher code.

```typescript
handler(UserCreated).handle(async (msg) => {
  const publishedBy = msg.attributes['auth.sub'];
  const clientApp = msg.attributes['auth.azp'];
});
```

## Structured Logging

Event records follow the cross-runtime logging contract in
`protocols/logging/conformance`: identical field names, messages, and severities
in Go and TypeScript. Messages are stable constants — identifiers and error text
are never interpolated into them — and every failure carries the logger's
structured `error` field. Loggers: `events.handler` (the single terminal record
per delivery), `events.broker` (retry / dead-letter / drop), `events.publish`.

Each delivery emits exactly one terminal record. Delivery metadata is accumulated
in the event log context while the handler runs:

```text
[trace-id] message handled {
  event: { topic: "user.created", messageId: "abc-123", attempt: 1, outcome: "success", durationMs: 12 }
}
```

On failure — including a DI scope that could not be created — the same record is
emitted at `error` level with the structured error, followed by the broker's
retry record:

```text
[trace-id] message handling failed {
  event: { topic: "user.created", messageId: "abc-123", attempt: 1, outcome: "failure", durationMs: 5 },
  error: { name: "Error", message: "Connection refused" }
}
[trace-id] message retry scheduled {
  event: { topic: "user.created", messageId: "abc-123", attempt: 1, nextAttempt: 2,
           delayMs: 1024, maxRetries: 10, outcome: "failure" },
  error: { name: "Error", message: "Connection refused" }
}
```

`event.attempt` is always the true 1-based delivery count; only `nextAttempt` is
`attempt + 1`.

After the last attempt the message is dead-lettered or dropped:

```text
message dead-lettered { event: { …, attempt: 10, dlqTopic: "user.created.dlq", outcome: "failure" } }
message dropped       { event: { …, attempt: 10, reason: "retries_exhausted", outcome: "failure" } }
```

`message dead-lettered` is emitted only once a `<topic>.dlq` subscriber has
actually accepted the message; otherwise the outcome is `message dropped` with
`event.reason` one of `queue_full`, `retry_enqueue_failed`,
`dlq_enqueue_failed`, `dlq_no_subscriber`, or `retries_exhausted` — so a
dead-letter record never points an operator at a DLQ the message never reached.
Messages dropped for `dlq_no_subscriber` stay inspectable through
`broker.getDeadLetters()`.

Exactly **one** message-level outcome is reported per dead-letter decision: the
transport enqueues first, then emits `message dead-lettered` if at least one
target accepted, or a single `message dropped` (`dlq_enqueue_failed`) if none
did. A per-target refusal is not a message-level drop when another target
accepted the message — the record describes the message's fate, not each attempt
to hand it over. Both runtimes behave identically here (TypeScript
`server/failure-router.ts` / `redis/stream-transport.ts`, Go `memory_broker.go` /
`redis_stream.go`).

**Severities**: the terminal record is `info` on success and `error` on failure;
a scheduled retry is `warn` (the failure is still recoverable); a dead-letter and
every drop are `error` (the message left the happy path, or is lost).

Successful publication is recorded by the `events.publish.{topic}` counter and a
`message published` record at `debug` level. Each publish also accumulates onto
the *calling* boundary's record — `event.publishes` (an appended
`{ topic, messageId }`) plus the paired `event.publishCount` — so a request that
published twice shows both on its HTTP terminal record.

### Conformance

These record shapes are pinned by executable golden tests against the shared
corpus in `protocols/logging/conformance`:
`test/logging-cross-language.test.ts` here and
`go/framework/events/logging_cross_language_test.go` in Go. Both drive the real
broker (and, for the publish-accumulation case, the real HTTP middleware) and
byte-compare the rendered JSON with the corpus, so changing a field name,
severity, or message in one runtime fails the build until the corpus and the other
runtime are updated in the same commit.

## Telemetry Metrics

When the telemetry plugin is active, the events module emits these metrics automatically:

### Handler Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `events.handle.{topic}.success` | Counter | Successful handler invocations |
| `events.handle.{topic}.failure` | Counter | Failed handler invocations |
| `events.handle.{topic}.duration` | Histogram | Handler execution time (ms) |
| `events.handle.{topic}.retry` | Counter | Retry attempts |
| `events.handle.{topic}.dlq` | Counter | Messages sent to dead-letter queue |

### Publisher Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `events.publish.{topic}` | Counter | Successfully published messages |
| `events.publish.{topic}.error` | Counter | Publish validation errors |

### Example: Monitoring in Practice

```typescript
// These metrics are emitted automatically — no code needed.
// Query examples (pseudo-SQL for your metrics backend):

// Messages processed per minute
// SELECT rate(events.handle.user.created.success, 1m)

// P99 handler latency
// SELECT p99(events.handle.user.created.duration)

// Error rate
// SELECT events.handle.user.created.failure / events.handle.user.created.success

// DLQ backlog
// SELECT sum(events.handle.user.created.dlq)
```

## Custom Metrics

Use the telemetry utils from `@putnami/application` inside handlers:

```typescript
import { incCounter, observeHistogram } from '@putnami/application';

handler(OrderPlaced).handle(async (msg) => {
  const start = Date.now();
  await processOrder(msg.payload);

  incCounter('app.orders.processed');
  observeHistogram('app.order.processing.duration', Date.now() - start);
});
```
