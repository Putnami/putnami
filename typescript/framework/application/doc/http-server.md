# HTTP Server

Complete guide to HTTP routing, middleware, and request handling.

## Overview

The HTTP plugin provides a powerful HTTP server with routing, middleware, and request/response handling built on Bun's native server.

## Basic Setup

```typescript
import { application, http, HttpPlugin } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .run(async () => {
    const httpPlugin = app.getPlugin(HttpPlugin);
    
    httpPlugin.get('/hello', () => {
      return { message: 'Hello, World!' };
    });
  });

await app.start();
```

## Route Registration

### HTTP Methods

Register routes for different HTTP methods:

```typescript
const httpPlugin = app.getPlugin(HttpPlugin);

httpPlugin.get('/users', () => ({ users: [] }));
httpPlugin.post('/users', async (ctx) => {
  const body = await ctx.body();
  return { created: body };
});
httpPlugin.put('/users/[id]', async (ctx) => {
  const id = ctx.params?.id;
  return { updated: id };
});
httpPlugin.delete('/users/[id]', async (ctx) => {
  const id = ctx.params?.id;
  return { deleted: id };
});
httpPlugin.patch('/users/[id]', async (ctx) => {
  const id = ctx.params?.id;
  return { patched: id };
});
```

### Generic Route Method

Use `route()` for any HTTP method:

```typescript
httpPlugin.route('GET', '/custom', handler);
httpPlugin.route('POST', '/custom', handler);
httpPlugin.route('OPTIONS', '/custom', handler);
```

### Route Matching

Bun's own router matches a `GET`, `POST`, `PUT`, `PATCH` or `DELETE` route
whose pattern holds only static segments and whole-segment parameters, when
no other route of that method can match the same path. The request then runs
the same middleware and handler as any other. The framework's router matches
everything else:

- `HEAD`, `OPTIONS`, and the 404 and 405 answers;
- every route of a method that has a wildcard, optional or suffixed pattern;
- a route whose path a `native()` route may share;
- every route of a server with rewrites;
- a request whose path Bun reads another way than its URL (an encoded
  parameter, a `.` or `..` segment, a leading `//`).

The pages of a React application, `api()` stream and WebSocket routes and
gRPC calls stay on the framework's router. Wrap a handler in `servedByFetch`
to keep it there too:

```typescript
import { servedByFetch } from '@putnami/application';

httpPlugin.get('/legacy/[id]', servedByFetch((ctx) => ({ id: ctx.params?.id })));
```

## Automatic HTTP Methods

The HTTP plugin automatically handles `HEAD`, `OPTIONS`, and (optionally) `TRACE` requests without requiring explicit route registration. This behaviour is enabled by default and can be configured or globally disabled.

### Configuration

```typescript
http({
  port: 3000,
  httpMethods: {
    head: true,      // default — derive HEAD from GET handlers
    options: true,   // default — respond with Allow header
    trace: false,    // default — disabled for security
  },
})
```

Pass `false` to disable all automatic methods:

```typescript
http({ port: 3000, httpMethods: false })
```

### HEAD

When a `HEAD` request matches a path that has a `GET` handler, the framework returns the status line and headers without the body.

**Static and cached content** — Routes registered with pre-computed `headMeta` (e.g. static files) skip handler execution entirely. The response is built from known metadata: `Content-Type`, `Content-Length`, `Last-Modified`, `ETag`, `Cache-Control`, `Content-Encoding`, and `Vary`. This also handles `If-None-Match` → `304 Not Modified` without touching the file body.

**Dynamic content** — When no `headMeta` is available, the framework executes the full GET handler and middleware chain so dynamic headers like `Content-Length` are computed, then strips the response body.

If you register an explicit `HEAD` route, it takes precedence over the automatic behaviour.

#### Custom HEAD metadata

Provide a `headMeta` callback in `RouteOptions` to short-circuit HEAD requests:

