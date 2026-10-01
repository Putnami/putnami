# File-based Routing

Automatic route discovery from your file structure using the API plugin.

## Overview

The API plugin automatically discovers routes from your file structure, eliminating the need to manually register routes.

## Basic Setup

```typescript
import { application, http, api } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(api())  // Scans ./api folder by default
  .run(async () => {
    console.log('Server ready!');
  });

await app.start();
```

## File Conventions

The plugin maps file names to HTTP methods:

| File | HTTP Method |
|------|-------------|
| `get.ts` | GET |
| `post.ts` | POST |
| `put.ts` | PUT |
| `patch.ts` | PATCH |
| `delete.ts` | DELETE |
| `route.ts` | Multiple methods |
| `ws.ts` | WebSocket |

## Directory Structure

Routes are created based on your directory structure:

```
src/api/
├── users/
│   ├── get.ts          → GET /users
│   ├── post.ts         → POST /users
│   └── [id]/
│       ├── get.ts      → GET /users/:id
│       ├── put.ts      → PUT /users/:id
│       └── delete.ts   → DELETE /users/:id
├── health/
│   └── get.ts          → GET /health
└── posts/
    ├── get.ts          → GET /posts
    └── [id]/
        └── comments/
            └── get.ts  → GET /posts/:id/comments
```

## Route Handlers

### Using `endpoint()` (Recommended)

The `endpoint()` builder provides type-safe, validated handlers with zero boilerplate. Inputs are validated before your handler runs:

```typescript
// src/api/users/get.ts
import { endpoint } from '@putnami/application';

export default endpoint((ctx) => {
  return { users: [] };
});
```

```typescript
// src/api/users/post.ts
import { endpoint, Email } from '@putnami/application';

export default endpoint()
  .body({ name: String, email: Email })
  .handle(async (ctx) => {
    const { name, email } = await ctx.body();
    // name and email are validated — invalid requests never reach here
    return { id: '1', name, email };
  });
```

```typescript
// src/api/users/[id]/put.ts
import { endpoint, Uuid, Optional } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .body({ name: Optional(String) })
  .handle(async (ctx) => {
    const body = await ctx.body();
    return { id: ctx.params.id, ...body, updated: true };
  });
```

```typescript
// src/api/users/[id]/delete.ts
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    return { deleted: ctx.params.id };
  });
```

See [Endpoint Builder](endpoint-builder.md) for the full guide on schemas, validation, and builder methods.

## Dynamic Routes

### Path Parameters

Use `[param]` folders for dynamic routes:

```
src/api/
└── users/
    └── [id]/
        └── get.ts  → GET /users/:id
```

Access parameters in your handler:

```typescript
// src/api/users/[id]/get.ts
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    return { userId: ctx.params.id };
  });
```

### Multiple Parameters

```
src/api/
└── users/
    └── [userId]/
        └── posts/
            └── [postId]/
                └── get.ts  → GET /users/:userId/posts/:postId
```

```typescript
import { endpoint, Uuid } from '@putnami/application';

export default endpoint()
  .params({ userId: Uuid, postId: Uuid })
  .handle((ctx) => {
    return { userId: ctx.params.userId, postId: ctx.params.postId };
  });
```

## Multiple Methods in One File

Use `route.ts` for multiple HTTP methods. Each method can use `endpoint()` independently:

```typescript
// src/api/users/[id]/route.ts
import { endpoint, Uuid, Optional } from '@putnami/application';

export const GET = endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    return { user: { id: ctx.params.id } };
  });

export const PUT = endpoint()
  .params({ id: Uuid })
  .body({ name: Optional(String) })
  .handle(async (ctx) => {
    const body = await ctx.body();
    return { id: ctx.params.id, ...body, updated: true };
  });

export const DELETE = endpoint()
  .params({ id: Uuid })
  .handle((ctx) => {
    return { deleted: ctx.params.id };
  });
```

## Route Prefix

By default, `api()` mounts routes at the root — no prefix. The `api` scan folder name is for file organisation only; it does **not** affect the URL.

`src/api/users/get.ts` → `GET /users`

**Prefix resolution** (first non-undefined wins):

1. Explicit `prefix` option — `api({ prefix: '/api' })`
2. Parent module `.path()` — `module('tasks').path('/tasks').use(api())`
3. No prefix (default)

### Add prefix via plugin config

```typescript
import { api } from '@putnami/application';

const app = application()
  .use(http())
  .use(api({ prefix: '/api' }));

// src/api/users/get.ts → GET /api/users
// src/api/health/get.ts → GET /api/health
```

### Add prefix via module path

