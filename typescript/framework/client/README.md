# @putnami/client

Smart client library for consuming Putnami APIs — typed clients, retry, telemetry, and DI integration.

## Features

- **Code generation** — generates fully-typed client classes from OpenAPI and Proto specs
- **Multi-transport** — REST/JSON, Connect (JSON and protobuf), SSE and WebSocket, dispatched in the order the provider declared
- **Interceptor pipeline** — composable auth, telemetry, context propagation, and retry
- **Resilience** — configurable retry with jitter, circuit breaker with health probing
- **Streaming** — server streams over SSE, WebSocket or Connect; client and bidirectional streams over WebSocket
- **Spec drift detection** — non-blocking warning when API has changed since generation

A *generated first-party* client — the path in Quick Start below — dispatches
every declared transport in the order the provider declared it, and the caller
names none of them. The `ClientBuilder`, `ConnectTransport` and
`WebSocketTransport` APIs stay available for tests and for contracts Putnami
does not own.

## Installation

```bash
putnami deps add @putnami/client
```

## Quick Start

Declare the provider identity and generate its client from the same API:

```typescript
import { application, http, api, openapi } from '@putnami/application';
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

In `src/api/items/[id]/get.ts`, declare the operation shape with
`endpoint().params({ id: String }).returns({ item: itemSchema })`.

Run `putnami clientgen`, then register the generated binding in the consumer:

```typescript
import { application } from '@putnami/application';
import { ItemsClient, registerItemsClient } from '@myorg/items-client';

const app = application();
registerItemsClient(app);
await app.start();

const items = app.context.get(ItemsClient);
const { item } = await items.getItems_id({ path: { id: '123' } });
console.log(item.name); // TypeScript knows the shape
```

Deployment config supplies the consumer identity, service URL, and credential
source. The generated package contains no token or secret:

```yaml
clients:
  clientId: orders.api
  services:
    catalog.items:
      url: https://items.internal
```

For a secured provider, install its authentication strategy as described in
[Application security](../application/doc/security.md), declare the service-token
profile on `api({ client })`, and configure that profile under the same binding.
A `gcp-id-token` credential requests the ID token for the binding URL, which is
what Cloud Run verifies it against; set `audience` on the credential to request
a different one. For the OAuth sources `audience` overrides the provider's
profile and contract audience:

```yaml
      credentials:
        service:
          source: gcp-id-token
          audience: https://items-abc.a.run.app
```

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

## Streaming

Generated first-party streams ride the transport the provider declared, through
one shared session:

```typescript
const stream = client.watchWidgets();          // server stream: the declared transports, in order
stream.onMessage((widget) => console.log(widget));
stream.onError((error) => console.error(error));
stream.onComplete(() => console.log('done'));

const upload = client.uploadWidgets();         // client stream: WebSocket
upload.send({ id: 'a' });
upload.end();
```

A WebSocket stream is opened as a browser must open it — the
`putnami.service.v1` subprotocol and nothing else — and everything the provider
needs to admit the call travels in the first frame. See
[doc/03-transports.md](doc/03-transports.md).

## Documentation

- **[Getting Started](doc/01-getting-started.md)** — end-to-end walkthrough and common patterns
- **[Code Generation](doc/02-code-generation.md)** — generator plugin, IR schema, generated output
- **[Transports](doc/03-transports.md)** — HTTP, Connect RPC, WebSocket, ClientBuilder negotiation
- **[Interceptors](doc/04-interceptors.md)** — auth, telemetry, context propagation, custom interceptors
- **[Resilience](doc/05-resilience.md)** — retry, circuit breaker, timeouts, error types

## API Overview

| Export | Description |
|--------|-------------|
| `register<Service>Client(target)` | Installs a generated binding resolved from application config/DI |
| `ClientBuilder<T>` | Fluent builder with auto-negotiated transport |
| `BaseClient` | Abstract base class for all generated clients |
| `authInterceptor(options?)` | JWT forwarding and M2M client credentials |
| `contextInterceptor()` | Propagates trace ID, request ID, region headers |
| `telemetryInterceptor(name)` | Records request count, errors, and latency |
| `retryInterceptor(config?)` | Exponential backoff with jitter |
| `circuitBreakerInterceptor(config?)` | Fail-fast on repeated downstream failures |
| `clientGenerator(config?)` | Plugin factory for the generator lifecycle |
| `resolveServiceUrl(name)` | Reads service URL from config or env var |
| `ClientError` / subclasses | Typed error hierarchy with service/method context |

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). It owns one public promise:

- **[Service clients](specs/service-clients.json)**, with the
  [one-latency-budget ADR](doc/adr/0001-one-latency-budget-per-call.md) and its
  [stream-session ADR](doc/adr/0003-one-stream-session-owns-every-transport-lifecycle.md).
  `timeoutMs` bounds the whole call, not one attempt: each attempt's budget is
  clamped to what remains and a backoff sleep that would overrun the deadline is
  skipped. Only network errors, per-attempt timeouts, and the statuses you
  declared retryable are retried, and the retry count is clamped into `[0, 10]`.
  A caller's abort signal ends the sequence rather than one attempt. The circuit
  breaker admits at most `halfOpenMaxConcurrent` trial requests while half-open.

Every generated method takes the declared input and an optional
`ClientCallOptions`, whose `signal` cancels that one call. A hand-written
subclass reaches the same option through the protected `request()` surface.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

## Caller-resolved shared page clients

A first-party generated client exports `bind<Client>(binding)`. Resolve the
owner endpoint from consumer configuration and reuse its binding while paging:

```ts
const pages = bindPageClient({
  url: ownerOrigin,
  clientId: 'replica',
  operationPaths: { [pageOperationId]: ownerPath },
  credentials: { owner: { source: 'gcp-id-token', audience: ownerAudience } },
});
try {
  // Call the generated page method with relation, afterKey and limit.
} finally {
  pages.dispose();
}
```

The path map is keyed by the published operation ID. It accepts only fixed
unary REST JSON paths, keeps an existing URL prefix, and preserves declared
security, schemas and resilience. Caller maps are snapshotted and owner caches
are isolated. Disposal releases the binding and refuses subsequent calls.

`pageTransportSchemas` and `validatePageTransportSchemas` from
`@putnami/client/generator` share the Go page contract. Owners specialize rows,
relation vocabulary and bounds, then test their resolved schemas for
conformance. The generated watermark is `bigint`; only empty or absent
`nextKey` ends a relation, and `ownerConfirmedAt` is the owner's clock.

## License

[FSL-1.1-MIT](../../../LICENSE.md)
