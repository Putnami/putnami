# Endpoint Builder

Type-safe, validated API route handlers with zero boilerplate.

## Overview

The `endpoint()` function provides a fluent builder for defining API route handlers with built-in schema validation, type inference, and security. Inputs are validated before your handler runs — invalid requests are rejected automatically with structured error responses.

## Quick Start

### Simple Mode

For handlers that don't need validation, pass a function directly:

```typescript
// src/api/health/get.ts
import { endpoint } from '@putnami/application';

export default endpoint((ctx) => {
  return { status: 'ok' };
});
```

### Builder Mode

Chain `.params()`, `.query()`, `.headers()`, `.body()`, `.returns()` to declare and validate inputs. Finalise with `.handle()`. Use `Stream()` in `.body()` or `.returns()` for WebSocket/SSE streaming. Autocomplete guides you at every step:

```typescript
// src/api/users/[id]/get.ts
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    // ctx.params.id — typed as string, validated as UUID
    return { id: ctx.params.id };
  });
```

Invalid requests are rejected automatically:

```
GET /users/not-a-uuid
→ 400 { "message": "params.id must be a valid UUID" }
```

## Schema Primitives

Use JavaScript constructors as schema types — no extra imports needed for basic types:

| Schema | TypeScript Type | Description |
|--------|----------------|-------------|
| `String` | `string` | String value |
| `Number` | `number` | Numeric value |
| `Boolean` | `boolean` | Boolean value |

### Constrained Types

Import these for stricter validation:

```typescript
import { Uuid, Email, Int, Url, DateIso, Min, Max, MinLength, MaxLength, Pattern, OneOf, Default, Desc } from '@putnami/application';
```

| Schema | TypeScript Type | Validation |
|--------|----------------|------------|
| `Uuid` | `string` | UUID v4 format |
| `Email` | `string` | Valid email address |
| `Int` | `number` | Integer (no decimals) |
| `Url` | `string` | Valid URL (parsed by `new URL()`) |
| `DateIso` | `string` | ISO 8601 date (e.g. `2024-01-15T10:30:00Z`) |
| `Min(n)` | `number` | Value >= n |
| `Max(n)` | `number` | Value <= n |
| `MinLength(n)` | `string` | String length >= n |
| `MaxLength(n)` | `string` | String length <= n |
| `Pattern(regex)` | `string` | Must match regular expression |

```typescript
// Examples
endpoint().body({
  age: Min(18),
  score: Max(100),
  username: MinLength(3),
  bio: MaxLength(500),
  slug: Pattern(/^[a-z0-9-]+$/),
  website: Url,
  publishedAt: DateIso,
});
```

### Combinators

```typescript
import { Optional, ArrayOf, Default, OneOf } from '@putnami/application';
```

| Combinator | Example | TypeScript Type |
|------------|---------|----------------|
| `Optional(T)` | `Optional(String)` | `string \| undefined` |
| `ArrayOf(T)` | `ArrayOf(String)` | `string[]` |
| `Default(T, value)` | `Default(Number, 0)` | `number` (optional in input, always present in output) |
| `OneOf(...values)` | `OneOf('a', 'b')` | `'a' \| 'b'` |
| `Desc(text, T)` | `Desc('User name', String)` | `string` (adds OpenAPI description) |

Combinators compose with constrained types:

```typescript
Optional(Uuid)       // string | undefined, validated as UUID when present
Optional(ArrayOf(Email))  // string[] | undefined
ArrayOf(Int)         // number[], each validated as integer
Default(OneOf('lax', 'strict', 'none'), 'lax')  // 'lax' | 'strict' | 'none', defaults to 'lax'
```

### Nested Object Schemas

Schema properties can be plain objects to validate nested structures:

```typescript
endpoint()
  .body({
    name: String,
    address: {
      street: String,
      city: String,
      zip: Optional(String),
    },
  })
  .handle(async (ctx) => {
    const body = await ctx.body();
    body.address.city; // string — fully typed and validated
  });
```

Nesting can go arbitrarily deep:

```typescript
endpoint().body({
  user: {
    name: String,
    location: {
      city: String,
      country: String,
    },
  },
});
```

### Sharing Schemas

