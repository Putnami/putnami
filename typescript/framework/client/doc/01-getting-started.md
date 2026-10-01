# Smart Client Library

`@putnami/client` generates typed API clients from OpenAPI and Proto specs. Consuming a Putnami service feels like calling local functions — transport, retry, auth, and telemetry are handled automatically.

## Caller-supplied endpoints

Every generated first-party method accepts an `endpoint` call option for a
trusted fleet member or a workspace URL resolved at runtime:

```typescript
const results = await Promise.allSettled(endpoints.map((endpoint) =>
  items.getItems_id({ path: { id: '123' } }, {
    endpoint,
    signal: AbortSignal.timeout(5_000),
  }),
));
```

The caller chooses concurrency and records each outcome. Omitting the option
uses the configured binding; an empty override fails before credentials or
network I/O. The selected endpoint must be trusted deployment data, because the
binding's credentials are sent there. Its HTTPS and `allowInsecure` policy,
declared schemas, security and resilience still apply, with redirects disabled.
A Google ID token defaults to the selected URL as audience; an explicitly
configured audience wins. Circuits, cached answers and coalesced calls stay
separate per endpoint. Cache invalidation still covers the whole service.
Stopping the application disposes every endpoint view and its open streams.
Clients constructed without a registered binding refuse endpoint overrides.
Unary and byte-stream refusals reject their returned promises; observer-based
streams report the same refusal through `onError` on the returned stream.

## Verbatim success bodies

When a caller verifies an answer over its exact bytes — a canonical body whose
digest the provider derived from the byte sequence it sent — pass a
`SuccessBody` sink with the call:

```typescript
import { SuccessBody } from '@putnami/client';

// items is the generated Items client of the service-to-service sample.
const raw = new SuccessBody();
const item = await items.getItems_id({ path: { id: '123' } }, { successBody: raw });
verifyDigest(raw.bytes); // the caller's own check over the provider's bytes, after the call accepted them
```

The generated method runs unchanged; on success `raw.bytes` holds the declared
JSON body exactly as the provider sent it, after the same status, media type,
schema and decode checks the value went through. A cached answer delivers the
stored bytes, as a copy. A failed call leaves `bytes` undefined, never an
earlier call's body. The option is refused before anything is sent on Connect
with the proto encoding, on a void or raw octet operation and on every stream
shape, and a sink serves one call at a time. It survives endpoint selection.
See [ADR 0007](adr/0007-a-call-delivers-its-success-body-through-a-caller-owned-sink.md).

## Quick Start

### 1. Declare the provider and add the generator

In the service that exposes the API:

```typescript
import { application, api, http, openapi } from '@putnami/application';
import { clientGenerator } from '@putnami/client/generator';

const app = application()
  .use(http({ port: 3000 }))
  .use(api({
    client: {
      service: { id: 'catalog.items', audience: 'api://catalog.items' },
      credentials: {},
    },
  }))
  .use(openapi({ title: 'Items API', version: '1.0.0' }))
  .use(clientGenerator({ packageName: '@myorg/items-client' }));
```

The endpoint declaration remains authoritative for its schema, security, and
resilience policy:

```typescript
// src/api/items/[id]/get.ts
export const GET = endpoint()
  .params({ id: String })
  .returns({ item: itemSchema })
  .handle(({ params }) => ({ item: getItem(params.id) }));
```

### 2. Generate the client

```bash
putnami clientgen --projects @myorg/items-service
```

This produces a publishable client package at `clients/ts/` with:

```
clients/ts/
├── package.json          → @myorg/users-client
├── src/
│   ├── index.ts          → Re-exports
│   ├── items-client.ts   → Typed ItemsClient class
│   └── types.ts          → Request/response interfaces
```

### 3. Register and call the generated binding

In the consuming service:

```typescript
import { application } from '@putnami/application';
import { ItemsClient, registerItemsClient } from '@myorg/items-client';

const app = application();
registerItemsClient(app);
await app.start();

const items = app.context.get(ItemsClient);
const { item } = await items.getItems_id({ path: { id: '123' } });
console.log(item.name);
```

