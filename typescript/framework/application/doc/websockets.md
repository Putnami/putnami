# WebSockets & Streaming

Real-time communication with typed, validated streaming endpoints.

## Overview

The Application framework supports real-time streaming through WebSocket and SSE transports. The `Stream()` combinator lets you define streaming endpoints using the same `endpoint()` builder as HTTP routes.

## Streaming Modes

Wrap a schema in `Stream()` to mark it as a stream of messages. The mode is derived from which slots are streams:

| `body` | `returns` | Mode | Transport |
| ----------- | ----------- | ------------- | -------------------- |
| `T` | `T` | Unary | REST (standard HTTP) |
| `T` | `Stream(T)` | Server-stream | SSE or WebSocket |
| `Stream(T)` | `T` | Client-stream | WebSocket |
| `Stream(T)` | `Stream(T)` | Bidirectional | WebSocket |

## Server-Stream

Push events to the client. Supports both SSE and WebSocket via protocol negotiation.

```typescript
// src/api/events/ws.ts
import { endpoint, Stream } from '@putnami/application';

export default endpoint()
  .returns(Stream({ event: String, payload: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'welcome', payload: 'hello' });
    // handler runs until it returns, then the connection closes
  });
```

The client can connect via:

- **WebSocket** (`Upgrade: websocket` header) — bidirectional transport
- **SSE** (`Accept: text/event-stream` header) — HTTP streaming, each `ctx.send()` emits an SSE `data:` frame

A stream declared in a file-based route file (`get.ts`) is registered like any
other route: the file exports it under the method name, and only GET carries a
stream because that is the request SSE and the WebSocket upgrade both use.

### What SSE decides before the response head

Everything that can refuse the operation runs before the first body byte, so a
consumer learns a stream was refused from the HTTP status and never from an
event:

- a request that carries a body is refused with 400 — a server stream is a GET
  and has no declared body;
- params and query are validated, and a failure is the ordinary non-2xx;
- `.secure(...)` runs as endpoint middleware, so 401 and 403 arrive as statuses.

### What SSE bounds after admission

| Bound | Default | Behavior past it |
| --- | --- | --- |
| `maxBufferedMessages` | 64 | The stream fails with its typed terminal event; one queue slot is reserved for that event. |
| `maxFrameBytes` | 1 MiB | Same — a frame past the bound is one the consumer contract requires the other end to reject. |
| `heartbeatMs` | 15000 | A `: heartbeat` comment is written. It carries no data, so a consumer parses it away and only its idle budget observes it. A heartbeat is skipped, never queued, while the consumer is behind. |

## Client-Stream

Consume a stream of messages from the client, return a final result:

```typescript
// src/api/upload/ws.ts
import { endpoint, Stream } from '@putnami/application';

export default endpoint()
  .body(Stream({ type: String, data: String }))
  .returns({ result: String })
  .handle(async (ctx) => {
    let count = 0;
    for await (const msg of ctx.messages()) {
      count++;
    }
    return { result: `processed ${count} messages` };
  });
```

When the handler returns, the return value is sent to the client and the connection closes.

## Bidirectional

Send and receive concurrently:

```typescript
// src/api/chat/[roomId]/ws.ts
import { endpoint, Stream, Uuid } from '@putnami/application';

export default endpoint()
  .params({ roomId: Uuid })
  .body(Stream({ type: String, data: String }))
  .returns(Stream({ event: String, payload: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'welcome', payload: ctx.params.roomId });
    for await (const msg of ctx.messages()) {
      ctx.send({ event: 'echo', payload: msg.data });
    }
    return { event: 'bye', payload: ctx.params.roomId };
  });
```

The handler returns its terminal value, typed by the `returns` element schema. A first-party client reads it as the stream's result; the raw WebSocket bridge discards it.

## Handler Context

The handler context adapts based on the streaming mode:

- `ctx.messages()` — available when body is `Stream()`, returns `AsyncIterable<T>`. The loop ends when the client disconnects.
- `ctx.send(data)` — available when returns is `Stream()`, pushes a validated message to the client.
- `ctx.params`, `ctx.queryParams()` — validated on connection open, same as HTTP endpoints.

Schema validation applies to all stream messages automatically. Invalid messages are rejected before reaching the handler.

## Protocol Negotiation (Server-Stream)

Server-stream endpoints support two transports:

**WebSocket** — the client sends `Upgrade: websocket`:

