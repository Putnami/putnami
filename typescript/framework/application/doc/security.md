# API Security

Built-in middleware for CORS, CSRF protection, rate limiting, and response compression. Each feature works as a global middleware on `HttpPlugin` or as a per-route builder step on the `endpoint()` API.

## CORS

Cross-Origin Resource Sharing controls which origins can access your API.

### Global middleware

```ts
import { http, CorsMiddleware } from '@putnami/application';

const server = http({ port: 3000 })
  .use(CorsMiddleware({
    origin: ['https://app.example.com', 'https://admin.example.com'],
    methods: ['GET', 'POST', 'PUT', 'DELETE'],
    allowedHeaders: ['Content-Type', 'Authorization'],
    exposedHeaders: ['X-Request-Id'],
    credentials: true,
    maxAge: 3600,
  }));
```

### Per-route builder

```ts
import { endpoint } from '@putnami/application';

export default endpoint()
  .cors({ origin: 'https://app.example.com', credentials: true })
  .handle(() => ({ data: 'accessible cross-origin' }));
```

### Options

| Option | Type | Default | Description |
|---|---|---|---|
| `origin` | `string \| string[] \| (origin: string) => boolean` | `'*'` | Allowed origins. `'*'` allows all. |
| `methods` | `string[]` | `['GET','HEAD','PUT','PATCH','POST','DELETE']` | Allowed HTTP methods. |
| `allowedHeaders` | `string[]` | Reflects request | Headers the client may send. |
| `exposedHeaders` | `string[]` | none | Headers exposed to the client. |
| `credentials` | `boolean` | `false` | Allow credentials (cookies, auth headers). |
| `maxAge` | `number` | none | Preflight cache duration in seconds. |

Preflight `OPTIONS` requests are handled automatically with a `204` response.

