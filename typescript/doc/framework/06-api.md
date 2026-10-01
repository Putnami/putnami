# API

Use `@putnami/application` for HTTP servers, middleware, WebSockets, and file-based API routes. Build REST APIs with automatic route discovery, typed request handling, and built-in schema validation.

## Getting started

### Entry point

```ts
import { application, api, http } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(api());
```

### Configuration options

```ts
api({
  scanFolder: 'api',    // Folder to scan for routes (default: 'api')
  autoScan: true,       // Auto-discover routes (default: true)
  prefix: '/api',       // URL prefix for all routes (optional)
})
```

When used inside a module with `.path()`, the module path is automatically used as the route prefix (unless an explicit `prefix` is set):

```ts
const tasksModule = module('tasks')
  .path('/tasks')
  .use(api());          // Routes prefixed with /tasks
```

## File-based routing

Routes are discovered from your `src/api/` folder. File names determine the HTTP method.

### Route file conventions

- **get.ts** - GET request handler
- **post.ts** - POST request handler
- **put.ts** - PUT request handler
- **patch.ts** - PATCH request handler
- **delete.ts** - DELETE request handler
- **\*.get.ts** - Named GET handler (e.g., `users.get.ts`)
- **\*.post.ts** - Named POST handler
- **route.ts** or **routes.ts** - Multiple methods in one file
- **\*.ws.ts** - WebSocket handler

### Example structure

```text
src/api/
  health/
    get.ts              # GET /health
  users/
    get.ts              # GET /users
    post.ts             # POST /users
    [id]/
      get.ts            # GET /users/:id
      put.ts            # PUT /users/:id
      delete.ts         # DELETE /users/:id
  search/
    users.get.ts        # GET /search/users
    products.get.ts     # GET /search/products
```

## Route handlers

All route handlers use the `endpoint()` builder. It provides type safety, automatic validation, and a consistent API across HTTP and WebSocket routes.

### Basic handler

```ts
// src/api/health/get.ts
import { endpoint } from '@putnami/application';

export default endpoint(() => {
  return { status: 'ok', timestamp: Date.now() };
});
```

### Handler with query parameters

```ts
// src/api/users/get.ts
import { endpoint, Optional } from '@putnami/application';

export default endpoint()
  .query({ limit: Optional(Number), offset: Optional(Number) })
  .handle((ctx) => {
    const { limit, offset } = ctx.queryParams();
    return { users: [], limit: limit ?? 10, offset: offset ?? 0 };
  });
```

### Handler with body validation

```ts
// src/api/users/post.ts
import { endpoint, Email } from '@putnami/application';

export default endpoint()
  .body({ email: Email, name: String })
  .handle(async (ctx) => {
    const body = await ctx.body();
    // body.email and body.name are validated — invalid requests never reach here
    const user = await createUser(body);
    return user;
  });
```

### Dynamic route parameters

```ts
// src/api/users/[id]/get.ts
import { endpoint, Uuid } from '@putnami/application';
import { NotFoundException } from '@putnami/runtime';

export default endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    const user = await findUser(ctx.params.id);

    if (!user) {
      throw new NotFoundException(`User ${ctx.params.id} not found`);
    }

    return user;
  });
```

### Catch-all routes

```ts
// src/api/files/[...path]/get.ts
import { endpoint } from '@putnami/application';

export default endpoint((ctx) => {
  const { path } = ctx.params;
  // path contains the full path after /api/files/
  // e.g., /api/files/docs/2024/report.pdf -> path = 'docs/2024/report.pdf'

  return { file: path };
});
```

## Endpoint builder

The `endpoint()` function provides a fluent builder for defining API handlers (HTTP and WebSocket) with built-in schema validation, type inference, and security. Inputs are validated before your handler runs — invalid requests are rejected automatically with structured error responses.

### Simple mode

For handlers that don't need validation, pass a function directly:

```ts
// src/api/health/get.ts
import { endpoint } from '@putnami/application';

export default endpoint((ctx) => {
  return { status: 'ok' };
});
```

### Builder mode

Chain `.params()`, `.query()`, `.body()` to declare and validate inputs:

```ts
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

```http
GET /users/not-a-uuid
→ 400 { "message": "params.id must be a valid UUID" }
```

### Builder methods

#### `.params(schema)`

Validate path parameters. Values are coerced from strings automatically (e.g., `"42"` → `42` for `Number`).

```ts
export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    ctx.params.id; // string, validated as UUID
  });
```

#### `.query(schema)`

Validate query string parameters. Values are coerced from strings automatically.

```ts
export default endpoint()
  .query({ page: Number, limit: Number, search: Optional(String) })
  .handle((ctx) => {
    const query = ctx.queryParams();
    query.page;   // number
    query.limit;  // number
    query.search; // string | undefined
  });
