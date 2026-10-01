# Resilience

`@putnami/client` provides layered resilience: per-request timeouts, automatic retry with exponential backoff, circuit breaker for fail-fast behavior, and a typed error hierarchy for structured error handling.

## Timeouts

Every request attempt has a timeout enforced via `AbortSignal.timeout()`. The default is **30,000ms** (30 seconds), configurable per client:

```typescript
new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  timeoutMs: 10_000,  // 10 seconds
});

// Or via ClientBuilder
ClientBuilder.for(UsersClient)
  .baseUrl('http://users-api:3000')
  .timeout(10_000)
  .buildSync();
```

When a request times out, a `ClientTimeoutError` is thrown:

```typescript
import { ClientTimeoutError } from '@putnami/client';

try {
  await users.getUser({ id: '123' });
} catch (error) {
  if (error instanceof ClientTimeoutError) {
    console.log(`Timed out after ${error.timeoutMs}ms`);
  }
}
```

**`timeoutMs` is per-attempt, with a total bound.** Each retry attempt gets its own fresh timeout budget, but `timeoutMs` also serves as the overall deadline for the whole retry sequence (all attempts plus backoff). This prevents retry amplification: a stalled downstream can no longer pin the caller for `maxRetries × timeoutMs`. Each attempt's timeout is clamped to the remaining budget, backoff that would overrun the deadline is skipped, and once the deadline is reached the request fails (with `ClientRetryExhaustedError` for transient failures, or the underlying `ClientTimeoutError`).

## Retry

The `retryInterceptor` is built into every client and retries automatically on transient failures. It uses exponential backoff with jitter to avoid thundering-herd problems.

### Default Configuration

| Setting | Default | Description |
|---|---|---|
| `maxRetries` | `3` | Maximum retry attempts after the first failure |
| `baseDelayMs` | `200` | Base delay for the first retry |
| `maxDelayMs` | `5000` | Maximum delay cap (backoff never exceeds this) |
| `retryableStatuses` | `[429, 502, 503, 504]` | HTTP codes that trigger a retry |
| `jitter` | `true` | Adds 50% random jitter to each delay |

### Backoff Formula

```
delay = min(baseDelayMs × 2^attempt, maxDelayMs)
// With jitter: delay × (0.5 + random × 0.5)
```

| Attempt | No jitter | With jitter (range) |
|---------|-----------|---------------------|
| 0 | 200ms | 100–200ms |
| 1 | 400ms | 200–400ms |
| 2 | 800ms | 400–800ms |
| 3 | 1600ms | 800–1600ms |

### What Gets Retried

**Retried:**
- HTTP responses with `retryableStatuses` codes (429, 502, 503, 504 by default)
- Network errors (DNS failure, connection refused) — `TypeError` from fetch

**Not retried:**
- 4xx responses (except 429) — client errors are not transient
- 5xx responses not in `retryableStatuses` (e.g., 500 Internal Server Error)
- Aborted requests (`AbortError`) — manual cancellations are intentional
- Timed-out requests (`TimeoutError`) — the signal has already fired

### Overriding Retry Config

Pass `Partial<RetryConfig>` to merge with defaults:

```typescript
// Per-client override
new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  retry: {
    maxRetries: 5,
    baseDelayMs: 500,
    maxDelayMs: 10_000,
  },
});

// Disable retries entirely
new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  retry: { maxRetries: 0 },
});
```

### `retryInterceptor(config?): Interceptor`

The built-in retry interceptor is also exported for use in custom pipelines. It accepts `Partial<RetryConfig>` merged with the defaults.

### `computeDelay(attempt, config): number`

Exported utility that computes the delay for a given retry attempt. Useful for testing retry timing.

| Parameter | Type | Description |
|-----------|------|-------------|
| `attempt` | `number` | Zero-based attempt index |
| `config` | `RetryConfig` | Full resolved config |

**Returns:** Delay in milliseconds.

## Circuit Breaker

The circuit breaker prevents cascading failures when a downstream service is unhealthy. When failures exceed a threshold, the circuit opens and subsequent requests fail immediately — without hitting the network.

```typescript
import { circuitBreakerInterceptor } from '@putnami/client';

new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  interceptors: [
    circuitBreakerInterceptor({
      failureThreshold: 5,
      resetTimeoutMs: 30_000,
      successThreshold: 2,
      healthCheckUrl: 'http://users-api:3000/healthz',
      healthCheckIntervalMs: 10_000,
    }),
  ],
});
```

### State Machine

```
CLOSED ──(failures ≥ threshold)──→ OPEN
  ↑                                   │
  │                          (resetTimeoutMs or health OK)
  │                                   ↓
  └──(successes ≥ threshold)── HALF-OPEN
```

| State | Behavior |
|---|---|
| **Closed** | Normal operation. Counts failures; resets count on success. |
| **Open** | Fail-fast. All requests throw `CircuitOpenError`. Probes health check if configured. |
| **Half-open** | Trial period. Allows requests through. Success → Closed. Failure → Open. |

### Transition Rules

**Closed → Open:** `failureCount >= failureThreshold`

