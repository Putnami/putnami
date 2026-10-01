# API Reference

Complete API documentation for `@putnami/application`.

## Module

### `Module`

Base class for composable units. Groups plugins, sub-modules, and DI providers. Application extends Module with a startable lifecycle.

#### Constructor

```typescript
new Module(name: string)
```

#### Factory

```typescript
module(name: string): Module
```

```typescript
composeModules(modules: Module[], options?: { name?: string; detectDuplicates?: boolean }): ComposedModule
```

`composeModules()` returns a module that preserves the supplied modules as
children for plugin discovery, but mounts their DI providers as one container.
Requirements from one child can be satisfied by providers from another child;
duplicate provider tokens across the composed subtree throw by default.

#### Methods

##### `path(value: string): this`

Set the base path for this module. Child plugins (`api()`, `react()`, `staticFiles()`) inherit this path as their route prefix unless they specify an explicit `prefix`.

**Parameters:**
- `value` - The base path (e.g., `/tasks`, `/auth`)

**Returns:** `Module` instance for chaining

**Example:**
```typescript
module('tasks')
  .path('/tasks')
  .use(api())     // Routes prefixed with /tasks
  .use(react());  // Pages prefixed with /tasks
```

##### `getPath(): string | undefined`

Get the base path for this module. Returns `undefined` if no path was set.

**Returns:** The module path or `undefined`

##### `secure(optionsOrGuard?: SecurityOptions | SecurityGuard): this`

Apply security to all handlers registered by plugins in this module.

**Parameters:**
- `optionsOrGuard` - Security options (e.g., `{ roles: ['admin'] }`) or a custom guard function. Omit for authentication-only.

**Returns:** `Module` instance for chaining

**Example:**
```typescript
module('admin')
  .secure({ roles: ['admin'] })
  .use(api());
```

##### `getSecurity(): SecurityOptions | SecurityGuard | undefined`

Get the module-level security options. Returns `undefined` if no security was set.

**Returns:** Security options, guard function, or `undefined`

##### `use(child: Plugin | Module): this`

Add a plugin or sub-module.

**Parameters:**
- `child` - Plugin instance or Module to register

**Returns:** `Module` instance for chaining

##### `provide(...args): this`

Register a DI provider in this module.

**Returns:** `Module` instance for chaining

##### `require<T>(token: Token<T>): this`

Declare that this module requires a token to be provided by its parent.

**Returns:** `Module` instance for chaining

##### `getPlugins(): Plugin[]`

Get all direct plugins (not from sub-modules).

**Returns:** Array of direct plugins

##### `getModules(): Module[]`

Get all direct sub-modules.

**Returns:** Array of direct sub-modules

##### `getPlugin<P extends Plugin>(type: new (...args: any[]) => P): P`

Get a plugin by its constructor type. Searches local plugins first, then walks up the parent chain. Throws if the plugin is not found.

**Parameters:**
- `type` - Plugin constructor class

**Returns:** Plugin instance

**Example:**
```typescript
const httpPlugin = owner.getPlugin(HttpPlugin);
```

##### `findPlugin<P extends Plugin>(type: new (...args: any[]) => P): P | undefined`

Find a plugin by type, searching local plugins then walking up the parent chain. Returns `undefined` if not found.

**Parameters:**
- `type` - Plugin constructor class

**Returns:** Plugin instance or `undefined`

##### `ensurePlugin<P extends Plugin>(type: new (...args: any[]) => P): Promise<P>`

Ensure a plugin exists, creating and warming it up if missing. Searches local plugins and parent chain. Creates locally if not found.

**Parameters:**
- `type` - Plugin constructor class

**Returns:** Promise resolving to plugin instance

**Example:**
```typescript
async warmup(owner: Module) {
  const http = await owner.ensurePlugin(HttpPlugin);
  http.get('/route', handler);
}
```

##### `onStop(hook: () => Promise<void>): this`