```typescript
httpPlugin.route('GET', '/data', handler, {
  headMeta: (ctx) => {
    if (ctx.headers.get('If-None-Match') === etag) {
      return new HttpResponse(undefined, { status: 304, headers: { ETag: etag } });
    }
    return new HttpResponse(undefined, {
      status: 200,
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': String(size),
        ETag: etag,
        'Cache-Control': 'public, max-age=3600',
      },
    });
  },
});
```

When `headMeta` returns `undefined`, the framework falls back to running the handler.

### OPTIONS

When an `OPTIONS` request arrives for a path that has registered handlers, the framework responds with `204 No Content` and an `Allow` header listing every method available at that path:

```
Allow: DELETE, GET, HEAD, OPTIONS, POST
```

The automatic response cooperates with the CORS middleware: when `CorsMiddleware` is active, it handles preflight `OPTIONS` requests (with an `Origin` header) itself. The auto-`OPTIONS` logic acts as a fallback for non-CORS clients.

### TRACE

`TRACE` is disabled by default because it can enable Cross-Site Tracing (XST) attacks. When explicitly enabled, the framework echoes the received request back as `message/http`. Sensitive headers (`Cookie`, `Authorization`, `Proxy-Authorization`, `Set-Cookie`) are always stripped from the echo to prevent information leaks.

```typescript
// Opt-in
http({ port: 3000, httpMethods: { trace: true } })
```

## Route Parameters

### Path Parameters

Use `[param]` syntax for path parameters:

```typescript
// Single parameter
httpPlugin.get('/users/[id]', (ctx) => {
  const userId = ctx.params?.id;
  return { userId };
});

// Multiple parameters
httpPlugin.get('/users/[userId]/posts/[postId]', (ctx) => {
  const { userId, postId } = ctx.params || {};
  return { userId, postId };
});
```

### Query Parameters

Access query parameters:

```typescript
httpPlugin.get('/search', (ctx) => {
  const query = ctx.queryParams();
  const q = query.get('q') || '';
  const page = query.get('page') || '1';
  
  return { q, page };
});
```

## Request Context

The `HttpRequestContext` provides access to request data:

```typescript
import type { HttpRequestContext } from '@putnami/application';

httpPlugin.get('/example', async (ctx: HttpRequestContext) => {
  // Request object
  const method = ctx.req.method;
  const url = ctx.req.url;
  
  // Parsed data
  const body = await ctx.body<{ name: string }>();
  const query = ctx.queryParams();
  const params = ctx.params;
  
  // Headers
  const headers = ctx.headers;
  const userAgent = ctx.headers.get('user-agent');
  
  // URL helpers
  const path = ctx.path();
  const host = ctx.host();
  const isSecure = ctx.secured();
  
  return { method, path, host };
});
```

The accessors (`body`, `queryParams`, `path`, `query`, `host`, `domain`,
`secured`) are built the first time a request reads them, and keep working
once destructured. A spread of the context (`{ ...ctx }`) copies its own
fields only, so it drops them: pass the context itself, or read the values
you need first.

### Context Properties

| Property | Type | Description |
|----------|------|-------------|
| `req` | `Request` | Native Fetch API Request object |
| `params` | `Record<string, string>` | Path parameters |
| `headers` | `Headers` | Request headers |
| `user` | `Record<string, unknown> \| undefined` | Authenticated user claims (when set by auth middleware) |
| `body<T>()` | `Promise<T>` | Parse the JSON, form or multipart body; a JSON body that does not parse throws a 400 Bad Request |
| `formData()` | `Promise<FormData>` | Parse form data |
| `queryParams()` | `URLSearchParams` | Query parameters |
| `path()` | `string` | URL path |
| `host()` | `string` | Request host |
| `secured()` | `boolean` | Is HTTPS? |
| `signal` | `AbortSignal` | Aborts when the request ends: its client left or it passed `requestTimeoutMs` |
| `deadlineAt` | `number \| undefined` | Epoch milliseconds at which the request times out |

### Request timeout