```

#### `.body(schema)`

Validate the request body (JSON). No coercion — types must match exactly.

```ts
export default endpoint()
  .body({ name: String, email: Email, tags: ArrayOf(String) })
  .handle(async (ctx) => {
    const body = await ctx.body();
    body.name;  // string
    body.email; // string, validated as email
    body.tags;  // string[]
  });
```

#### `.returns(schema)` / `.returns(status, description?, schema?)`

Declare the expected response shape. Used for OpenAPI spec generation and dev-mode response validation.

**Plain return** — sets the primary type and default 200 response:

```ts
export default endpoint()
  .returns({ id: String, name: String, email: Email })
  .handle((ctx) => {
    return { id: '1', name: 'John', email: 'john@example.com' };
  });
```

**Additional status entries** — document multiple response codes for OpenAPI via `.response()`:

```ts
export default endpoint()
  .response(200, 'Success', { users: ArrayOf(String) })
  .response(201, 'Created', { id: String })
  .handle((ctx) => ({ users: [] }));
```

In non-production environments, the framework validates handler responses against the primary schema and logs warnings on mismatches — without breaking the request. This catches contract drift early during development.

#### `.mayThrow(...codes)` and `.throws(status, description, schema?)`

OpenAPI automatically documents the framework-guaranteed `400` validation response and
`500` fallback response for every operation.

Use `.mayThrow()` to declare framework-known errors your handler may emit. The OpenAPI
generator maps each code to the standard error envelope and HTTP status:

```ts
export default endpoint()
  .mayThrow('NotFound', 'Conflict')
  .handle((ctx) => ({ ok: true }));
```

Use `.mayThrowDetails(code, schema)` when the error carries structured data. The
schema describes the envelope's `details` member for that code, never the
`{code, error, message}` envelope. The first-party contract publishes it, and
generated clients (TypeScript and Go) validate `details` against it and expose it
typed on the error for that code. Throw the matching exception with the details as
its response object. It declares no retry classification; chain `.mayThrowWith()`
for one. A `.throws(status, …, schema)` at the same status describes the details of
the other codes there, never of a code that declares its own. `'BadRequest'` and
`'InternalServerError'` are refused: every endpoint answers those implicit codes, and
the framework writes their response. The OpenAPI document publishes each details
schema as a component, and a status that several codes with details share documents
`anyOf` their schemas, in error-code order:

```ts
export default endpoint()
  .mayThrowDetails('Conflict', {
    rejections: ArrayOf({ project: String, error: String }),
    retryable: Boolean,
  })
  .handle(() => {
    throw new ConflictException({ rejections: [{ project: 'a', error: 'image not found' }], retryable: false });
  });
```

Use `.throws()` for custom descriptions or schemas. Explicit `.throws()` entries override
auto-generated responses for the same status. Metadata only — does not affect runtime.

```ts
export default endpoint()
  .throws(400, 'Validation failed', { message: String })
  .throws(404, 'Not found')
  .handle((ctx) => ({ ok: true }));
```

Global throws on the `api()` plugin apply to all routes:

```ts
api({ autoScan: false })
  .throws(401, 'Unauthorized', { message: String })
  .throws(500, 'Internal error')
```

#### `.description(text)`

Add a human-readable description to the OpenAPI operation.

```ts
export default endpoint()
  .description('List all users with optional pagination')
  .query({ page: Optional(Number) })
  .handle((ctx) => ({ users: [] }));
```

#### `.cors(options)`

Enable CORS for a single route.

```ts
export default endpoint()
  .cors({ origin: 'https://app.example.com', credentials: true })
  .handle(() => ({ ok: true }));
```

#### `.rateLimit(options)`

Apply rate limiting per route.

```ts
export default endpoint()
  .rateLimit({ max: 10, windowMs: 60_000 })
  .handle(() => ({ ok: true }));
```

#### `.secure(optionsOrGuard?)`

Require authentication and enforce access rules.

```ts
export default endpoint()
  .secure({
    scopes: ['notes:read'],
    roles: ['admin'],
  })
  .handle(() => ({ ok: true }));
```

Use `.secure()` to enforce authentication and access rules on any endpoint.

#### `.handle(handler)`

Provide the handler function. This finalises the endpoint definition and must be called last.

When `.body()` or `.returns()` use `Stream()`, `.handle()` produces a `StreamEndpointDefinition` (WebSocket/SSE). Otherwise it produces a standard `EndpointDefinition` (HTTP).

#### Streaming with `Stream()`

Wrap a schema in `Stream()` to mark it as a stream of messages. The mode is derived from which slots are streams:

| `body` | `returns` | Mode | Transport |
| ----------- | ----------- | ------------- | -------------------- |
| `T` | `T` | Unary | REST (standard HTTP) |
| `T` | `Stream(T)` | Server-stream | SSE or WebSocket |
| `Stream(T)` | `T` | Client-stream | WebSocket |
| `Stream(T)` | `Stream(T)` | Bidirectional | WebSocket |

```ts
// src/api/chat/[roomId]/ws.ts
import { endpoint, Stream, Uuid } from '@putnami/application';