Register a shutdown hook. Hooks are called in reverse order during stop().

**Parameters:**
- `hook` - Async function to run during shutdown

**Returns:** `Module` instance for chaining

##### `collectPlugins(): Array<{ plugin: Plugin; owner: Module }>`

Collect all plugins from this module and its sub-modules, depth-first.

**Returns:** Array of plugin/owner pairs

##### `getRegistrations(): readonly Registration[]`

Returns all DI registrations in this module.

##### `getRequirements(): readonly Token[]`

Returns all DI requirements in this module.

## Application

### `Application`

Main application class that orchestrates plugins and manages lifecycle. Extends `Module`.

#### Factory

```typescript
application(): Application
```

**Example:**
```typescript
import { application } from '@putnami/application';

const app = application()
  .use(http())
  .use(api());
```

#### Methods

Inherits all methods from `Module`, plus:

##### `run(runner: () => Promise<void>): this`

Define the main application runner. This is the entry point for your application logic.

**Parameters:**
- `runner` - Async function to run after all plugins start

**Returns:** `Application` instance for chaining

**Example:**
```typescript
app.run(async () => {
  console.log('Application ready');
});
```

##### `isRunning(): boolean`

Check if the application is currently running.

**Returns:** `true` if running, `false` otherwise

##### `build(): Promise<GenerateResult>`

Run the build phase (generate) on all plugins.

**Returns:** Promise resolving to generation results

##### `start(): Promise<void>`

Start the application lifecycle:
1. Warmup all plugins (sequentially, in tree order)
2. Start all plugins (in parallel)
3. Execute the runner (if defined)

**Returns:** Promise that resolves when application starts

##### `stop(): Promise<void>`

Graceful shutdown:
1. Run shutdown hooks in reverse order (from all modules)
2. Stop plugins in reverse tree order

**Returns:** Promise that resolves when application stops

## Plugin Interface

### `Plugin`

Interface for all plugins. Plugins receive their owning `Module` as the first argument.

```typescript
interface Plugin {
  generate?(owner: Module): Promise<GenerateResult>;
  postGenerate?(owner: Module, generated?: GenerateResult): Promise<GenerateResult>;
  warmup?(owner: Module): Promise<void>;
  migrate?(owner: Module): Promise<void>;
  start?(owner: Module): Promise<void>;
  stop?(owner: Module): Promise<void>;
}
```

## HTTP Plugin

### `http(options?: ServerOptions): HttpPlugin`

Create an HTTP plugin instance.

**Parameters:**
- `options` - Optional server configuration

**Returns:** `HttpPlugin` instance

**Example:**
```typescript
const app = application()
  .use(http({ port: 3000 }));
```

### `ServerOptions`

| Option | Type | Default | Description |
|---|---|---|---|
| `port` | `number` | `3000` | Server port. Use `0` for a random available port. |
| `httpMethods` | `HttpMethodsOptions \| false` | `{ head: true, options: true, trace: false }` | Automatic HEAD / OPTIONS / TRACE handling. Pass `false` to disable all. |

### `HttpMethodsOptions`

| Option | Type | Default | Description |
|---|---|---|---|
| `head` | `boolean` | `true` | Derive HEAD responses from GET handlers. Uses `headMeta` when available (no handler execution); otherwise runs handler and strips body. |
| `options` | `boolean` | `true` | Respond to OPTIONS with `204` and an `Allow` header listing registered methods. |
| `trace` | `boolean` | `false` | Echo requests back as `message/http`. Disabled by default for security — sensitive headers are always stripped. |

### `HttpPlugin`

HTTP server plugin class.

#### Methods

##### `use(middleware: HttpMiddleware): this`

Register HTTP middleware.

**Parameters:**
- `middleware` - Middleware function

**Returns:** `HttpPlugin` instance for chaining

##### `prepend(middleware: HttpMiddleware): this`

Add middleware to the beginning of the chain.

