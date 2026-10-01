# Proto & gRPC

Auto-generate Protocol Buffer definitions from your API schemas and serve your API over gRPC with zero extra code.

## Overview

Putnami can automatically produce a `.proto` file that mirrors your HTTP API and optionally start a gRPC server alongside your HTTP server. If you already define typed endpoints with schemas, you get gRPC for free.

Two plugins work together:

- **`ProtoPlugin`** — generates a `.proto` file at build time from your `DiscoveredRoute` schemas
- **`GrpcPlugin`** — registers gRPC routes on the **same HTTP server** using the Connect protocol, bridging calls to your existing route handlers

## Quick start

```typescript
import { application, api, http, proto, grpc } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(api())
  .use(proto({ packageName: 'myapp.v1' }))
  .use(grpc());

await app.start();
```

This will:

1. Scan your API routes and generate `.gen/schema/api.proto`
2. Start an HTTP server on port 3000
3. Register gRPC routes (Connect protocol) on the **same port**

Clients can call both:
- HTTP: `GET /users`
- gRPC/Connect: `POST /myapp.v1.UsersService/ListUsers`

## Proto generation

### How it works

The `ProtoPlugin` traverses your API routes (discovered via `ApiPlugin`) and converts the schema definitions into proto3 messages and services.

**Type mapping:**

| Schema type | Proto type | Notes |
|---|---|---|
| `String` | `string` | |
| `Number` | `double` | |
| `Boolean` | `bool` | |
| `Int` | `int32` | Varint encoding (more compact than double) |
| `Uuid` | `string` | Comment: `// UUID format` |
| `Email` | `string` | Comment: `// Email format` |
| `DateIso` | `string` | Comment: `// ISO 8601 date` |
| `OneOf('a','b')` | `enum` | Proto3 enum with `UNSPECIFIED = 0` sentinel |
| `Optional(T)` | `optional T` | Proto3 optional field |
| `ArrayOf(T)` | `repeated T` | Packed encoding for scalars (proto3 default) |
| `MapOf(K, V)` | `map<K, V>` | Proto3 map field (keys must be scalar) |
| Nested object | Sub-message | Auto-generated message type |

**Wire format support:**

The codec handles all proto3 scalar types with correct wire encoding:

| Wire type | Proto types | Encoding |
|---|---|---|
| 0 (varint) | `bool`, `int32`, `uint32`, `enum` | Standard varint (32-bit) |
| 0 (varint) | `int64`, `uint64` | Standard varint (64-bit, BigInt for values > 2^53) |
| 0 (varint) | `sint32` | ZigZag + varint (32-bit, efficient for negatives) |
| 0 (varint) | `sint64` | ZigZag + varint (64-bit, BigInt for values > 2^53) |
| 1 (64-bit) | `double` | Little-endian IEEE 754 double |
| 1 (64-bit) | `fixed64` | Little-endian unsigned 64-bit integer |
| 1 (64-bit) | `sfixed64` | Little-endian signed 64-bit integer |
| 2 (length-delimited) | `string`, `bytes`, nested messages, packed repeated | Length-prefixed |
| 5 (32-bit) | `float` | Little-endian 4 bytes (IEEE 754) |
| 5 (32-bit) | `fixed32` | Little-endian unsigned 4 bytes |
| 5 (32-bit) | `sfixed32` | Little-endian signed 4 bytes |

**64-bit integer handling:** The codec uses BigInt-based varint encoding/decoding for `int64`, `uint64`, and `sint64` types. Fixed-width 64-bit types (`fixed64`, `sfixed64`) use proper integer encoding via `DataView.setBigUint64`/`setBigInt64`. Values that fit in `Number.MAX_SAFE_INTEGER` are returned as `number`; larger values are returned as `bigint`. Both `number` and `bigint` inputs are accepted for encoding.

Repeated scalar fields use **packed encoding** by default (proto3 spec): a single field tag + varint length + concatenated values. Repeated strings and messages use individual field tags.

Map fields use the proto3 `map<K, V>` syntax. On the wire, maps are encoded as repeated entries where each entry is a sub-message with field 1 = key and field 2 = value.

**OneOf vs proto3 oneof:**

Putnami's `OneOf('a', 'b', 'c')` maps to a proto3 **enum** — a set of named integer constants representing string literal unions. This is different from proto3 `oneof`, which is a field union (mutually exclusive typed fields within a message).

| Putnami schema | Proto3 construct | Wire encoding |
|---|---|---|
| `OneOf('active', 'inactive')` | `enum` | Varint integer |
| — | `oneof` (field union) | Not projected |

