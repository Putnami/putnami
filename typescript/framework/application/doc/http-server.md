# HTTP Server

Complete guide to HTTP routing, middleware, and request handling.

## Overview

The HTTP plugin provides a powerful HTTP server with routing, middleware, and request/response handling built on Bun's native server.

## Basic Setup

```typescript
import { application, http, HttpPlugin } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .run(async (app) => {
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

### Context Properties

| Property | Type | Description |
|----------|------|-------------|
| `req` | `Request` | Native Fetch API Request object |
| `params` | `Record<string, string>` | Path parameters |
| `headers` | `Headers` | Request headers |
| `user` | `Record<string, unknown> \| undefined` | Authenticated user claims (when set by auth middleware) |
| `body<T>()` | `Promise<T>` | Parse JSON body |
| `formData()` | `Promise<FormData>` | Parse form data |
| `queryParams()` | `URLSearchParams` | Query parameters |
| `path()` | `string` | URL path |
| `host()` | `string` | Request host |
| `secured()` | `boolean` | Is HTTPS? |

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

#### Trace Middleware

Adds request tracing:

```typescript
import { TraceMiddleware } from '@putnami/application';

httpPlugin.use(TraceMiddleware());
```

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
httpPlugin.get('/protected', handler, {
  // Route-specific options
});
```

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

Register a global error handler via the `http()` options. The handler is called whenever a route handler throws. Return a value to use as the response body (it will be content-negotiated), or `undefined` to fall through to the default error handling.

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
import { HttpException } from '@putnami/application';

httpPlugin.get('/error', () => {
  throw new HttpException(404, 'Resource not found');
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