The framework resolves deployment-owned values after config bootstrap:

```yaml
clients:
  clientId: orders.api
  services:
    catalog.items:
      url: https://items.internal
```

This first call is anonymous because the provider declared no credential
profiles. For secured operations, first install the provider-side strategy from
[Application security](../../application/doc/security.md), then declare the
service-token profile and endpoint security in the provider contract. The
consumer adds that profile's token source under the same service binding; the
generated client performs acquisition and renewal.

### Static non-secret headers

Static non-secret headers go in `headers`, beside the credentials. Each call
still forwards its own user token:

```yaml
clients:
  clientId: mcp-consumer
  services:
    intelligence:
      url: https://intelligence.example
      headers:
        X-Putnami-Observed-Revision: revision-one
      credentials:
        user:
          source: forwarded-user
```

A programmatic binding takes the same `headers` map, and the client snapshots
it. Explicit operation headers win, and cache keys see the value that is sent.
Every retry and stream carries the defaults; a first-party WebSocket stream
sends them in its init frame, apart from its credentials. Keep secrets in
`credentials`. These names fail with `client.config`: invalid names, case
aliases, credential, identity, request-context (`X-Trace-Id`, `X-Region`,
`X-Experiments`), tracing, origin and transport headers, and every header a
provider credential profile declares. A value must be visible ASCII, with
spaces or tabs inside it but not at either end. A binding that supplies an
operation's idempotency key fails that call before dispatch. The Go runtime
applies the same rules to the same configuration.

`ClientBuilder`, direct client construction, and low-level interceptors remain
available for tests and external services whose contracts are not first-party.

## Transport

Transport is determined automatically — the user never chooses:

- **Proto spec** → Connect protocol + binary proto encoding (preferred when both exist)
- **OpenAPI spec** → HTTP/JSON transport

The `ClientBuilder` auto-detects: if the generated client has embedded proto metadata, it uses Connect + binary. If the service doesn't support Connect, it falls back to HTTP/JSON.