export default endpoint()
  .params({ roomId: Uuid })
  .body(Stream({ type: String, content: String }))
  .returns(Stream({ event: String, payload: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'welcome', payload: ctx.params.roomId });
    for await (const msg of ctx.messages()) {
      ctx.send({ event: 'echo', payload: msg.content });
    }
  });
```

Simple server-stream:

```ts
import { endpoint, Stream } from '@putnami/application';

export default endpoint()
  .returns(Stream({ event: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'hello' });
  });
```

See [WebSockets & Streaming](/docs/frameworks/typescript/websockets) for protocol negotiation, SSE, and advanced patterns.

## Schema validation

### Primitive types

Use JavaScript constructors as schema types:

| Schema | TypeScript Type | Description |
| ------ | --------------- | ----------- |
| `String` | `string` | String value |
| `Number` | `number` | Numeric value |
| `Boolean` | `boolean` | Boolean value |

### Constrained types

Import these for stricter validation:

```ts
import { Uuid, Email, Int, Url, DateIso, Min, Max, MinLength, MaxLength, Pattern } from '@putnami/application';
```

| Schema | TypeScript Type | Validation |
| ------ | --------------- | ---------- |
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

```ts
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

```ts
import { Optional, ArrayOf } from '@putnami/application';
```

| Combinator | Example | TypeScript Type |
| ---------- | ------- | --------------- |
| `Optional(T)` | `Optional(String)` | `string \| undefined` |
| `ArrayOf(T)` | `ArrayOf(String)` | `string[]` |
| `Desc(text, T)` | `Desc('User name', String)` | `string` (adds OpenAPI description) |

Combinators compose with constrained types:

```ts
Optional(Uuid)            // string | undefined, validated as UUID when present
Optional(ArrayOf(Email))  // string[] | undefined
ArrayOf(Int)              // number[], each validated as integer
```

### Nested object schemas

Schema properties can be plain objects to validate nested structures:

```ts
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

### Sharing schemas

Extract shared schemas into modules and reuse them across routes:

```ts
import { ArrayOf, MinLength, Optional, schema } from '@putnami/application';

export const NoteBodySchema = schema({
  title: MinLength(1),
  content: MinLength(2),
  tags: Optional(ArrayOf(String)),
});
```

You can derive the TypeScript type from a schema:

```ts
import type { InferSchema } from '@putnami/application';

export type NoteBody = InferSchema<typeof NoteBodySchema>;
```

### Validation errors

Validation errors return a `400 Bad Request` with a descriptive message and a structured list of issues:

```http
POST /users
Content-Type: application/json
{ "name": "John" }

→ 400 { "message": "body.email is required", "errors": [{ "field": "body.email", "message": "body.email is required" }] }
```

```http
POST /users
Content-Type: application/json
{ "name": "John", "email": "invalid" }

→ 400 { "message": "body.email must be a valid email address", "errors": [{ "field": "body.email", "message": "body.email must be a valid email address" }] }
```

Multiple validation errors are combined:

```http
POST /users
Content-Type: application/json
{}