**Parameters:**
- `middleware` - Middleware function

**Returns:** `HttpPlugin` instance for chaining

##### `route(method: HttpMethod, path: string, handler: RouteHandler, options?: RouteOptions): this`

Register a route for any HTTP method.

**Parameters:**
- `method` - HTTP method
- `path` - Route path
- `handler` - Route handler function
- `options` - Optional route options

**Returns:** `HttpPlugin` instance for chaining

##### `get(path: string, handler: RouteHandler, options?: RouteOptions): this`

Register a GET route.

##### `post(path: string, handler: RouteHandler, options?: RouteOptions): this`

Register a POST route.

##### `put(path: string, handler: RouteHandler, options?: RouteOptions): this`

Register a PUT route.

##### `delete(path: string, handler: RouteHandler, options?: RouteOptions): this`

Register a DELETE route.

##### `patch(path: string, handler: RouteHandler, options?: RouteOptions): this`

Register a PATCH route.

##### `merge(other: HttpPlugin): this`

Merge routes and middleware from another HttpPlugin.

**Parameters:**
- `other` - HttpPlugin to merge from

**Returns:** `HttpPlugin` instance for chaining

##### `getServer(): Server | undefined`

Get the underlying Bun server instance.

**Returns:** Server instance or undefined

## API Plugin

### `api(config?: Partial<ApiConfig>): ApiPlugin`

Create an API plugin instance for file-based routing.

**Parameters:**
- `config` - Optional API configuration

| Option | Type | Default | Description |
|---|---|---|---|
| `scanFolder` | `string` | `'api'` | Folder to scan for routes (relative to caller) |
| `autoScan` | `boolean` | `true` | Auto-discover routes from file structure |
| `prefix` | `string` | none | URL prefix for all routes. Resolution: explicit `prefix` > module `.path()` > no prefix (default). |

**Returns:** `ApiPlugin` instance

**Example:**
```typescript
// Default: no prefix — routes mount at root
const app = application()
  .use(api());  // src/api/users/get.ts → GET /users

// Inside a module (prefix inherited from module path)
const tasks = module('tasks')
  .path('/tasks')
  .use(api());  // src/api/get.ts → GET /tasks

// Explicit prefix
app.use(api({ prefix: '/v1' }));  // → GET /v1/users
```

### `ApiPlugin`

API plugin class for file-based route discovery.

#### Methods

##### `registerHandler(method: HttpMethod, path: string, handler: RouteHandler): this`

Manually register a route handler.

**Parameters:**
- `method` - HTTP method
- `path` - Route path
- `handler` - Route handler function

**Returns:** `ApiPlugin` instance for chaining

##### `registerWebSocket(path: string, handlers: WebSocketHandlers): this`

Register a WebSocket handler.

**Parameters:**
- `path` - WebSocket path
- `handlers` - WebSocket handler functions

**Returns:** `ApiPlugin` instance for chaining

## Endpoint Builder

### `endpoint(handler): EndpointDefinition`

Create a simple endpoint definition from a handler function (no validation).

**Parameters:**
- `handler` - Route handler function

**Returns:** `EndpointDefinition`

**Example:**
```typescript
export default endpoint((ctx) => {
  return { hello: 'world' };
});
```

### `endpoint(): EndpointBuilder`

Create a fluent endpoint builder for adding validation and metadata.

**Returns:** `EndpointBuilder` instance

**Example:**
```typescript
export default endpoint()
  .params({ id: Uuid })
  .body({ name: String, email: Email })
  .handle(async (ctx) => {
    const body = await ctx.body();
    return { id: ctx.params.id, name: body.name };
  });
```

### `EndpointBuilder`

Fluent builder for endpoint definitions with schema validation.

#### Methods

##### `.params<S>(schema: S): EndpointBuilder`

Declare and validate path parameters. String values are coerced to the target type.

##### `.query<S>(schema: S): EndpointBuilder`