`http({ requestTimeoutMs })` bounds each request. Past it, the server answers
408 and aborts `ctx.signal` with a `TimeoutError`, so the request's work stops:
the queries of the database session store, the calls of a generated client
built without a service binding, and the server render. Pass `ctx.signal` to
your own long waits. The request ends at `deadlineAt`, even when a client call
bound by that deadline stops a moment before the signal aborts. The deadline
holds until the response is sent: a stream that runs on after it is no longer
held to `deadlineAt`, and the timeout no longer aborts its signal.

A handler that stops because its request ended logs and records no error: the
request answers 408. A stop is the signal's own reason, an `AbortError`, a
`TimeoutError`, a `QueryAbortError` or a `ClientCanceledError`, thrown directly
or as a `cause`, and a `ClientDeadlineError` when the request reached its
deadline. Any other error is logged, recorded and passed to `onError` as it
would be during the request, and the request still answers 408.

## Response Handling

### Automatic Response Wrapping

Handlers can return plain values - they're automatically wrapped:

```typescript
// Objects → JSON response (200)
httpPlugin.get('/users', () => ({ users: [] }));

// Strings → text response (200)
httpPlugin.get('/health', () => 'OK');

// Numbers → text response (200)
httpPlugin.get('/count', () => 42);
```

A raw route that returns `undefined` has not answered: the next route that matches the request runs, and the request answers 404 when none does. An [`endpoint()`](./endpoint-builder.md) served by `api()` is the operation its path names, so its `undefined` answers 204 No Content instead.

### HttpResponse

Use `HttpResponse` for full control. You can use either the class-based API or standalone factory functions:

```typescript
import { HttpResponse, json, redirect, notFound, unauthorized, forbidden } from '@putnami/application';

// JSON responses — standalone function
httpPlugin.get('/data', () => {
  return json({ data: 'value' });
});

httpPlugin.post('/create', () => {
  return json({ created: true }, { status: 201 });
});

// Redirects — standalone function
httpPlugin.get('/old', () => {
  return redirect('/new');
});

httpPlugin.get('/permanent', () => {
  return redirect('/new', 301);
});

// Error responses — standalone functions
httpPlugin.get('/not-found', () => {
  return notFound();
});

httpPlugin.get('/unauthorized', () => {
  return unauthorized();
});

httpPlugin.get('/forbidden', () => {
  return forbidden();
});

// Custom headers — class-based API for chaining
httpPlugin.get('/custom', () => {
  return json({ data: 'value' })
    .setHeader('X-Custom', 'header');
});
```

The standalone factory functions (`json()`, `redirect()`, `notFound()`, etc.) return `HttpResponse` instances, so you can chain `.setHeader()` on them just like the class-based API.

### HttpResponse Methods

| Method | Description |
|--------|-------------|
| `json(data, options?)` | JSON response |
| `redirect(url, status?)` | Redirect response |
| `notFound()` | 404 response |
| `unauthorized()` | 401 response |
| `forbidden()` | 403 response |
| `badRequest()` | 400 response |
| `noContent()` | 204 response |
| `.setHeader(name, value)` | Add custom header |

## Middleware

Middleware intercepts requests and responses. Register middleware with `use()`:

```typescript
import type { HttpMiddleware } from '@putnami/application';

// Custom middleware
const loggingMiddleware: HttpMiddleware = async (ctx, next) => {
  const start = Date.now();
  console.log(`[${ctx.req.method}] ${ctx.path()}`);
  
  const response = await next();
  
  const duration = Date.now() - start;
  console.log(`Completed in ${duration}ms`);
  
  return response;
};

httpPlugin.use(loggingMiddleware);
```

### Middleware Order

Middleware is executed in registration order:

```typescript
httpPlugin
  .use(middleware1)  // First
  .use(middleware2)  // Second
  .use(middleware3); // Third

// Execution order:
// middleware1 → middleware2 → middleware3 → handler → middleware3 → middleware2 → middleware1
```

### Prepend Middleware

