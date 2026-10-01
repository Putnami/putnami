# API Reference

Complete API documentation for `@putnami/runtime`.

## Configuration

### `Config(path, schema)`

Creates a typed configuration definition.

```typescript
function Config<S extends SchemaDefinition>(path: string, schema: S): ConfigDefinition<S>
```

**Parameters:**
- `path` (string): YAML path where config lives
- `schema` (SchemaDefinition): Schema object defining the config shape

**Example:**
```typescript
const DatabaseConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Number, 5432),
});
```

### `useConfig(config, options?)`

Loads and validates a typed configuration.

```typescript
function useConfig<S extends SchemaDefinition>(
  config: ConfigDefinition<S>,
  options?: ConfigParams<InferSchema<S>>
): InferSchema<S>
```

**Parameters:**
- `config`: Configuration definition created with `Config()`
- `options.path` (string): Override the config path
- `options.confInit` (Partial<InferSchema<S>>): Programmatic defaults (overridden by YAML and env vars)

**Returns:** Validated configuration object

**Example:**
```typescript
const config = useConfig(DatabaseConfig);
const authDb = useConfig(DatabaseConfig, { path: 'database.auth' });
```

### `resolveConfigs(...configs)`

Resolves all async `Resolve()` descriptors at bootstrap.

```typescript
async function resolveConfigs(...configs: ConfigDefinition[]): Promise<void>
```

**Example:**
```typescript
await resolveConfigs(AppConfig, SecretConfig);
// Now useConfig() returns resolved values synchronously
```

### `getEnv()`

Determines the current environment name.

```typescript
function getEnv(): string
```

**Returns:** `'local'`, `'test'`, or `'production'`

### `resetConfigLoader()`

Resets the configuration loader, clearing cache.

```typescript
function resetConfigLoader(): void
```

### `registerSourceDiscoverer(discoverer)`

Registers an extension-owned config source discoverer. Discoverers are called by
`getDefaultSources()` and can return a `ConfigSource` when their environment is
enabled, or `undefined` otherwise.

```typescript
function registerSourceDiscoverer(discoverer: () => ConfigSource | undefined): void
```

### `registerConfigLoaderResetHook(reset)`

Registers an extension-owned cache reset hook. `resetConfigLoader()` invokes
registered hooks after clearing the core config caches.

```typescript
function registerConfigLoaderResetHook(reset: () => void): void
```

### `InferConfig<C>`

Utility type to extract the inferred TypeScript type from a `ConfigDefinition`.

```typescript
type InferConfig<C extends ConfigDefinition> = InferSchema<C['schema']>
```

**Example:**
```typescript
type DbConfig = InferConfig<typeof DatabaseConfig>;
```

## Schema

### Schema Primitives

Use JavaScript constructors as base types:

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
| `Url` | `string` | Valid URL |
| `DateIso` | `string` | ISO 8601 date |
| `Min(n)` | `number` | Value >= n |
| `Max(n)` | `number` | Value <= n |
| `MinLength(n)` | `string` | String length >= n |
| `MaxLength(n)` | `string` | String length <= n |
| `Pattern(regex)` | `string` | Must match regex |

### Combinators

#### `Optional(type)`

Mark a field as optional.

```typescript
function Optional<P extends SchemaPrimitive>(type: P): SchemaDescriptor
```

**Example:**
```typescript
{ nickname: Optional(String) }  // string | undefined
```

#### `ArrayOf(type)`

Define an array of items.

```typescript
function ArrayOf<P extends SchemaPrimitive>(type: P): SchemaDescriptor
```

**Example:**
```typescript
{ tags: ArrayOf(String) }  // string[]
```

#### `MapOf(keyType, valueType)`

Define a map (dictionary) from scalar keys to values. Maps to proto3 `map<K, V>`.

```typescript
function MapOf<K, V extends SchemaPrimitive>(keyType: K, valueType: V): SchemaDescriptor
```

Key types are restricted to scalars: `String`, `Number`, `Boolean`, `Int`.
Value types can be any `SchemaPrimitive`.

**Example:**
```typescript
{ labels: MapOf(String, String) }    // Record<string, string>
{ prices: MapOf(String, Number) }    // Record<string, number>
{ counts: MapOf(String, Int) }       // Record<string, number>
```

#### `Default(type, value)`

Set a default value. The field is optional in input but always present in output.