Declare and validate query string parameters. String values are coerced to the target type.

##### `.body<S>(schema: S): EndpointBuilder`

Declare and validate the JSON request body.

##### `.returns<S>(schema: S): EndpointBuilder`

Declare the expected response shape (for documentation).

##### `.handle(handler): EndpointDefinition`

Provide the handler function. Must be called last.

**Parameters:**
- `handler` - Function receiving a typed `EndpointRequestContext`

**Returns:** `EndpointDefinition`

### Schema Primitives

Use JavaScript constructors for basic types:

| Schema | TypeScript Type |
|--------|----------------|
| `String` | `string` |
| `Number` | `number` |
| `Boolean` | `boolean` |

### Constrained Types

| Type | TypeScript Type | Validation |
|------|----------------|------------|
| `Uuid` | `string` | UUID v4 format |
| `Email` | `string` | Valid email address |
| `Int` | `number` | Integer value |

### Schema Combinators

##### `Optional(type: SchemaPrimitive): SchemaDescriptor`

Mark a field as optional.

**Example:**
```typescript
{ nickname: Optional(String) }  // string | undefined
```

##### `ArrayOf(type: SchemaPrimitive): SchemaDescriptor`

Define an array of items.

**Example:**
```typescript
{ tags: ArrayOf(String) }  // string[]
```

##### `Default(type, value)`

Set a default value for an optional field.

**Example:**
```typescript
{ port: Default(Number, 3000) }  // number (defaults to 3000)
```

##### `OneOf(...values)`

Restrict a string field to specific literal values.

**Example:**
```typescript
{ level: OneOf('debug', 'info', 'warn', 'error') }  // 'debug' | 'info' | 'warn' | 'error'
```

## Static Files Plugin

### `staticFiles(config?: Partial<StaticConfig>): StaticPlugin`

Create a static files plugin instance.

**Parameters:**
- `config` - Optional static files configuration

**Returns:** `StaticPlugin` instance

**Example:**
```typescript
const app = application()
  .use(staticFiles({ prefix: '/assets' }));
```

### `StaticPlugin`

Static files plugin class.

#### Methods

##### `routeStatic(path: string, file: string, options?: StaticRouteOptions): this`

Manually register a static file route.

**Parameters:**
- `path` - URL path
- `file` - File path
- `options` - Optional route options

**Returns:** `StaticPlugin` instance for chaining

## Config Plugin

### `config(env?: string): ConfigPlugin`

Create a config plugin instance.

**Parameters:**
- `env` - Environment name (default: `process.env.NODE_ENV || 'local'`)

**Returns:** `ConfigPlugin` instance

**Example:**
```typescript
const app = application()
  .use(config('production'));
```

## OAuth Plugin

### `oAuth2(config?: Partial<OAuthConfig>): OAuthPlugin`

Create a generic OAuth2 / OIDC client plugin (authorization-code flow with `state`, OIDC nonce, optional PKCE, OIDC discovery, and form-encoded token requests). Registers `loginRoute`, `callbackRoute` (when distinct), and `logoutRoute` against `HttpPlugin`.

See [oauth.md](oauth.md) for the full guide and provider recipes.

**Parameters:**
- `config` - Optional programmatic overrides for `OAuthConfig` fields.

**Returns:** `OAuthPlugin` instance.

**Example:**
```typescript
const app = application()
  .use(http())
  .use(oAuth2({
    discoveryUri: 'https://auth.example.com/.well-known/openid-configuration',
    redirectUri:  'https://app.example.com/auth/callback',
    callbackRoute: '/auth/callback',
    scopes: ['openid', 'profile', 'email'],
  }));
```

## OAuth Functions

### `accessToken(options?: { redirect?: boolean }): Promise<string | undefined>`

Returns the current user's access token. Resolution order: `Authorization: Bearer` header → session-stored token (refreshed via `refresh_token` when expired) → redirect to `loginRoute`.