Connect calls carry the gRPC protocol headers automatically: gzip is advertised (`grpc-accept-encoding` / `accept-encoding`) and decoded on the way back, and the per-call timeout is sent as `grpc-timeout` so the server enforces the same deadline the client does. See [Transports](./03-transports.md#connecttransport).

## Interceptors

Every request passes through an interceptor chain:

```
Auth → Telemetry → Context → Retry → Transport
```

### Built-in interceptors

| Interceptor | Purpose |
|---|---|
| `authInterceptor` | Forwards JWT from incoming request context, or uses client credentials |
| `telemetryInterceptor` | Records request count, error count, and duration histogram |
| `contextInterceptor` | Propagates trace ID, request ID, region, and experiment headers |
| `retryInterceptor` | Exponential backoff with jitter on 429/502/503/504 and network errors |

### Adding interceptors

```typescript
import { authInterceptor, telemetryInterceptor, contextInterceptor } from '@putnami/client';
import { OAuthService } from '@putnami/application';
import { get } from '@putnami/runtime';

const users = new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  interceptors: [
    authInterceptor({
      clientId: 'orders-service',
      tokenProvider: () => get(OAuthService).clientToken(),
    }),
    telemetryInterceptor('users'),
    contextInterceptor(),
  ],
});
```

## Authentication & Client Identity

### JWT Forwarding

When a user calls Service A, and Service A calls Service B, the user's JWT is automatically forwarded:

```
User → [JWT] → Service A → [same JWT forwarded] → Service B
```

This happens automatically: the HTTP middleware captures the `Authorization` header into the async context, and the `authInterceptor` reads it when making outgoing calls. No code needed.

### Client Credentials (M2M)

When no user JWT is available (background jobs, cron, service-initiated calls), the `tokenProvider` obtains a client credentials token:

```typescript
authInterceptor({
  tokenProvider: () => get(OAuthService).clientToken(), // cached + auto-refreshed
})
```

### Client Identity (`X-Client-Id`)

Each client declares its identity via `clientId`:

```typescript
authInterceptor({ clientId: 'orders-service' })
```

This sends `X-Client-Id: orders-service` on every request.

### Access Control (receiving side)

The receiving service restricts who can call it:

```typescript
import { requireClient, SecurityMiddleware } from '@putnami/application';

// Header-based identity check
app.post('/internal/sync', requireClient(['orders-service', 'billing-service']), handler);

// JWT-based client verification (cryptographic — checks azp/client_id claims)
app.post('/internal/sync', SecurityMiddleware({ client: ['orders-service'] }), handler);

// Both layers for defense in depth
app.post('/internal/sync',
  SecurityMiddleware({ client: ['orders-service'] }),
  requireClient(['orders-service']),
  handler,
);
```

## Retry & Backoff

Default retry configuration:

| Setting | Default | Description |
|---|---|---|
| `maxRetries` | 3 | Maximum retry attempts |
| `baseDelayMs` | 200 | Base delay between retries |
| `maxDelayMs` | 5000 | Maximum delay (cap) |
| `retryableStatuses` | [429, 502, 503, 504] | HTTP codes that trigger retry |
| `jitter` | true | Randomize delay to prevent thundering herd |

Override per-client:

```typescript
const users = new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  retry: { maxRetries: 5, baseDelayMs: 500 },
});
```

## Circuit Breaker

Protect against cascading failures when a downstream service is unhealthy. When failures exceed a threshold, the circuit opens and requests fail immediately instead of hitting the network.

```typescript
import { circuitBreakerInterceptor } from '@putnami/client';

const users = new UsersClient({
  baseUrl: 'http://users-api:3000',
  transport: 'http',
  interceptors: [
    circuitBreakerInterceptor({
      failureThreshold: 5,         // Open after 5 failures
      resetTimeoutMs: 30000,       // Try again after 30s
      successThreshold: 2,         // Close after 2 successes in half-open
      healthCheckUrl: 'http://users-api:3000/healthz',
      healthCheckIntervalMs: 10000,
    }),
  ],
});
```

### States

| State | Behavior |
|---|---|
| **Closed** | Normal operation. Failures are counted; resets on success. |
| **Open** | Fail-fast with `CircuitOpenError`. Probes health check if configured. |
| **Half-open** | Allows trial requests. Success → Closed; Failure → Open. |

### Health Probing

When `healthCheckUrl` is configured, the circuit breaker probes it periodically while open. If the health endpoint responds OK, the circuit transitions to half-open — allowing trial requests without waiting for the full `resetTimeoutMs`.

### Error Handling

```typescript
import { CircuitOpenError } from '@putnami/client';

try {
  await users.getUser({ id: '123' });
} catch (error) {
  if (error instanceof CircuitOpenError) {
    // Circuit is open — service is likely down
    console.log(error.circuitState); // 'open'
  }
}
```

### Configuration

| Setting | Default | Description |
|---|---|---|
| `failureThreshold` | 5 | Failures before opening |
| `resetTimeoutMs` | 30000 | Time before half-open (ms) |
| `successThreshold` | 2 | Successes in half-open before closing |
| `healthCheckUrl` | — | Optional health endpoint to probe |
| `healthCheckIntervalMs` | 10000 | Health probe interval (ms) |
| `failureStatuses` | [500, 502, 503, 504] | HTTP codes that count as failures |

## Proto Binary Encoding

Generated clients from proto specs use binary protobuf encoding by default — zero config needed. The generated client embeds the proto field metadata at generation time and auto-selects binary encoding.

Binary encoding uses the same `encodeProto`/`decodeProto` codec from `@putnami/application`, ensuring wire-level compatibility with gRPC servers. If the service doesn't support Connect, the client falls back to HTTP/JSON automatically via the `ClientBuilder`.

| Encoding | Content-Type | When |
|---|---|---|
| `proto` (default for proto) | `application/proto` | Service supports Connect |
| `json` (fallback) | `application/json` | Service only supports HTTP/JSON |

## Spec Drift Detection

Generated clients embed a hash of the API spec at generation time. When `checkDrift()` is enabled on the builder, the client fetches the live spec at startup and compares hashes:

```typescript
const users = await ClientBuilder.for(UsersClient)
  .baseUrl('http://users-api:3000')
  .checkDrift() // enable drift detection
  .build();
```

If the spec has changed, a loud warning is logged:

```
[client:spec-drift] API spec has changed since this client was generated!
  Client was built from: a1b2c3d4e5f67890
  Live service spec:     f0e1d2c3b4a59876
  Service:               http://users-api:3000
  Action: Regenerate the client with `putnami build` to pick up the latest API.
```

Drift detection is non-blocking — it never prevents the client from starting.

## Streaming

Streaming RPCs (server-streaming, client-streaming, bidirectional) are automatically detected from the proto spec and generated as streaming methods. The transport is picked per mode, and you never choose it:

- **Server-streaming** rides Connect first — the protocol carries a server stream over HTTP/1.1 as envelope frames — and falls back to WebSocket when the server answers gRPC status 12 `UNIMPLEMENTED` or a `200` that is not a Connect stream. Other errors (a `404`, a `503`) surface typed rather than being masked by the fallback.
- **Client- and bidirectional-streaming** go straight to WebSocket: a client stream needs HTTP/2, which `Bun.serve` does not provide as of Bun 1.4, and the framework's Connect route answers those RPCs with gRPC status 12 `UNIMPLEMENTED`.

Either way the caller sees the same observer. When a stream ends with a non-OK gRPC status the error handler receives a `ClientStreamError` carrying `grpcCode`/`grpcStatus`; when a fallback is needed and the runtime has no `WebSocket`, it receives a `ClientTransportUnavailableError` instead of a raw 501.

```typescript
// Server-streaming: subscribe to events
const stream = client.watchOrders({ userId: '123' });
stream.onMessage((order) => console.log('New order:', order));
stream.onError((err) => console.error('Stream error:', err));
stream.onComplete(() => console.log('Stream ended'));

// Cancel when done
stream.cancel();
```

For bidirectional streaming:

```typescript
const chat = client.chat();
chat.onMessage((msg) => console.log('Received:', msg));
chat.send({ text: 'Hello' });
chat.send({ text: 'World' });
chat.end(); // Signal end of input
```

## Service Discovery

Service URLs come from configuration — no service registry needed:

```yaml
# conf/.env.local.yaml
client:
  services:
    users-api: http://localhost:3000
    orders-api: http://localhost:3001

# conf/.env.production.yaml
client:
  services:
    users-api: https://users-api.internal:443
    orders-api: https://orders-api.internal:443
```

Or via environment variables:

```bash
CLIENT_SERVICE_USERS_API_URL=http://localhost:3000
```

## DI Integration

Register clients as singletons in the DI container:

```typescript
import { set, get } from '@putnami/runtime';

// Register
set(UsersClient, new UsersClient({ baseUrl, transport: 'http' }));

// Resolve anywhere
const users = get(UsersClient);
const user = await users.getUser({ id: '123' });
```

## Error Handling

All client errors extend `ClientError` with service name, method, and status:

```typescript
import { ClientRequestError, ClientServerError, ClientTimeoutError } from '@putnami/client';

try {
  await users.getUser({ id: 'invalid' });
} catch (error) {
  if (error instanceof ClientRequestError) {
    // 4xx — client-side error (bad input, unauthorized, etc.)
    console.log(error.status, error.responseBody);
  }
  if (error instanceof ClientServerError) {
    // 5xx — server-side error
  }
  if (error instanceof ClientTimeoutError) {
    // Request timed out
    console.log(error.timeoutMs);
  }
}
```

## Multi-Technology Vision

The generator architecture supports multiple output targets. TypeScript is the first implementation. The intermediate representation (IR) is language-agnostic — adding Go or Python generators is a matter of implementing a new code emitter from the same IR.

```
OpenAPI / Proto spec
        ↓
    Spec Reader (openapi-reader / proto-reader)
        ↓
    Intermediate Representation (IR)
        ↓
    Code Generator (ts-generator / go-generator / python-generator)
        ↓
    Generated Client Package
```