→ 400 { "message": "body.name is required; body.email is required", "errors": [
  { "field": "body.name", "message": "body.name is required" },
  { "field": "body.email", "message": "body.email is required" }
] }
```

## Content negotiation

Responses are automatically serialized based on the request's `Accept` header. When a handler returns an object or array, the framework picks the best format:

| Accept Header | Response Format |
| ------------- | --------------- |
| `application/json` (or `*/*`) | JSON (default) |
| `text/plain` | YAML as plain text (human-readable) |
| `text/html` | JSON wrapped in a minimal HTML page |
| `application/xml` / `text/xml` | Simple XML serialization |
| `application/yaml` / `text/yaml` | YAML serialization |

If the client requests a type that isn't supported, a `406 Not Acceptable` response is returned.

```http
GET /users/1
Accept: application/xml

→ 200
Content-Type: application/xml; charset=utf-8
<?xml version="1.0" encoding="UTF-8"?>
<root><id>1</id><name>John</name></root>
```

Handlers that return an `HttpResponse` or a plain string bypass content negotiation — they are sent as-is.

## Request context

The handler context (`ctx`) provides access to all request information. When using `endpoint()` with schemas, params, query, and body are automatically validated and typed.

### Properties

```ts
ctx.url;           // Full URL string
ctx.req;           // Raw Bun Request object
ctx.headers;       // Headers object
ctx.route;         // Matched route pattern
ctx.params;        // Route parameters { id: '123' } — typed when .params() is used
ctx.statusCode;    // Status code (can be modified)
ctx.server;        // Bun server instance
```

### Methods

```ts
ctx.queryParams();   // Validated query params (typed when .query() is used)
ctx.body();          // Validated body (typed when .body() is used)
ctx.secured();       // true if HTTPS
ctx.host();          // 'example.com:3000'
ctx.domain();        // 'example.com'
ctx.path();          // '/api/users'
ctx.query();         // '?limit=10'
```

## Response handling

### Return values

Handlers can return different types:

| Return type | Response |
| ----------- | -------- |
| Object / Array | JSON response |
| String | Text response |
| `HttpResponse` | Full control over status, headers, body |
| `undefined` | 204 No Content |

### HttpResponse

Use `HttpResponse` and standalone factory functions for full response control:

```ts
import { endpoint, HttpResponse, json, notFound, unauthorized, forbidden, badRequest } from '@putnami/application';

// JSON with custom status
export default endpoint(() => {
  return json({ created: true }, { status: 201 });
});

// Redirect
HttpResponse.redirect('/dashboard', 302);

// Error responses
notFound('Resource not found');
unauthorized('Please log in');
forbidden('Access denied');
badRequest('Invalid input');

// Custom headers
json({ data: 'value' })
  .setHeader('X-Custom-Header', 'value')
  .setHeader('Cache-Control', 'max-age=3600');
```

## Multi-method routes

Use `route.ts` to define multiple HTTP methods in a single file. Each method is an `endpoint()` named export:

```ts
// src/api/items/route.ts
import { endpoint, Optional } from '@putnami/application';

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

## Error handling

### Throwing HTTP exceptions

```ts
import { endpoint, Uuid } from '@putnami/application';
import { NotFoundException, ForbiddenException } from '@putnami/runtime';

export default endpoint()
  .params({ id: Uuid })
  .handle(async (ctx) => {
    const user = await findUser(ctx.params.id);

    if (!user) {
      throw new NotFoundException(`User ${ctx.params.id} not found`);
    }

    if (!hasAccess(ctx, user)) {
      throw new ForbiddenException('You cannot access this user');
    }

    return user;
  });
```

Available exception classes: `BadRequestException`, `UnauthorizedException`, `ForbiddenException`, `NotFoundException`, `ConflictException`.

## OpenAPI generation

The API plugin can generate an OpenAPI 3.0.3 specification from your routes. HTTP routes defined with the `endpoint()` builder automatically contribute their `.params()`, `.query()`, `.body()`, `.returns()`, `.mayThrow()`, `.throws()`, `.description()`, and `.secure()` metadata to the spec. Schema-level `Desc()` descriptions are included on individual properties.

### Build-time generation

Enable build-time spec generation with the `openapi` config option. An `openapi.json` file is written to `.gen/` during the build step.

```ts
export const app = () =>
  application()
    .use(http())
    .use(
      api({
        openapi: {
          title: 'My API',
          version: '1.0.0',
          description: 'Service description',
          servers: [{ url: 'https://api.example.com' }],
        },
      }),
    );
```

### Runtime generation

You can also generate the spec at runtime from the plugin instance:

```ts
const apiPlugin = api();
// ... after warmup
const spec = apiPlugin.openapi({
  info: { title: 'My API', version: '1.0.0' },
});
```

### OpenAPI configuration options

```ts
api({
  scanFolder: 'api',    // Folder to scan for routes (default: 'api')
  autoScan: true,       // Auto-discover routes (default: true)
  prefix: '/api',       // URL prefix for all routes (optional)
  openapi: {            // OpenAPI spec generation (optional)
    title: 'My API',        // Required: API title
    version: '1.0.0',       // Required: API version
    description: '...',     // Optional: API description
    servers: [               // Optional: server URLs
      { url: 'https://api.example.com', description: 'Production' },
    ],
  },
})
```

## Response validation

When a route declares a `.returns()` schema, the framework validates handler responses in non-production environments (`NODE_ENV !== 'production'`). Mismatches are logged as warnings without interrupting the request, so your dev server keeps running while you catch contract drift early.

```log
WARN [response-validation] /users — response.email is required
```

Response validation catches:

- Missing required fields
- Type mismatches (e.g., returning a number where a string is expected)
- Constraint violations (e.g., invalid UUID format)

Responses that are `HttpResponse` instances, `undefined`/`null`, or non-JSON strings are skipped. In production, response validation is disabled entirely.

## Related guides

- [Build an API service](/docs/how-to/build-an-api-service)
- [Structure business logic with DI](/docs/how-to/structure-business-logic-with-di) — modules, providers, `.inject()` in endpoints
- [Dependency Injection](/docs/frameworks/typescript/dependency-injection) — full DI reference
- [HTTP & Middleware](/docs/frameworks/typescript/http-and-middleware)
- [Errors & Responses](/docs/frameworks/typescript/errors-and-responses)