**Parameters:**
- `options.redirect` - When `false`, return `undefined` instead of throwing a redirect. Default `true`.

**Returns:** Promise resolving to the access token, or `undefined`.

### `useUser<T>(options?: { redirect?: boolean }): Promise<T | undefined>`

Returns the verified user identity. Tries: JWT verify of the access token against the discovered `jwks_uri` → JWT verify of the OIDC `id_token` → userinfo endpoint (when configured).

**Type Parameters:**
- `T` - User payload shape.

**Parameters:**
- `options.redirect` - Same semantics as `accessToken()`.

**Returns:** Promise resolving to the user payload, or `undefined`.

### `clientToken(): Promise<string | undefined>`

Returns a cached client-credentials access token for service-to-service calls.

### `useOAuthService(): OAuthService`

Returns the active `OAuthService` instance. Use for low-level operations such as `exchangeCode(...)`, `refreshToken(...)`, `userInfo(...)`, `verify(...)`, or `resolveEndpoints()`.

**Throws:** Error if no `OAuthPlugin` has been registered.

## Session Functions

### `useSession<T>(key: string): T | undefined`

Get a value from the session.

**Type Parameters:**
- `T` - Value type

**Parameters:**
- `key` - Session key

**Returns:** Session value or undefined

### `setSession<T>(key: string, value: T): void`

Set a value in the session.

**Type Parameters:**
- `T` - Value type

**Parameters:**
- `key` - Session key
- `value` - Value to store

### `deleteSession(key: string): void`

Delete a key from the session.

**Parameters:**
- `key` - Session key to delete

### `deleteSessionAll(): void`

Clear the entire session.

### `useSessionAll<S = Record<string, unknown>>(): S`

Get all session data.

**Type Parameters:**
- `S` - Session data type

**Returns:** All session data

### `setSessionAll<T>(session: T): void`

Set all session data at once.

**Type Parameters:**
- `T` - Session data type

**Parameters:**
- `session` - Session data object

### `hasSession(key: string): boolean`

Check if a session key exists.

**Parameters:**
- `key` - Session key

**Returns:** `true` if key exists, `false` otherwise

### `useSessionId(): string`

Get the current session ID.

**Returns:** Session ID string

## Session Store

### `registerSessionStore(name: string, factory: () => SessionStore): void`

Register a custom session store.

**Parameters:**
- `name` - Store name
- `factory` - Factory function that returns a SessionStore instance

### `SessionStore`

Interface for session stores.

```typescript
interface SessionStore {
  get<T>(sessionId: string, key: string): T | undefined;
  set<T>(sessionId: string, key: string, value: T): void;
  delete(sessionId: string, key: string): void;
  getAll<S>(sessionId: string): S;
  setAll<S>(sessionId: string, session: S): void;
  deleteAll(sessionId: string): void;
  exists(sessionId: string): boolean;
}
```

## HttpResponse

### `HttpResponse`

Class for building HTTP responses. Also available as standalone factory functions.

#### Standalone Factory Functions

##### `json(data: any, options?: ResponseInit): HttpResponse`

Create a JSON response.

**Parameters:**
- `data` - Data to serialize as JSON
- `options` - Optional response options

**Returns:** `HttpResponse` instance

##### `redirect(url: string, status?: number): HttpResponse`

Create a redirect response.

**Parameters:**
- `url` - Redirect URL
- `status` - HTTP status code (default: 302)

**Returns:** `HttpResponse` instance

##### `notFound(): HttpResponse`

Create a 404 Not Found response.

**Returns:** `HttpResponse` instance

##### `unauthorized(): HttpResponse`

Create a 401 Unauthorized response.

**Returns:** `HttpResponse` instance

##### `forbidden(): HttpResponse`

Create a 403 Forbidden response.

**Returns:** `HttpResponse` instance

##### `badRequest(): HttpResponse`

