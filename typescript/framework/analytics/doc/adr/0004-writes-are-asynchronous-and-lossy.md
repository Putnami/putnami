# ADR 0004 — Analytics writes are asynchronous, bounded, and lossy by design

- **Status**: accepted
- **Scope**: `@putnami/analytics` (`typescript/framework/analytics`)

## Context

A measurement must not become a dependency of what it measures. Awaiting a
five-statement transaction before returning every page, answering every beacon,
or completing every `track()` couples page latency to database latency, turns an
outage into timeouts and a beacon storm into a connection storm. The rule:
analytics never perturbs serving performance, and losing events under pressure
is acceptable.

The framework targets serverless, request-based CPU. Between requests a
container gets little or no CPU, so a promise nobody awaits stops. It scales to
zero after a SIGTERM. A periodic keep-alive ping keeps instances warm and is an
ordinary request.

## Decision

Every accepted row goes into a bounded in-memory queue. No response path waits
for the sink. Three triggers drive the queue:

1. **The elected request.** The global page-view middleware asks for a flush at
   the start of every request, before `next()`. The queue starts one only when
   it holds rows, no flush is in flight, and a full batch is waiting or
   `flushIntervalMs` (default 5 000) has passed since the last flush. That
   request overlaps the flush with its own render and then waits for it, for at
   most `flushWaitMs` (default 1 000). Every other request pays one timestamp
   comparison. This is the trigger that guarantees progress: a flush nobody
   awaits gets no CPU after the response.
2. **An unref-ed ticker.** `setInterval(() => queue.kick(), flushIntervalMs)`,
   installed at warmup and cleared in `stop()`. On request-based CPU it runs at
   the next CPU window; it covers routes that bypass the middleware chain. It
   never keeps a process alive on its own.
3. **The drain in `stop()`.** `AnalyticsPlugin.stop()` awaits
   `queue.drain(flushDeadlineMs)`. `installSignalHandlers` force-exits after
   `SHUTDOWN_TIMEOUT_MS` (10 000 ms); the default deadline is half of that, so
   the drain returns inside the grace period. This is the only place analytics
   waits for the database.

The accepted costs, both configuration: a row may sit in the queue for
`flushIntervalMs` plus the gap to the next request, and one request every
`flushIntervalMs` has a p99 degraded by at most `flushWaitMs`.

Queue rules:

- **Bounded memory.** Past `queueCapacity` rows (default 5 000) the oldest are
  dropped and counted as `analytics.ingest.dropped.overflow`.
- **One flush in flight, process-wide.** A slow database never accumulates
  connections or open transactions.
- **No retry.** A failed write drops its rows, counted as
  `analytics.ingest.dropped.sink_error`, and logs at most once per 60 s.
  Re-enqueuing would spend every later flush on a poison batch.
- **A bounded loop.** One flush writes batches while rows remain and the loop
  has run under 1 000 ms. A backlog drains across several flushes.

`flushAnalytics(deadlineMs = 5000)` drains on demand, for a consuming
application's tests and for an operator before a maintenance stop. No response
path calls it.

## Invariants

- No response path awaits `Sink.write`: not the page-view middleware, not the
  ingest route, not `track()`.
- One flush is in flight at a time; the in-flight marker clears on success and
  on failure.
- Queue memory is bounded by `queueCapacity` rows.
- A failed write drops its rows; nothing is retried.
- `drain(deadlineMs)` returns at its deadline even when the sink never settles,
  and never throws.
- An elected request waits at most `flushWaitMs`; every other request waits for
  nothing.

## Rejected alternatives

- **Awaiting the write on every request.** Couples page latency to the database.
- **Fire-and-forget only.** Rows stall silently between requests; loss becomes
  unbounded.
- **The ticker only.** Throttled between requests, dead at scale to zero.
- **Retrying a failed write.** A poison batch consumes every later flush.
- **Dropping the newest row on overflow.** It hides pressure exactly where an
  operator looks, in the freshest data.
- **A background worker or external queue.** A second thing to operate for a
  measurement allowed to lose events.
- **Silent loss.** Every drop is counted under `analytics.ingest.dropped.*`, and
  queue depth is a gauge.

## Consequences

- A row is not in the database when the response returns. A test or tool that
  reads rows back calls `flushAnalytics()` first.
- Collection is at-most-once with bounded loss: a crash, a flush outliving the
  shutdown deadline or an overflow loses rows. `ON CONFLICT (event_id)` keeps a
  delivered event idempotent.
- A slow database shows as queue depth and `dropped.overflow`, not page latency.
  Watch `analytics.queue.size` and `analytics.queue.flush_ms`.
- The operator owns `flushIntervalMs`: lower means fresher rows and more
  requests paying, higher means more drift.
