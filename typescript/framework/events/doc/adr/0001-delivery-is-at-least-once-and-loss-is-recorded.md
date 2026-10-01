# ADR 0001 — Delivery is at-least-once, and every loss leaves exactly one record

- **Status**: accepted
- **Scope**: `@putnami/events` (`typescript/framework/events`)

## Context

Over an unreliable network a broker offers at-most-once or at-least-once.
Exactly-once needs an idempotent handler, because the broker cannot know whether
a side effect happened. At-least-once means handlers see duplicates, which local
development never produces, so non-idempotent handlers ship.

A permanent failure can end several ways: retries exhausted with or without a
dead-letter queue, a re-delivery refused by a full queue, a dead-letter topic
whose subscribers all refuse. If each path logs differently, "did we lose
anything?" has no answer, and a drop record for a dead-lettered message sends
operators after messages that were never lost. A broker that forgets queued
messages on stop loses work during every deploy.

## Decision

1. **At-least-once, demonstrable.** Handlers must be idempotent. The in-memory
   broker's opt-in `simulateDuplicates` redelivers about 2% of handled messages.
   It is off by default so local runs stay deterministic.
2. **One dispatch pipeline.** Every transport shares async context, DI scope,
   payload validation, timeout, manual-ack assertion, lifecycle logging, and
   telemetry. Transports own only envelope decoding and ack/nack. A rejecting
   scope factory produces the same terminal failure record as a throwing handler.
3. **One record per message, never per subscriber.** A dead-letter record is
   emitted once, after at least one `<topic>.dlq` subscriber accepted the message.
   Otherwise one drop record is emitted with a reason: `retries_exhausted`,
   `retry_enqueue_failed`, `dlq_no_subscriber`, `dlq_enqueue_failed`, or
   `queue_full`. The recorded attempt is the true final attempt.
4. **An unrouted dead letter is kept.** `dlq: true` with no `<topic>.dlq`
   subscriber stores the full envelope and error in a bounded, configurable
   buffer that operators and tests drain; the oldest entries are evicted first.
5. **Backpressure is declared per handler.** `concurrency` caps parallel
   invocations and `queueLimit` caps parked deliveries; both default to `0`
   (unlimited). When the queue is full, `overflow` throws (default) or drops with
   an error-level `queue_full` record. A retry re-enters the same queue and is
   subject to the same policy.
6. **Shutdown drains what was admitted.** `stop()` refuses new publishes and
   drains in-flight and queued deliveries until empty or the drain timeout
   (default 10 s), then reports how many are pending. Pending retry timers are
   cancelled.
7. **Transports are structural interfaces, not SDKs.** Redis, Google Pub/Sub,
   and Postgres describe the client shape they need and the application supplies
   it. The package has no vendor dependency, so the root barrel re-exports every
   transport and the browser entry stays limited to topics, client, and protocol.

## Rejected alternatives

- **Claim exactly-once.** It is a property of the handler, which the broker does
  not control.
- **Default-on duplicates in development.** Makes every local run
  non-deterministic.
- **A drop record per refusing dead-letter subscriber.** Reads as N losses for
  one message.
- **Record the dead letter before enqueueing it.** Claims a destination the
  message may never reach.
- **Reduce an unrouted dead letter to a log line.** Discards the only copy.
- **Drop queued deliveries on stop.** Those messages were already accepted.
- **Depend on the Redis and Pub/Sub SDKs.** Every consumer carries both and the
  browser entry cannot exist.

## Consequences

- Application code pays for idempotency; it is the only place it can be paid.
- Without `concurrency` and `queueLimit`, a burst is admitted without bound;
  services that need backpressure declare both.
- Supplying a transport means supplying a client object, which keeps the vendor
  dependency out.
- The shared cross-language corpus pins the delivery records, so a vocabulary
  change here without the Go broker fails the suite.