```typescript
function Default<P extends SchemaPrimitive, D>(type: P, value: D): SchemaDescriptor
```

**Example:**
```typescript
{ port: Default(Number, 3000) }  // number (defaults to 3000)
```

#### `OneOf(...values)`

Restrict a string field to specific literal values.

```typescript
function OneOf<const V extends string[]>(...values: V): SchemaDescriptor<V[number]>
```

**Example:**
```typescript
{ level: OneOf('debug', 'info', 'warn', 'error') }  // 'debug' | 'info' | 'warn' | 'error'
```

#### `Env(envVar, type)`

Bind a field to an environment variable.

```typescript
function Env<P extends SchemaPrimitive>(envVar: string, type: P): SchemaDescriptor
```

**Example:**
```typescript
{ port: Env('PORT', Number) }
```

#### `Resolve(fn, type)`

Define an async resolver for secret values. Must call `resolveConfigs()` at bootstrap.

```typescript
function Resolve<P extends SchemaPrimitive>(fn: () => Promise<unknown>, type: P): SchemaDescriptor
```

**Example:**
```typescript
{ apiKey: Resolve(() => fetchSecret('API_KEY'), String) }
```

#### `Sensitive(type)`

Mark a field as sensitive. Values are redacted in error messages.

```typescript
function Sensitive<P extends SchemaPrimitive>(type: P): SchemaDescriptor
```

**Example:**
```typescript
{ password: Sensitive(String) }
```

#### `Desc(description, type)`

Add a human-readable description to a schema field. The description is used in OpenAPI documentation.

```typescript
function Desc<P extends SchemaPrimitive>(description: string, type: P): SchemaDescriptor
```

**Example:**
```typescript
{
  name: Desc('Full name of the user', String),
  email: Desc('Primary email address', Email),
  age: Desc('Age in years', Optional(Int)),
}
```

### `validateSchema(schema, value, options?)`

Validate a plain object against a schema definition.

```typescript
function validateSchema<S extends SchemaDefinition>(
  schema: S,
  value: unknown,
  options?: ValidateOptions
): ValidateResult
```

**Parameters:**
- `schema`: Schema definition
- `value`: Value to validate
- `options.coerce` (boolean): Coerce strings to target types
- `options.label` (string): Label for error messages

**Returns:** `{ data: Record<string, unknown>, errors: ValidationError[] }`

### `InferSchema<S>`

Utility type to infer TypeScript type from a schema definition.

```typescript
type InferSchema<S extends SchemaDefinition> = { ... }
```

**Example:**
```typescript
const UserSchema = { name: String, email: Email, age: Optional(Number) };
type User = InferSchema<typeof UserSchema>;
// { name: string; email: string; age?: number }
```

## Dependency Injection

### `ContainerContext`

The primary DI interface. Manages the full lifecycle: mount → validate → resolve → live → close.

```typescript
class ContainerContext {
  constructor(name: string, options?: ContainerContextOptions);
  register<T>(registration: Registration<T>): void;
  mount(holder: ContainerHolder, name: string): Container;
  start(): Promise<void>;
  close(): Promise<void>;
  get<T>(token: Token<T>): T;
  list<T>(filter: FilterOptions): T[];
  has(token: Token): boolean;
  scope<R>(fn: (scope: ScopeContext) => R | Promise<R>): Promise<R>;
  refresh(): Promise<void>;
  fork(): ContainerContextFork;
  describe(): ContainerDescription;
  [Symbol.asyncDispose](): Promise<void>;
}
```

**Example:**
```typescript
const ctx = new ContainerContext('app');
ctx.register(provide(Database));
ctx.register(provide(UserService, { deps: [Database] }));
await ctx.start();
const db = ctx.get(Database);
await ctx.close();
```

### `ContainerContextFork`

A test fork of a `ContainerContext`. Copies all root registrations and the mounted module-container structure, then allows overriding before `start()`.

```typescript
class ContainerContextFork extends ContainerContext {
  override<T>(token: Token<T>, factory: SyncFactory<T>): this;
  override<T>(token: Token<T>, factory: AsyncFactory<T>): this;
}
```

**Example:**
```typescript
const testCtx = ctx.fork()
  .override(Database, () => new MockDatabase());
await testCtx.start();
```

### `provide(...)`

Creates a provider registration. Returns inert data — no instantiation until `start()`.