```typescript
const ws = new WebSocket('ws://localhost:3000/events');
ws.onmessage = (e) => console.log(JSON.parse(e.data));
```

**SSE** — the client sends `Accept: text/event-stream`:

```typescript
const source = new EventSource('http://localhost:3000/events');
source.onmessage = (e) => console.log(JSON.parse(e.data));
```

Client-stream and bidirectional endpoints are WebSocket-only.

## Manual Registration

Register stream endpoints manually with `register()` — the same method used for HTTP routes. Stream endpoints are auto-detected from the `Stream()` combinator:

```typescript
import { application, api, http, endpoint, Stream, Uuid } from '@putnami/application';

const apiPlugin = api({ autoScan: false });

apiPlugin.register('/chat', endpoint()
  .params({ roomId: Uuid })
  .body(Stream({ type: String, data: String }))
  .returns(Stream({ event: String, payload: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'welcome', payload: ctx.params.roomId });
    for await (const msg of ctx.messages()) {
      ctx.send({ event: 'echo', payload: msg.data });
    }
  }),
);

const app = application()
  .use(http())
  .use(apiPlugin);
```

## Client-Side Usage

Connect from a browser:

```typescript
// WebSocket
const ws = new WebSocket('ws://localhost:3000/chat');

ws.onopen = () => {
  console.log('Connected');
  ws.send(JSON.stringify({ type: 'chat', data: 'Hello!' }));
};

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  console.log('Received:', msg);
};

ws.onclose = () => console.log('Disconnected');
```

```typescript
// SSE (server-stream only)
const source = new EventSource('http://localhost:3000/events');

source.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  console.log('Event:', msg);
};

source.onerror = () => console.log('Connection lost');
```

## First-Party Service Streams

A stream endpoint under a first-party client contract — `api({ client: … })` —
speaks the published `putnami.service.v1` wire instead of raw JSON. That wire is
what a generated client, in TypeScript or Go, knows how to talk.

Nothing changes in the endpoint. Declaring the contract on the plugin is what
turns the socket into a service stream:

```typescript
const apiPlugin = api({
  autoScan: false,
  client: {
    service: { id: 'catalog.items', audience: 'api://widgets' },
    credentials: { 'service-key': { kind: 'api-key', header: 'X-Api-Key' } },
  },
});

apiPlugin.register('/widgets/watch', endpoint()
  .returns(Stream({ id: String }))
  .secure({ principalKind: 'apikey' })
  .client({ security: { alternatives: [{ allOf: [{ profile: 'service-key' }] }] }, idempotency: { kind: 'safe' } })
  .handle(async (ctx) => { ctx.send({ id: 'one' }); }),
  'GET',
);
```

### Admission happens in the first frame, not in the upgrade

A browser cannot put a header on a `WebSocket` upgrade, so no first-party
consumer does. The caller opens `new WebSocket(url, 'putnami.service.v1')` and
sends one `init` frame carrying its identity, the credentials for the profiles
the operation declares, its deadline and budget, the propagation context, and
any ordinary headers.

The provider rebuilds the request from that frame, runs the endpoint's own
security chain on it, re-reads cancellation and application shutdown, then
answers `ready`. The handler never runs before that. See
[ADR 0003](adr/0003-websocket-admission-runs-the-security-chain-on-the-rebuilt-request.md).

An `init` frame may not carry `Authorization`, `Cookie`, `traceparent`,
`X-Client-Id` or any `Sec-WebSocket-*` header as an ordinary header: those are
the frame's own members, and an ordinary header that would shadow a declared
credential is refused.

### The declared budgets

`resilience.stream` on the operation, then the document default, then the
framework floor:

| Field | Effect | Floor |
| --- | --- | --- |
| `handshakeTimeoutMs` | open to `ready` inclusive | `attemptTimeoutMs`, else 10 s |
| `idleTimeoutMs` | interval between two client frames after admission | 30 s |
| `heartbeatMs` | provider `ping` cadence | **0 — no heartbeat** |
| `maxFrameBytes` | one reassembled frame, never a TCP frame | 1 MiB |
| `maxBufferedMessages` | inbound queue depth | 16 |

No heartbeat is sent unless one is declared: a heartbeat nobody asked for hides a
dead socket instead of revealing it. Passing the queue depth ends the stream with
a typed back-pressure error rather than dropping a message.

### Terminals