Create a 400 Bad Request response.

**Returns:** `HttpResponse` instance

##### `noContent(): HttpResponse`

Create a 204 No Content response.

**Returns:** `HttpResponse` instance

#### Instance Methods

##### `setHeader(name: string, value: string): HttpResponse`

Set a response header.

**Parameters:**
- `name` - Header name
- `value` - Header value

**Returns:** `HttpResponse` instance for chaining

##### `get(): Response`

Build and return the Response object.

**Returns:** Fetch API Response object

## HTTP Context

### `HttpRequestContext`

Context object passed to route handlers.

```typescript
interface HttpRequestContext {
  req: Request;
  params?: Record<string, string>;
  headers: Headers;
  body<T>(): Promise<T>;
  formData(): Promise<FormData>;
  queryParams(): URLSearchParams;
  path(): string;
  host(): string;
  secured(): boolean;
}
```

#### Methods

##### `body<T>(): Promise<T>`

Parse the request body as JSON.

**Type Parameters:**
- `T` - Body type

**Returns:** Promise resolving to parsed body

##### `formData(): Promise<FormData>`

Parse the request body as form data.

**Returns:** Promise resolving to FormData

##### `queryParams(): URLSearchParams`

Get query parameters.

**Returns:** URLSearchParams object

##### `path(): string`

Get the request path.

**Returns:** URL path string

##### `host(): string`

Get the request host.

**Returns:** Host string

##### `secured(): boolean`

Check if the request is over HTTPS.

**Returns:** `true` if HTTPS, `false` otherwise

## WebSocket Context

### `WebSocketContext`

Context object passed to WebSocket handlers.

```typescript
interface WebSocketContext {
  ws: ServerWebSocket;
  message?: string;
  data?: any;
}
```

## HTTP Exception

### `HttpException`

Exception class for HTTP errors.

#### Constructor

```typescript
new HttpException(status: number, message?: string)
```

**Parameters:**
- `status` - HTTP status code
- `message` - Error message

### `HttpAbortException`

Exception for aborting requests.

#### Constructor

```typescript
new HttpAbortException(message?: string)
```

**Parameters:**
- `message` - Error message

## Cache System

Putnami provides a layered caching system with in-memory (fast) and disk-based (persistent) storage. The cache is automatically isolated by build version, ensuring fresh data after deployments.

### `cache(name: string): CacheBuilder`

Create a fluent cache builder for server-side caching.

**Parameters:**
- `name` - Cache namespace identifier (e.g., 'user', 'posts')

**Returns:** `CacheBuilder` instance

**Example:**
```typescript
import { cache } from '@putnami/application';

// Basic usage with static key
const data = await cache('loader')
  .ttl(60000)
  .for('myKey')
  .fetch(() => fetchExpensiveData());

// Dynamic key from arguments
const user = await cache('user')
  .ttl(300000)
  .for((id: string) => id)
  .fetch((id) => fetchUser(id), userId);

// Without TTL (no expiration)
const config = await cache('config')
  .for('app')
  .fetch(() => loadConfig());
```

### `CacheBuilder`

Fluent builder for caching operations.

#### Methods

##### `.ttl(ms: number): CacheBuilder`

Set time-to-live for cached entries in milliseconds.

##### `.for(key: string | ((...args) => string)): CacheBuilder`

Set the cache key. Can be a static string or a function that generates a key from arguments.

##### `.fetch<R>(fn: () => R | Promise<R>, ...args): Promise<R>`

Fetch data, using cache if available. On cache miss, executes the function and stores the result.

### Cache Functions

#### `evictCache(pattern: string): Promise<number>`

Evict cache entries matching a glob pattern.

**Parameters:**
- `pattern` - Glob pattern to match (e.g., `'user:*'`, `'loader:/posts/*'`)

**Returns:** Promise resolving to number of evicted entries