Extract shared schemas into modules and reuse them across routes:

```typescript
import { ArrayOf, MinLength, Optional, schema } from '@putnami/application';

export const NoteBodySchema = schema({
  title: MinLength(1),
  content: MinLength(2),
  tags: Optional(ArrayOf(String)),
});
```

You can derive the TypeScript type from a schema:

```typescript
import type { InferSchema } from '@putnami/application';

export type NoteBody = InferSchema<typeof NoteBodySchema>;
```

## Builder Methods

### `.params(schema)`

Validate path parameters. Values are coerced from strings automatically (e.g., `"42"` → `42` for `Number`).

```typescript
// src/api/users/[id]/get.ts
export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    ctx.params.id; // string, validated as UUID
  });
```

### `.query(schema)`

Validate query string parameters. Values are coerced from strings automatically.

```typescript
// src/api/users/get.ts
export default endpoint()
  .query({ page: Number, limit: Number, search: Optional(String) })
  .handle((ctx) => {
    const query = ctx.queryParams();
    query.page;   // number
    query.limit;  // number
    query.search; // string | undefined
  });
```

### `.headers(schema)`

Validate request headers. Values are coerced from strings automatically (like `.params()` and `.query()`). The validated, typed headers are exposed on `ctx.headerParams()` — the raw `ctx.headers` (a `Headers` object) is left untouched, so content negotiation and middleware keep working normally.

```typescript
// src/api/traced/get.ts
export default endpoint()
  .headers({ 'x-request-id': Uuid, 'x-count': Optional(Number) })
  .handle((ctx) => {
    const headers = ctx.headerParams();
    headers['x-request-id']; // string, validated as UUID
    headers['x-count'];      // number | undefined, coerced from the header string
  });
```

Declared headers are also emitted as OpenAPI `in: "header"` parameters. A missing required header is rejected with a `400`, exactly like `.params()`/`.query()`.

### `.body(schema, options?)`

Validate the request body. By default, bodies are parsed as `application/json` with no coercion — types must match exactly.

```typescript
// src/api/users/post.ts
export default endpoint()
  .body({ name: String, email: Email, tags: ArrayOf(String) })
  .handle(async (ctx) => {
    const body = await ctx.body();
    body.name;  // string
    body.email; // string, validated as email
    body.tags;  // string[]
  });
```

#### Form-Encoded Bodies

Pass `{ contentType: 'application/x-www-form-urlencoded' }` as the second argument to parse form-encoded request bodies. Values are coerced from strings automatically (like `.params()` and `.query()`).

This is required for OAuth2/OIDC endpoints (token, introspect, revoke) where the spec mandates `application/x-www-form-urlencoded`.

```typescript
// src/api/oauth/token/post.ts
import { endpoint, Optional } from '@putnami/application';

export default endpoint()
  .body(
    { grant_type: String, code: Optional(String), redirect_uri: Optional(String) },
    { contentType: 'application/x-www-form-urlencoded' },
  )
  .handle(async (ctx) => {
    const body = await ctx.body();
    // body.grant_type — string, validated
    // body.code — string | undefined
  });
```

The `contentType` option also affects OpenAPI spec generation — the request body schema uses the specified content type instead of `application/json`.

### `.returns(schema)` and `.response(status, ...)`

Declare success response shapes. `.returns(schema)` sets the primary success type — used for TypeScript inference, dev-mode response validation, and the default 200 OpenAPI entry. `.response(status, ...)` adds additional status-specific entries to the OpenAPI spec.

**Primary return:**

```typescript
export default endpoint()
  .returns({ id: String, name: String, email: Email })
  .handle((ctx) => {
    return { id: '1', name: 'John', email: 'john@example.com' };
  });
```

**Additional status entries** — document multiple response codes for OpenAPI:

```typescript
export default endpoint()
  .response(200, 'Success', { users: ArrayOf(String) })
  .response(201, 'Created', { id: String })
  .handle((ctx) => {
    return { users: [] };
  });
```

The two methods compose naturally:

```typescript
export default endpoint()
  .returns({ id: String, name: String })      // primary type + default 200
  .response(201, 'Created', { id: String })   // additional OpenAPI entry
  .handle((ctx) => ({ id: '1', name: 'Alice' }));
```