Exactly one terminal reaches the caller. A server stream delivers its values as
`message` frames and ends with a `result` that carries no payload; a client or
bidirectional stream ends with a `result` carrying its single declared value. A
handler that throws produces a typed `error` frame carrying the stable wire code
and, when the endpoint declared a schema for that status, its validated details.
Application shutdown ends every live conversation with a typed `503` rather than
cutting the socket.

### Rules the wire enforces

- **Resume is a declaration, on both halves.** A stream continues over a new
  socket only when the operation declares `resume: true` on a WebSocket
  transport *and* `resilience.stream.reconnect`, and only for a `safe` server
  stream. A request for a continuation the operation did not declare is refused
  with `client_contract.invalid_resilience`. The handler reads the position it
  resumes after from `ctx.resumeFrom`; one that ignores it re-sends what the
  consumer already read.
- **Protobuf is not the payload encoding.** Wire v1 keeps `proto` in the
  contract vocabulary, and both emitters refuse a WebSocket transport declaring
  it at generation, before a client exists. The provider refuses it again at
  admission, so a stale descriptor cannot open a socket.
- **A JSON `null` never travels inside an application payload.** The published
  wire refuses `null` anywhere in a frame, so a nullable field is not carried on
  a first-party stream. The provider refuses to emit it rather than writing
  bytes no conforming consumer accepts.

## Provider-Owned Wires

Some routes speak a wire the provider owns instead of the first-party
conversation: a byte tunnel, or an existing JSON protocol with its own
subprotocol. Both are bidirectional and both admit on the upgrade request.

```typescript
import { ByteStream, endpoint, Stream } from '@putnami/application';

// Byte stream: raw octets in binary messages, both ways.
export const connect = endpoint()
  .query({ database: String })
  .body(ByteStream())
  .returns(ByteStream())
  .secure({ scopes: ['gateway:connect'] })
  .handle(async (ctx) => {
    for await (const chunk of ctx.messages()) ctx.send(chunk); // Uint8Array both ways
  });

// Provider-owned subprotocol: one JSON value per text message, no envelope.
export const events = endpoint()
  .body(Stream(EventClientFrame))
  .returns(Stream(EventServerFrame))
  .subprotocol('putnami.events.v1')
  .handle(async (ctx) => {
    for await (const frame of ctx.messages()) ctx.send(answer(frame));
  });
```

- **Admission is the upgrade.** The endpoint's middleware (`.secure()`
  included) and params/query validation run on the upgrade request, exactly as
  for SSE, and refuse with an ordinary HTTP response before any socket exists.
- **One token.** The route speaks exactly its declared subprotocol, or none
  when a byte stream declares none. Any other offer is refused with 400.
- **The framework owns the socket.** The declared `maxFrameBytes` bounds a
  message both ways (outgoing octets are split, an oversized frame is refused),
  `idleTimeoutMs` bounds client silence, and `heartbeatMs` sends RFC 6455 pings.
  The handler ends the stream by returning (`1000`); a thrown error closes with
  the code its status maps to (`1008` for 4xx, `1001` for 503, `1011`
  otherwise); a message of the wrong kind closes with `1003`, a frame that is
  not a valid value with `1007`, an overflowing inbound queue with `1008`, and
  shutdown with `1001`.
- **The contract says so.** The operation publishes one `websocket` transport
  with `wire: "provider"` and encoding `binary` or `json`. Generated clients
  return a `ByteStream` or a `FrameStream` (see `@putnami/client`). The rules
  are `protocols/clientcontract` ADR 0010 and this package's ADR 0005.

## Best Practices

1. **Use `Stream()` for typed endpoints** — schema validation catches issues early
2. **Handle errors gracefully** — always handle connection errors
3. **Authenticate connections** — verify user identity before allowing connections
4. **Limit message size** — prevent abuse with large messages
5. **Clean up resources** — the `for await` loop ends automatically on disconnect
6. **Prefer SSE for server-push** — simpler, works through proxies, auto-reconnects
7. **Decide admission with `.secure()`, not inside the handler** — a handler throw is a terminal event on a 200 response; endpoint middleware is a status

## Next Steps

- Learn about [Endpoint Builder](endpoint-builder.md) for the full schema reference
- Explore [File-based Routing](file-based-routing.md) for route discovery
- Check [HTTP Server](http-server.md) for HTTP routing
- See [API Reference](api-reference.md) for complete API
