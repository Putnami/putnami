# Smart Client Library

`@putnami/client` runs the clients Putnami generates from a provider
declaration. The provider owns the schemas, the typed errors, the credential
profiles, the transports and the resilience policy; the consumer injects one
binding and calls typed methods.

The happy path has four steps, and none of them is transport code:

```
declare the provider  →  generate  →  register the binding  →  call
   endpoint(), api()     putnami       register<Service>Client
   clientGenerator()     clientgen     (or DI injection)        items.getItems(input)
```

`typescript/samples/10-service-to-service` runs all four end to end.

## 1. Declare the provider

An endpoint is declared once, in `src/api/**`. `.mayThrow()` names the framework
errors the operation can return, so they arrive at the consumer as typed errors
rather than a formatted string:

```typescript
import { endpoint } from '@putnami/application';
import { NotFoundException } from '@putnami/runtime';

export const GET = endpoint()
  .params({ id: String })
  .returns(itemSchema)
  .mayThrow('NotFound')
  .handle(async (ctx) => {
    const item = items.get(ctx.params.id);
    if (!item) throw new NotFoundException('item not found');
    return item;
  });
```

`api({ client })` declares the service identity and the credential profiles the
generated clients must satisfy. Profiles are names and header placements;
credential *values* stay in consumer configuration and never enter generated
source:

```typescript
api({
  client: {
    service: { id: 'catalog.items', audience: 'api://catalog.items' },
    credentials: { 'catalog-key': { kind: 'api-key', header: CATALOG_KEY_HEADER } },
  },
})
```

`.client({...})` carries the client-facing policy the route cannot express:
which credential alternatives satisfy the operation, the resilience policy, the
transport preference order, and whether a server stream may be continued.

```typescript
export const GET = endpoint()
  .params({ id: String })
  .returns(Stream(itemRevisionSchema))
  .mayThrow('NotFound')
  .client({
    security: { alternatives: [{ allOf: [{ profile: 'catalog-key' }] }] },
    transports: ['websocket', 'sse'],
    resume: true,
    resilience: { stream: { handshakeTimeoutMs: 2000, idleTimeoutMs: 5000, reconnect: true } },
  })
  .handle(async (ctx) => {
    const from = ctx.resumeFrom ?? 0n;
    // A continuation is told where the consumer left off.
  });
```

An SSE server stream continues across a broken connection, on another instance
included, when the provider declares how. Cursor mode names the output field
that carries the provider's opaque position and the query parameter that
receives it on a reopening:

```typescript
export const GET = endpoint()
  .query({ cursor: Optional(String), until: Number })
  .returns(Stream(itemChangeSchema))
  .mayThrow('NotFound')
  .client({
    transports: ['sse'],
    sseContinuation: { mode: 'cursor', cursor: { outputField: 'cursor', queryParameter: 'cursor' } },
    resilience: { stream: { reconnect: true } },
  })
  .handle(async (ctx) => {
    const { cursor, until } = ctx.queryParams();
    let after = changes.position(cursor);
    // A position this provider never issued is the declared NotFound: the feed
    // never restarts from the beginning.
    if (after === undefined) throw new NotFoundException('unknown position');
    while (after < until) {
      const change = await changes.next(after, ctx.signal);
      if (!change) return; // drained: no terminal, the consumer continues elsewhere
      ctx.send(change);
      after = change.revision;
    }
  });
```

`cursor` is a required string of `itemChangeSchema` and a declared string query
parameter, and the handler continues exclusively after the position it is
given. `typescript/samples/10-service-to-service` serves this route, and its
generated consumers prove the continuation across an instance change.
`{ mode: 'best-effort' }` is the other mode: the reopening sends the original
query and no position, and the stream may miss or repeat messages.

