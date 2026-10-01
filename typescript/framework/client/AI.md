# @putnami/client

Typed service clients for Putnami APIs with code generation, transport negotiation, interceptors, retry, circuit breaker, and drift detection.

Use this package when you need:

- generated TypeScript clients for Putnami services
- service-to-service calls that feel like local function calls
- automatic transport selection between HTTP and Connect
- auth/context propagation and resilience features

Do not use this package for:

- hand-written wrappers around third-party APIs unless you intentionally extend `BaseClient`
- server implementation; this package is for consuming APIs, not exposing them

## Producer Setup

In the service exposing the API, add the generator after the API spec plugins:

The generator is a build-time concern, so it is imported from the
`@putnami/client/generator` subpath (the package root only exports the runtime).

```ts
import { application, api, grpc, http, openapi, proto } from '@putnami/application';
import { clientGenerator } from '@putnami/client/generator';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(api())
    .use(openapi({ title: 'Users API', version: '1.0.0' }))
    .use(proto({ packageName: 'users.v1' }))
    .use(grpc())
    .use(clientGenerator({ packageName: '@myorg/users-client' }));
```

Then generate:

```bash
putnami build --impacted
```

## Consumer Setup

Use generated clients through `ClientBuilder`:

```ts
import { ClientBuilder } from '@putnami/client';
import { UsersClient } from '@myorg/users-client';

const users = await ClientBuilder.for(UsersClient)
  .baseUrl('http://users-api:3000')
  .clientId('orders-service')
  .timeout(10_000)
  .retry({ maxRetries: 5 })
  .checkDrift()
  .build();
```

Then call methods as normal typed functions:

```ts
const user = await users.getUser({ id: '123' });
const created = await users.createUser({ name: 'Alice', email: 'alice@example.com' });
```

## ClientBuilder

Recommended builder API:

| Method | Purpose |
|--------|---------|
| `.baseUrl(url)` | Required service URL |
| `.clientId(id)` | Sends `X-Client-Id` |
| `.timeout(ms)` | Per-request timeout |
| `.retry(config)` | Override retry config |
| `.interceptors(list)` | Add custom interceptors |
| `.checkDrift()` | Warn when live spec differs from generated client |
| `.build()` | Async build with transport negotiation |
| `.buildSync()` | Sync build using embedded metadata only |

Important:

- use `.interceptors([...])`, not `.intercept(...)`
- `build()` is preferred because it probes for Connect support
- `buildSync()` skips the probe and derives transport from metadata only

## Transport Model

For a client generated from a **first-party contract**, the provider decides. Each operation carries
the ordered `transports` list the provider published, and the runtime takes the first entry it can
carry — `connect:proto`, then `connect:json`, then `rest-json` for a unary operation on a provider
that installs `grpc()`. The caller names no transport, and the credential, deadline, retry and
circuit rules are identical on every wire. See ADR 0004.

The provider's protobuf descriptor travels in the contract and is embedded in the generated client,
so the binary codec reads field numbers, presence and `json_name` from the provider.

For a hand-written or legacy generated client with no contract, transport is selected from
`ClientConfig.transport` and the embedded metadata:

- proto-generated client → prefers Connect
- OpenAPI-only client → HTTP/JSON
- Connect unavailable at runtime → async builder falls back to HTTP/JSON

Most code should not instantiate transports directly. Generated clients + `ClientBuilder` are the default path.

### Connect facts

- A code the protocol does not define — `{}`, `{"code": null}`, the gRPC spelling `NOT_FOUND` — is
  no code at all; the status then decides through the protocol's **HTTP-to-code** table, where a bare
  `404` is `unimplemented` and a bare `400` is `internal`.
- A declared error arrives as the same `ClientFrameworkError` a REST call raises, rebuilt from the
  `putnami.client.v1.FrameworkError` detail.
- A response stream that ends without an `EndStreamResponse`, sends two, sends a frame after one,
  sets a reserved flag, or compresses without declaring an encoding is a **failure**, never a
  completion.
- The deadline sent is the call's *remaining* budget, as `Connect-Timeout-Ms`.

## Opaque JSON

A provider value declared `{"x-putnami-json": "any"}` is typed `unknown`, and a
free-form object (`{type: object, additionalProperties: true}`) is
`Record<string, unknown>`. The codec carries them as plain JSON values: an
integer outside the safe range is a `bigint` and is written back as the same
digits, and an explicit `null` stays apart from an absent member. A `Date`, a
`Map`, a class instance, `NaN` or a cycle is refused instead of encoded. Opaque
JSON is never a parameter and never travels over Connect.

## Raw Octet Payloads