Add middleware to the beginning of the chain:

```typescript
httpPlugin.prepend(importantMiddleware);
```

### Built-in Middleware

#### Logger Middleware

Emits one completion log per request. Method, route, status, and duration are
stored under the entry's `http` context together with fields accumulated by
`logger.with()` during handling:

```typescript
import { LoggerMiddleware } from '@putnami/application';

httpPlugin.use(LoggerMiddleware());
```

The completion log is written at `error` for a 5xx answer with a recorded
error and for an error that escapes the chain, and at `debug` otherwise. Every
record a logger writes during the request carries the request's `method` and
`routePath` under `http`; the completion log adds `status`, `durationMs` and
`outcome`. That context is built when the first record of the request is
written: below `debug`, a request that ends without an error writes no record
and builds no context.

#### Trace Middleware

Adds request tracing:

```typescript
import { TraceMiddleware } from '@putnami/application';

httpPlugin.use(TraceMiddleware());
```

#### Security Headers

A secure server (the default) adds hardened response headers to every
pipeline response. A response that is not JSON gets `X-Content-Type-Options:
nosniff`, `X-Frame-Options: DENY`, a self-only `Content-Security-Policy`,
`Referrer-Policy`, `Permissions-Policy` and `X-XSS-Protection: 0`. A JSON
response (`application/json` or a `+json` type) is never rendered as a
document, so it gets only `X-Content-Type-Options: nosniff` and
`Content-Security-Policy: frame-ancestors 'none'`. Both add
`Strict-Transport-Security` over HTTPS, and both carry every header you set
explicitly through `securityHeaders`:

```typescript
import { http } from '@putnami/application';

http({ securityHeaders: { frameOptions: 'SAMEORIGIN', referrerPolicy: false } });
```

#### Session Loading

With an asynchronous session store (`session.store: database`), the server
loads the request's session before every middleware but the outermost ones
(telemetry), and writes it back after the response. The server reads
`session.store` once, when it starts: an app on the default cookie store runs
no session middleware. Changing `session.store` needs a restart; until then, a
switch from the cookie store to an asynchronous one makes session calls fail.

### Middleware Examples

#### Authentication Middleware

```typescript
import { unauthorized } from '@putnami/application';

const authMiddleware: HttpMiddleware = async (ctx, next) => {
  const token = ctx.headers.get('authorization');

  if (!token) {
    return unauthorized();
  }
  
  // Verify token, set user context, etc.
  (ctx as any).user = await verifyToken(token);
  
  return next();
};

httpPlugin.use(authMiddleware);
```

#### CORS Middleware

```typescript
import { CorsMiddleware } from '@putnami/application';

httpPlugin.use(CorsMiddleware({
  origin: 'https://app.example.com',
  credentials: true,
}));
```

See `security.md` for CSRF, rate limiting, and compression middleware.

#### Error Handling Middleware

```typescript
import { json } from '@putnami/application';

const errorMiddleware: HttpMiddleware = async (ctx, next) => {
  try {
    return await next();
  } catch (error) {
    console.error('Request error:', error);
    return json(
      { error: 'Internal server error' },
      { status: 500 }
    );
  }
};

httpPlugin.use(errorMiddleware);
```

## Route Options

Routes can have additional options:

```typescript
httpPlugin.get('/page', handler, {
  // Media types this route answers. A request matches when it accepts one of
  // them, accepts anything (`*/*` or no Accept), or the route lists `*/*`
  // (that match runs after every other route that matches the request).
  accept: ['text/html', '*/*'],
});
```

| Option | Meaning |
|--------|---------|
| `accept` | Media types the route answers. Without it the route answers any `Accept`. A route that lists `*/*` answers any `Accept` too, but a match through its `*/*` alone runs after every other route that matches the request: a route with `accept: ['text/markdown']` answers `Accept: text/markdown` first, and the `['text/html', '*/*']` route answers everything else. |
| `statusCode` | Set on `ctx.statusCode` before the handler runs |
| `csrfExempt` | Skip CSRF token validation (set for `api()` routes) |
| `minimal` | Skip the always-on security middleware for a trusted hot path |
| `securityHeaders` | `false` skips only the secure response headers |