```typescript
function provide<T extends object>(classRef: Type<T>): Registration<T>;
function provide<T extends object>(classRef: Type<T>, options: ProvideOptions<T>): Registration<T>;
function provide<T>(token: Token<T>, factory: SyncFactory<T>): Registration<T>;
function provide<T>(token: Token<T>, factory: SyncFactory<T>, options: ProvideOptions<T>): Registration<T>;
function provide<T>(token: Token<T>, factory: AsyncFactory<T>): Registration<T>;
function provide<T>(token: Token<T>, factory: AsyncFactory<T>, options: ProvideOptions<T>): Registration<T>;
```

**Example:**
```typescript
provide(AppConfig)
provide(UserService, { deps: [Database] })
provide(Database, () => new Database('postgres://localhost'))
provide(Database, async (resolve) => {
  const config = resolve(AppConfig);
  return Database.connect(config.url);
}, { onClose: (db) => db.disconnect() })
```

### `named<T>(name)`

Creates a named token for type-safe DI resolution of non-class values.

```typescript
function named<T>(name: string): NamedToken<T>
```

**Example:**
```typescript
const DbUrl = named<string>('db-url');
provide(DbUrl, () => process.env.DATABASE_URL!);
const url = ctx.get(DbUrl); // string
```

### `tagged<T>(tag)`

Creates a tag selector for type-safe multi-resolution via `resolveInjection()`.

```typescript
function tagged<T = unknown>(tag: string): TagSelector<T>
```

**Example:**
```typescript
provide(PluginA, { tags: ['plugin'] });
provide(PluginB, { tags: ['plugin'] });
const plugins = ctx.list<Plugin>({ tags: 'plugin' }); // Plugin[]
```

### `ProvideOptions<T>`

Options for provider registration.

```typescript
interface ProvideOptions<T = unknown> {
  deps?: Token[];
  scope?: 'singleton' | 'scoped';
  visibility?: 'private' | 'public';
  tags?: string[];
  onClose?: (instance: T) => void | Promise<void>;
  proxy?: boolean | ProxyOptions;
  dynamic?: boolean;
  lazy?: boolean;
}
```

### `FilterOptions`

Filter options for `list()`. Extensible — starts with tag-based filtering.

```typescript
interface FilterOptions {
  tags?: string | string[];
}
```

### `ContainerContextOptions`

Options for `ContainerContext` configuration.

```typescript
interface ContainerContextOptions {
  proxy?: ProxyOptions;
  traceSink?: TraceSink;
}
```

### `ContainerHolder`

Interface for objects that provide registrations and requirements to a `ContainerContext`.

```typescript
interface ContainerHolder {
  getRegistrations(): readonly Registration[];
  getRequirements(): readonly Token[];
}
```

### `TraceSink`

Interface for receiving trace events from tracing proxies.

```typescript
interface TraceSink {
  emit(data: TraceData): void;
}
```

### `TraceData`

Data emitted by tracing proxies.

```typescript
interface TraceData {
  token: string;
  module?: string;
  method: string | symbol;
  duration: number;
  error?: unknown;
}
```

### `ResolveFn`

The resolve function available inside factory providers.

```typescript
interface ResolveFn {
  <D>(token: Token<D>): D;
  all<D = unknown>(filter: FilterOptions): D[];
}
```

**Example:**
```typescript
provide(PluginManager, (resolve) => {
  const db = resolve(Database);                        // single
  const plugins = resolve.all<Plugin>({ tags: 'plugin' }); // all tagged
  return new PluginManager(db, plugins);
})
```

### `ScopeContext`

Interface for the scoped container passed to `scope()` callbacks.

```typescript
interface ScopeContext {
  get<T>(token: Token<T>): T;
  list<T>(filter: FilterOptions): T[];
  has(token: Token): boolean;
}
```

### DI Error Classes

| Error | Description |
|-------|-------------|
| `DiError` | Base error for all DI errors |
| `NotRegisteredError` | Token not found in container hierarchy |
| `CircularDependencyError` | Circular dependency detected |
| `ContainerValidationError` | Validation fails during `start()` |
| `ContainerClosedError` | `get()` called after `close()` |
| `DuplicateProviderError` | Same token registered twice |
| `RequirementNotMetError` | Module requirement not met |
| `ScopeViolationError` | Singleton depends on scoped |

## Context Management

### `runInContext<C, R>(context, fn)`