> **Note:** The framework also provides an automatic `OPTIONS` responder that returns an `Allow` header listing every method registered for a path. When `CorsMiddleware` is active it handles preflight requests (those carrying an `Origin` header) itself; the auto-`OPTIONS` response acts as a fallback for non-CORS clients. See [HTTP Server — Automatic HTTP Methods](http-server.md#automatic-http-methods).

---

## TRACE

The HTTP `TRACE` method is **disabled by default** because it can enable Cross-Site Tracing (XST) attacks — a technique where an attacker's script issues a `TRACE` request and reads back `HttpOnly` cookies or `Authorization` headers from the echoed response.

When explicitly enabled via `httpMethods: { trace: true }`, the framework strips sensitive headers (`Cookie`, `Authorization`, `Proxy-Authorization`, `Set-Cookie`) from the echo to reduce the risk. Even so, only enable TRACE in environments where it is genuinely needed (e.g. local debugging).

```ts
import { http } from '@putnami/application';

// Opt-in — NOT recommended in production
http({ port: 3000, httpMethods: { trace: true } });
```

See [HTTP Server — Automatic HTTP Methods](http-server.md#automatic-http-methods) for the full configuration surface.

---

## Origin Guard

Built-in cross-origin request protection that validates `Origin` and `Sec-Fetch-Site` headers on state-changing requests. **Enabled by default** — no configuration required.

### How it works

On every `POST`, `PUT`, `DELETE`, and `PATCH` request the server checks:

1. **`Sec-Fetch-Site` header** (sent by all modern browsers):
   - `same-origin` or `none` — allowed
   - `same-site` — allowed by default (subdomains of the same registrable domain)
   - `cross-site` — rejected with `403` unless the origin is trusted
2. **Fallback to `Origin` vs `Host`** (when `Sec-Fetch-Site` is absent):
   - Origin host matches server host — allowed
   - Mismatch — rejected with `403` unless the origin is trusted
   - No `Origin` header — allowed (non-browser client like curl or server-to-server)

Safe methods (`GET`, `HEAD`, `OPTIONS`) are never checked.

### Configuration

```ts
import { http } from '@putnami/application';

http({
  port: 3000,
  originGuard: {
    trustedOrigins: ['https://admin.example.com'],  // allow cross-origin from these
    allowSameSite: true,   // allow same-site requests (default)
    mode: 'enforce',       // 'enforce' (default) or 'report'
  },
});
```

### Disabling

```ts
http({ port: 3000, originGuard: false });
```

### Report mode

Use `mode: 'report'` to log violations without blocking requests. Useful for gradual rollout:

```ts
http({ port: 3000, originGuard: { mode: 'report' } });
```

### Options

| Option | Type | Default | Description |
|---|---|---|---|
| `trustedOrigins` | `string[]` | none | Full origin strings to trust (e.g. `'https://admin.example.com'`). |
| `allowSameSite` | `boolean` | `true` | Allow requests from subdomains of the same registrable domain. |
| `mode` | `'enforce' \| 'report'` | `'enforce'` | Block violations or log and allow through. |

### Relationship to CSRF tokens

The origin guard provides zero-config protection that covers most CSRF attacks. For additional defense-in-depth (financial apps, admin panels), enable the double-submit cookie via `http({ csrf: true })`:

| Layer | Protection | Configuration |
|---|---|---|
| Origin Guard (default) | Origin + Sec-Fetch-Site + SameSite | None — always on |
| CSRF tokens (opt-in) | Signed double-submit cookie | `http({ csrf: true })` or `http({ csrf: { secret: '...' } })` |

---

## CSRF Protection (Token-Based)

Double-submit cookie pattern for defense-in-depth on top of the built-in origin guard.

### Enabling CSRF protection

```ts
import { http } from '@putnami/application';

// Simple — enable with defaults
const server = http({ port: 3000, csrf: true });

// With options
const server = http({
  port: 3000,
  csrf: {
    cookieName: '_csrf',
    headerName: 'X-CSRF-Token',
    sameSite: 'Strict',
    secret: process.env.CSRF_SECRET,
  },
});
```

The CSRF cookie is `Secure` by default. For plain HTTP local development, opt out explicitly with `csrf: { secure: false }`; do not use that override in production.

### How it works

1. On the first matched safe request without a valid token, the middleware sets a `_csrf` cookie (readable by JS — not `HttpOnly`). A valid incoming cookie is reused without rewriting it.
2. Safe methods (`GET`, `HEAD`, `OPTIONS`) pass through without validation.
3. Unsafe methods (`POST`, `PUT`, `DELETE`, `PATCH`) must include the token either in the `X-CSRF-Token` header **or** in a form body field named `_csrf`. The header is checked first; the body field is used as a fallback for plain HTML form submissions.
4. Mismatched or missing tokens result in `403 Forbidden`.
5. Form-body token extraction enforces the server's `maxBodySizeBytes` limit (default 1 MiB) before buffering: an oversized form body is rejected with `413 Payload Too Large` — the same guard `ctx.body()` applies. Override per middleware with `csrf: { maxBodySizeBytes }`.

### Client integration

When CSRF protection is enabled, the `@putnami/web` client runtime **automatically injects the `X-CSRF-Token` header** on state-changing requests. No manual setup is needed for:

- **Form actions** — the `<Form>` component's client-side action handler reads the `_csrf` cookie and attaches the token to every POST submission.
- **`useFetch` hook** — for `POST`, `PUT`, `DELETE`, and `PATCH` requests, the hook reads the `_csrf` cookie and adds the `X-CSRF-Token` header automatically. Safe methods (`GET`, `HEAD`, `OPTIONS`) skip token injection.

**No code changes are required in your pages or components** — enable `http({ csrf: true })` on the server and the client handles the rest.

#### Manual requests

For `fetch()` calls outside of `useFetch` or `<Form>`, read the token from the cookie manually:

```js
const csrfToken = document.cookie
  .split('; ')
  .find(c => c.startsWith('_csrf='))
  ?.split('=')[1];

fetch('/api/submit', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'X-CSRF-Token': csrfToken,
  },
  credentials: 'same-origin',
  body: JSON.stringify({ data: 'value' }),
});
```

### API routes are automatically exempt

Routes registered via `api()` are **automatically excluded** from CSRF validation. API endpoints are designed for programmatic access (CLIs, backends, SPAs) where clients authenticate with `Authorization` headers or client credentials — not browser cookies. CSRF protection is unnecessary and harmful for these routes.

```ts
import { application, http, api } from '@putnami/application';

application()
  .use(http({ csrf: true }))      // CSRF enabled globally
  .use(api());                     // api() routes bypass CSRF automatically
```

In this setup:
- **Web routes** (registered on `http()` directly or via `react()`) require CSRF tokens on `POST`/`PUT`/`DELETE`/`PATCH`.
- **API routes** (registered via `api()`) are exempt — external clients can call them without a CSRF token.

This is the correct default for OAuth2 authorization servers: browser-submitted consent/logout forms are CSRF-protected, while token/register/revoke/introspect endpoints work for all HTTP clients.

#### Per-route opt-out

For routes registered directly on the `http()` plugin, use the `csrfExempt` route option:

```ts
const server = http({ port: 3000, csrf: true });
server.post('/token', tokenHandler, { csrfExempt: true });
```

### Options

| Option | Type | Default | Description |
|---|---|---|---|
| `cookieName` | `string` | `'_csrf'` | Cookie name for the CSRF token. |
| `headerName` | `string` | `'X-CSRF-Token'` | Header the client must send. |
| `fieldName` | `string` | `'_csrf'` | Form body field name accepted as fallback. |
| `ignoreMethods` | `string[]` | `['GET','HEAD','OPTIONS']` | Methods exempt from validation. |
| `sameSite` | `'Strict' \| 'Lax' \| 'None'` | `'Strict'` | Cookie `SameSite` attribute. |
| `secure` | `boolean` | `true` | Restrict cookie to HTTPS. Set to `false` only for plain HTTP local development. |
| `storage` | `'cookie' \| 'memory'` | `'cookie'` | Storage strategy for CSRF tokens. |
| `store` | `CsrfStore` | none | Custom store (Redis/DB) for CSRF tokens. |
| `secret` | `string` | none | Sign cookie values to prevent tampering. |
| `ttlMs` | `number` | `3600000` | Token TTL for memory/custom stores. |
| `cookieMaxAge` | `number` | none | `Max-Age` for CSRF cookie (seconds). |

---

## Rate Limiting

Sliding-window rate limiting per client key.

### Global middleware

```ts
import { http, RateLimitMiddleware } from '@putnami/application';

const server = http({ port: 3000 })
  .use(RateLimitMiddleware({
    windowMs: 60_000,   // 1 minute
    max: 100,           // 100 requests per window
  }));
```

### Per-route builder

```ts
import { endpoint } from '@putnami/application';

// Strict limit on a sensitive endpoint
export default endpoint()
  .rateLimit({ max: 5, windowMs: 60_000 })
  .body({ email: String, password: String })
  .handle(async (ctx) => {
    const body = await ctx.body();
    return login(body.email, body.password);
  });
```

### Response headers

When `headers: true` (default), every response includes:

```
RateLimit-Limit: 100
RateLimit-Remaining: 97
RateLimit-Reset: 1706745600
```

When the limit is exceeded, the response is `429 Too Many Requests` with a `Retry-After` header.

### Custom key generator

By default, the rate limiter uses the client IP from `X-Forwarded-For` or `X-Real-IP`. You can customize this:

```ts
RateLimitMiddleware({
  max: 50,
  keyGenerator: (ctx) => ctx.headers.get('Authorization') || 'anonymous',
})
```

### Custom store (Redis/DB)

Async stores MUST implement `consume` — it applies the whole token accounting as one atomic
operation per key. With only `get`/`set`, the middleware falls back to a non-atomic
read-modify-write that spans an `await`, so concurrent requests for the same key can each read the
same token count and bypass the limit. Keep the atomicity server-side: Redis `INCR` +
`PEXPIRE ... NX` (or a Lua script), or a database upsert with `RETURNING` — never a client-side
get → mutate → set.

```ts
import { RateLimitMiddleware } from '@putnami/application';

const store = {
  async consume(key, limit, windowMs) {
    // Single atomic operation in Redis/DB, e.g. INCR + PEXPIRE NX or an upsert ... RETURNING.
    // Return { allowed, remaining, resetAt }.
  },
  async get(key) {
    // lookup in Redis/DB (non-atomic fallback; only safe for synchronous stores)
  },
  async set(key, entry) {
    // persist in Redis/DB
  },
  async delete(key) {
    // optional cleanup
  },
};

RateLimitMiddleware({
  windowMs: 60_000,
  max: 100,
  store,
});
```

### Options

| Option | Type | Default | Description |
|---|---|---|---|
| `windowMs` | `number` | `60_000` | Time window in milliseconds. |
| `max` | `number` | `100` | Max requests per window per key. |
| `keyGenerator` | `(ctx) => string` | Client IP | Function to derive the rate-limit key. |
| `message` | `string \| object` | `{ error: 'Too Many Requests' }` | Response body when limited. |
| `headers` | `boolean` | `true` | Include `RateLimit-*` response headers. |
| `storage` | `'memory'` | `'memory'` | Storage strategy for rate-limit entries. |
| `store` | `RateLimitStore` | none | Custom store (Redis/DB) for rate-limit entries. |

---

## Response Compression

Automatic gzip/deflate compression for compressible response types.

### Global middleware

```ts
import { http, CompressionMiddleware } from '@putnami/application';

const server = http({ port: 3000 })
  .use(CompressionMiddleware({
    threshold: 1024,             // minimum bytes before compressing
    encodings: ['gzip', 'deflate'],
  }));
```

### How it works

1. Checks the response `Content-Type` against compressible types (`application/json`, `text/html`, `text/css`, `application/javascript`, `image/svg+xml`, etc.).
2. Negotiates encoding from the request's `Accept-Encoding` header.
3. If the response body exceeds the threshold, compresses and sets `Content-Encoding` and `Vary: Accept-Encoding`.
4. Existing response headers (CORS, cookies, rate limit, custom) are preserved.
5. Responses already encoded or below the threshold pass through unchanged.

### Options

| Option | Type | Default | Description |
|---|---|---|---|
| `threshold` | `number` | `1024` | Minimum body size in bytes before compressing. |
| `encodings` | `('gzip' \| 'deflate')[]` | `['gzip', 'deflate']` | Preferred encodings in priority order. |

---

## Combining Features

All security middleware can be combined on the endpoint builder:

```ts
import { endpoint, Uuid, Email } from '@putnami/application';

export default endpoint()
  .params({ id: Uuid })
  .body({ email: Email })
  .cors({ origin: 'https://app.example.com' })
  .rateLimit({ max: 10, windowMs: 60_000 })
  .handle(async (ctx) => {
    const body = await ctx.body();
    return updateUser(ctx.params.id, body.email);
  });
```

Or stack them globally:

```ts
import { http, CorsMiddleware, CsrfMiddleware, RateLimitMiddleware, CompressionMiddleware } from '@putnami/application';

const server = http({ port: 3000 })
  .use(CompressionMiddleware())
  .use(CorsMiddleware({ origin: 'https://app.example.com' }))
  .use(CsrfMiddleware())
  .use(RateLimitMiddleware({ max: 100 }));
```

Middleware executes in registration order. The **origin guard** runs automatically before all user middleware and does not need to be registered. A recommended ordering for the rest:

1. **Compression** — wraps the final response
2. **CORS** — handles preflight early
3. **CSRF** — validates tokens before reaching handlers (opt-in defense-in-depth)
4. **Rate limiting** — rejects excess traffic before expensive work

---

## Access Control (`.secure()`)

The `.secure()` builder step enforces authentication and authorization on endpoints, loaders, and actions. It works with the **global identity resolver** added by the OAuth plugin — `ctx.user` is already populated before `.secure()` runs.

### Require any authenticated user

```ts
import { endpoint } from '@putnami/application';

export default endpoint()
  .secure()
  .handle((ctx) => ({ user: ctx.user }));
```

Returns `401 Unauthorized` when `ctx.user` is `undefined`.

### Declarative options

```ts
export default endpoint()
  .secure({
    roles: ['admin'],              // require ALL listed roles
    rolesAny: ['admin', 'editor'], // require ANY listed role
    scopes: ['write'],             // require ALL listed scopes
    scopesAny: ['read', 'write'],  // require ANY listed scope
    client: 'my-app',             // restrict to a specific client ID
  })
  .handle(() => ({ ok: true }));
```

Returns `403 Forbidden` when the user exists but lacks the required permissions.

### Custom guard function

For complex authorization logic, pass a function instead of options:

```ts
export default endpoint()
  .secure((user, ctx) => user.orgId === ctx.params?.orgId)
  .handle((ctx) => ({ data: 'org-scoped' }));
```

The guard receives the user claims and request context. Return `true` to allow, `false` to deny with `403`.

### Module-level security

Protect all endpoints in a module:

```ts
import { module, api } from '@putnami/application';

const adminModule = module('admin')
  .secure({ roles: ['admin'] })
  .use(api());
```

All API handlers registered by plugins in this module are automatically protected.

> **Note:** Module-level `.secure()` currently applies to `api()` routes only. React pages and static files served via `react()` or `staticFiles()` within the same module are **not** covered. Use `.secure()` on individual `page()`, `layout()`, or `loader()` builders to protect React routes.

### Works on all builders

| Builder | Method | Package |
|---|---|---|
| `endpoint()` | `.secure()` | `@putnami/application` |
| `loader()` | `.secure()` | `@putnami/web` |
| `action()` | `.secure()` | `@putnami/web` |
| `page()` | `.secure()` | `@putnami/web` |
| `layout()` | `.secure()` | `@putnami/web` |
| `middleware()` | `.secure()` | `@putnami/web` |
| `module()` | `.secure()` | `@putnami/application` |

### SecurityOptions reference

| Option | Type | Description |
|---|---|---|
| `roles` | `string[]` | Require ALL of these roles. |
| `rolesAny` | `string[]` | Require ANY of these roles. |
| `scopes` | `string[]` | Require ALL of these scopes. |
| `scopesAny` | `string[]` | Require ANY of these scopes. |
| `client` | `string \| string[]` | Expected client id(s) — checked against `azp`, `client_id`, `aud`. |
| `issuer` | `string \| string[]` | Expected issuer claim(s). |
| `audience` | `string \| string[]` | Expected audience claim(s). |
| `scopeClaim` | `string \| string[]` | Override the claim path(s) used to resolve scopes. |
| `roleClaim` | `string \| string[]` | Override the claim path(s) used to resolve roles. |

See [OAuth](oauth.md) for identity resolution and provider configuration.