```typescript
import { module, api } from '@putnami/application';

const tasks = module('tasks')
  .path('/tasks')
  .use(api());

// src/api/get.ts       → GET /tasks
// src/api/[id]/get.ts  → GET /tasks/[id]
```

If both module `.path()` and plugin `prefix` are set, the plugin `prefix` takes priority.

## Path-scoped middleware (`.use()`)

Attach middleware to a subset of routes registered through this api plugin by matching their path:

```typescript
import { api, SecurityMiddleware, RateLimitMiddleware } from '@putnami/application';

const app = application()
  .use(http())
  .use(
    api()
      .use('/admin/*', SecurityMiddleware({ roles: ['admin'] }))
      .use('/internal/*', RateLimitMiddleware({ max: 10 })),
  );
```

**Pattern syntax:**

| Pattern | Matches |
|---------|---------|
| `*` | every path |
| `/prefix/*` | `/prefix` and any path under `/prefix/` |
| `/exact` | only `/exact` |

The pattern is evaluated against the **post-prefix** path: with `api({ prefix: '/v1' }).use('/admin/*', mw)`, `mw` runs on requests to `/v1/admin/...`. Patterns may be written with or without the leading slash.

For middleware that should always run (regardless of path), apply it to the underlying `HttpPlugin` directly via `http().use(mw)` — `api(...).use(pattern, mw)` is for **scoped** application.

## Configuration

### Custom Scan Folder

```typescript
api({
  scanFolder: 'routes', // Scans src/routes/ instead of src/api/
})
```

### Disable Auto-Scan

```typescript
api({
  autoScan: false, // Don't automatically discover routes
})
```

### Several Scan Roots

A workload can use several `api()` plugins, each scanning its own folder:

```typescript
import { resolve } from 'node:path';

application()
  .use(api({ scanPath: resolve(import.meta.dir, 'internal-api'), csrf: true }))
  .use(api()); // scans src/api/
```

Each scanning plugin gets its own generated route loader, so the packaged workload serves every
scan root. Routes are relative to their own scan root: `src/internal-api/revocations/post.ts` serves
`POST /revocations`. Use `prefix` to mount a root under a path.

## WebSocket Routes

Create WebSocket handlers with `ws.ts` using the `endpoint()` builder with `Stream()`:

```typescript
// src/api/chat/ws.ts
import { endpoint, Stream } from '@putnami/application';

export default endpoint()
  .body(Stream({ type: String, data: String }))
  .returns(Stream({ event: String, payload: String }))
  .handle(async (ctx) => {
    ctx.send({ event: 'welcome', payload: 'hello' });
    for await (const msg of ctx.messages()) {
      ctx.send({ event: 'echo', payload: msg.data });
    }
    return { event: 'bye', payload: 'goodbye' };
  });
```

See [WebSockets & Streaming](websockets.md) for details on streaming modes and protocol negotiation.

## Manual Route Registration

Register routes manually using `register()`:

```typescript
import { api, endpoint, http, application } from '@putnami/application';

const apiPlugin = api({ autoScan: false });

// Single method
apiPlugin.register('/custom', endpoint(() => ({ custom: true })), 'GET');

// Multi-method
apiPlugin.register('/items', {
  GET: endpoint(() => ({ items: [] })),
  POST: endpoint()
    .body({ name: String })
    .handle(async (ctx) => {
      const body = await ctx.body();
      return { created: body.name };
    }),
});

const app = application()
  .use(http())
  .use(apiPlugin);
```

## Best Practices

1. **Follow naming conventions** - Use `get.ts`, `post.ts`, etc.
2. **Organize by resource** - Group related routes in folders
3. **Use dynamic routes** - `[id]` folders for parameters
4. **Keep handlers focused** - Extract business logic to services
5. **Handle errors** - Return appropriate error responses

## Project Structure Example

Here's a complete example structure:

```
src/
├── main.ts
├── api/
│   ├── users/
│   │   ├── get.ts          # GET /users
│   │   ├── post.ts         # POST /users
│   │   └── [id]/
│   │       ├── get.ts      # GET /users/:id
│   │       ├── put.ts      # PUT /users/:id
│   │       └── delete.ts   # DELETE /users/:id
│   ├── posts/
│   │   ├── get.ts          # GET /posts
│   │   └── [id]/
│   │       ├── get.ts      # GET /posts/:id
│   │       └── comments/
│   │           └── get.ts  # GET /posts/:id/comments
│   └── health/
│       └── get.ts          # GET /health
└── lib/
    └── db.ts
```

## Next Steps

- Read the [Endpoint Builder](endpoint-builder.md) guide for schema validation and type safety
- Learn about [HTTP Server](http-server.md) for routing details
- Explore [WebSockets](websockets.md) for real-time features
- Check [API Reference](api-reference.md) for complete API