| Declaration | What it produces |
|---|---|
| `.returns(schema)` | A typed unary result |
| `.returns(Stream(schema))` | A server stream |
| `.body(Stream(schema))` + `.returns(schema)` | A client stream with one declared result |
| `.body(Stream(schema))` + `.returns(Stream(schema))` | A bidirectional stream |
| `.body(Binary({ mediaType, maxBytes }))` | A raw octet request body, bounded |
| `.returns(Binary({ mediaType, maxBytes }))` | A raw octet response, bounded |
| `.mayThrow('NotFound')` | A typed client error carrying that stable code |
| `.mayThrowDetails('Conflict', schema)` | The same typed error, with its `details` typed by `schema` |
| `.client({ transports: [...] })` | The dispatch order every generated client follows |
| `.client({ connectEncodings: [...] })` | The Connect payload encoding order a generated client dispatches |
| `.client({ resume: true })` | A server stream a broken socket may continue |
| `.client({ sseContinuation: { mode: 'cursor', cursor: {...} } })` | An SSE server stream a broken connection continues after the last delivered cursor, on any instance; `{ mode: 'best-effort' }` reopens with the original query and may miss or repeat messages |
| `.client({ external: 'OCI Distribution Specification v1.1' })` | Nothing: the route stays served and published, and an external authority owns it |

The bound on `Binary` is mandatory. An unbounded octet body is the one shape
generation refuses outright, because no consumer could size a buffer for it.

A provider that serves a standard protocol beside its own routes — an OCI
registry, a Go module proxy, an npm registry — marks each standard leg with
`external`. The leg stays in the OpenAPI document with
`x-putnami-external-contract` instead of `x-putnami-client`, and no generated
client, proto document or gRPC route includes it; its callers use the
standard's own adapters, and its own schemas are published the way a provider
without a contract publishes them. `external` stands alone: blank, beside
another client option, or declared without `api({ client })`, it fails before
the route is bound and again at OpenAPI generation.

Finally, declare the targets to emit. The generator is a build-time concern, so
it comes from the `@putnami/client/generator` subpath — the package root exports
the runtime only:

```typescript
import { clientGenerator } from '@putnami/client/generator';

.use(
  clientGenerator({
    packageName: '@example/items-client',
    targets: ['ts', 'go'],
    go: { modulePath: 'go.putnami.dev/examples/ts-items-client', packageName: 'itemsclient' },
  }),
)
```

## 2. Generate

```bash
putnami clientgen --projects items-provider
```

Generation is strict for a provider-owned contract: a semantic the shared client
IR cannot carry fails with the operation named and a remedy, rather than
emitting a client that silently loses it. The workspace commands, the drift
guard and the full diagnostic list are in
[Generated clients](/docs/tooling-&-workspace/generated-clients).

## 3. Register the binding

```typescript
import { registerItemsClient } from '@example/items-client';

registerItemsClient(instance);
```

Deployment values arrive through the typed `clients` config block, so the
generated package holds no URL, token or key:

```yaml
clients:
  clientId: orders.api
  services:
    catalog.items:
      url: https://items.internal
```

An operation that declares a credential is never sent anonymously: an unbound
profile fails the call before dispatch instead of reaching the provider
anonymously.

## 4. Call

The client arrives through dependency injection. No base URL, no interceptor and
no header is written at the callsite:

```typescript
export const GET = endpoint()
  .inject({ itemsClient: ItemsClient })
  .returns({ items: ArrayOf(itemSchema) })
  .handle(async (ctx) =>
    ctx.deps.itemsClient.getItems({ query: { search: '', limit: 10 } }),
  );
```

Every generated method takes the declared input and an optional
`ClientCallOptions` carrying a per-call `AbortSignal`:

```typescript
const controller = new AbortController();
await items.getItems({ query: { search: '', limit: 10 } }, { signal: controller.signal });
```

A declared error arrives as a `ClientFrameworkError` carrying the stable code,
the status, the retry hint and the redacted declared details — the same object a
Connect or WebSocket call produces for the same scene.

### The shapes a generated client exposes

| Declared shape | Generated entry point |
|---|---|
| Unary | `items.getItems(input, options?)` |
| Raw octets | the same method, returning `{ status, contentType, body }` with the octets unchanged |
| Server stream | `items.watchItems(input)` — SSE, WebSocket or Connect, in declared order |
| Client stream | `items.adjustItems(input)` — WebSocket, with one declared result |
| Bidirectional stream | `items.negotiateItems(input)` — WebSocket |
| Byte stream (provider-owned wire) | `gateway.connectDatabase(input)` — `Promise<ByteStream>`, a readable and writable pair of `Uint8Array`; Go: `*client.ByteStream`, an `io.ReadWriteCloser` |
| Provider-owned subprotocol | `events.subscribeEvents(input)` — `FrameStream<Send, Message>` of the declared JSON frames, no envelope; Go: `*client.FrameStream[In, Out]` |
| Connect unary | the same method; the transport is chosen from the declared order |

