# ADR 0001 — Resilience is one ordered interceptor chain

- **Status**: accepted
- **Scope**: `go.putnami.dev/client` (`go/framework/client`)

## Context

Identity headers, caller interceptors, a total deadline, a circuit breaker
and retries all wrap one call, and each means something different depending on
its position. Retry outside the breaker turns one failure into
`MaxRetries + 1` fast rejections. A breaker outside retry counts a whole retry
loop as one failure. A total deadline inside the retry loop bounds one attempt
instead of the call.

## Decision

The chain is fixed and composed once at `Build`:

```
ClientID → caller interceptors → TotalTimeout → CircuitBreaker → Retry → Transport
```

- Client identity is set once per call. Caller interceptors observe the
  logical call, not attempts.
- `TotalTimeout` bounds the whole call including backoffs, and flows into each
  attempt: an attempt is bounded by the smaller of `Timeout` and the remaining
  total budget.
- One logical call is one breaker observation, and an open breaker rejects
  before any retry is planned.
- Retries sit next to the transport, where an attempt is a network attempt.
- Backoff is exponential with up to 25% jitter, capped at `MaxDelay`. The cap
  is applied in floating point before conversion to a duration, because
  `baseDelay × 2^attempt` overflows `int64` and a negative duration would
  retry immediately.
- A transport error names scheme, host and path, never query values.

## Rejected alternatives

- **Retry outside the breaker.** One outage becomes a burst of rejections.
- **Compose the chain per request.** One closure per interceptor per call for
  a chain fixed for the client's lifetime.
- **One `Timeout` for the whole call.** Callers cannot say "2s per attempt, 5s
  per call".
- **Include the full URL in transport errors.** Tokens travel in query
  parameters, and error strings reach logs.

## Consequences

- The order is not configurable; a caller wraps the client instead.
- Per-attempt observation goes through `RetryConfig.OnRetry`.
- A new stage must take a recorded position relative to every existing stage.
- Tests assert backoff bounds, not exact delays.