Runs code within an async context.

```typescript
function runInContext<C extends Context, R = unknown>(
  context: C,
  fn: () => R | Promise<R>
): Promise<R>
```

**Parameters:**
- `context`: Context object to set
- `fn`: Function to execute within context

**Returns:** Result of `fn`

**Example:**
```typescript
await runInContext({ userId: '123' }, async () => {
  const ctx = useContext();
  // ctx.userId === '123'
});
```

### `useContext<C>()`

Gets the current context.

```typescript
function useContext<C = unknown>(): C
```

**Returns:** Current context object or `undefined`

## Error Handling

### `HttpException`

Base class for all HTTP exceptions.

```typescript
class HttpException extends Error {
  statusCode: number;
  message: string | object;

  constructor(
    response: string | object,
    status: number,
    options?: HttpExceptionOptions
  );

  static from(error: unknown): HttpException;
  toJSON(): object;
}
```

### Standard HTTP Exceptions

All standard HTTP status codes have corresponding exception classes:

**Client Errors (4xx):**
- `BadRequestException` (400)
- `UnauthorizedException` (401)
- `ForbiddenException` (403)
- `NotFoundException` (404)
- `MethodNotAllowedException` (405)
- `NotAcceptableException` (406)
- `RequestTimeoutException` (408)
- `ConflictException` (409)
- `GoneException` (410)
- `PayloadTooLargeException` (413)
- `UriTooLongException` (414)
- `UnsupportedMediaTypeException` (415)
- `RangeNotSatisfiableException` (416)
- `ExpectationFailedException` (417)
- `UnprocessableEntityException` (422)
- `TooManyRequestsException` (429)

**Server Errors (5xx):**
- `InternalServerErrorException` (500)
- `NotImplementedException` (501)
- `BadGatewayException` (502)
- `ServiceUnavailableException` (503)
- `GatewayTimeoutException` (504)
- `HttpVersionNotSupportedException` (505)
- `MisdirectedException` (421)
- `PreconditionFailedException` (412)
- `LengthRequiredException` (411)
- `FailedDependencyException` (424)
- `ProxyAuthenticationRequiredException` (407)
- `ImATeapotException` (418)

**Special:**
- `ValidationException` (400) - For validation errors
- `AggregateException` (500) - For multiple errors

### `createHttpException(status, message?, options?)`

Creates an HTTP exception based on status code.

```typescript
function createHttpException(
  status: number,
  message?: string | object,
  descriptionOrOptions?: string | HttpExceptionOptions
): HttpException
```

### `isHttpException(value)`

Type guard to check if value is an HttpException.

```typescript
function isHttpException(value: unknown): value is HttpException
```

### `HttpStatus`

Enum of HTTP status codes.

```typescript
enum HttpStatus {
  OK = 200,
  BAD_REQUEST = 400,
  UNAUTHORIZED = 401,
  // ... all standard status codes
}
```

## Logging

### `useLogger(name?)`

Gets a context-aware logger instance.

```typescript
function useLogger(loggerName?: string): Logger
```

**Parameters:**
- `loggerName`: Optional logger name for organization

**Returns:** Logger instance

**Example:**
```typescript
const logger = useLogger('user-service');
logger.info('Processing request');
```

### `Logger`

Context-aware logger that writes to `LogSink[]`.

```typescript
class Logger {
  constructor(sinks: LogSink[], name?: string);
  named(name: string): Logger;
  with(key: string, value: unknown, options?: WithOptions): this;
  debug(message?: unknown, ...params: unknown[]): void;
  info(message?: unknown, ...params: unknown[]): void;
  warn(message?: unknown, ...params: unknown[]): void;
  error(message?: unknown, ...params: unknown[]): void;
  flush(): Promise<void>;
  close(): Promise<void>;
}
```

**Methods:**
- `named(name)`: Create a child logger with a different name (shares sinks)
- `with(key, value, options?)`: Add context data to all logs in the current async context
- `debug/info/warn/error(...)`: Log at respective levels
- `flush()`: Flush all sinks (useful with BufferSink)
- `close()`: Close all sinks (flush + cleanup)

### `LogSink`

Interface for log output destinations.

```typescript
interface LogSink {
  write(entry: LogEntry): void;
  flush?(): Promise<void>;
  close?(): Promise<void>;
}
```

### `LogEntry`