```typescript
const stream = items.watchItems({ path: { id: '123' } });
stream.onMessage((item) => console.log(item));
stream.onError((error) => console.error(error));
stream.onComplete(() => console.log('done'));
```

The caller never names a transport. Credentials, deadline, retry, circuit
breaker, trace propagation and metrics are the same chain on every wire.
`typescript/framework/client/AI.md` and `doc/03-transports.md` hold the
per-transport rules: admission, the five session phases, the four stream
budgets, declared fallback and declared resume.

A WebSocket stream is opened the way a browser must open it —
`new WebSocket(url, 'putnami.service.v1')`, no header and no credential in the
URL. Identity, credentials, budget and propagation context travel in the `init`
frame.

A provider-owned wire is the exception, declared by the provider with
`api.ByteStream()` / `ByteStream()` or `.Subprotocol()` / `.subprotocol()`: its
upgrade request is the admission, so it carries the declared credential and
identity headers, and a browser — which cannot set them — refuses to open it
rather than dialing anonymously.

### Rules, not gaps

These are contract decisions, and they do not change with a later release:

- A WebSocket transport declaring `proto` encoding is refused at generation. The
  wire keeps the encoding in the contract; the client carries JSON messages.
- A unary call never falls back to another transport. A server stream falls back
  once, before admission, and only when the provider *answered* that this wire
  is not served here.
- A stream never continues unless the provider declared `resume` or
  `sseContinuation` on a `safe` server stream and the operation's effective
  `resilience.stream.reconnect` is on. A continuation follows a transport break
  and nothing else: a typed error, an explicit `complete`, a contract error, a
  cancellation or an expired budget ends the stream. Five continuations per
  session is the framework cap.
- A delivered message is never replayed by `resume` or by a cursor
  continuation: the position advances only when the subscribed caller took the
  value, and the provider continues exclusively after it. A best-effort
  continuation is the one declared exception — it reopens the original query,
  and messages may be missing or repeated — and it is never substituted for a
  cursor continuation that failed.
- A cursor is a position, not a credential. Every reopening resolves the
  binding's `credentials()` again and runs the provider's full security chain,
  and the provider answers a stale, forged or out-of-scope cursor with a typed
  error, never by restarting from the beginning.
- A declared SSE continuation speaks a negotiated wire: the request carries
  `X-Putnami-Stream-Wire: putnami.sse.v1`, the provider acknowledges it on the
  response head and ends a successful stream with `event: complete`. A provider
  that does not acknowledge fails the call before any message; a consumer that
  does not ask reads the legacy framing. Upgrade the provider first, then its
  consumers.
- A nullable member over `connect+proto` is refused: proto3 has one absence, so
  null and absent would become one value.

## Cross-language clients (TS ↔ Go)

One provider generates clients in both languages from one contract. Set
`targets` to include both, as above. How emission is split:

- The **same-language** client is emitted in-app during `putnami build` (the
  TypeScript client for a TypeScript provider; a Go provider emits its Go client
  the same way through `api.Clients(...)`).
- The **cross-language** client is emitted by **`putnami clientgen`**, a neutral
  scheduler-mediated command that runs the other toolchain's emitter against the
  provider's own contract. No extension shells out to the other. A provider
  opts in by listing `/tooling/clientgen-extension` in its `putnami.json`
  `extensions`.

The result lives side by side under `clients/`:

```
clients/
├── ts/   → the TypeScript package
└── go/   → a standalone Go module (go.mod + client.gen.go), or a package when modulePath is empty
```

Generated clients are committed, like a `.pb.go`, and regenerated on demand. The
workspace check fails the build when a committed client drifts from the
contract.

## Resilience and observability

### Interceptor chain

Every outgoing request passes through:

```
Auth → Telemetry → Routing Context → Retry → Transport
```

| Interceptor | What it does |
|---|---|
| **Auth** | Forwards the incoming JWT, or obtains a client-credentials token |
| **Telemetry** | Records `client.{service}.request`, `.error`, `.duration` metrics |
| **Routing Context** | Propagates trace ID, request ID, region, experiment headers |
| **Retry** | Exponential backoff with jitter on transient failures (429, 502, 503, 504) |
| **Circuit Breaker** | Fail-fast when the downstream is unhealthy, with optional health probing |

