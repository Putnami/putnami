# Proto & gRPC

Auto-generate Protocol Buffer definitions from your API schemas and serve your API over gRPC — with zero extra handler code.

## Why

If you define typed API endpoints with Putnami's schema system, you already have a complete contract for your API. The proto/gRPC plugins leverage that contract to:

- **Generate `.proto` files** — share with teams using Go, Python, Java, Rust, or any gRPC-supported language
- **Serve gRPC on the same port** — serve the same handlers over both HTTP and gRPC on a single port (Connect protocol), Cloud Run compatible

No duplicate definitions. No manual proto authoring. Your HTTP API schema is the single source of truth.

## Setup

### Proto generation only

```typescript
import { application, api, http, proto } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(api())
  .use(proto({ packageName: 'myapp.v1', exposeRoute: true }));
```

The `.proto` file is generated at `.gen/schema/api.proto` and optionally served at `/_/api.proto`.

### Proto + gRPC server (same port)

```typescript
import { application, api, http, proto, grpc } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(api())
  .use(proto({ packageName: 'myapp.v1' }))
  .use(grpc());

await app.start();
// HTTP and gRPC both on :3000
// HTTP:  GET /users
// gRPC:  POST /myapp.v1.UsersService/ListUsers
```

No extra dependencies required. The `GrpcPlugin` uses the Connect protocol — supporting both JSON and binary protobuf over HTTP POST — so it runs on the same port as your HTTP API. Works on Cloud Run and any single-port environment.

## How schemas map to proto

Given this endpoint:

```typescript
// api/users/[id]/get.ts
export const GET = endpoint()
  .params({ id: Uuid })
  .query({ include: Optional(String) })
  .returns({ id: String, name: String, email: Email })
  .handle(async (ctx) => {
    return db.users.findById(ctx.params.id);
  });
```

The proto plugin generates:

```protobuf
message GetUsersByIdRequest {
  string id = 1; // UUID format
  optional string include = 2;
}

message GetUsersByIdResponse {
  string id = 1;
  string name = 2;
  string email = 3; // Email format
}

service UsersService {
  rpc GetUsersById(GetUsersByIdRequest) returns (GetUsersByIdResponse);
}
```

### Type mapping

| Schema | Proto | Notes |
|---|---|---|
| `String` | `string` | |
| `Number` | `double` | |
| `Boolean` | `bool` | |
| `Int` | `int32` | Varint encoding (more compact than double) |
| `Uuid` | `string` | `// UUID format` comment |
| `Email` | `string` | `// Email format` comment |
| `OneOf('a','b')` | `enum` | Proto3 enum with `UNSPECIFIED = 0` sentinel |
| `Optional(T)` | `optional T` | |
| `ArrayOf(T)` | `repeated T` | Packed encoding for scalars (proto3 default) |
| `MapOf(K, V)` | `map<K, V>` | Keys must be scalar (`String`, `Int`, `Boolean`) |
| Nested object | Sub-message | Auto-generated message |

The binary codec supports all proto3 scalar types including `sint32`/`sint64` (ZigZag encoding), `fixed32`/`sfixed32` (unsigned/signed 32-bit), and `fixed64`/`sfixed64` (proper 64-bit integer encoding). 64-bit integer types (`int64`, `uint64`, `sint64`, `fixed64`, `sfixed64`) use BigInt-based encoding internally — values fitting in `Number.MAX_SAFE_INTEGER` are returned as `number`, larger values as `bigint`. Repeated scalar fields use packed encoding by default per the proto3 spec.

### RPC naming conventions

| HTTP | Path | RPC |
|---|---|---|
| `GET` | `/users` | `ListUsers` |
| `GET` | `/users/[id]` | `GetUsersById` |
| `POST` | `/users` | `CreateUsers` |
| `PUT` | `/users/[id]` | `UpdateUsersById` |
| `DELETE` | `/users/[id]` | `DeleteUsersById` |

### Streaming

Stream endpoints map to gRPC streaming RPCs in the generated proto:

```typescript
// Server stream → gRPC server streaming (Connect + WebSocket + SSE)
endpoint().returns(Stream({ event: String })).handle(...)

// Client stream → gRPC client streaming (WebSocket only)
endpoint().body(Stream({ chunk: String })).handle(...)

// Bidirectional → gRPC bidirectional streaming (WebSocket only)
endpoint().body(Stream({ msg: String })).returns(Stream({ reply: String })).handle(...)
```