A proto3 `oneof` is a field union with no schema-level discriminator, so nothing
in a Putnami schema declares one. Declare the variants as an object with a
discriminating member instead; that is what both emitters can carry losslessly
in every language.

**Service grouping:**

Routes are grouped into services by their first path segment:

- `/users`, `/users/[id]` → `UsersService`
- `/orders`, `/orders/[id]/items` → `OrdersService`

**RPC naming:**

| HTTP method | Path | RPC name |
|---|---|---|
| `GET` | `/users` | `ListUsers` |
| `GET` | `/users/[id]` | `GetUsersById` |
| `POST` | `/users` | `CreateUsers` |
| `PUT` | `/users/[id]` | `UpdateUsersById` |
| `DELETE` | `/users/[id]` | `DeleteUsersById` |
| `PATCH` | `/users/[id]` | `PatchUsersById` |

**Request messages** combine path params, query params, and body fields into a single message. **Response messages** mirror the `returns` schema.

### Streaming

Stream endpoints map directly to gRPC streaming RPCs in the generated proto:

| Stream mode | gRPC equivalent | Connect (HTTP/1.1) | WebSocket |
|---|---|---|---|
| `server` | `returns (stream Response)` | **Supported** — Connect envelope framing | Supported |
| `client` | `stream Request returns (Response)` | `UNIMPLEMENTED` (status 12) | **Supported** |
| `bidirectional` | `stream Request returns (stream Response)` | `UNIMPLEMENTED` (status 12) | **Supported** |

**Why client/bidirectional streaming doesn't work over Connect:** The Connect protocol runs on HTTP/1.1. Server streaming works because the server can send a chunked response with multiple envelope frames. But client streaming requires the client to send multiple messages in a single request body, which HTTP/1.1 POST does not support — it expects a single, complete request body. HTTP/2 framing would solve this, but `Bun.serve` — the framework's transport — still operates on HTTP/1.1 as of Bun 1.4 (Bun does not yet serve HTTP/2; Bun 1.4 fixed the `node:http2` *server*, which the framework does not use). Calling a client/bidi RPC on the Connect route answers a clean gRPC status 12 `UNIMPLEMENTED` pointing at the WebSocket transport.

**The WebSocket bridge:** All three streaming modes work over WebSocket using the same `endpoint()` + `Stream()` definition. The framework automatically serves the endpoint over the right transport:

- **gRPC clients** connect via `POST /{package}.{Service}/{Method}` (server streaming only)
- **WebSocket clients** connect via `ws://host/{path}` (all streaming modes)
- **SSE clients** connect via `GET /{path}` with `Accept: text/event-stream` (server streaming only)

