# Transports

`@putnami/client` supports three transport implementations. The `ClientBuilder` selects the best one automatically — you rarely need to choose manually.

## Overview

| Transport | Protocol | When used |
|-----------|----------|-----------|
| `HttpTransport` | HTTP/JSON | OpenAPI-generated clients; fallback when Connect unavailable |
| `ConnectTransport` | Connect RPC v1 (JSON or binary proto) | Proto-generated clients — unary **and** server-streaming |
| `WebSocketTransport` | WebSocket/JSON | Client/bidi-streaming; fallback for server-streaming |

## First-party dispatch: the provider decides

A client generated from a first-party contract does not choose a transport. Each
operation carries the ordered list the provider published, and the runtime takes
the first entry it can carry:

```jsonc
// x-putnami-client, on one operation
"transports": [
  { "protocol": "connect", "encoding": "proto", "protobufMethod": "/catalog.gadgets.v1.GadgetsService/GetGadgetsById" },
  { "protocol": "connect", "encoding": "json",  "protobufMethod": "/catalog.gadgets.v1.GadgetsService/GetGadgetsById" },
  { "protocol": "rest-json", "path": "/gadgets/{id}", "encoding": "json" }
]
```

A provider that installs `grpc()` advertises Connect first, so the same generated
method travels as binary protobuf — and the whole interceptor chain, credentials,
deadline, retry and circuit included, is the one a REST call obeys. Drop the
`grpc()` plugin and the same client falls to `rest-json` with no code change on
either side.

The provider's protobuf descriptor is published in the document and embedded in
the generated client, so the binary codec reads field numbers, presence and
`json_name` from the provider rather than re-deriving them.

### Streaming Matrix

Streaming transport is chosen per RPC mode, automatically:

| RPC mode | First choice | Fallback |
|----------|--------------|----------|
| Server-streaming | Connect (`application/connect+json` or `application/connect+proto`) | WebSocket, after gRPC status 12 `UNIMPLEMENTED` or a `200` that is not a Connect stream |
| Client-streaming | WebSocket | — (Connect over HTTP/1.1 cannot carry a client stream) |
| Bidirectional-streaming | WebSocket | — |

Client- and bidi-streaming skip Connect entirely rather than pay a round trip to be told `UNIMPLEMENTED`. A client/bidi RPC that does reach the Connect route (for instance through `stream()`) reads the typed `UNIMPLEMENTED` and re-routes to WebSocket without the caller re-subscribing; if the runtime has no `WebSocket` global, the stream fails with `ClientTransportUnavailableError` instead of a raw 501.

Other failures are *not* masked by the fallback: a `404`, a `503`, or any other non-`UNIMPLEMENTED` Connect error surfaces as a typed error, so a typo'd RPC path stays visible instead of turning into a WebSocket connection failure.

**When to use `ClientBuilder`:** whenever you have a generated client class. It auto-detects the right transport based on the client's embedded metadata.

**When to use transports directly:** when writing a custom (non-generated) client class that extends `BaseClient`.

## ClientBuilder

`ClientBuilder` is the recommended way to create clients. It performs auto-negotiation and handles drift detection:

```typescript
import { UsersClient } from '@myorg/users-client';
import { ClientBuilder } from '@putnami/client';

// Async build with transport negotiation (recommended)
const users = await ClientBuilder.for(UsersClient)
  .baseUrl('http://users-api:3000')
  .checkDrift()
  .build();

// Sync build (skips network probe, uses embedded metadata)
const users = ClientBuilder.for(UsersClient)
  .baseUrl('http://users-api:3000')
  .buildSync();
```

### Transport Negotiation

`build()` (async) performs active negotiation:

1. If the generated client has embedded proto metadata → preferred transport is Connect
2. Probes `GET /_/api.proto` with a 3-second timeout to confirm Connect is available
3. If probe succeeds → uses Connect + binary proto
4. If probe fails (service offline or HTTP-only) → falls back to HTTP/JSON and logs a warning

`buildSync()` skips the probe and derives transport from the client's static metadata:
- Client has `specHash` → Connect (binary proto if proto metadata present)
- Client has no `specHash` → HTTP/JSON

### Builder Methods

All methods return `this` for chaining.

#### `ClientBuilder.for(ClientClass): ClientBuilder<T>`

Start building a client for the given generated class.

| Parameter | Type | Description |
|-----------|------|-------------|
| `ClientClass` | `new (config) => T` | The generated client class |