**Open → Half-open:** Either `resetTimeoutMs` has elapsed since the last failure, or the health check probe returns a successful response.

**Half-open → Closed:** `successCount >= successThreshold` consecutive successes.

**Half-open → Open:** Any single failure.

### Failure Detection

A request counts as a failure if:
- The response status is in `failureStatuses` (default: `[500, 502, 503, 504]`)
- The request throws an exception (e.g., network error)

**Note:** 4xx responses (other than those in `failureStatuses`) do not count as failures — they are valid responses indicating client errors, not service degradation.

### Health Probing

When `healthCheckUrl` is configured, the circuit breaker probes it periodically (every `healthCheckIntervalMs`) while the circuit is open:

```typescript
circuitBreakerInterceptor({
  healthCheckUrl: 'http://users-api:3000/healthz',
  healthCheckIntervalMs: 10_000,  // probe every 10s
})
```

If the health endpoint returns a 2xx response, the circuit transitions to half-open immediately — without waiting for `resetTimeoutMs`. Health probe failures are logged at `debug` level and do not affect the circuit state.

**Security:** `healthCheckUrl` must use `http://` or `https://`. Other schemes throw `Error` at construction.

### Configuration Reference

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `failureThreshold` | `number` | `5` | Failures before circuit opens |
| `resetTimeoutMs` | `number` | `30000` | ms before trying again (half-open) |
| `successThreshold` | `number` | `2` | Successes in half-open to close |
| `healthCheckUrl` | `string` | — | URL to probe when circuit is open |
| `healthCheckIntervalMs` | `number` | `10000` | Health probe interval in ms |
| `failureStatuses` | `number[]` | `[500, 502, 503, 504]` | HTTP codes counted as failures |

### `CircuitOpenError`

Thrown when a request is rejected because the circuit is open:

```typescript
import { CircuitOpenError } from '@putnami/client';

try {
  await users.getUser({ id: '123' });
} catch (error) {
  if (error instanceof CircuitOpenError) {
    console.log(error.circuitState);  // 'open' or 'half-open'
    // Provide fallback / cached data
  }
}
```

| Property | Type | Description |
|---|---|---|
| `circuitState` | `CircuitState` | The state when the request was rejected |
| `message` | `string` | Human-readable description |

### `CircuitBreaker` Class

The `CircuitBreaker` class is exported for advanced use cases — embedding a circuit breaker in non-interceptor code, or inspecting state programmatically:

```typescript
import { CircuitBreaker } from '@putnami/client';

const breaker = new CircuitBreaker({ failureThreshold: 3 });

// Check state
breaker.getState();    // 'closed' | 'open' | 'half-open'
breaker.allowRequest(); // boolean — whether a request should proceed

// Record outcomes
breaker.onSuccess();
breaker.onFailure();

// Health probing
breaker.startHealthProbing();
breaker.stopHealthProbing();

// Cleanup (stop timers)
breaker.dispose();
```

## Error Hierarchy

All client errors extend `ClientError`, which extends `Error`. They carry the service name and method path for structured error handling:

```
Error
└── ClientError
    ├── service: string
    ├── method: string
    ├── status: number
    └── responseBody: unknown (non-enumerable)
        ├── ClientRequestError    (4xx responses)
        ├── ClientServerError     (5xx responses)
        ├── ClientRetryExhaustedError
        │   ├── attempts: number
        │   └── lastError: Error
        └── ClientTimeoutError
            └── timeoutMs: number

Error
└── CircuitOpenError
    └── circuitState: CircuitState
```

### `ClientError`

Base class. All other client errors extend this.