## Server Access

Get the underlying Bun server:

```typescript
const httpPlugin = app.getPlugin(HttpPlugin);
const server = httpPlugin.getServer();

// Access server properties
console.log(server.port);
```

## WebSocket Support

The HTTP plugin also supports WebSocket connections. See [WebSockets](websockets.md) for details.

## Error Handling

### Global Error Handler (`onError`)

Register a global error handler via the `http()` options. The handler is called whenever a route handler throws, except when it throws the stop of a request that already ended (see [Request timeout](#request-timeout)). Return a value to use as the response body (it will be content-negotiated), or `undefined` to fall through to the default error handling.

The handler may be synchronous or asynchronous — async handlers are properly awaited.

```typescript
import { http } from '@putnami/application';

http({
  port: 3000,
  onError: async (error, ctx) => {
    // Report to external service
    await reportToSentry(error);
    // Return a custom error body (negotiated to JSON by default)
    return { message: 'Something went wrong', traceId: ctx.traceId };
  },
});
```

**Behaviour:**

| Handler returns | Result |
|---|---|
| A value (object, string, `HttpResponse`) | Used as the response body with status 500 |
| `undefined` | Falls through to default error handling |
| Throws | Logged, then falls through to default error handling |

### HttpException

Throw `HttpException` for HTTP errors:

```typescript
import { HttpException } from '@putnami/runtime';

httpPlugin.get('/error', () => {
  throw new HttpException('Resource not found', 404);
});
```

### Dev Error Pages

In development (`NODE_ENV !== 'production'`), when a handler throws and the request accepts `text/html`, the framework renders a rich HTML error overlay instead of raw JSON. The overlay includes:

- Error name and message with HTTP status badge
- **Source context** — the file and surrounding lines where the error originated
- **Stack trace** — every frame with clickable `vscode://` links
- **Request details** — method, URL, route pattern, params, and headers

API clients (e.g. `curl` with `Accept: application/json`) also receive error details in development — the JSON response includes the error message and stack trace. To suppress error details in non-production environments, set `PUTNAMI_DEV_ERRORS=false`.

In production the dev error page is never rendered — behaviour is unchanged.

```
GET /users/42  →  handler throws TypeError
Browser (Accept: text/html)  →  rich HTML error page
curl (Accept: application/json)  →  { "statusCode": 500, ... }
```

The helpers `isDevHtmlRequest(ctx)` and `renderDevErrorPage(error, ctx)` are exported from `@putnami/application` for use in custom error handlers:

```typescript
import { http, isDevHtmlRequest, renderDevErrorPage } from '@putnami/application';

http({
  port: 3000,
  onError: async (error, ctx) => {
    await reportToSentry(error);
    // Fall through to the built-in dev error page
    if (isDevHtmlRequest(ctx)) {
      return renderDevErrorPage(error, ctx);
    }
    return { message: 'Something went wrong' };
  },
});
```

### Try-Catch in Handlers

```typescript
import { json } from '@putnami/application';

httpPlugin.get('/safe', async (ctx) => {
  try {
    const data = await fetchData();
    return { data };
  } catch (error) {
    return json(
      { error: error.message },
      { status: 500 }
    );
  }
});
```

## Best Practices

1. **Use HttpResponse for errors** - Consistent error responses
2. **Validate input early** - Check parameters and body in middleware
3. **Handle async errors** - Use try-catch or error middleware
4. **Keep handlers focused** - Extract business logic to services
5. **Use middleware for cross-cutting concerns** - Auth, logging, CORS

## Next Steps

- Learn about [File-based Routing](file-based-routing.md) for automatic route discovery
- Explore [Middleware](http-server.md#middleware) for request interception
- Check [API Reference](api-reference.md) for complete HTTP API