In development (`NODE_ENV !== 'production'`), the handler's return value is validated against the primary schema. Mismatches are logged as warnings — the response is still sent so the server keeps running.

### `.throws(status, description, schema?)` and `.throws(...specs)`

Document error responses for OpenAPI. This is metadata only — it does not affect runtime behavior.

**Inline:**

```typescript
export default endpoint()
  .throws(400, 'Validation failed', { message: String, fields: ArrayOf(String) })
  .throws(404, 'Not found')
  .handle((ctx) => ({ ok: true }));
```

**Reusable bundle** — define a `ResponseMeta[]` once and spread it into multiple endpoints:

```typescript
import type { ResponseMeta } from '@putnami/application';

export const AuthErrors: ResponseMeta[] = [
  { status: 401, description: 'Unauthorized' },
  { status: 403, description: 'Forbidden' },
];

export default endpoint()
  .throws(...AuthErrors)
  .throws(404, 'Not found')
  .handle((ctx) => ({ ok: true }));
```

Global throws can be declared on the `api()` plugin to apply to all routes:

```typescript
const apiPlugin = api({ autoScan: false })
  .throws(401, 'Unauthorized', { message: String })
  .throws(500, 'Internal error');
```

Endpoint-level `.throws()` overrides global throws for the same status code.

On an endpoint that publishes a first-party client contract, each `.throws(status)` needs at least one declared error code at that status: a `.mayThrow()` code, or the implicit `http.bad_request` (400) and `http.internal_server` (500) codes every first-party endpoint declares. Several codes can share a status: the codes tell a generated client which error it received, and `.throws()` documents the status. A `.throws(status)` that no declared code shares has no stable wire code, and the contract is refused when it is generated. The Go `openapi` plugin applies the same rule.

A `.throws()` schema describes the `details` of every `.mayThrow()` code at its status. The implicit 400 and 500 codes carry only the error envelope, so they never take that schema.

```typescript
export default endpoint()
  .mayThrow('Conflict', 'AlreadyExists')        // both answer 409
  .throws(409, 'Document conflict', { reason: String })
  .handle((ctx) => ({ ok: true }));
```

### `.description(text)`

Add a human-readable description to the endpoint. Appears as the `description` field on the OpenAPI operation.

```typescript
export default endpoint()
  .description('List all users with optional pagination')
  .query({ page: Optional(Number), limit: Optional(Number) })
  .handle((ctx) => {
    return { users: [] };
  });
```

### `.cors(options)` / `.rateLimit(options)`

Enable built-in security middleware per route. See `security.md` for full options.

```typescript
import { endpoint } from '@putnami/application';

export default endpoint()
  .cors({ origin: 'https://app.example.com', credentials: true })
  .rateLimit({ max: 10, windowMs: 60_000 })
  .handle(() => ({ ok: true }));
```

### `.secure(optionsOrGuard?)`

Require authentication and enforce access rules at the route level.

- Pass no argument to require any authenticated user.
- Pass security options for scopes/roles checks.
- Pass a custom guard function for advanced authorization.

```typescript
import { endpoint } from '@putnami/application';

export default endpoint()
  .secure({
    scopes: ['notes:read'],
    roles: ['admin'],
  })
  .handle(() => ({ ok: true }));
```

Use `.secure()` to enforce authentication and access rules on any endpoint.

### `.csrfExempt()`

Skip CSRF token validation for the route this endpoint produces. This is opt-in and scoped to **this endpoint only** — it never widens exemption to sibling routes. Use it for trusted machine-to-machine or webhook endpoints that authenticate with a bearer token or signature rather than a browser session cookie.

```typescript
import { endpoint } from '@putnami/application';

// api({ csrf: true }) enforces CSRF app-wide; this one webhook opts out.
export default endpoint()
  .csrfExempt()
  .body({ event: String })
  .handle(async (ctx) => {
    const body = await ctx.body();
    return { received: body.event };
  });
```

`.csrfExempt()` is sugar over the route's existing `csrfExempt` flag (the same flag `api()` routes set by default) — it does not introduce a separate CSRF code path. Endpoints default to **not** exempt.

### `.handle(handler)`

