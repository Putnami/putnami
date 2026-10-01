# Interceptors

The interceptor pipeline is the primary extension point for cross-cutting concerns. Every request made through a `BaseClient` passes through a chain of interceptors before reaching the transport.

## Overview

An interceptor is a function that wraps a request, can modify it, and calls `next` to continue the chain:

```typescript
type Interceptor = (
  request: ClientRequest,
  next: (request: ClientRequest) => Promise<ClientResponse>,
) => Promise<ClientResponse>;
```

Interceptors are applied **outer-to-inner** in the order they appear in `config.interceptors`. The chain always ends with the built-in `retryInterceptor` before the transport:

```
[user interceptors in order] → retryInterceptor → transport
```

For example, if you provide `[auth, telemetry, context]`, requests flow:

```
auth → telemetry → context → retry → transport
```

## Built-in Interceptors

### `authInterceptor(options?): Interceptor`

Handles both JWT user token forwarding and M2M client credentials.

#### Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `clientId` | `string` | — | Sent as `X-Client-Id` header on every request |
| `tokenProvider` | `() => Promise<string \| undefined>` | — | Called when no user JWT is in context; returns a bearer token |

#### JWT Forwarding

When a user's request flows through Service A and A calls Service B, the user's JWT is automatically forwarded — no code needed. The HTTP middleware captures the `Authorization` header into async context, and `authInterceptor` reads it:

```
User → [JWT in Authorization] → Service A → [same JWT forwarded] → Service B
```

This is the default behavior even without `tokenProvider`.

#### Client Credentials (M2M)

When no user JWT is present (background jobs, cron, service-initiated calls), `tokenProvider` is called to obtain a machine-to-machine token:

```typescript
import { authInterceptor } from '@putnami/client';
import { OAuthService } from '@putnami/application';
import { get } from '@putnami/runtime';

authInterceptor({
  clientId: 'orders-service',
  tokenProvider: () => get(OAuthService).clientToken(), // cached + auto-refreshed
})
```

#### Backward Compatibility

`authInterceptor` also accepts a function directly as its argument (legacy form):

```typescript
authInterceptor(async () => 'my-token')
// equivalent to:
authInterceptor({ tokenProvider: async () => 'my-token' })
```

#### Behavior

1. If `Authorization` header is already set on the request → does nothing (no override)
2. If user JWT is in async context (`__authorizationHeader`) → sets it as `Authorization: Bearer <jwt>`
3. Otherwise, if `tokenProvider` is set → calls it and sets `Authorization: Bearer <token>` if non-undefined
4. If `clientId` is set and `X-Client-Id` is not already present → sets `X-Client-Id: <clientId>`

#### `CLIENT_ID_HEADER` Constant

```typescript
import { CLIENT_ID_HEADER } from '@putnami/client';
// CLIENT_ID_HEADER === 'X-Client-Id'
```

Use this constant in server middleware that validates client identity:

```typescript
import { requireClient } from '@putnami/application';
import { CLIENT_ID_HEADER } from '@putnami/client';

// Restrict an endpoint to specific callers
app.post('/internal/sync', requireClient(['orders-service', 'billing-service']), handler);
```

---

### `contextInterceptor(): Interceptor`

Propagates routing context headers from the current async context to outgoing requests. Enables distributed tracing, geo-routing, and A/B experiment propagation across service boundaries.

#### Headers Propagated

| Context field | Header | Description |
|---|---|---|
| `traceId` | `X-Trace-Id` | Distributed trace identifier |
| `requestId` | `X-Request-Id` | Per-request correlation ID |
| `__region` | `X-Region` | Geographic routing hint |
| `__experiments` | `X-Experiments` | Active A/B experiment variant IDs |

#### Behavior

- Only sets a header if it is **not already present** on the request (no override)
- Strips CR, LF, and NUL from values to prevent header injection attacks
- When no async context exists (e.g., in tests), all four headers are left unset

```typescript
import { contextInterceptor } from '@putnami/client';

new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  interceptors: [contextInterceptor()],
});
```

---

### `telemetryInterceptor(serviceName: string): Interceptor`