| Stream mode | gRPC (Connect, HTTP/1.1) | WebSocket | SSE |
|---|---|---|---|
| Server | **Supported** | Supported | Supported |
| Client | Not supported | **Supported** | — |
| Bidirectional | Not supported | **Supported** | — |

See [Streaming: bridging the gap](#streaming-bridging-the-gap) for why and how to use WebSocket for client/bidi streaming.

## Configuration reference

### `proto(config?)`

| Option | Type | Default | Description |
|---|---|---|---|
| `packageName` | `string` | Computed from `package.json` name | Proto package name (e.g. `@myorg/app` → `myorg.app.v1`) |
| `goPackage` | `string` | — | Go package option |
| `exposeRoute` | `boolean` | `false` | Serve proto file via HTTP |
| `publicRoute` | `string` | `/_/api.proto` | HTTP route for proto file |

When `packageName` is omitted, it is automatically derived from your `package.json` `name`:

- `@putnami/my-api` → `putnami.my.api.v1`
- `my-cool-app` → `my.cool.app.v1`
- `server` → `server.v1`
- *(no package.json)* → `api.v1`

Use `computeProtoPackageName(name)` to preview the computed name.

### `grpc(config?)`

| Option | Type | Default | Description |
|---|---|---|---|
| `acceptContentTypes` | `string[]` | `['application/json', 'application/proto', ...]` | Content types accepted for gRPC requests |
| `compression` | `boolean` | `true` | Enable gzip compression for gRPC responses |

Routes are registered as POST on the same HTTP server. No extra port configuration needed.

## Architecture

The gRPC plugin uses **direct handler dispatch** — route handlers are resolved from the HTTP router once at startup and invoked directly for each gRPC call. This avoids the overhead of an internal `server.fetch()` loopback.

**Zero-copy proto encoding** — binary protobuf responses bypass JSON serialization entirely. When an API handler returns a plain object, it is encoded directly to protobuf. When middleware wraps the result in a JSON `HttpResponse`, the raw data is preserved and read back without a `JSON.parse()` round-trip.

**Content-type negotiation** supports both encodings:

| Content type | Encoding | Use case |
|---|---|---|
| `application/json` | JSON | Connect protocol default |
| `application/proto` | Binary protobuf | Native gRPC clients |
| `application/grpc-web+json` | JSON | gRPC-Web clients (JSON) |
| `application/grpc-web+proto` | Binary protobuf | gRPC-Web clients (binary) |
| `application/connect+streaming+json` | JSON envelopes | Server streaming |
| `application/connect+streaming+proto` | Binary envelopes | Server streaming (binary) |

### Streaming: bridging the gap

**Server streaming** is bridged automatically — `ctx.send()` calls are wrapped in Connect envelope frames. One handler serves gRPC, WebSocket, and SSE simultaneously.

**Client streaming and bidirectional streaming cannot work over Connect (HTTP/1.1).** Here's why: HTTP/1.1 POST sends a single, complete request body. The server can stream a *response* back in chunks (server streaming), but the client cannot stream multiple messages *into* a single request. HTTP/2 solves this with bidirectional framing — but `Bun.serve` still operates on HTTP/1.1 as of Bun 1.4 (Bun does not yet serve HTTP/2). Calling a client/bidi RPC on the Connect route answers gRPC status 12 `UNIMPLEMENTED` naming the WebSocket transport.

**The solution: use WebSocket for client/bidi streaming.** The same `endpoint()` + `Stream()` definition works over WebSocket natively. No code changes needed — the framework routes to the right transport automatically.

#### Server streaming — define once, serve everywhere

```typescript
// src/api/events/ws.ts
export default endpoint()
  .query({ topic: String })
  .returns(Stream({ event: String, seq: Int }))
  .handle(async (ctx) => {
    for (let i = 0; i < 100; i++) {
      await new Promise((r) => setTimeout(r, 1000));
      ctx.send({ event: `update`, seq: i });
    }
  });
```

Three transports, one handler:

```bash
# gRPC (Connect streaming)
POST /myapp.v1.EventsService/ListEvents

# SSE (browser-native)
GET /events?topic=orders  (Accept: text/event-stream)

# WebSocket
ws://localhost:3000/events?topic=orders
```

#### Client streaming — WebSocket transport

```typescript
// src/api/upload/ws.ts
export default endpoint()
  .body(Stream({ chunk: String, index: Int }))
  .returns({ processed: Int })
  .handle(async (ctx) => {
    let count = 0;
    for await (const msg of ctx.messages()) {
      count++;
      // Each msg is validated against the body schema
    }
    return { processed: count };
  });
```

Client connects via WebSocket:

```typescript
const ws = new WebSocket('ws://localhost:3000/upload');
ws.onopen = () => {
  ws.send(JSON.stringify({ chunk: 'part-1', index: 0 }));
  ws.send(JSON.stringify({ chunk: 'part-2', index: 1 }));
  ws.close(); // Signal end of client stream
};
ws.onmessage = (e) => {
  const { processed } = JSON.parse(e.data); // Server's final response
};
```

The generated proto still declares `rpc SendUpload(stream SendUploadRequest) returns (SendUploadResponse)` — giving cross-language clients the correct type signature. The transport is WebSocket; the proto documents the contract.

#### Bidirectional streaming — WebSocket transport

```typescript
// src/api/chat/ws.ts
export default endpoint()
  .body(Stream({ type: String, content: String }))
  .returns(Stream({ event: String, content: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'connected', content: 'Welcome!' });
    for await (const msg of ctx.messages()) {
      ctx.send({ event: 'echo', content: msg.content });
    }
  });
```

```typescript
const ws = new WebSocket('ws://localhost:3000/chat');
ws.onmessage = (e) => console.log(JSON.parse(e.data));
ws.onopen = () => {
  ws.send(JSON.stringify({ type: 'msg', content: 'Hello' }));
};
// Logs: { event: 'connected', content: 'Welcome!' }
// Logs: { event: 'echo', content: 'Hello' }
```

#### How `ctx.messages()` works

`ctx.messages()` returns an `AsyncIterable` that bridges WebSocket's push-based messages to a pull-based `for await` loop:

1. WebSocket **opens** — handler starts, `MessageStream` is created
2. Each **message** arrives — validated against the body schema, then pushed to the stream
3. `for await (const msg of ctx.messages())` — wakes up for each message
4. WebSocket **closes** — stream signals done, the `for await` loop exits

Invalid messages are rejected by schema validation before reaching the handler.

#### Will HTTP/2 fix this?

Yes. When `Bun.serve` gains HTTP/2 — still pending as of Bun 1.4 — Connect can carry all streaming modes natively. Your existing handlers will work without changes — the framework will route client/bidi streams through Connect instead of WebSocket. The `endpoint()` + `Stream()` API is transport-agnostic by design. Separately, Bun 1.4 made the `node:http2` server compatible with `@grpc/grpc-js` and ConnectRPC, which opens the door to an optional native-gRPC server on a dedicated port (the Go framework's model) as a future feature.

### Health check

A standard `grpc.health.v1.Health/Check` endpoint is registered automatically, returning `SERVING` status. Load balancers and orchestrators (Kubernetes, Cloud Run) can use this for health probes.

### Compression

gzip compression is enabled by default. Responses are compressed when the client advertises gzip support via `grpc-accept-encoding` or `accept-encoding` headers. Compressed requests (`grpc-encoding: gzip`) are decompressed automatically. In streaming RPCs, each message frame is compressed individually. Set `compression: false` to disable.

### Deadline propagation

Clients can set deadlines via the `grpc-timeout` header (e.g. `5S` for 5 seconds, `500m` for 500ms). The deadline propagates through the request context — SQL queries are automatically cancelled, transactions roll back, and the response returns `DEADLINE_EXCEEDED` on timeout.

### Error mapping and details

HTTP exceptions map to standard gRPC status codes: 400→`INVALID_ARGUMENT`, 401→`UNAUTHENTICATED`, 403→`PERMISSION_DENIED`, 404→`NOT_FOUND`, 409→`ALREADY_EXISTS`, 429→`RESOURCE_EXHAUSTED`, 503→`UNAVAILABLE`. Streaming responses include `grpc-status` in end-of-stream trailers.

Error responses include a Connect `details` array with structured error information:

- **Validation errors** → `google.rpc.BadRequest` with `fieldViolations` (field path + description)
- **Typed error codes** → `google.rpc.ErrorInfo` with reason (gRPC code) and domain (`putnami`)

This makes errors machine-parseable by Connect clients — validation failures include the exact field paths and descriptions from schema validation.

### Unknown fields

The binary codec silently skips unknown fields during decoding (proto3 default). Clients with newer schemas can send messages containing fields the server doesn't recognize without breaking parsing. Unknown field preservation (for proxy passthrough) is not implemented — Putnami is a leaf application framework, not a proxy.

### Server reflection

The gRPC plugin registers the [gRPC Server Reflection](https://grpc.io/docs/guides/reflection/) service automatically. Tools like `grpcurl`, `buf curl`, Postman, and Kreya can discover services and methods without importing the `.proto` file.

```bash
# Discover available services
curl -X POST http://localhost:3000/grpc.reflection.v1.ServerReflection/ServerReflectionInfo \
  -H 'Content-Type: application/json' \
  -d '{"listServices": ""}'

# Get the proto descriptor for a service
curl -X POST http://localhost:3000/grpc.reflection.v1.ServerReflection/ServerReflectionInfo \
  -H 'Content-Type: application/json' \
  -d '{"fileContainingSymbol": "myapp.v1.UsersService"}'
```

Both `grpc.reflection.v1` and `grpc.reflection.v1alpha` endpoints are registered. The reflection service encodes a full `FileDescriptorProto` (messages, fields, enums, services, methods) as base64 in JSON responses. No configuration needed — it activates when `GrpcPlugin` is registered.

### `google.protobuf` well-known types

Generated `.proto` files are self-contained — no `import "google/protobuf/..."` required. Schema types map semantically to well-known types (e.g. `DateIso → string` maps to `Timestamp` in RFC 3339 format) without adding proto import dependencies.

## Testing gRPC services

The `createGrpcTestClient` helper provides a lightweight client for testing gRPC services in integration tests. It makes real HTTP calls — no mocking.

```typescript
import { createGrpcTestClient } from '@putnami/application';

// Create a test client
const client = createGrpcTestClient({
  baseUrl: `http://localhost:${server.port}`,
  packageName: 'myapp.v1',
});

// Unary call
const res = await client.unary('UsersService/ListUsers', { page: 1, limit: 10 });
expect(res.data.users).toHaveLength(10);
expect(res.status).toBe(200);

// Server streaming
const stream = await client.serverStream('EventsService/WatchEvents', {});
expect(stream.messages).toHaveLength(3);
expect(stream.trailers['grpc-status']).toBe(0);

// Service discovery via reflection
const services = await client.listServices();
expect(services).toContain('myapp.v1.UsersService');

// Health check
const status = await client.checkHealth();
expect(status).toBe(1); // SERVING

await client.close();
```

### `GrpcTestClient` API

| Method | Returns | Description |
|---|---|---|
| `unary(path, data?, options?)` | `{ data, status, headers }` | Unary RPC via Connect JSON |
| `serverStream(path, data?, options?)` | `{ messages, trailers }` | Collect all stream messages |
| `listServices()` | `string[]` | List services via reflection |
| `checkHealth()` | `number` | Health status (1 = SERVING) |
| `close()` | `void` | Clean up |

Short paths like `UsersService/ListUsers` are expanded with the `packageName`. Fully-qualified paths (containing a dot) are used as-is.

## Cross-language clients

Expose your proto at `/_/api.proto` and generate clients in any language:

```bash
# Go (binary protobuf by default)
protoc --go_out=. --go-grpc_out=. api.proto

# Python
python -m grpc_tools.protoc -I. --python_out=. --grpc_python_out=. api.proto

# Java
protoc --java_out=. --grpc-java_out=. api.proto
```

Generated clients can connect using binary protobuf natively — no special proxy or configuration needed.

See also: [API endpoints](/docs/frameworks/typescript/api), [WebSockets](/docs/frameworks/typescript/websockets), [Plugins](/docs/frameworks/typescript/plugins-and-lifecycle)