Structured log entry passed to sinks.

```typescript
interface LogEntry {
  level: LogLevel;
  message: string;
  timestamp: string;
  logger?: string;
  traceId?: string;
  context?: Record<string, unknown>;
  error?: { name: string; message: string; stack?: string };
  data?: unknown[];
}
```

### `LogLevel`

Type for log levels.

```typescript
type LogLevel = 'debug' | 'info' | 'warn' | 'error';
```

### Built-in Sinks

- `ConsoleSink` — text output to stdout/stderr
- `JsonSink` — structured JSON (Cloud Logging compatible)
- `BufferSink` — groups by traceId, flushes by size/time/error
- `MemorySink` — in-memory capture for tests (import from `@putnami/runtime/testing`)

### `MemoryLogger`

Test utility that captures `LogEntry[]`. Import from the testing subpath:

```typescript
import { MemoryLogger } from '@putnami/runtime/testing';

class MemoryLogger extends Logger {
  readonly entries: LogEntry[];
  clear(): void;
}
```

### `installExceptionHandler(logger)`

Installs global `uncaughtException`/`unhandledRejection` handlers.

```typescript
function installExceptionHandler(logger: Logger): () => void
```

**Returns:** Cleanup function to remove handlers.

## Types

### `Token<T>`

A reference to an injectable class, named token, or symbol.

```typescript
type Token<T = unknown> = Type<T> | NamedToken<T> | symbol;
```

### `Scope`

Type for dependency injection scopes.

```typescript
type Scope = 'singleton' | 'scoped';
```

### `Visibility`

Type for provider visibility in the container hierarchy.

```typescript
type Visibility = 'private' | 'public';
```

### `Type<T>`

Type for class constructors.

```typescript
type Type<T = unknown> = new (...args: unknown[]) => T;
```

### `ContainerDescription`

Debug description of a container's state. Returned by `ctx.describe()`.

```typescript
interface ContainerDescription {
  name: string;
  closed: boolean;
  providers: ProviderDescription[];
  children: ContainerDescription[];
}

interface ProviderDescription {
  token: string;
  scope: string;
  visibility: string;
  async: boolean;
  lazy: boolean;
  dynamic: boolean;
  tags: string[];
  deps: string[];
  resolved: boolean;
}
```

### `Context`

Type for context objects.

```typescript
type Context = Record<string, unknown> & {
  logger?: Logger;
  traceId?: string;
  logContext?: Record<string, unknown>;
  signal?: AbortSignal;
};
```

### `HttpExceptionOptions`

Options for HTTP exceptions.

```typescript
interface HttpExceptionOptions {
  cause?: Error;
  description?: string;
}
```

### `ConfigDefinition<S>`

Configuration definition created by `Config()`.

```typescript
interface ConfigDefinition<S extends SchemaDefinition = SchemaDefinition> {
  __config: symbol;
  path: string;
  schema: S;
}
```

### `ConfigParams<T>`

Parameters for `useConfig()`.

```typescript
interface ConfigParams<T> {
  path?: string;
  confInit?: Partial<T>;
}
```

### `SchemaDefinition`

A plain object mapping field names to schema primitives.

```typescript
type SchemaDefinition = Record<string, SchemaPrimitive>;
```

### `SchemaPrimitive`

A schema type: a constructor, a `SchemaDescriptor`, or a nested schema object.

```typescript
type SchemaPrimitive = typeof String | typeof Number | typeof Boolean | SchemaDescriptor | NestedSchema;
```

### `SchemaDescriptor`

Describes a schema field with type, constraints, and metadata.

```typescript
interface SchemaDescriptor<T = unknown> {
  __schema: symbol;
  baseType: string;
  optional?: boolean;
  array?: boolean;
  items?: SchemaPrimitive;
  constraints?: SchemaConstraint[];
  default?: T;
  env?: string;
  resolve?: () => Promise<unknown>;
  sensitive?: boolean;
  description?: string;
}
```

### `ValidationError`

Format for validation errors.

```typescript
interface ValidationError {
  field: string;
  message: string;
}
```

## Next Steps

- See [Getting Started](getting-started.md) for setup instructions
- Explore [Configuration](configuration.md) for config usage
- Learn about [Dependency Injection](dependency-injection.md) for DI patterns
- Check [Error Handling](error-handling.md) for exception usage
- Review [Logging](logging.md) for logging patterns