Records metrics for every client call using `@putnami/application`'s telemetry system. All metrics are no-ops when telemetry is not initialized.

#### Parameter Validation

`serviceName` must match `[a-zA-Z0-9_-]+`. Throws `Error` at construction if the pattern doesn't match. This prevents metric key injection.

#### Metrics Recorded

| Metric key | Type | Description |
|---|---|---|
| `client.<service>.request` | Counter | Total requests made |
| `client.<service>.<statusCode>` | Counter | Requests per HTTP status code |
| `client.<service>.error` | Counter | Failed requests (thrown errors) |
| `client.<service>.duration` | Histogram | Response time in milliseconds |

```typescript
import { telemetryInterceptor } from '@putnami/client';

new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  interceptors: [telemetryInterceptor('users')],
});
// Records: client.users.request, client.users.200, client.users.duration
```

---

## Recommended Interceptor Order

When combining multiple interceptors, use this order for consistent behavior:

```typescript
import {
  authInterceptor,
  telemetryInterceptor,
  contextInterceptor,
  circuitBreakerInterceptor,
} from '@putnami/client';
import { OAuthService } from '@putnami/application';
import { get } from '@putnami/runtime';

new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  interceptors: [
    authInterceptor({
      clientId: 'orders-service',
      tokenProvider: () => get(OAuthService).clientToken(),
    }),
    telemetryInterceptor('users'),       // measures full round-trip including auth
    contextInterceptor(),                // propagates trace headers
    circuitBreakerInterceptor({          // fail-fast before retry
      healthCheckUrl: 'http://users-api:3000/healthz',
    }),
  ],
});
```

Order rationale:
- **Auth first** — other interceptors may need the auth header in place
- **Telemetry second** — measures the full request latency including auth overhead
- **Context third** — adds tracing headers before the request goes out
- **Circuit breaker last** — prevents retries when the circuit is open

---

## Writing Custom Interceptors

An interceptor is any function matching the `Interceptor` type. Here is a minimal example that adds a custom header:

```typescript
import type { Interceptor } from '@putnami/client';

function apiVersionInterceptor(version: string): Interceptor {
  return async (request, next) => {
    request.headers.set('X-Api-Version', version);
    return next(request);
  };
}
```

Interceptors can inspect responses, catch errors, and add logic on both sides of the call:

```typescript
import type { Interceptor } from '@putnami/client';
import { getLogger } from '@putnami/utils';

const logger = getLogger('client');

function loggingInterceptor(serviceName: string): Interceptor {
  return async (request, next) => {
    logger.debug(`[${serviceName}] → ${request.method} ${request.path}`);
    try {
      const response = await next(request);
      logger.debug(`[${serviceName}] ← ${response.status}`);
      return response;
    } catch (error) {
      logger.error(`[${serviceName}] ✗ ${error instanceof Error ? error.message : String(error)}`);
      throw error;
    }
  };
}
```

### Short-circuiting

An interceptor can return a response without calling `next` — useful for caching, mocking in tests, or fail-fast scenarios:

```typescript
function mockInterceptor(mockResponse: unknown): Interceptor {
  return async (_request, _next) => ({
    data: mockResponse,
    status: 200,
    headers: new Headers(),
  });
}
```

## ClientRequest and ClientResponse

```typescript
interface ClientRequest {
  method: string;                    // HTTP method
  path: string;                      // URL path
  params?: Record<string, string>;   // Path parameters
  query?: Record<string, string>;    // Query parameters
  body?: unknown;                    // Request body
  headers: Headers;                  // Mutable request headers
  signal?: AbortSignal;              // Timeout/cancellation signal
}

interface ClientResponse<T = unknown> {
  data: T;                           // Parsed response body
  status: number;                    // HTTP status code
  headers: Headers;                  // Response headers
}
```

## Boundaries

- **Scope:** Request-response pipeline for unary RPCs; custom interceptors can be added by the caller
- **Out of scope:** Interceptors for streaming RPCs (WebSocket connections go through `setupWebSocket` directly)
- **Dependencies:** `retryInterceptor` is always appended as the last interceptor before the transport (not configurable)
- **Extension points:** `ClientConfig.interceptors` — any number of custom interceptors
