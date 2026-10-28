# ADR 0001 — Make at-least-once retry and terminal outcomes explicit

- **Status**: accepted
- **Scope**: `go.putnami.dev/events` (`go/framework/events`)

## Context

Memory, Redis, provider push, and managed publish transports can all see a
timeout after the other side accepted a message, so retry produces duplicates.
A broadcast retry placed on a shared stream would reach every group. Operators
need one terminal outcome, not both dead-letter and drop.

## Decision

Delivery is at least once. Envelope ID and dedupe key stay stable across
retries; deduplication is the handler's job. Each invocation has a timeout.
Retry delay grows exponentially from the base, is capped, and has jitter. Redis
Stream retries go to a stream scoped to topic and consumer group, so a failing
group never re-invokes groups that succeeded.

Competing delivery selects one matching handler; broadcast selects all. After
exhaustion the broker tries DLQ enqueue, records dead-letter only if a DLQ
consumer accepted the message, and records drop otherwise. Managed publish
classifies responses as permanent, retryable, or ambiguous; an ambiguous one
returns to the caller with no fallback or shadow publication.

Memory broker shutdown closes the retry signal and refuses new publication. It
cancels each pending retry timer and reports that retry dropped with reason
`broker_stopped`. Up to the drain deadline, it waits for in-flight deliveries
and runs the deliveries queued behind a handler's concurrency limit: their
publish already returned `nil`, so they are accepted messages. At the deadline,
it discards the deliveries still queued and reports each one dropped with
reason `broker_stopped`. A drop is both a `message dropped` record and an
`Observer.Dropped` call. The TypeScript memory broker also drains its queue up
to the deadline.

## Rejected alternatives

- **Exactly once from acknowledgements.** A lost response cannot prove
  acceptance.
- **Retry on the shared topic stream.** Other broadcast groups get duplicates.
- **Record dead-letter before enqueue.** It can point to a DLQ that never got
  the message.
- **Fall back after an ambiguous publish.** It can duplicate an accepted
  message.
- **Discard queued deliveries at `Stop`.** Even reported as drops, it loses
  messages that a graceful shutdown has time to deliver, and the TypeScript
  memory broker drains them.

## Consequences

- Handlers with external effects must be idempotent, for example by storing
  the dedupe key.
- Queue overflow, a missing or refusing DLQ, and work that `Stop` abandons
  are visible data loss.
- A queued delivery can run after `Stop` was called, until the drain deadline.
- `Stop` can return a drain-timeout error when a handler ignores cancellation.