**Example:**
```typescript
import { evictCache } from '@putnami/application';

// Evict all user caches
await evictCache('user:*');

// Evict specific loader cache
await evictCache('loader:/posts/123');
```

#### `clearCache(): Promise<void>`

Clear all cache entries across all layers.

**Returns:** Promise that resolves when cache is cleared

#### `setCacheStorage(storage: CacheStorage): void`

Set a custom cache storage implementation.

**Parameters:**
- `storage` - Cache storage instance implementing `CacheStorage` interface

#### `getCacheStorage(): CacheStorage`

Get the current cache storage.

**Returns:** Cache storage instance

### Cache Storage Implementations

#### `InMemoryCacheStorage`

Fast, volatile in-memory cache. Data is lost on restart.

```typescript
import { InMemoryCacheStorage, setCacheStorage } from '@putnami/application';

setCacheStorage(new InMemoryCacheStorage());
```

#### `DiskCacheStorage`

Persistent disk-based cache. Survives restarts but slower than memory.

```typescript
import { DiskCacheStorage, setCacheStorage } from '@putnami/application';

setCacheStorage(new DiskCacheStorage('app'));
```

#### `LayeredCacheStorage`

Combines multiple cache backends with write-through and read-populate strategies. Default configuration.

```typescript
import { LayeredCacheStorage, InMemoryCacheStorage, DiskCacheStorage, setCacheStorage } from '@putnami/application';

// Default: fast memory layer backed by persistent disk
setCacheStorage(new LayeredCacheStorage([
  new InMemoryCacheStorage(),  // L1: fast, volatile
  new DiskCacheStorage('app'), // L2: slower, persistent
]));
```

### Cache Interface

```typescript
interface CacheStorage {
  get<T>(key: string): Promise<CacheEntry<T> | undefined>;
  set<T>(key: string, value: T, ttl?: number): Promise<void>;
  delete(key: string): Promise<void>;
  deletePattern(pattern: string): Promise<number>;
  clear(): Promise<void>;
}

interface CacheEntry<T> {
  value: T;
  expiresAt: number | null;
}

## Type Definitions

### `HttpMethod`

```typescript
type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE' | 'HEAD' | 'OPTIONS' | 'TRACE' | 'MESSAGE' | 'OPEN' | 'CLOSE';
```

`HEAD`, `OPTIONS`, and `TRACE` are handled automatically by the framework (see `HttpMethodsOptions`). `MESSAGE`, `OPEN`, and `CLOSE` are used for WebSocket lifecycle events.

### `HttpMiddleware`

```typescript
type HttpMiddleware = (
  context: HttpRequestContext,
  next: () => Promise<HttpResponse | undefined>
) => Promise<HttpResponse | undefined>;
```

### `RouteHandler`

```typescript
type RouteHandler<C extends HttpRequestContext = HttpRequestContext> = (
  context: C
) => RouteHandlerResult | Promise<RouteHandlerResult>;
```

### `RouteHandlerResult`

```typescript
type RouteHandlerResult = HttpResponse | string | object | undefined;
```

### `RouteOptions`

| Option | Type | Default | Description |
|---|---|---|---|
| `accept` | `string[]` | `undefined` | Accepted content types for this route. |
| `statusCode` | `number` | `200` | Default status code for the response. |
| `headMeta` | `(ctx: HttpRequestContext) => HttpResponse \| undefined` | `undefined` | Pre-computed HEAD metadata callback. When provided, HEAD requests skip handler execution and use this response instead. Return `undefined` to fall back to the handler. Static routes set this automatically. |

### `GenerateResult`

```typescript
interface GenerateResult {
  assets?: Record<string, string>;
  exports?: Record<string, string>;
}
```

## Next Steps

- See [Getting Started](getting-started.md) for setup instructions
- Check [Configuration](configuration.md) for configuration options
- Explore [Plugins](plugins.md) for plugin architecture
- Review [HTTP Server](http-server.md) for routing details