### Circuit breaker

Three states: **Closed** (normal) → **Open** (fail-fast) → **Half-open** (trial
requests). When `healthCheckUrl` is set, the breaker probes it while open and
transitions to half-open when the service recovers.

```typescript
circuitBreakerInterceptor({
  failureThreshold: 5,
  resetTimeoutMs: 30000,
  healthCheckUrl: 'http://users-api:3000/healthz',
});
```

### Spec drift detection

Generated clients embed a spec hash. `checkDrift()` warns at startup when the
live service spec differs. It is a warning, not a gate — the workspace check is
what fails a build on drift.

## Low-level client

`ClientBuilder`, `BaseClient` and the transports stay supported for tests and
for contracts Putnami does not own. They are not part of the first-party path
above, and a handwritten first-party transport fails the workspace guard.

```typescript
import { ClientBuilder } from '@putnami/client';

const search = await ClientBuilder.for(SearchClient)
  .baseUrl('https://external.example.com')
  .clientId('orders-service')
  .timeout(10_000)
  .build();
```

### ClientBuilder

| Method | Description |
|---|---|
| `.baseUrl(url)` | Service base URL (required) |
| `.clientId(id)` | Client identity for `X-Client-Id` |
| `.timeout(ms)` | Request timeout |
| `.retry(config)` | Retry configuration |
| `.interceptors([...])` | Custom interceptors |
| `.checkDrift()` | Enable spec drift detection |
| `.build()` | Async build with transport negotiation |
| `.buildSync()` | Sync build using embedded metadata |

### ClientConfig

| Option | Type | Default | Description |
|---|---|---|---|
| `baseUrl` | `string` | required | Service base URL |
| `transport` | `'http' \| 'connect'` | required | Transport protocol |
| `timeoutMs` | `number` | 30000 | Whole-call budget |
| `retry.maxRetries` | `number` | 3 | Max retry attempts |
| `retry.baseDelayMs` | `number` | 200 | Base backoff delay |
| `retry.maxDelayMs` | `number` | 5000 | Max backoff delay |
| `retry.retryableStatuses` | `number[]` | [429,502,503,504] | Retryable HTTP codes |
| `retry.jitter` | `boolean` | true | Randomize delay |
| `interceptors` | `Interceptor[]` | [] | Custom interceptors |

### ClientGeneratorConfig

| Option | Type | Default | Description |
|---|---|---|---|
| `targets` | `('ts' \| 'go')[]` | `['ts']` | Languages to generate |
| `output` | `string` | `clients/ts` | TypeScript output directory |
| `packageName` | `string` | auto | npm package name for the TS client |
| `go.output` | `string` | `clients/go` | Go output directory |
| `go.modulePath` | `string` | `''` | Go module path — set it to make `clients/go` a standalone module |
| `go.packageName` | `string` | `client` | Go package name |
| `go.clientName` | `string` | `Client` | Generated Go client struct name |

## Related guides

- [Generated clients](/docs/tooling-&-workspace/generated-clients) — the workspace generate/sync/check commands, the drift guard, and migrating a handwritten client
- [Service Clients](/docs/frameworks/go/service-clients) — the same four steps in Go
- [WebSockets](/docs/frameworks/typescript/websockets) — the provider side of a stream
- [Proto & gRPC](/docs/frameworks/typescript/proto-grpc) — the Connect provider

## Support and compatibility

`@putnami/client` is a public, documented, maintained package classified
`stable`. Its service-clients specification and its accepted decision records
live next to the package source.

The promise is that a call has one latency budget. `timeoutMs` bounds the whole
call rather than a single attempt: each attempt's timeout is clamped to the
budget that remains, a backoff sleep that would overrun the deadline is skipped,
and a caller who wants retries to cost more time says so explicitly with
`maxElapsedMs`. Only network errors, per-attempt timeouts, and the statuses you
declared retryable are retried, and the retry count is clamped into `[0, 10]`. A
caller's abort signal ends the whole sequence, not just the running attempt. The
circuit breaker is a separate interceptor, so it observes the outcome of the
retry sequence rather than each attempt, and while half-open it admits at most
`halfOpenMaxConcurrent` trial requests.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