Provide the handler function. This finalises the endpoint definition and must be called last.

When `.body()` or `.returns()` use `Stream()`, `.handle()` produces a `StreamEndpointDefinition` (WebSocket/SSE). Otherwise it produces a standard `EndpointDefinition` (HTTP).

### Streaming with `Stream()`

Wrap a schema in `Stream()` to mark it as a stream of messages. The streaming mode is derived from which slots are streams:

| `body` | `returns` | Mode | Transport |
| ----------- | ----------- | ------------- | -------------------- |
| `T` | `T` | Unary | REST (standard HTTP) |
| `T` | `Stream(T)` | Server-stream | SSE or WebSocket |
| `Stream(T)` | `T` | Client-stream | WebSocket |
| `Stream(T)` | `Stream(T)` | Bidirectional | WebSocket |

**Server-stream** — push events to the client:

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

**Client-stream** — consume a stream of messages, return a final result:

```typescript
// src/api/upload/ws.ts
import { endpoint, Stream } from '@putnami/application';

export default endpoint()
  .body(Stream({ type: String, data: String }))
  .returns({ result: String })
  .handle(async (ctx) => {
    for await (const msg of ctx.messages()) {
      // process each incoming message
    }
    return { result: 'done' };
  });
```

**Bidirectional** — send and receive concurrently:

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
  });
```

The handler context adapts based on the streaming mode:

- `ctx.messages()` — available when body is `Stream()`, returns `AsyncIterable<T>`
- `ctx.send(data)` — available when returns is `Stream()`, pushes a message to the client

Schema validation applies to all stream messages automatically. Invalid messages are rejected before reaching the handler.

See [WebSockets & Streaming](websockets.md) for transport details, protocol negotiation, and plain handler objects.

## Full Example

A complete CRUD API using `endpoint()`:

```typescript
// src/api/users/get.ts
import { endpoint, Optional } from '@putnami/application';

export default endpoint()
  .query({ page: Optional(Number), limit: Optional(Number) })
  .handle((ctx) => {
    const { page, limit } = ctx.queryParams();
    return { users: [], page: page ?? 1, limit: limit ?? 20 };
  });
```

```typescript
// src/api/users/post.ts
import { endpoint, Email } from '@putnami/application';

export default endpoint()
  .body({ name: String, email: Email })
  .handle(async (ctx) => {
    const { name, email } = await ctx.body();
    const user = { id: crypto.randomUUID(), name, email };
    return { user };
  });
```

```typescript
// src/api/users/[id]/get.ts
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    return { user: { id: ctx.params.id } };
  });
```

```typescript
// src/api/users/[id]/put.ts
import { endpoint, Uuid, Email, Optional } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .body({ name: Optional(String), email: Optional(Email) })
  .handle(async (ctx) => {
    const { name, email } = await ctx.body();
    return { user: { id: ctx.params.id, name, email } };
  });
```

```typescript
// src/api/users/[id]/delete.ts
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    return { deleted: true, id: ctx.params.id };
  });
```

## Multi-Method Routes

Use `endpoint()` with named exports in `route.ts` files:

```typescript
// src/api/items/route.ts
import { endpoint, Uuid, Optional } from '@putnami/application';

export const GET = endpoint()
  .query({ category: Optional(String) })
  .handle((ctx) => {
    return { items: [] };
  });

export const POST = endpoint()
  .body({ name: String, price: Number })
  .handle(async (ctx) => {
    const item = await ctx.body();
    return { created: item };
  });
```

## Error Handling

Validation errors return a `400 Bad Request` with a descriptive message and a structured list of issues:

```
POST /users
Content-Type: application/json
{ "name": "John" }

→ 400 { "message": "body.email is required", "errors": [{ "field": "body.email", "message": "body.email is required" }] }
```

```
POST /users
Content-Type: application/json
{ "name": "John", "email": "invalid" }

→ 400 { "message": "body.email must be a valid email address", "errors": [{ "field": "body.email", "message": "body.email must be a valid email address" }] }
```

Multiple validation errors are combined:

```
POST /users
Content-Type: application/json
{}