A declared binary operation takes `Uint8Array | ArrayBuffer | ReadableStream<Uint8Array>`
and hands back `{ status, contentType, body }`, with the octets unchanged. The
declared bound is applied to the source before a socket exists and to the
response read on 2xx only, so a provider's error envelope stays readable. See
[transports](doc/03-transports.md#raw-octet-payloads).

## Verbatim Success Bodies

A caller that verifies an answer over its exact bytes passes a `SuccessBody`
sink as the generated `successBody` call option. The ordinary generated method
runs unchanged and, on success, `sink.bytes` is the declared JSON body exactly
as the provider sent it, after the same status, media type, schema and decode
checks the value went through. A cached answer delivers the stored bytes, as a
copy; a failed call leaves `bytes` undefined. Connect with the proto encoding,
void and raw octet operations and every stream shape refuse the sink before
anything is sent. See
[transports](doc/03-transports.md#verbatim-success-bodies).

## Streaming

Generated first-party streams go through `BaseClient`:

| Method | Declared shape | Transport |
| --- | --- | --- |
| `serviceStream` | `stream: 'server'` | the declared transports, in declared order, with a fallback before admission |
| `serviceClientStream` | `stream: 'client'` | WebSocket |
| `serviceBidiStream` | `stream: 'bidirectional'` | WebSocket |

Facts to rely on:

- The socket is opened the way a browser must open it:
  `new WebSocket(url, 'putnami.service.v1')`. No header, no credential in the
  URL, no token in the subprotocol list. Identity, credentials, budget and
  propagation context travel in the `init` frame.
- Every stream, SSE or WebSocket, traverses one `StreamSession`: five phases
  (`connecting → admitted → active → terminal → closed`), one terminal, one call
  measurement, one breaker verdict, four budgets (`handshakeMs`, `idleMs`,
  `maxFrameBytes`, `maxBufferedMessages`) distinct from the declared duration.
- Admission is the provider's protocol acceptance, never the first message.
  Retry is legal only before admission.
- A wire refusal carries the contract's own diagnostic code on
  `error.contractCode` (`client_contract.*`), the same code the Go client and
  both providers report for the same scene.
- `resumed: true` is accepted only when this client asked for a resume on a
  transport that declares one.
- A 401 or 403 admission refusal invalidates the credential exactly once and
  never replays the operation.

### Declared preference and fallback

`serviceStream` walks the declared transports in order and opens the first it can
carry. It opens the **next** one only when the provider *answered* that this wire
is not served at this path: HTTP 404, 405 or 426, gRPC status 12
`UNIMPLEMENTED`, or a completed handshake that did not select
`putnami.service.v1`. Every other answer is a fact about the call and is
surfaced as it is.

A fallback is only legal for an operation the provider declared `safe` or
`idempotent`, and there is none after admission. `serviceClientStream` and
`serviceBidiStream` never fall back: the caller's own messages would travel
twice.

### Declared resume

A server stream continues over a new socket after a transport break when the
transport declares `websocket.resume`, the operation declares
`resilience.stream.reconnect`, and the operation is declared `safe`. The
continuation presents the token the previous `ready` issued and the sequence of
the last message the caller completely received, re-resolves its credentials
before it dials, and is bounded by `MAX_STREAM_RESUME_ATTEMPTS` (5). A provider
that answers it with a fresh stream is refused rather than consumed. Nothing
continues unless the declaration says so.

`websocket.resume` is one of two second halves: an SSE transport that declares
`sse.continuation` satisfies the same `reconnect`, and
[Declared SSE continuation](#declared-sse-continuation) holds its rules. In
both, the binding's `credentials()` is resolved again on every reopening, so
each connection carries a credential valid when it dials
(`test/runtime/sse-continuation.test.ts`, "continues on another instance after
the last position the caller received, with a renewed credential").

### Provider-owned WebSocket wires

An operation whose `websocket` transport declares `wire: "provider"` emits a
method returning `Promise<ByteStream>` (encoding `binary`: `readable`,
`writable`, `closed`, `close()`) or `FrameStream<Send, Message>` (encoding
`json`: `send()`, `close()` and the observer callbacks). `BaseClient` opens them
with `serviceByteStream` and `serviceFrameStream`. The socket is opened with the
upgrade request's own headers and exactly the declared token; a runtime whose
`WebSocket` cannot carry headers (a browser) fails with
`ClientTransportUnavailableError` before dialing. A refused upgrade is a
`ClientResponseContractError`: the platform hides its status. Frames are
validated against the declared schemas, octets are split under the declared
frame budget, a normal close completes and any other close fails with its code.

## Interceptors

Useful built-ins:

| Export | Purpose |
|--------|---------|
| `authInterceptor()` | JWT forwarding or client-credentials auth |
| `contextInterceptor()` | Trace/request/region header propagation |
| `telemetryInterceptor(name)` | Client-side metrics |
| `retryInterceptor(config?)` | Exponential backoff |
| `circuitBreakerInterceptor(config?)` | Fail fast on unhealthy downstreams |

Example:

```ts
import {
  ClientBuilder,
  authInterceptor,
  circuitBreakerInterceptor,
  contextInterceptor,
  telemetryInterceptor,
} from '@putnami/client';
import { OAuthService } from '@putnami/application';
import { get } from '@putnami/runtime';
import { UsersClient } from '@myorg/users-client';

const users = await ClientBuilder.for(UsersClient)
  .baseUrl('http://users-api:3000')
  .clientId('orders-service')
  .interceptors([
    authInterceptor({
      clientId: 'orders-service',
      tokenProvider: () => get(OAuthService).clientToken(),
    }),
    telemetryInterceptor('users'),
    contextInterceptor(),
    circuitBreakerInterceptor({
      healthCheckUrl: 'http://users-api:3000/healthz',
    }),
  ])
  .build();
```

## Authentication And Identity

The client package supports:

- forwarding the incoming user JWT to downstream services
- machine-to-machine tokens through `tokenProvider`
- `X-Client-Id` identity via `.clientId(...)` or `authInterceptor({ clientId })`

This means:

- user-driven calls can preserve end-user identity downstream
- background jobs can call services with client credentials

A generated client's `ServiceBinding.headers` (`clients.services.<id>.headers`
in config) supplies static non-secret defaults such as
`X-Putnami-Observed-Revision`. The binding snapshots the map; explicit
operation headers win case-insensitively, and cache keys, retries and every
stream carrier see the effective values. Credential, identity, request-context,
tracing, origin and transport headers, declared credential headers, case
aliases, and values outside visible ASCII or with surrounding whitespace fail
with `client.config`, exactly as in the Go runtime. A static operation
idempotency key fails the call. Keep secrets in `credentials`.

## Resilience

### Retry

Retry is built in. Default retryable cases are transient failures like `429`, `502`, `503`, `504`, and network errors.

### Circuit breaker

Use `circuitBreakerInterceptor()` to avoid hammering an unhealthy service:

```ts
circuitBreakerInterceptor({
  failureThreshold: 5,
  resetTimeoutMs: 30_000,
  successThreshold: 2,
  healthCheckUrl: 'http://users-api:3000/healthz',
});
```

### Response cache

A provider declares a cache on one safe or idempotent unary operation with
`.client({ resilience: { cache: { freshMs, staleMs, maxEntries, keyFields, invalidationFields } } })`.
The generated client honors it with no consumer code: a fresh answer comes from
memory, one call per key and forwarded identity goes upstream at a time, and a
transport error, the operation's own timeout, an open breaker or a retryable
answer left after the retries returns the stored answer while it is younger
than `staleMs` — counted as `rpc.client.cache.stale_served` and logged — and so
does a caller whose own deadline passes while the provider hangs; a cancellation
never does. Entries are partitioned by forwarded user identity and binding only:
a tenant carried another way must be in the operation's key, which the provider
owns. The cache belongs to the application's registry and ends with it,
aborting the shared calls in flight. Revoke answers at once:

```ts
client.invalidateResponses('effectiveAccess?'); // one operation, every identity
client.invalidateResponses('effectiveAccess?body.principal=%22p1%22');
```

The key is `<operationId>?<field>=<value>&…` (ADR 0007 of `protocols/clientcontract`),
the same bytes the Go runtime renders.

When the provider lists a response property in `invalidationFields`, drop every
answer that carries one value of it, across the service's operations and for
every identity — a revocation that names a `principalId` the request never
carried:

```ts
client.invalidateResponsesByField('principalId', event.principalId); // returns how many were dropped
```

The value is a string, a boolean, a safe integer or a bigint, compared in the
canonical form the Go runtime renders, so `'42'` and `42` differ. A fraction,
an unsafe integer number or `null` throws a `TypeError` instead of matching
nothing. A route that must act on the provider's current answer bypasses the
cache for one call; a bypassed call neither reads, stores nor joins a call in
flight, and no stored answer masks its failure:

```ts
const access = await identity.effectiveAccess(input, { withoutResponseCache: true });
```

The option exists on a generated client's `ClientCallOptions` only when its
contract declares a cache (ADR 0007 of `protocols/clientcontract`).

### Error types

Useful error classes:

- `ClientError`
- `ClientRequestError`
- `ClientServerError`
- `CircuitOpenError`

A `ClientFrameworkError.message` is a local synthetic line
(`<service> request failed with <code>`) unless the binding asked for the
provider's own envelope `message`:

```ts
registerServiceClient(app, WidgetsClient, descriptor, {
  url: 'https://widgets.internal',
  clientId: 'orders-service',
  carryRemoteMessage: true,
});
```

The same flag is `clients.services.<id>.carryRemoteMessage` in config. Set it on
a consumer that shows the failure to the human who made the request; that
consumer then owns what it logs. The carried text is redacted of this call's own
credential material, on a declared and an undeclared code alike. The envelope's
`error` member is never carried. Mirrors `ServiceBinding.CarryRemoteMessage` in
the Go runtime; see ADR 0006 of the client contract.

## Drift Detection

`checkDrift()` compares the generated client’s embedded spec hash with the live service spec at startup and logs a warning when they differ.

Important:

- it is non-blocking
- it warns, it does not prevent startup

## Custom Clients

If you need to wrap a non-generated or third-party API, extend `BaseClient` directly:

```ts
import { BaseClient, type ClientConfig } from '@putnami/client';

export class SearchClient extends BaseClient {
  readonly serviceName = 'search';

  constructor(config: ClientConfig) {
    super(config);
  }

  async search(query: { q: string }) {
    return this.request('GET', '/search', {
      query: { q: query.q },
    });
  }
}
```

Use this only when generated clients are not available.

## Common Pitfalls

- Do not hand-pick `HttpTransport` / `ConnectTransport` for generated clients unless you have a specific reason
- Do not use `.intercept(...)`; the builder API is `.interceptors([...])`
- Do not forget `.baseUrl(...)`; builder creation without it throws
- Do not assume `checkDrift()` is blocking validation; it only warns
- Do not expect an undeclared response property on a decoded value: the decoder drops it and validates only declared properties, so adding an optional response property is safe for deployed clients (protocols/clientcontract ADR 0014)
- Do not use this package to expose routes or RPC methods; that belongs to the server packages

## Detailed Documentation

See `doc/`:

- `01-getting-started.md`
- `02-code-generation.md`
- `03-transports.md`
- `04-interceptors.md`
- `05-resilience.md`

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It owns the
[service-clients specification](specs/service-clients.json) with its
[one-latency-budget ADR](doc/adr/0001-one-latency-budget-per-call.md) and its
[stream-session ADR](doc/adr/0003-one-stream-session-owns-every-transport-lifecycle.md).

Facts to rely on when generating code:

- `timeoutMs` bounds the whole call, not one attempt. Retries spend that budget;
  they never extend it unless `maxElapsedMs` is set explicitly.
- Retryable = network error, per-attempt timeout, or a status in
  `retryableStatuses` (default `429, 502, 503, 504`). Everything else, including
  a caller abort, is raised on the first attempt. `maxRetries` is clamped to
  `[0, 10]`.
- Cancellation is per call. A generated method takes it as the optional second
  argument, `ClientCallOptions`; a hand-written subclass passes the same
  `{ signal }` through the protected `request()` options.
- The circuit breaker is a separate interceptor from retry, so it observes the
  whole sequence; while half-open it admits `halfOpenMaxConcurrent` trials.
- `dispose()` (and `using`) releases interceptor-owned timers.

Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.

## Declared SSE continuation

An SSE transport that declares `sse.continuation` (clientcontract ADR 0013)
speaks the negotiated wire on every connection: the request carries
`X-Putnami-Stream-Wire: putnami.sse.v1`, and a response without that
acknowledgment is a contract error before any message, with no fallback. The
stream ends only at `event: complete` or a typed error; an end of body before
either is an interruption. With `resilience.stream.reconnect` on a safe stream,
an interruption reopens the same operation in the same session:

- **Cursor mode** reopens with the declared query parameter set to the cursor of
  the **last message the subscribed caller received**. A value is handed over
  only while `onMessage` has a handler, so a received value is one the caller
  took; values decoded before the caller subscribed are dropped on the
  reopening and sent again by the provider. Nothing arrives twice.
- **Best-effort mode** reopens with the original query and keeps its queue.
  Messages produced while no connection was open may be missing or repeated.
- Credentials are resolved again for every reopening. A reopening refused with
  401 re-mints a forwarded user credential once through the binding's `refresh`;
  the first opening never re-mints.
- Reopenings share `MAX_STREAM_RESUME_ATTEMPTS` with WebSocket resume. A typed
  error, a contract error, cancellation or an expired budget never reopens.
- One session, one breaker verdict, one terminal and one call measurement,
  whichever connection carried the stream.

A generated client whose contract declares a continuation calls
`requireClientRuntimeCapabilities(['sse-continuation'])` when it loads.
