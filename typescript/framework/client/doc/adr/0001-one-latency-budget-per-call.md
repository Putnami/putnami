# ADR 0001 — A call has one latency budget, and retries spend it

- **Status**: accepted
- **Scope**: `@putnami/client` (`typescript/framework/client`)

## Context

A request timeout and a retry policy look independent and are not:
`timeoutMs: 30000` with `maxRetries: 3` is a worst case near two minutes, and
the server sees four requests. Cancelling "the request" is ambiguous when a
request is a sequence of attempts and backoff sleeps. Retries help a transient
failure and multiply load during an outage.

## Decision

**One budget per call.** `timeoutMs` is the per-attempt timeout and, by
default, the deadline for the whole sequence. Each attempt's timeout is
clamped to the remaining budget, a backoff that would overrun the deadline is
skipped, and reaching the deadline ends the sequence. A caller who wants
retries to cost more time sets `maxElapsedMs`.

**Retry only what a retry can fix:** network errors, per-attempt timeouts, and
the statuses declared retryable (default 429, 502, 503, 504). Other 4xx and
non-`Error` throwables are not retried. The retry count is clamped to
`[0, 10]`.

**Cancellation is a property of the sequence.** The caller's signal combines
with each attempt's timeout, the backoff sleep listens to it, and an attempt
ended by the caller is never retried.

**The breaker is a separate interceptor from retry**, so it observes the
outcome of the whole sequence. While half-open it admits at most
`halfOpenMaxConcurrent` trials and rejects the rest with `CircuitOpenError`.

**Every decision is measurable.** Retry attempts, exhaustion and recovery have
counters; the circuit has a state gauge and a per-transition counter. Retry
sits inside the telemetry interceptor, so without these counters retry
amplification is invisible.

## Rejected alternatives

- **Let `timeoutMs` bound only the attempt.** The configured timeout becomes
  unfalsifiable.
- **Retry every 5xx.** A deterministic 500 is retried and logged repeatedly,
  adding load when the service can least take it.
- **Reject the caller's promise and let the attempt finish.** The socket and
  the work continue for nobody.
- **Fold the breaker into retry.** Each attempt would count as a failure.
- **Admit every request on half-open.** The recovering service gets the whole
  queued load at once.

## Consequences

- A sequence that runs out of budget reports `ClientRetryExhaustedError` with
  the attempt count and last error. A transport `TimeoutError` reaching
  `BaseClient` maps to `ClientTimeoutError` carrying the configured budget.
- Generated methods accept a per-call `signal`.
- A client with a `healthCheckUrl` breaker owns a timer; `dispose()` (and
  `using`) releases it, and `BaseClient` discovers disposable interceptors.