→ 400 { "message": "body.name is required; body.email is required", "errors": [
  { "field": "body.name", "message": "body.name is required" },
  { "field": "body.email", "message": "body.email is required" }
] }
```

## Content Negotiation

Responses are automatically serialized based on the request's `Accept` header. When a handler returns an object or array, the framework picks the best format:

| Accept Header | Response Format |
|---------------|----------------|
| `application/json` (or `*/*`) | JSON (default) |
| `text/plain` | YAML as plain text (human-readable) |
| `text/html` | JSON wrapped in a minimal HTML page |
| `application/xml` / `text/xml` | Simple XML serialization |
| `application/yaml` / `text/yaml` | YAML serialization |

If the client requests a type that isn't supported, a `406 Not Acceptable` response is returned.

```
GET /users/1
Accept: application/xml

→ 200
Content-Type: application/xml; charset=utf-8
<?xml version="1.0" encoding="UTF-8"?>
<root><id>1</id><name>John</name></root>
```

Handlers that return a `HttpResponse` or a plain string bypass content negotiation — they are sent as-is.

## OpenAPI Spec Generation

Routes defined with `route()` are automatically discoverable. Call `openapi()` on the `ApiPlugin` instance to generate a complete OpenAPI 3.0.3 specification with zero extra annotations:

```typescript
import { api } from '@putnami/application';

const apiPlugin = api();

// After warmup, generate the spec
const spec = apiPlugin.openapi({
  info: { title: 'My API', version: '1.0.0' },
  servers: [{ url: 'https://api.example.com' }],
});
```

The generator maps:
- `.params()` → OpenAPI path parameters
- `.query()` → OpenAPI query parameters
- `.headers()` → OpenAPI header parameters (`in: "header"`)
- `.body()` → OpenAPI request body (`application/json` or `application/x-www-form-urlencoded`)
- `.returns()` → OpenAPI 200 response schema
- `.returns(status, ...)` → OpenAPI multi-status responses
- `.throws(status, ...)` → OpenAPI error responses (4xx/5xx)
- `.description()` → OpenAPI operation `description`
- `.secure()` → OpenAPI `security` requirement + `components.securitySchemes`
- `Desc('...', type)` → OpenAPI property `description`
- `Uuid`, `Email`, `Int` → `format` hints (`uuid`, `email`, `int64`)
- `ArrayOf()` → `type: array` with `items`
- `Optional()` → `required: false`

Security metadata from `.secure(options)` is automatically propagated: when any route has security, a `bearerAuth` (JWT) security scheme is added to `components.securitySchemes`, and each secured operation gets a `security` entry with roles/scopes.

You can also use `generateOpenApiSpec()` directly with an array of `DiscoveredRoute` objects, or access the tracked routes via `apiPlugin.routes`.

### Build-Time Generation

Pass the `openapi` config option to generate `openapi.json` during `putnami build`. The spec is written to `.gen/` alongside the route loader, making it available to codegen tools in any language:

```typescript
app.use(api({
  openapi: {
    title: 'User Service',
    version: '1.0.0',
    description: 'User management API',
    servers: [{ url: 'https://api.example.com' }],
  },
}));
```

After build, the file is available at `.gen/<scanPath>/openapi.json` and registered as a build asset. Other tools (Go, Python, Java clients, etc.) can consume it directly.

## Response Validation

When `NODE_ENV` is not `production`, routes with a `.returns()` schema validate the handler's response at runtime. Contract mismatches are logged as warnings — the response is still delivered so the dev server stays running:

```
WARN [response-validation] /users — response.email is required
```

This catches common issues early:
- Missing required fields in the response
- Wrong value types (returning a number where a string is expected)
- Constraint violations (e.g. returning an invalid UUID)

Response validation is skipped for:
- `HttpResponse` instances (opaque responses)
- `undefined`/`null` returns
- Non-JSON string returns
- Production builds (`NODE_ENV=production`)

## Next Steps

- Learn about [File-based Routing](file-based-routing.md) for route conventions
- See [WebSockets & Streaming](websockets.md) for real-time communication with `Stream()`
- Check [HTTP Server](http-server.md) for middleware and request handling
- See [TypeScript ↔ Go Parity](go-parity.md) for how these capabilities map to the Go framework
- See [API Reference](api-reference.md) for the complete API
