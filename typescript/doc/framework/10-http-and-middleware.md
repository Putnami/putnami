# HTTP & Middleware

`@putnami/application` provides the HTTP server, routing, and middleware layer built on Bun's native server.

## HTTP server

### Basic setup

```ts
import { application, http } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 3000 }));
```

### Configuration options

```ts
http({
  port: 3000,              // Server port
  hostname: '0.0.0.0',     // Bind address
  originGuard: {           // Cross-origin CSRF protection (enabled by default)
    trustedOrigins: ['https://admin.example.com'],
  },
  securityHeaders: {       // Secure response headers (enabled by default)
    contentSecurityPolicy: "default-src 'self'; script-src 'self' https://cdn.example.com",
  },
  // SSL options (optional)
  tls: {
    cert: Bun.file('cert.pem'),
    key: Bun.file('key.pem'),
  },
})
```

> **Built-in CSRF protection:** The origin guard validates `Origin` and `Sec-Fetch-Site` headers on all state-changing requests (POST, PUT, DELETE, PATCH) and rejects cross-origin requests with `403`. It is enabled by default. Set `originGuard: false` to disable. See [Auth security considerations](/docs/frameworks/typescript/auth#security-considerations) for full details.

> **Secure-by-default headers:** Every response carries hardened security headers — `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, a restrictive `Content-Security-Policy`, `Referrer-Policy`, `Permissions-Policy`, `Strict-Transport-Security`, and `X-XSS-Protection` — without any extra plugin. Override individual headers via `securityHeaders` (set a header to `false` to omit it) or set `securityHeaders: false` to disable them entirely.

### YAML configuration

```yaml
http:
  port: 3000
  hostname: '0.0.0.0'
```

## Manual routing

While file-based routing is recommended, you can register routes programmatically.

### Basic routes

```ts
import { http } from '@putnami/application';

const httpPlugin = http({ port: 3000 });

// GET route
httpPlugin.get('/health', () => ({ status: 'ok' }));

// POST route
httpPlugin.post('/users', async (ctx) => {
  const body = await ctx.body();
  return { created: true, user: body };
});

// PUT route
httpPlugin.put('/users/:id', async (ctx) => {
  const { id } = ctx.params;
  const body = await ctx.body();
  return { updated: true, id, data: body };
});

// DELETE route
httpPlugin.delete('/users/:id', (ctx) => {
  const { id } = ctx.params;
  return { deleted: true, id };
});

// PATCH route
httpPlugin.patch('/users/:id', async (ctx) => {
  const { id } = ctx.params;
  const body = await ctx.body();
  return { patched: true, id, data: body };
});
```

### Route with options

```ts
httpPlugin.route('GET', '/api/data', handler, {
  accept: ['application/json'],
  statusCode: 200,
});
```

### Merging routes

```ts
// Merge routes from another controller
const apiRoutes = new RouteController();
apiRoutes.route('GET', '/users', listUsers);
apiRoutes.route('POST', '/users', createUser);

httpPlugin.merge('/api', apiRoutes);
// Results in: GET /api/users, POST /api/users
```

## Middleware

Middleware functions process requests before they reach route handlers.

### Middleware signature

```ts
type HttpMiddleware = (
  context: HttpRequestContext,
  next: () => Promise<HttpResponse | undefined>,
) => Promise<HttpResponse | undefined>;
```

### Creating middleware

```ts
import type { HttpMiddleware } from '@putnami/application';
import { unauthorized, badRequest } from '@putnami/application';

// Logging middleware
export const logger: HttpMiddleware = async (ctx, next) => {
  const start = Date.now();
  console.log(`--> ${ctx.method} ${ctx.path()}`);

  const response = await next();

  const duration = Date.now() - start;
  console.log(`<-- ${ctx.method} ${ctx.path()} ${duration}ms`);

  return response;
};

// Authentication middleware
export const requireAuth: HttpMiddleware = async (ctx, next) => {
  const token = ctx.headers.get('authorization')?.replace('Bearer ', '');

  if (!token) {
    return unauthorized('Missing token');
  }

  try {
    const user = await verifyToken(token);
    // Store user for later use (via context or DI)
    return next();
  } catch {
    return unauthorized('Invalid token');
  }
};

// Content-type validation
export const requireJson: HttpMiddleware = async (ctx, next) => {
  const contentType = ctx.headers.get('content-type');

  if (ctx.method !== 'GET' && !contentType?.includes('application/json')) {
    return badRequest('Content-Type must be application/json');
  }

  return next();
};
```

### Registering middleware

```ts
const httpPlugin = http({ port: 3000 });

// Append middleware (runs after existing middleware)
httpPlugin.use(logger);
httpPlugin.use(requireJson);

// Prepend middleware (runs before existing middleware)
httpPlugin.prepend(corsMiddleware);
```

### Middleware order

Middleware executes in the order registered:

```ts
httpPlugin.use(middleware1);  // Runs 1st
httpPlugin.use(middleware2);  // Runs 2nd
httpPlugin.use(middleware3);  // Runs 3rd
// Route handler runs last
```

### Early return

Middleware can return early without calling `next()`:

```ts
import { json } from '@putnami/application';

export const maintenanceMode: HttpMiddleware = async (ctx, next) => {
  if (isMaintenanceMode()) {
    return json(
      { error: 'Service temporarily unavailable' },
      { status: 503 }
    );
  }
  return next();
};
```

### Modifying responses

Middleware can modify responses from downstream handlers:

```ts
export const addSecurityHeaders: HttpMiddleware = async (ctx, next) => {
  const response = await next();

  if (response) {
    response.setHeader('X-Content-Type-Options', 'nosniff');
    response.setHeader('X-Frame-Options', 'DENY');
    response.setHeader('X-XSS-Protection', '1; mode=block');
  }

  return response;
};
```

## Common middleware patterns

### CORS

```ts
import { CorsMiddleware, http, endpoint } from '@putnami/application';

http().use(CorsMiddleware({
  origin: 'https://app.example.com',
  credentials: true,
}));

export default endpoint()
  .cors({ origin: 'https://app.example.com', credentials: true })
  .handle(() => ({ ok: true }));
```

### CSRF protection

```ts
import { http } from '@putnami/application';

// Enable token-based CSRF (double-submit cookie) on the server
const server = http({ port: 3000, csrf: true });

// Or with options
const server2 = http({
  port: 3000,
  csrf: {
    headerName: 'X-CSRF-Token',
    secret: process.env.CSRF_SECRET,
  },
});
```

When CSRF is enabled, the `@putnami/web` client runtime **automatically injects the `X-CSRF-Token` header** on state-changing requests:

- **`<Form>` actions** — the token is read from the `_csrf` cookie and attached to every POST submission.
- **`useFetch` hook** — the token is injected for `POST`, `PUT`, `DELETE`, and `PATCH` requests. Safe methods (`GET`, `HEAD`, `OPTIONS`) are skipped.

No client-side code changes are required — enable `csrf: true` on the server and the React client handles the rest. For manual `fetch()` calls, read the token from `document.cookie` and include it in the `X-CSRF-Token` header.

### Rate limiting

```ts
import { RateLimitMiddleware, http, endpoint } from '@putnami/application';

http().use(RateLimitMiddleware({ windowMs: 60_000, max: 100 }));

export default endpoint()
  .rateLimit({ windowMs: 60_000, max: 10 })
  .handle(() => ({ ok: true }));
```

Custom store example (Redis/DB). Async stores MUST implement `consume` atomically (e.g. Redis
`INCR` + Lua, or a DB upsert with `RETURNING`); the `get`/`set` pair is a non-atomic fallback that
concurrent requests can bypass:

```ts
import { RateLimitMiddleware } from '@putnami/application';

const store = {
  async consume(key, limit, windowMs) {
    /* one atomic server-side op; return { allowed, remaining, resetAt } */
  },
  async get(key) {},
  async set(key, entry) {},
};

http().use(RateLimitMiddleware({ store }));
```

### Request timing

```ts
export const timing: HttpMiddleware = async (ctx, next) => {
  const start = performance.now();

  const response = await next();

  const duration = performance.now() - start;
  response?.setHeader('X-Response-Time', `${duration.toFixed(2)}ms`);

  return response;
};
```

### Error handling

```ts
import { json, internalServerError } from '@putnami/application';

export const errorHandler: HttpMiddleware = async (ctx, next) => {
  try {
    return await next();
  } catch (error) {
    console.error('Unhandled error:', error);

    if (error instanceof HttpException) {
      return json(
        { error: error.message },
        { status: error.status }
      );
    }

    return internalServerError('An unexpected error occurred');
  }
};
```

### Compression

```ts
import { CompressionMiddleware, http } from '@putnami/application';

http().use(CompressionMiddleware({
  threshold: 1024,
  encodings: ['gzip', 'deflate'],
}));
```

## Automatic HTTP methods

The HTTP plugin automatically handles `HEAD`, `OPTIONS`, and (optionally) `TRACE` requests. No explicit route registration is needed.

### Configuration

```ts
http({
  port: 3000,
  httpMethods: {
    head: true,      // default — derive HEAD from GET handlers
    options: true,   // default — respond with Allow header
    trace: false,    // default — disabled for security
  },
})
```

Disable all automatic methods:

```ts
http({ port: 3000, httpMethods: false })
```

| Option | Type | Default | Description |
|---|---|---|---|
| `head` | `boolean` | `true` | Derive HEAD responses from GET handlers. Static routes skip the handler and return pre-computed metadata; dynamic routes execute the handler and strip the body. |
| `options` | `boolean` | `true` | Respond with `204 No Content` and an `Allow` header listing every method registered for the path (plus `HEAD` and `OPTIONS` themselves). |
| `trace` | `boolean` | `false` | Echo the request back as `message/http`. Sensitive headers (`Cookie`, `Authorization`, `Proxy-Authorization`) are excluded. Disabled by default to prevent XST attacks. |

### HEAD

A `HEAD` request to a path that has a `GET` handler returns the status line and headers without the body.

**Static and cached content** — Routes with pre-computed `headMeta` (like static files) skip handler execution entirely and return known metadata: `Content-Type`, `Content-Length`, `Last-Modified`, `ETag`, `Cache-Control`, `Content-Encoding`, and `Vary`. `If-None-Match` → `304` is also handled without touching the body.

**Dynamic content** — When no `headMeta` is available, the framework executes the full middleware chain and GET handler, then strips the response body.

You can provide custom HEAD metadata via `RouteOptions`:

```ts
httpPlugin.route('GET', '/data', handler, {
  headMeta: (ctx) => new HttpResponse(undefined, {
    status: 200,
    headers: {
      'Content-Type': 'application/json',
      'Content-Length': String(size),
      ETag: etag,
    },
  }),
});
```

Return `undefined` from `headMeta` to fall back to handler execution.

### OPTIONS

An `OPTIONS` request returns `204` with an `Allow` header:

```
Allow: DELETE, GET, HEAD, OPTIONS, POST
```

When `CorsMiddleware` is active, it handles CORS preflight `OPTIONS` (requests carrying an `Origin` header). The automatic `OPTIONS` responder is a fallback for non-CORS clients.

### TRACE

TRACE is **disabled by default** because it can enable Cross-Site Tracing (XST) attacks. When enabled, the framework echoes the request back but always strips sensitive headers to reduce risk.

```ts
// Opt-in — not recommended in production
http({ port: 3000, httpMethods: { trace: true } })
```

### Explicit routes take precedence

If you register an explicit `HEAD`, `OPTIONS`, or `TRACE` route with `httpPlugin.route(method, path, handler)`, it takes precedence over the automatic behaviour.

## Route-level middleware

Apply middleware to specific routes in file-based routing:

```ts
// src/app/api/admin/get.ts
import { endpoint } from '@putnami/application';
import type { HttpMiddleware } from '@putnami/application';

export const middleware: HttpMiddleware[] = [
  requireAuth,
  requireAdmin,
];

export default endpoint(() => {
  return { admin: true };
});
```

## Request context

### Available properties

```ts
export async function handler(ctx: HttpRequestContext) {
  // Request information
  ctx.url;        // URL object
  ctx.method;     // HTTP method
  ctx.headers;    // Headers object
  ctx.req;        // Raw Bun Request

  // Route information
  ctx.route;      // Matched route pattern
  ctx.params;     // Route parameters
  ctx.statusCode; // Response status (modifiable)
  ctx.user;       // Authenticated user claims (when set by auth middleware)

  // Server
  ctx.server;     // Bun server instance
}
```

### Available methods

```ts
export async function handler(ctx: HttpRequestContext) {
  // URL helpers
  ctx.queryParams();  // URLSearchParams
  ctx.secured();      // true if HTTPS
  ctx.host();         // Host with port
  ctx.domain();       // Domain without port
  ctx.path();         // URL path
  ctx.query();        // Query string

  // Body
  const body = await ctx.body<MyType>();

  // Error
  ctx.throw(400, 'Bad request');
}
```

## Response handling

### HttpResponse and factory functions

```ts
import { HttpResponse, json, badRequest, unauthorized, forbidden, notFound, internalServerError } from '@putnami/application';

// JSON response
json({ data: 'value' });
json({ data: 'value' }, { status: 201 });

// Redirect
HttpResponse.redirect('/new-location');
HttpResponse.redirect('/new-location', 301);

// Error responses
badRequest('Invalid input');
unauthorized('Not authenticated');
forbidden('Not allowed');
notFound('Resource not found');
internalServerError('Server error');
```

### Custom responses

```ts
import { HttpResponse, json } from '@putnami/application';

// Create from Response
const response = new HttpResponse(new Response('Hello', {
  status: 200,
  headers: { 'Content-Type': 'text/plain' },
}));

// Modify headers
response.setHeader('X-Custom', 'value');
response.pushHeader('Set-Cookie', 'session=abc');

// Chain modifications
json({ ok: true })
  .setHeader('Cache-Control', 'no-store')
  .setHeader('X-Request-Id', requestId);
```

## Related guides

- [API](/docs/frameworks/typescript/api)
- [Plugins & Lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle)