#### `.baseUrl(url: string): this`

Set the service base URL. **Required** — throws `Error` if omitted when calling `build()` or `buildSync()`.

#### `.clientId(id: string): this`

Set the client identity string sent as `X-Client-Id` header on every request. Used for service-to-service authentication.

```typescript
.clientId('orders-service')
```

#### `.timeout(ms: number): this`

Override the default request timeout (30,000ms). Applies to every request.

```typescript
.timeout(10_000)  // 10 seconds
```

#### `.retry(config: Partial<RetryConfig>): this`

Override retry behavior. Merged with defaults.

```typescript
.retry({ maxRetries: 5, baseDelayMs: 500 })
```

#### `.interceptors(interceptors: Interceptor[]): this`

Add custom interceptors applied before the built-in retry interceptor.

```typescript
.interceptors([circuitBreakerInterceptor(), telemetryInterceptor('users')])
```

#### `.checkDrift(): this`

Enable spec drift detection at startup. Fetches the live API spec and compares it against the hash embedded in the generated client. Logs a warning if they differ. Non-blocking — never prevents startup. See [Getting Started](./01-getting-started.md#spec-drift-detection) for the warning format.

#### `.build(): Promise<T>`

Async build with transport negotiation. Preferred when your init path is async.

**Throws:** `Error` if `baseUrl` is not set.

#### `.buildSync(): T`

Synchronous build. Derives transport from client metadata without network probes. Use when async init isn't possible (e.g., module-level initialization).

**Throws:** `Error` if `baseUrl` is not set.

## HttpTransport

HTTP/JSON transport using the Fetch API. Used for OpenAPI-generated clients.

### Behavior

- Sends requests as `POST` (Connect) or the appropriate HTTP method (GET, POST, PUT, DELETE, PATCH)
- Serializes body as JSON
- Substitutes `{param}` placeholders in paths with values from `params`
- Appends query parameters as URL search params
- Throws `ClientRequestError` for 4xx responses and `ClientServerError` for 5xx
- The error message is extracted from a `message` field in the JSON response body, or defaults to `HTTP {status}`
- Validates the response body against the provider's published schema. A property the schema does not declare is dropped from the returned value, not refused, so a provider can add an optional response property and roll out before its consumers; every declared property is still validated. Stream messages and declared error `details` follow the same rule, and a request body with an undeclared property is still refused. See [ADR 0014](../../../../protocols/clientcontract/doc/adr/0014-a-client-drops-a-response-property-it-does-not-declare.md).

### Direct Usage (Custom Clients)

```typescript
import { BaseClient, type ClientConfig } from '@putnami/client';

export class MyClient extends BaseClient {
  readonly serviceName = 'my-service';

  constructor(config: ClientConfig) {
    super(config);
  }

  async getItem(params: { id: string }): Promise<Item> {
    return this.request('GET', '/items/{id}', { params: { id: params.id } });
  }

  async createItem(body: CreateItemBody): Promise<Item> {
    return this.request('POST', '/items', { body });
  }

  async searchItems(query: { q: string; limit?: number }): Promise<Item[]> {
    return this.request('GET', '/items', {
      query: { q: query.q, limit: String(query.limit ?? 20) },
    });
  }
}

const client = new MyClient({ baseUrl: 'http://my-service:3000', transport: 'http' });
```

## Raw octet payloads

A provider that declares a bounded raw octet payload (`Binary({ mediaType, maxBytes })`)
gets a generated method that carries the bytes unchanged:

```ts
const stored = await client.putBlob({ path: { id: '1' }, body: file.stream() });
const blob = await client.getBlob({ path: { id: '1' } });
// blob.status, blob.contentType, blob.body — a Uint8Array
```

- **The request payload is `Uint8Array | ArrayBuffer | ReadableStream<Uint8Array>`.**
  `readBoundedBody` reads it under the declared bound and refuses one octet
  past it, so an oversized source is never drained and no socket is opened.
- **The success payload carries the octets, the status and the content type.**
  The transport reads the body once as octets and decodes text only where a
  JSON representation was declared.
- **The response read is capped by the declaration**, on 2xx only: an error
  envelope is not the payload the contract bounded, and capping it there would
  make a provider's own refusal unreadable.
- **Nothing is base64-encoded and nothing is wrapped in JSON.** `format: binary`
  inside a JSON document is refused at generation; base64 bytes in a document
  stay `format: byte`.
- **Only `rest-json` carries octets.** A contract that dispatches them on
  Connect is refused at generation.

See [ADR 0005](../../../../go/framework/api/doc/adr/0007-a-raw-octet-payload-is-declared-and-bounded.md).

### Raw HTTP streams

`BinaryStream({ maxBytes })` declares an unframed HTTP upload or download. It
publishes `*/*` binary content with `x-putnami-streamed: true` and the positive
`x-putnami-max-bytes` transfer bound.
The generated input requires a concrete `contentType` and a `BinarySource`:

```ts
const result = await client.transfer({ body: file.stream(), contentType: 'image/png' });
await result.body.pipeTo(destination);
// result.status and result.contentType retain the provider's metadata.
```

The client sends the source directly and returns a `ReadableStream<Uint8Array>`
before EOF. Consume or cancel it to release the connection. The content type's
parameters survive unchanged; even JSON-labelled bytes remain opaque. The
whole-body buffering does not apply to this explicit stream; the declared bound
is enforced incrementally, while error envelopes remain bounded and use the
declared error contract.

Uploads cannot retry or remint-and-replay a consumed source. Response caches
are refused for streamed operations. Authentication, cancellation and deadlines
still apply. This remains unary HTTP and adds no message or WebSocket framing.
Existing bounded `Binary(...)` declarations retain their previous behavior.

See [ADR 0011](../../../../go/framework/api/doc/adr/0011-streamed-octets-carry-their-own-media-type.md).

## Verbatim success bodies

A JSON operation whose caller owns a canonical-form check takes the declared
success body unchanged through the generated `successBody` call option:

```ts
// items is the generated Items client of the service-to-service sample.
const raw = new SuccessBody();
const item = await items.getItems_id({ path: { id: '123' } }, { successBody: raw });
// raw.bytes: the provider's body, after status, media type, schema and decode checks
```

- **Only a transport that carries the document unchanged delivers it**:
  `rest-json` and Connect with the `json` encoding, whose codec is the JSON
  document itself. Connect with the `proto` encoding is refused with a
  `client.config` error before dispatch: the runtime rebuilds the JSON from the
  protobuf reply, and nothing it could hand over is the provider's.
- **Raw octets and streams have their own paths** and refuse the sink; a void
  operation declares nothing to deliver.
- **A cached answer delivers the stored bytes**, copied, so the sink never
  aliases the cache. A failed call leaves the sink empty.

See [ADR 0007](adr/0007-a-call-delivers-its-success-body-through-a-caller-owned-sink.md).

## Provider-owned WebSocket wires

An operation whose `websocket` transport declares `wire: "provider"` is not the
first-party conversation: the provider owns its messages. The emitted method
states the shape:

| Declared wire | Emitted method returns |
| --- | --- |
| `encoding: "binary"` (byte stream) | `Promise<ByteStream>` — `readable`, `writable`, `closed`, `close()` |
| `encoding: "json"` under a declared subprotocol | `FrameStream<Send, Message>` — `send()`, `close()`, `onMessage`, `onError`, `onComplete`, `cancel()` |

```typescript
const tunnel = await gateway.connectDatabase({ query: { database: 'main' } });
const writer = tunnel.writable.getWriter();
await writer.write(bytes);
for await (const chunk of tunnel.readable) handle(chunk);
await tunnel.close();
```

The upgrade request is the admission: the socket is opened with the request's
own headers (declared credential, client identity, trace context) and exactly
the declared subprotocol. The platform `WebSocket` of a browser takes no
headers, so there the opening fails with `ClientTransportUnavailableError`
before anything is dialed; Bun and Node carry them. The platform also hides the
status of a refused upgrade, so a refusal is a `ClientResponseContractError`
(`provider refused the websocket upgrade`) rather than the declared typed error
the Go client decodes.

After admission the runtime applies the declared frame budget (outgoing octets
are split, an oversized frame is refused), validates each frame against the
declared schema, applies the idle, heartbeat and operation budgets, completes on
a normal close and fails with the code of any other close.

## ConnectTransport

Connect RPC protocol transport. Supports two encoding modes:

| Encoding | Content-Type | Description |
|---|---|---|
| `json` | `application/json` | Standard JSON — compatible everywhere |
| `proto` | `application/proto` | Binary protobuf — more efficient on the wire |

Proto-generated clients default to binary encoding and embed the field metadata needed for encoding/decoding. No manual configuration required.

### Encoding Fallback

When a Connect request receives a JSON response (e.g., from a service that doesn't fully support binary proto on all endpoints), the transport automatically falls back to JSON parsing.

### Server Streaming

`ConnectTransport.stream(path, options)` consumes a server-streaming RPC over Connect envelope frames — the same framing `GrpcPlugin`'s `buildStreamRpcHandler` writes:

```
[flags: 1 byte][length: 4 bytes big-endian][payload]
```

| Flag | Meaning |
|------|---------|
| `0x00` | Message payload |
| `0x01` | Message payload, gzip-compressed (per-message) |
| `0x02` | The `EndStreamResponse` — JSON `{}` on success, `{"error": {"code": …}}` on failure |

Frames may straddle chunk boundaries; the decoder reassembles them and bounds each frame by the configured `maxResponseSize`.

A response stream ends with **exactly one** `EndStreamResponse`, and the client holds the provider to that. Each of these is a failure, not a completion:

| The provider… | The client… |
|---|---|
| ends without an `EndStreamResponse` | fails, rather than reading a truncated stream as success |
| writes a second one, or any frame after it | fails, naming the frame after the end of the stream |
| sets a reserved flag bit (the six most significant) | fails, rather than guessing what it meant |
| sets the compressed bit with no `Connect-Content-Encoding` | fails, rather than decompressing what was not declared |
| writes `{"error": {}}` or `{"error": null}` | fails: those are invalid, and a failure that cannot be read is never a success |

`cancel()` aborts the fetch, which closes the connection and stops the server's writer.

A request stream is enveloped too, even when it carries one message: a bare body is a unary call and a conforming server reads it as one.

Streams never carry a `Connect-Timeout-Ms` derived from the per-call timeout — they are long-lived by definition.

### Compression

A unary call advertises `accept-encoding: gzip, identity`; a stream advertises `Connect-Accept-Encoding: gzip, identity`, which is the header the protocol reserves for per-message compression. Responses are decoded from `grpc-encoding`, `connect-content-encoding`, or `content-encoding`; a body the fetch layer already decompressed is left alone (the gzip magic header is checked before decoding). Decompressed output is bounded by `maxResponseSize`, so a small payload cannot inflate unbounded.

Request compression is opt-in via `ClientConfig.compressRequests` — the body is gzipped and declared with `Content-Encoding: gzip`.

### Deadlines

The call's remaining budget is sent as `Connect-Timeout-Ms`, in whole milliseconds, so the server aborts the handler on the same deadline the client enforces locally. It is the budget *left*, not the configured timeout: a retried attempt carries less than the first one did. `grpc-timeout` belongs to gRPC and is not sent — a conforming Connect server would see no deadline at all.

### Error Format

A Connect error is a JSON body with one of sixteen lower-case `code` strings, an optional `message`, and optional `details`. The transport maps it onto a typed error carrying the numeric status:

| Property | Meaning |
|---|---|
| `status` | HTTP status (`0` for stream and transport errors) |
| `grpcCode` | Numeric `google.rpc.Code`, e.g. `12` — see the exported `GrpcStatus` map |
| `grpcStatus` | Canonical gRPC name, e.g. `'UNIMPLEMENTED'` |
| `details` | Connect error details, each `{type, value}` with `value` unpadded base64 protobuf |

A body whose `code` the protocol does not define — `{}`, `{"code": null}`, the gRPC spelling `NOT_FOUND` — carries no code at all. The status then decides, through the protocol's **"HTTP to Error Code"** table, which is deliberately not the inverse of the code table:

| Bare status | Inferred code | Why |
|---|---|---|
| 400 | `internal` | a bare 400 is usually an intermediary's, not the service's |
| 404 | `unimplemented` | the RPC has no route here, which is not a missing resource |
| 401 / 403 | `unauthenticated` / `permission_denied` | |
| 429, 502, 503, 504 | `unavailable` | |
| anything else | `unknown` | |

### Declared errors over Connect

A first-party operation declares a *stable* code (`not_found`) and a typed `details` member, and D0.1 fixes that envelope for every transport. Connect's sixteen codes are categories, so the framework envelope rides in a `putnami.client.v1.FrameworkError` detail — the protocol's own mechanism for typed messages. The generated client reads it and raises the same `ClientFrameworkError` a REST call would have raised: same `code`, same `status`, same declared `details`, narrowable by the generated `isXxxError` guard.

| Error | Raised when |
|---|---|
| `ClientRequestError` | 4xx Connect response |
| `ClientServerError` | 5xx Connect response |
| `ClientStreamError` | A Connect stream ended with a failing `EndStreamResponse` |
| `ClientResponseContractError` | A provider broke a response-stream rule, or answered a shape the contract does not declare |
| `ClientFrameworkError` | The provider declared this error; the code, status and details are the contract's |
| `ClientTransportUnavailableError` | A stream needed the WebSocket fallback and the runtime has no `WebSocket` |

## WebSocketTransport

WebSocket transport for client/bidi-streaming RPCs, and the fallback for server-streaming when Connect answers `UNIMPLEMENTED`. Used automatically for methods annotated with streaming in the proto spec — no manual setup needed.

### Protocol

1. Connects to `ws(s)://host/path` (automatically converted from `http(s)://`)
2. Sends `Authorization` as the **first JSON frame** (not in sub-protocols, to avoid proxy log exposure):
   ```json
   { "__auth": "Bearer <token>" }
   ```
3. Sends the initial request body as a second JSON frame
4. Receives server messages as JSON frames (one per WebSocket frame)
5. Server closing the connection signals stream completion
6. Client can cancel by calling `stream.cancel()`

### Trace ID Propagation

The `X-Trace-Id` header is encoded as a WebSocket sub-protocol when it matches the token pattern `[a-zA-Z0-9\-_.~]{1,128}`:

```
protocols: ["trace.abc123xyz"]
```

### Pending Buffer

For bidirectional streams, messages sent before the connection is established are buffered (up to 512 messages). They are flushed in order once the connection opens. If the buffer overflows, `onError` is called and the connection closes.

## First-party service streams

`WebSocketTransport` above is the raw JSON socket a hand-written client uses.
A **generated first-party client** uses `ServiceWebSocketTransport`, which
speaks the published `putnami.service.v1` wire and is driven through
`BaseClient`:

| `BaseClient` method | Declared shape | Transport chosen |
|---|---|---|
| `serviceStream` | `stream: 'server'` | the first declared transport this runtime carries — SSE, else WebSocket |
| `serviceClientStream` | `stream: 'client'` | WebSocket |
| `serviceBidiStream` | `stream: 'bidirectional'` | WebSocket |

Transport choice follows the order the provider declared in `x-putnami-client`.
The caller never branches on it.

### Admission

The socket is opened the way a browser must open it:

```typescript
new WebSocket(url, 'putnami.service.v1');
```

No header, no credential in the URL, no token hidden in the subprotocol list —
the browser `WebSocket` constructor accepts none of those. Identity,
credentials, deadline, budget, propagation context and ordinary headers travel
in the first frame the client sends (`init`). The provider answers `ready`, and
only then is the stream admitted.

If the provider does not echo the subprotocol, the client refuses with
`ClientResponseContractError` before sending anything.

### One session for every transport

SSE and WebSocket streams both run through `StreamSession`
([ADR 0003](adr/0003-one-stream-session-owns-every-transport-lifecycle.md)):

- five phases — `connecting → admitted → active → terminal → closed` — always
  reaching `closed`;
- exactly one terminal and exactly one call measurement;
- four budgets kept distinct from the declared operation duration:
  `handshakeTimeoutMs` (open → `ready`), `idleTimeoutMs` (between provider
  frames after admission), `maxFrameBytes`, `maxBufferedMessages`;
- one breaker verdict, recorded per phase: a refused handshake that reached the
  provider opens the circuit, admission records a success, and a break after
  admission records nothing at all;
- retry only before admission, and only for a declared retryable class on a
  `safe` or `idempotent` operation.

### Typed refusals

A frame the wire refuses fails the stream with the contract's own diagnostic
code on `error.contractCode` — for example `client_contract.invalid_transport`
for a message out of sequence, `client_contract.invalid_resilience` for a
`resumed: true` this client never asked for. The Go client and both providers
report the same code for the same scene: the shared corpus in
`protocols/clientcontract/fixtures/websocket` is replayed on every side.

A 401 or 403 admission refusal invalidates the credential exactly once and never
replays the operation.

### Not supported yet

`proto`-encoded payloads are refused when the socket opens, stream resumption is
requested only when the selected transport declares it, and a JSON `null` inside
an application payload is refused by the published wire in both directions.

## BaseClient

`BaseClient` is the abstract base class all generated clients extend. Use it to build custom (non-generated) clients.

### `protected request<T>(method, path, options?): Promise<T>`

Execute a typed unary request through the interceptor chain.

| Parameter | Type | Description |
|-----------|------|-------------|
| `method` | `string` | HTTP method: `'GET'`, `'POST'`, `'PUT'`, `'DELETE'`, `'PATCH'` |
| `path` | `string` | URL path, may contain `{param}` placeholders |
| `options.params` | `Record<string, string>` | Path parameter values |
| `options.query` | `Record<string, string>` | Query parameter values |
| `options.body` | `unknown` | Request body (serialized per transport) |
| `options.headers` | `Record<string, string>` | Additional request headers |

**Returns:** Parsed response body cast to `T`.

**Throws:** `ClientRequestError` (4xx), `ClientServerError` (5xx), `ClientTimeoutError` (timeout).

### `protected stream<T>(path, options?): StreamObserver<T>`

Open a server-streaming connection. Connect clients attempt Connect first and fall back to WebSocket; HTTP clients go straight to WebSocket. See the [streaming matrix](#streaming-matrix).

| Parameter | Type | Description |
|-----------|------|-------------|
| `path` | `string` | RPC path (Connect) or WebSocket endpoint path |
| `options.body` | `unknown` | Initial request message |

**Returns:** `StreamObserver<T>` — subscribe with `onMessage`, `onError`, `onComplete`, or cancel with `cancel()`.

### `protected streamDuplex<TIn, TOut>(path, options?): DuplexStream<TIn, TOut>`

Open a bidirectional WebSocket stream. Connect cannot carry a client stream over HTTP/1.1, so this never attempts Connect.

**Returns:** `DuplexStream<TIn, TOut>` — extends `StreamObserver<TOut>` with `send(data: TIn)` and `end()`.

## ClientConfig

Full configuration object accepted by any client constructor and `BaseClient`.

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `baseUrl` | `string` | **required** | Service base URL |
| `transport` | `'http' \| 'connect'` | **required** | Transport protocol |
| `packageName` | `string` | — | Proto package (required for Connect) |
| `encoding` | `'json' \| 'proto'` | `'json'` | Connect encoding mode |
| `protoMeta` | `ProtoMeta` | — | Proto field metadata for binary encoding |
| `clientId` | `string` | — | Sent as `X-Client-Id` on every request |
| `timeoutMs` | `number` | `30000` | Per-attempt request timeout (ms); also sent as `Connect-Timeout-Ms` on Connect and bounds total retry time (see [Resilience](./05-resilience.md)) |
| `maxResponseSize` | `number` | `33554432` | Response body cap (bytes); also bounds each stream frame and gzip-decompressed output |
| `compressRequests` | `boolean` | `false` | Gzip Connect request bodies (`grpc-encoding: gzip`) |
| `retry` | `Partial<RetryConfig>` | defaults | Retry configuration (see [Resilience](./05-resilience.md)) |
| `interceptors` | `Interceptor[]` | `[]` | Custom interceptors, prepended to the chain |

## Service URL Configuration

Service URLs can come from Putnami config or environment variables:

```yaml
# conf/.env.local.yaml
client:
  services:
    users-api: http://localhost:3000
    orders-api: http://localhost:3001
  timeoutMs: 5000
```

```bash
# Environment variable alternative
CLIENT_SERVICE_USERS_API_URL=http://localhost:3000
```

Use `resolveServiceUrl()` to read the URL at runtime:

```typescript
import { resolveServiceUrl } from '@putnami/client';

const users = new UsersClient({
  baseUrl: resolveServiceUrl('users-api'),
  transport: 'http',
});
```

`resolveServiceUrl` validates that the URL uses `http://` or `https://` and throws a descriptive error if the URL is missing or uses a disallowed scheme.

## Boundaries

- **Scope:** Unary REST/JSON and raw octets, Connect (unary and server streaming), SSE server streams, WebSocket server/client/bidirectional streams
- **Out of scope:** gRPC-Web and native gRPC HTTP/2 (use the `grpc()` plugin on the server side); Connect client and bidirectional streaming, which HTTP/1.1 cannot carry — a duplex operation declares `websocket`
- **Dependencies:** Native Fetch API and WebSocket API (available in Bun and modern browsers)
- **Extension points:** Implement the `Transport` interface to add a custom transport; provide custom interceptors via `ClientConfig.interceptors`