See [Streaming transport guide](#streaming-transport-guide) below for concrete examples.

### Default package name

When `packageName` is omitted, it is computed from your `package.json` `name` using proto naming conventions:

| `package.json` name | Computed proto package |
|---|---|
| `@putnami/my-api` | `putnami.my.api.v1` |
| `my-cool-app` | `my.cool.app.v1` |
| `server` | `server.v1` |
| *(none)* | `api.v1` |

You can also call `computeProtoPackageName(name)` directly to preview the result.

### Configuration

```typescript
proto({
  // Proto package name (default: computed from package.json name)
  // e.g. @myorg/my-api → myorg.my.api.v1
  packageName: 'myapp.v1',

  // Go package option for generated Go clients
  goPackage: 'github.com/myorg/myapp/pb',

  // Expose .proto file via HTTP route (default: false)
  exposeRoute: true,

  // Custom route path (default: /_/api.proto)
  publicRoute: '/schema/api.proto',
});
```

### Generated output

The proto file is written to `.gen/schema/api.proto` during the build phase. Example output:

```protobuf
syntax = "proto3";

package myapp.v1;

option go_package = "github.com/myorg/myapp/pb";

message ListUsersRequest {
  optional double page = 1;
  optional double limit = 2;
}

message ListUsersResponse {
  string id = 1;
  string name = 2;
}

message CreateUsersRequest {
  string name = 1;
  string email = 2; // Email format
}

message CreateUsersResponse {
  string id = 1;
  string name = 2;
  string email = 3;
}

message GetUsersByIdRequest {
  string id = 1; // UUID format
}

message GetUsersByIdResponse {
  string id = 1;
  string name = 2;
  string email = 3;
}

service UsersService {
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse);
  rpc CreateUsers(CreateUsersRequest) returns (CreateUsersResponse);
  rpc GetUsersById(GetUsersByIdRequest) returns (GetUsersByIdResponse);
}
```

## gRPC server (Connect protocol)

The `GrpcPlugin` uses the **Connect protocol** — running on the same HTTP port, supporting both JSON and binary protobuf. No extra dependencies, no extra ports. Works on Cloud Run and any single-port environment.

### How it works

The `GrpcPlugin`:

1. Reads the proto definition from `ProtoPlugin` to know which services/RPCs exist
2. Resolves each API route handler directly from the HTTP router during warmup
3. For each RPC, registers a POST route at `/{package}.{Service}/{Method}` on the existing HTTP server
4. When a gRPC/Connect request arrives, the handler:
   - Detects the encoding from `Content-Type` (JSON or binary protobuf)
   - Parses the request body using the appropriate codec
   - Splits fields into params/query/body using the route's schema metadata
   - Dispatches directly to the resolved API handler (no `server.fetch()` loopback)
   - Encodes the response based on the client's `Accept` header
5. All endpoint-level middleware, validation, and auth from the original route still apply

**Direct dispatch** means handler references are resolved once at startup and invoked via `runInContext()`, avoiding `server.fetch()` loopback overhead. gRPC routes are registered on the HTTP server like any other route, so **global HTTP middleware (logger, telemetry, trace) runs on every gRPC request**. The trace/correlation ID is propagated from the outer request context to the inner handler context, ensuring logs emitted inside handlers include the correct trace ID.

**Zero-copy proto encoding** — when an API handler returns a plain object, the gRPC plugin encodes it to binary protobuf directly without intermediate JSON serialization. Even when middleware wraps the result in a JSON response, `HttpResponse` preserves the raw data so the gRPC plugin can read it back without a JSON parse round-trip. This means binary protobuf responses avoid the `JSON.stringify()` → `JSON.parse()` overhead entirely.

**Performance optimizations**:
- **Accept header negotiation** — multi-value headers (`application/proto, application/json`), quality factors (`application/proto;q=0.9`), and wildcards (`application/*`) are handled correctly, so binary proto encoding is selected whenever the client supports it
- **No JSON round-trip in dispatch** — the gRPC handler passes the parsed request body directly to the API handler via context, avoiding `JSON.stringify` → `JSON.parse` on every request
- **Native gzip compression** — uses `Bun.gzipSync`/`Bun.gunzipSync` for both unary response compression and per-message streaming compression, avoiding Web Streams API overhead
- **Pooled encoding buffer** — the proto encoder uses a pre-allocated, geometrically-growing `WriteBuffer` instead of allocating a `Uint8Array` per field, reducing GC pressure under high QPS

### Content types

The plugin negotiates encoding based on `Content-Type` and `Accept` headers:

| Content type | Encoding | Use case |
|---|---|---|
| `application/json` | JSON | Connect unary (default) |
| `application/proto` | Binary protobuf | Connect unary, and native gRPC clients (protoc-generated) |
| `application/connect+json` | JSON envelopes | Connect **streaming** |
| `application/connect+proto` | Binary envelopes | Connect **streaming**, binary |
| `application/grpc-web+json` | JSON | gRPC-Web clients (JSON) |
| `application/grpc-web+proto` | Binary protobuf | gRPC-Web clients (binary) |

The `application/connect+…` types belong to streaming calls only; a unary call
uses the bare codec type. Earlier releases spelled the streaming types
`application/connect+streaming+json`, which no conforming Connect client
negotiates. See [Connect protocol conformance](#connect-protocol-conformance).

Clients generated from the `.proto` via `protoc` (Go, Python, Java, etc.) can use binary protobuf natively — no proxy or special configuration required.

Content-Type headers are parsed robustly — parameters like `charset=utf-8` and Accept lists with quality factors are handled correctly. The media type is extracted before comparison, so `application/proto; charset=utf-8` is recognized as binary protobuf.

### gRPC-Web binary

gRPC-Web binary (`application/grpc-web+proto`) uses the same envelope framing as the Connect protocol. Request bodies are envelope-wrapped (5-byte header + payload), and responses include a data frame followed by a trailer frame (flag `0x80`) containing gRPC status headers.

This allows browser-based gRPC-Web clients (e.g. `@connectrpc/connect-web`) to communicate using the efficient binary protobuf format.

### Streaming transport guide

This section explains how each streaming mode maps to transports, why client/bidirectional streaming doesn't work over gRPC/Connect, and how to use WebSocket as the bridge.

#### Server streaming — works on all transports

Server streaming works over gRPC (Connect), WebSocket, and SSE. Define once, serve everywhere:

```typescript
// src/api/events/ws.ts
import { endpoint, Stream, String, Int } from '@putnami/application';

export default endpoint()
  .query({ topic: String })
  .returns(Stream({ event: String, seq: Int }))
  .handle(async (ctx) => {
    const topic = ctx.queryParams().topic;
    for (let i = 0; i < 100; i++) {
      await new Promise((r) => setTimeout(r, 1000));
      ctx.send({ event: `${topic}:update`, seq: i });
    }
  });
```

This single handler serves **three transports** simultaneously:

```bash
# 1. gRPC/Connect — binary or JSON, envelope-framed
curl -X POST http://localhost:3000/myapp.v1.EventsService/ListEvents \
  -H 'Content-Type: application/connect+json' \
  -H 'Accept: application/connect+json' \
  --data-binary @<(printf '\x00\x00\x00\x00\x12{"topic":"orders"}')

# 2. SSE — text/event-stream (browser-native)
curl http://localhost:3000/events?topic=orders \
  -H 'Accept: text/event-stream'

# 3. WebSocket
# ws://localhost:3000/events?topic=orders
```

The gRPC plugin wraps each `ctx.send()` call in a Connect envelope frame:

```
[flags: 1 byte][length: 4 bytes big-endian][payload]
```

- `0x00` = uncompressed message
- `0x01` = gzip-compressed message (only when the response declares `Connect-Content-Encoding: gzip`)
- `0x02` = the `EndStreamResponse` that ends the stream — exactly one, always last

The six most significant bits are reserved: a conforming peer never sets them,
and this provider refuses a request frame that does.

#### Client streaming — WebSocket only

Client streaming requires the client to send multiple messages in a single connection. HTTP/1.1 POST expects a single request body, so **Connect cannot carry client streams**. Use WebSocket directly:

```typescript
// src/api/upload/ws.ts
import { endpoint, Stream, String, Int } from '@putnami/application';

export default endpoint()
  .body(Stream({ chunk: String, index: Int }))
  .returns({ processed: Int, bytes: Int })
  .handle(async (ctx) => {
    let processed = 0;
    let bytes = 0;
    for await (const msg of ctx.messages()) {
      processed++;
      bytes += msg.chunk.length;
    }
    // Final response sent when client disconnects
    return { processed, bytes };
  });
```

Client code (browser or Node/Bun):

```typescript
const ws = new WebSocket('ws://localhost:3000/upload');

ws.onopen = () => {
  // Send multiple messages (client streaming)
  ws.send(JSON.stringify({ chunk: 'data-part-1', index: 0 }));
  ws.send(JSON.stringify({ chunk: 'data-part-2', index: 1 }));
  ws.send(JSON.stringify({ chunk: 'data-part-3', index: 2 }));
  ws.close(); // Signal end of stream
};

ws.onmessage = (e) => {
  // Server's final response
  const { processed, bytes } = JSON.parse(e.data);
  console.log(`Processed ${processed} chunks, ${bytes} bytes`);
};
```

**What happens on the gRPC side:** The proto file still declares `rpc SendUpload(stream ...)` so external gRPC clients generated from your proto know the method exists and its types. However, calling it over Connect HTTP/1.1 answers gRPC status 12 `UNIMPLEMENTED`. The proto serves as documentation of the interface — the actual transport is WebSocket.

#### Bidirectional streaming — WebSocket only

Bidirectional streaming is the same pattern but with `Stream()` on both body and returns:

```typescript
// src/api/chat/ws.ts
import { endpoint, Stream, String } from '@putnami/application';

export default endpoint()
  .body(Stream({ type: String, content: String }))
  .returns(Stream({ event: String, content: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'connected', content: 'Welcome!' });

    for await (const msg of ctx.messages()) {
      if (msg.type === 'ping') {
        ctx.send({ event: 'pong', content: '' });
      } else {
        ctx.send({ event: 'echo', content: msg.content });
      }
    }
    // Stream ends when client disconnects
  });
```

Client code:

```typescript
const ws = new WebSocket('ws://localhost:3000/chat');

ws.onopen = () => {
  ws.send(JSON.stringify({ type: 'ping', content: '' }));
  ws.send(JSON.stringify({ type: 'message', content: 'Hello' }));
};

ws.onmessage = (e) => {
  const { event, content } = JSON.parse(e.data);
  console.log(`[${event}] ${content}`);
  // [connected] Welcome!
  // [pong]
  // [echo] Hello
};
```

#### How `ctx.messages()` works under the hood

The framework bridges push-based WebSocket messages to a pull-based `AsyncIterable`:

1. On **WebSocket open** — the handler starts (non-blocking), a `MessageStream` is created
2. On **each WebSocket message** — the message is validated against the body schema, then pushed to the `MessageStream`
3. The handler's `for await (const msg of ctx.messages())` loop wakes up for each pushed message
4. On **WebSocket close** — the `MessageStream` signals `done`, the `for await` loop exits, and the handler completes

Schema validation happens on every incoming message. Invalid messages are rejected before reaching the handler.

#### Transport decision matrix

| Question | Answer |
|---|---|
| **Server pushes data, client just listens?** | Use `returns(Stream(...))`. Works on gRPC, SSE, and WebSocket. |
| **Client sends a stream, server responds once?** | Use `body(Stream(...))`. WebSocket only. |
| **Both sides send messages?** | Use `body(Stream(...))` + `returns(Stream(...))`. WebSocket only. |
| **Need cross-language gRPC clients?** | Server streaming works. For client/bidi, share the `.proto` for type documentation and use WebSocket for the actual transport. |
| **Will HTTP/2 fix this?** | Yes. When `Bun.serve` gains HTTP/2 (still pending as of Bun 1.4), the Connect protocol can carry all streaming modes natively. The same handlers will work without changes. Bun 1.4 did make the `node:http2` server solid enough for `@grpc/grpc-js`/ConnectRPC, so a native-gRPC sidecar server on a dedicated port is now feasible as a separate feature. |

### Health check

The `GrpcPlugin` automatically registers a standard gRPC health check endpoint at:

```
POST /grpc.health.v1.Health/Check
```

This follows the [gRPC Health Checking Protocol](https://grpc.io/docs/guides/health-checking/). The endpoint always returns `SERVING` status. Load balancers and orchestrators (Kubernetes, Cloud Run) can use this for health probes.

### Deadline propagation

The plugin reads the deadline from either protocol on the route. Connect names it
`Connect-Timeout-Ms` and gives it whole milliseconds; gRPC and gRPC-Web name it
`grpc-timeout` and give it a unit suffix.

```
Connect-Timeout-Ms: 5000   # 5 seconds, the Connect spelling
grpc-timeout: 5S    # 5 seconds
grpc-timeout: 500m  # 500 milliseconds
grpc-timeout: 2M    # 2 minutes
```

The plugin creates an `AbortController` with the specified timeout and propagates the signal through the request context. This means:

- SQL queries using `abortableQuery()` are automatically cancelled on deadline
- Transaction middleware cleans up on abort
- The handler receives `DEADLINE_EXCEEDED` (gRPC code 4) on timeout

Supported time units: `H` (hours), `M` (minutes), `S` (seconds), `m` (milliseconds), `u` (microseconds), `n` (nanoseconds).

### Observability

gRPC routes are served on the same HTTP server, so all global middleware registered via `httpPlugin.use()` applies to gRPC requests:

- **Logger** — gRPC requests are logged with method, route, status, and duration (route is the gRPC path, e.g. `/myapp.v1.UsersService/ListUsers`)
- **Telemetry** — per-route counters and histograms are collected for gRPC endpoints (metric key: `http.POST./{package}.{Service}/{Method}.{status}`)
- **Trace** — `X-Cloud-Trace-Context` and `X-Correlation-ID` headers are extracted and propagated to the inner handler context via `ctx.traceId`

The trace ID propagation is automatic. When a gRPC request carries a trace header, the middleware sets `ctx.traceId` on the outer context. The gRPC handler then copies `traceId`, `logContext`, and `user` to the inner API handler context. This means `useLogger()` calls inside route handlers will include the trace ID in log output.

```typescript
// Trace headers are forwarded through the gRPC dispatch:
// Client → gRPC request (X-Correlation-ID: abc-123)
//   → TraceMiddleware sets ctx.traceId = "abc-123"
//   → gRPC handler propagates traceId to inner context
//   → Handler's useLogger() includes traceId in logs
```

Endpoint-level middleware (`.secure()`, `.cors()`, `.rateLimit()`, validation) also runs on gRPC requests since the resolved handler includes these wrappers.

### Error handling

An `HttpException` is named with one of the sixteen Connect codes, and shipped
under the HTTP status the specification pairs with that code. The framework's own
status — finer-grained than sixteen categories — travels in the
`putnami.client.v1.FrameworkError` detail below.

| Raised status | Connect code | `google.rpc.Code` | Status shipped |
|---|---|---|---|
| 400, 422 | `invalid_argument` | 3 | 400 |
| 401 | `unauthenticated` | 16 | 401 |
| 403 | `permission_denied` | 7 | 403 |
| 404 | `not_found` | 5 | 404 |
| 405, 501 | `unimplemented` | 12 | 501 |
| 408, 504 | `deadline_exceeded` | 4 | 504 |
| 409 | `already_exists` | 6 | 409 |
| 412 | `failed_precondition` | 9 | 400 |
| 413, 429 | `resource_exhausted` | 8 | 429 |
| 416 | `out_of_range` | 11 | 400 |
| 499 | `canceled` | 1 | 499 |
| 503 | `unavailable` | 14 | 503 |
| any other 5xx | `internal` | 13 | 500 |

The code is lower case: `NOT_FOUND` is the gRPC spelling, and the Connect
protocol defines no such code. The one spelling the two vocabularies disagree on
is cancellation — gRPC writes `CANCELLED`, Connect writes `canceled`.

A streaming response ends with an `EndStreamResponse`: `{}` on success, and
`{"error": {"code": …, "message": …, "details": […]}}` on failure.

#### Error details (Connect protocol)

An error carries a `details` array following the [Connect error detail
format](https://connectrpc.com/docs/protocol/#error-and-endstreamresponse):
each detail is `{type, value}` where `value` is unpadded base64 of the binary
protobuf, plus an optional `debug` rendering a client must not depend on.

**Validation errors** — when schema validation fails, the violations travel as a
`google.rpc.BadRequest`, which any Connect client already knows how to read:

```json
{
  "code": "invalid_argument",
  "message": "body.name is required; body.email must be a valid email address",
  "details": [
    {
      "type": "google.rpc.BadRequest",
      "value": "CiIKCWJvZHkubmFtZRIVYm9keS5uYW1lIGlzIHJlcXVpcmVk",
      "debug": { "fieldViolations": [{ "field": "body.name", "description": "body.name is required" }] }
    },
    { "type": "putnami.client.v1.FrameworkError", "value": "…", "debug": { "code": "http.bad_request", "httpStatus": 400 } }
  ]
}
```

**The first-party envelope** — Connect's sixteen codes are categories, and a
Putnami operation declares a finer, stable code (`not_found`,
`http.bad_request`) plus a typed `details` member. Both ride in a
`putnami.client.v1.FrameworkError` detail:

```proto
message FrameworkError {
  string code = 1;         // the stable framework code
  int32 http_status = 2;   // the status the same error carries over REST
  string details_json = 3; // the declared `details` member, canonical JSON
}
```

A generated Putnami client reads that detail and rebuilds exactly the
`ClientFrameworkError` a REST call would have raised — same code, same status,
same declared details. A third-party Connect client ignores it and still gets a
well-formed error.

| Detail type | When | Content |
|---|---|---|
| `google.rpc.BadRequest` | Schema validation fails | `fieldViolations[]` with `field` and `description` |
| `putnami.client.v1.FrameworkError` | Every `HttpException` | the stable code, the REST status, the declared details |

#### Connect protocol conformance

The provider and the generated-client runtime read the protocol from one module,
`src/grpc/connect-protocol.ts`. That module is checked against
`test/grpc/connect-conformance/corpus.json`, transcribed from the [published
specification](https://connectrpc.com/docs/protocol/) — not against the other
half of the framework, which would agree with itself whatever it wrote.

Three tables the specification separates, and which are not each other's inverse:

| Table | Used by | Example |
|---|---|---|
| code → HTTP status | the server, choosing the status to ship | `not_found` → 404 |
| HTTP status → code | a client whose response carried no readable code | a bare 404 → `unimplemented` |
| code → `google.rpc.Code` | a caller branching on a numeric status | `not_found` → 5 |

Reading the second table as the inverse of the first reports a missing route as a
missing resource. The provider therefore names its own code from the status it
raised, and ships the status the specification pairs with that code.

### Unknown field handling

The binary codec **silently skips** unknown fields during decoding. When a client sends a message with fields the server doesn't recognize (e.g., a newer client with an updated schema), those fields are consumed correctly based on their wire type and discarded. Remaining known fields continue to decode normally.

This is the proto3 default behavior. The codec correctly skips unknown fields of all standard wire types:

| Wire type | Skip strategy |
|---|---|
| 0 (varint) | Consume varint bytes |
| 1 (64-bit) | Skip 8 bytes |
| 2 (length-delimited) | Read length prefix, skip that many bytes |
| 5 (32-bit) | Skip 4 bytes |

Wire types 3/4 (deprecated proto2 group delimiters) and 6/7 (reserved) cannot be safely skipped without additional context. These are never used in proto3.

**Not implemented**: Unknown field preservation (storing raw bytes for re-serialization). This would be needed for proxy/middleware scenarios where a service forwards messages without full schema knowledge. Since Putnami is a leaf application framework (not a proxy), discarding is the pragmatic default.

### `google.protobuf` well-known types

The proto generation does **not** import `google/protobuf/*.proto` files. Generated `.proto` files are self-contained — no external proto dependencies required. This keeps the `protoc` workflow simple for consumers.

Semantic mappings from Putnami schema types to `google.protobuf` equivalents:

| Schema type | Proto field | `google.protobuf` equivalent | Notes |
|---|---|---|---|
| `DateIso` | `string` | `google.protobuf.Timestamp` | RFC 3339 string; comment: `// ISO 8601 date` |
| void return | empty message | `google.protobuf.Empty` | Already emitted as `message FooResponse {}` |
| `MapOf(K, V)` | `map<K, V>` | Native proto3 map | No wrapper needed |

**Not imported** (by design):

| Type | Reason |
|---|---|
| `google.protobuf.Any` | Used only in error details (JSON `debug` field, not binary) |
| `google.protobuf.Struct` / `Value` | Defeats typed proto benefits; no size reduction over JSON |
| `google.protobuf.FieldMask` | Could be useful for PATCH; deferred pending demand |
| `google.protobuf.Timestamp` / `Duration` | `DateIso → string` is simpler; adding proto imports adds `protoc` complexity |
| Wrapper types (`StringValue`, etc.) | Obsolete since proto3 supports `optional` |

### Compression

The gRPC plugin supports **gzip compression** for both requests and responses. Compression is enabled by default and negotiated via standard gRPC headers.

**Response compression** — when the client advertises gzip support via `grpc-accept-encoding: gzip` (gRPC standard) or `accept-encoding: gzip` (HTTP standard), the server compresses the response body. Small payloads (< 64 bytes) are skipped since gzip overhead exceeds savings.

**Request decompression** — when the client sends a compressed request body with `grpc-encoding: gzip` or `content-encoding: gzip`, the server automatically decompresses before decoding.

**Streaming compression** — in server-streaming RPCs, each message frame is compressed individually using the Connect envelope compression flag (`0x01`). The `Connect-Content-Encoding: gzip` header advertises per-message compression.

Compression is not applied to gRPC-Web responses (gRPC-Web proxies handle their own compression).

### Configuration

```typescript
grpc({
  // Content types to accept (default includes Connect + gRPC-Web + binary protobuf)
  acceptContentTypes: [
    'application/json',
    'application/proto',
    'application/connect+json',
    'application/grpc-web+json',
    'application/grpc-web+proto',
    'application/connect+json',
    'application/connect+proto',
  ],

  // Enable gzip compression (default: true)
  compression: true,
});
```

### Plugin order

The `GrpcPlugin` must be registered after both `ApiPlugin` and `ProtoPlugin`:

```typescript
const app = application()
  .use(http({ port: 3000 }))  // HTTP server
  .use(api())                  // API route discovery
  .use(proto())                // Proto generation (requires ApiPlugin)
  .use(grpc());                // gRPC routes on same port (requires ProtoPlugin + ApiPlugin)
```

### Calling gRPC endpoints

gRPC routes use the standard gRPC path convention:

```bash
# Connect JSON (human-readable)
curl -X POST http://localhost:3000/myapp.v1.UsersService/ListUsers \
  -H 'Content-Type: application/json' \
  -d '{"page": 1, "limit": 10}'

# Binary protobuf (for native gRPC clients)
curl -X POST http://localhost:3000/myapp.v1.UsersService/GetUsersById \
  -H 'Content-Type: application/proto' \
  -H 'Accept: application/proto' \
  --data-binary '<protobuf-encoded-body>'
```

Client headers are forwarded automatically from the gRPC request to the API handler — including `Authorization`, `Cookie`, tracing headers (`traceparent`, etc.), tenant identifiers, and any custom headers. Only hop-by-hop headers (`Connection`, `Transfer-Encoding`, etc.), content negotiation headers (`Content-Type`, `Accept`), and gRPC-specific headers (`grpc-timeout`, `grpc-encoding`, etc.) are excluded.

### Server reflection

The `GrpcPlugin` automatically registers the [gRPC Server Reflection](https://grpc.io/docs/guides/reflection/) service at startup. This enables tools like `grpcurl`, `buf curl`, Postman, and Kreya to discover services and methods without importing the `.proto` file manually.

**Endpoints:**

```
POST /grpc.reflection.v1.ServerReflection/ServerReflectionInfo
POST /grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo
```

Both v1 and v1alpha paths are registered for maximum tool compatibility.

**Supported operations:**

| Request type | Description |
|---|---|
| `list_services` | Returns all fully-qualified service names |
| `file_containing_symbol` | Returns the `FileDescriptorProto` for the file containing a symbol |
| `file_by_filename` | Returns the `FileDescriptorProto` for `api.proto` |

**Example — list services:**

```bash
curl -X POST http://localhost:3000/grpc.reflection.v1.ServerReflection/ServerReflectionInfo \
  -H 'Content-Type: application/json' \
  -d '{"listServices": ""}'
```

Response:

```json
{
  "listServicesResponse": {
    "service": [
      { "name": "myapp.v1.UsersService" },
      { "name": "myapp.v1.OrdersService" }
    ]
  }
}
```

**FileDescriptorProto encoding** — the reflection service encodes the full proto schema as a binary `google.protobuf.FileDescriptorProto`, base64-encoded in the JSON response. This includes all messages, fields, enums, services, and method signatures. Tools decode this to build request/response forms.

**No extra configuration** — reflection activates automatically when `GrpcPlugin` is registered. There is no opt-out flag; the cost is two POST routes and a one-time binary encoding at startup.

## Test helpers

The `createGrpcTestClient` function provides a lightweight client for testing gRPC services in integration tests. It makes real HTTP calls via the Connect protocol — no mocking required.

```typescript
import { createGrpcTestClient } from '@putnami/application';

const client = createGrpcTestClient({
  baseUrl: `http://localhost:${server.port}`,
  packageName: 'myapp.v1',
});
```

### Unary calls

```typescript
// Short path (expanded with packageName)
const res = await client.unary('UsersService/ListUsers', { page: 1, limit: 10 });
expect(res.data.users).toHaveLength(10);
expect(res.status).toBe(200);

// Fully-qualified path
const res2 = await client.unary('myapp.v1.UsersService/GetUsersById', { id: '123' });

// With custom headers and timeout
const res3 = await client.unary('UsersService/ListUsers', {}, {
  headers: { Authorization: 'Bearer token' },
  timeout: 5000,
});
```

### Server streaming

```typescript
const result = await client.serverStream('EventsService/WatchEvents', {});
expect(result.messages).toHaveLength(3);
expect(result.trailers['grpc-status']).toBe(0);
```

### Service discovery

```typescript
// List services via reflection
const services = await client.listServices();
expect(services).toContain('myapp.v1.UsersService');

// Health check
const status = await client.checkHealth();
expect(status).toBe(1); // SERVING
```

### `GrpcTestClient` interface

| Method | Description |
|---|---|
| `unary(path, data?, options?)` | Make a unary RPC call. Returns `{ data, status, headers }` |
| `serverStream(path, data?, options?)` | Collect all stream messages. Returns `{ messages, trailers }` |
| `listServices()` | List services via reflection. Returns service names |
| `checkHealth()` | Check health status. Returns serving status number |
| `close()` | Clean up (no-op for HTTP, included for interface consistency) |

### Path resolution

The client resolves short paths using the `packageName` option:

| Input path | Resolved URL |
|---|---|
| `UsersService/ListUsers` | `/{packageName}.UsersService/ListUsers` |
| `myapp.v1.UsersService/ListUsers` | `/myapp.v1.UsersService/ListUsers` |

Fully-qualified paths (containing a dot) are used as-is.

## Proto-only usage

You can use `ProtoPlugin` without `GrpcPlugin` to generate `.proto` files for clients in other languages:

```typescript
const app = application()
  .use(http({ port: 3000 }))
  .use(api())
  .use(proto({ packageName: 'myapp.v1', exposeRoute: true }));
```

This generates the `.proto` file at build time and optionally serves it at `/_/api.proto`. Clients in Go, Python, Java, etc. can download the proto and generate their own stubs — using binary protobuf by default.