| Property | Type | Description |
|---|---|---|
| `service` | `string` | Service identifier (the `packageName` or transport URL) |
| `method` | `string` | Request path or RPC method |
| `status` | `number` | HTTP status code; `0` for non-HTTP errors |
| `responseBody` | `unknown` | Raw server response body (non-enumerable — won't appear in `JSON.stringify` to prevent leaking sensitive server data) |

### `ClientRequestError`

Thrown for **4xx** HTTP responses. Indicates a client-side error (bad input, unauthorized, not found, etc.).

### `ClientServerError`

Thrown for **5xx** HTTP responses. Indicates a server-side error (internal error, bad gateway, etc.).

### `ClientRetryExhaustedError`

Thrown when all retry attempts have been exhausted.

| Property | Type | Description |
|---|---|---|
| `attempts` | `number` | Total number of attempts made |
| `lastError` | `Error` | The last error that caused the retry |

### `ClientTimeoutError`

Thrown when the request exceeds `timeoutMs`.

| Property | Type | Description |
|---|---|---|
| `timeoutMs` | `number` | The timeout value that was exceeded |

### Error Handling Patterns

```typescript
import {
  ClientError,
  ClientRequestError,
  ClientServerError,
  ClientRetryExhaustedError,
  ClientTimeoutError,
  CircuitOpenError,
} from '@putnami/client';

try {
  await users.getUser({ id: '123' });
} catch (error) {
  if (error instanceof ClientRequestError) {
    // 4xx: bad input, unauthorized, not found
    console.log(error.status, error.message);
    // error.responseBody has the server response (non-enumerable, won't serialize)
  } else if (error instanceof ClientServerError) {
    // 5xx: server-side failure
    console.error(`Server error from ${error.service}: ${error.message}`);
  } else if (error instanceof ClientRetryExhaustedError) {
    // All retries failed
    console.error(`Failed after ${error.attempts} attempts: ${error.lastError.message}`);
  } else if (error instanceof ClientTimeoutError) {
    // Timed out
    console.error(`Request timed out after ${error.timeoutMs}ms`);
  } else if (error instanceof CircuitOpenError) {
    // Circuit is open — service likely down
    console.error(`Service unavailable (circuit ${error.circuitState})`);
  }
}
```

## Combining Retry and Circuit Breaker

Place the circuit breaker **before** the retry interceptor (which is always last) to prevent retrying when the circuit is open:

```typescript
new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  interceptors: [
    circuitBreakerInterceptor({
      failureThreshold: 5,
      healthCheckUrl: 'http://users-api:3000/healthz',
    }),
    // retry is implicitly added after all custom interceptors
  ],
  retry: { maxRetries: 3 },
});
```

When the circuit opens, `CircuitOpenError` is thrown — which is not a network error and is not retried. This means once the circuit opens, failures are immediate without burning through retry budget.

## Response Cache

A first-party provider declares a response cache on one operation, beside its
retry and circuit policy:

```typescript
endpoint()
  .params({ id: String })
  .client({
    idempotency: { kind: 'safe' },
    resilience: {
      cache: {
        freshMs: 5_000,
        staleMs: 300_000,
        maxEntries: 1_000,
        keyFields: ['path.id'],
        invalidationFields: ['principalId'],
      },
    },
  })
  .handle(/* ... */);
```

Only a unary operation whose idempotency is `safe` or `idempotent` may declare
it, and never as a document default. `keyFields` names `path.<name>`,
`query.<name>`, `header.<name>`, `body` or `body.<property>`; without it the key
is every parameter plus the whole body. A key field that names no declared
input is refused when the contract is published. `invalidationFields` names
top-level properties of the success body — each a string, integer or boolean,
never a `format: byte` or `binary` string —
that tag every stored answer; one that names anything else is refused too.

The generated client runs the cache first in its chain:

| Stored answer age | Provider call | Result |
|---|---|---|
| below `freshMs` | none | the stored answer |
| otherwise | one per key and identity; concurrent callers wait for it | its answer, stored |
| below `staleMs`, call failed with a transport error, the operation's timeout, an open breaker or a retryable answer after the retries | as above | the stored answer, `rpc.client.cache.stale_served` counted, one warning logged |
| below `staleMs`, the caller's own deadline passed while the provider hung (a signal aborted with `TimeoutError`, or the ambient `deadlineAt`) | as above; it keeps running for the other callers | the stored answer, counted and logged |
| any other failure, an explicit cancellation, or past `staleMs` | as above | the error |

The shared provider call is bounded by the operation's own budget, not by any
one caller's signal or deadline. A `TypeError` counts as a transport error only
when the transport's own network call raised it; any other one is a defect and
reaches the caller.

The forwarded user identity is always part of the entry, so no answer crosses a
forwarded user identity or a service binding. That is the runtime's whole
partition: a tenant carried any other way — a header an interceptor sets from
the request context after the cache, such as `X-Region`, or a field left out of
`keyFields` — is not. The provider owns that: an operation whose answer depends
on an input must declare it and keep it in the key, or not declare a cache. The
cache belongs to the application registry: when the registry ends, every entry
is dropped and every shared call in flight is aborted.
`client.invalidateResponses(keyPrefix)` drops matching entries for every
identity, and a matching call in flight is never stored.
`client.invalidateResponsesByField(field, value)` drops every entry whose
declared field carries `value`, across the service's operations and for every
identity. The value is compared in the canonical form the Go runtime renders
(`'42'` and `42` differ), and a value no runtime can compare — a fraction, an
unsafe integer number, `null` — throws a `TypeError`. Every call in flight for
an operation that declares the field still answers its callers but is never
stored, because its answer is not known yet.

A call made with `{ withoutResponseCache: true }` asks for the provider's
current answer: it neither reads nor stores an entry, never waits on a shared
call in flight, and no stored answer masks its failure. The generated
`ClientCallOptions` offers the option only when the contract declares a cache. A generated module
that declares a cache calls `requireClientRuntimeCapabilities(['response-cache'])`,
so an older `@putnami/client` fails to load it instead of running uncached.

## Boundaries

- **Scope:** Retry for network errors and retryable status codes; circuit breaker for per-client fail-fast protection; timeouts via `AbortSignal`; the provider-declared response cache
- **Out of scope:** Rate limiting, bulkhead pattern, hedged requests, distributed circuit breaker state (circuit state is per-client-instance, in-memory)
- **Extension points:** `Interceptor` interface — implement custom resilience patterns (e.g., fallback) as interceptors
