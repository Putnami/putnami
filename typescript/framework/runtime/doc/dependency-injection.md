# Dependency Injection

Complete guide to the dependency injection system in `@putnami/runtime`.

## Overview

The DI system provides:

- Explicit provider registration via `provide()` — no auto-instantiation
- Hierarchical containers with parent/child resolution
- `ContainerContext` — a consolidated resolution view over a container tree
- Lifecycle management: register → validate → resolve → live → close
- Scope-aware instances (singleton and scoped)
- Named tokens and tag-based multi-resolution via `list()`
- Circular dependency detection at startup
- Automatic scope proxies for safe singleton → scoped relationships
- Lazy singleton support — defer resolution until first access
- Debug introspection via `describe()`
- No decorators or reflect-metadata required

## Quick Start

```typescript
import { ContainerContext, provide, named, tagged } from '@putnami/runtime';

class Database {
  async query(sql: string) { /* ... */ }
}

class UserService {
  constructor(private db: Database) {}
  async getUser(id: string) {
    return this.db.query(`SELECT * FROM users WHERE id = '${id}'`);
  }
}

const ctx = new ContainerContext('app');
ctx.register(provide(Database));
ctx.register(provide(UserService, { deps: [Database] }));

await ctx.start();

const users = ctx.get(UserService);
await users.getUser('123');

await ctx.close();
```

## Provider Registration

### Class provider (no deps)

```typescript
provide(AppConfig)
```

### Class provider with dependencies

```typescript
provide(UserService, { deps: [Database, EmailService] })
```

### Sync factory

```typescript
provide(Database, () => new Database('postgres://localhost'))
```

### Async factory with resolve

```typescript
provide(Database, async (resolve) => {
  const config = resolve(AppConfig);
  const db = new Database(config.url);
  await db.connect();
  return db;
}, { onClose: (db) => db.disconnect() })
```

### Named token

```typescript
const Version = named<string>('version');
provide(Version, () => '2.0.0')
```

### Provider options

All `provide()` calls accept an options object:

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `deps` | `Token[]` | `[]` | Constructor dependencies (class providers) |
| `scope` | `'singleton' \| 'scoped'` | `'singleton'` | Instance lifecycle |
| `visibility` | `'private' \| 'public'` | `'public'` | Visibility in container hierarchy |
| `tags` | `string[]` | `[]` | Tags for `list({ tags: '...' })` |
| `onClose` | `(instance) => void` | — | Dispose hook called during `close()` |
| `proxy` | `boolean \| ProxyOptions` | — | Per-provider tracing/telemetry override |
| `dynamic` | `boolean` | `false` | Allow runtime refresh via `context.refresh()` |
| `lazy` | `boolean` | `false` | Defer resolution until first access |

## ContainerContext

`ContainerContext` is the primary DI interface. It manages the full lifecycle:
mount → validate → resolve → live → close.

### Creating and starting

```typescript
const ctx = new ContainerContext('app');

// Register providers
ctx.register(provide(Database));
ctx.register(provide(UserService, { deps: [Database] }));

// Mount modules (ContainerHolder instances)
ctx.mount(authModule, 'auth');

// Start: validate → resolve singletons → cache scoped → apply tracing
await ctx.start();
```

### Resolving

```typescript
const db = ctx.get(Database);              // Resolve by class
const version = ctx.get(Version);           // Resolve by named token
const plugins = ctx.list<Plugin>({ tags: 'plugin' }); // Resolve by tag
const hasDb = ctx.has(Database);            // Check availability
```

### Closing

```typescript
await ctx.close();
// Runs onClose hooks in reverse registration order
// Children close before parents
```

### Auto-dispose with `await using`

`ContainerContext` implements `Symbol.asyncDispose`:

```typescript
{
  await using ctx = new ContainerContext('app');
  ctx.register(provide(Database, async () => Database.connect(url), {
    onClose: (db) => db.disconnect(),
  }));

  await ctx.start();
  // ... use the ctx ...
} // ctx.close() is called automatically when the block exits
```

## Scopes

### Singleton (default)

One instance shared across the application lifetime:

```typescript
provide(Database)
// or explicitly:
provide(Database, { scope: 'singleton' })
```

### Scoped

New instance per scope invocation (HTTP request, job, event, worker):

```typescript
provide(RequestData, { scope: 'scoped' })
```

### Using scopes

```typescript
await ctx.scope(async (scope) => {
  const data = scope.get(RequestData);  // Fresh instance for this scope
  const db = scope.get(Database);       // Singleton from parent

  // Same instance within the scope
  const data2 = scope.get(RequestData);
  console.log(data === data2); // true
});

// Each scope gets independent instances
await ctx.scope(async (scope) => {
  const data = scope.get(RequestData); // Different instance
});
```

### Scope proxy (singleton → scoped)

When a singleton depends on a scoped provider, the DI system automatically creates a scope proxy. The proxy delegates every property access and method call to the scoped instance from the current active scope:

```typescript
class RequestId {
  id = crypto.randomUUID();
}

class Logger {
  constructor(private req: RequestId) {}
  log(msg: string) {
    // req.id always resolves from the current scope
    console.log(`[${this.req.id}] ${msg}`);
  }
}

const ctx = new ContainerContext('app');
ctx.register(provide(RequestId, { scope: 'scoped' }));
ctx.register(provide(Logger, (resolve) => new Logger(resolve(RequestId)), {
  deps: [RequestId],
}));

await ctx.start();

// Logger is a singleton, but req delegates to current scope
await ctx.scope(async () => {
  ctx.get(Logger).log('hello'); // [abc-123] hello
});
await ctx.scope(async () => {
  ctx.get(Logger).log('hello'); // [def-456] hello (different scope)
});
```

The scope proxy supports:
- Property access (`get`)
- Property mutation (`set`)
- `in` operator (`has`)
- `Object.keys()` (`ownKeys`)
- `Object.getPrototypeOf()` (`getPrototypeOf`)
- `Object.getOwnPropertyDescriptor()` (`getOwnPropertyDescriptor`)

Outside a scope, accessing the proxy throws an error.

### Lazy singletons

Providers marked with `{ lazy: true }` are not resolved during `start()`. They are instantiated on first `get()` access:

```typescript
provide(ExpensiveService, () => new ExpensiveService(), { lazy: true })
```

## ContainerHolder

`ContainerHolder` is the interface for objects that provide registrations and requirements to a `ContainerContext`. Implement it to integrate with the DI container tree:

```typescript
interface ContainerHolder {
  getRegistrations(): readonly Registration[];
  getRequirements(): readonly Token[];
}
```

`Module` and `Application` in `@putnami/application` implement this interface.

### Mounting holders

```typescript
const ctx = new ContainerContext('app');
ctx.register(provide(Database));

// Mount a ContainerHolder — validates requirements, creates child container
ctx.mount(authModule, 'auth');
```

## Tags and Multi-Resolution

Tag providers for grouped resolution via `list()`:

```typescript
class PluginA { name = 'A'; }
class PluginB { name = 'B'; }

const ctx = new ContainerContext('app');
ctx.register(provide(PluginA, { tags: ['plugin'] }));
ctx.register(provide(PluginB, { tags: ['plugin'] }));

await ctx.start();
const plugins = ctx.list<Plugin>({ tags: 'plugin' }); // [PluginA, PluginB]
```

When `tags` is an array, `list()` matches providers that have **all** specified tags (AND logic):

```typescript
ctx.list<Middleware>({ tags: ['http', 'auth'] }); // Must have both 'http' AND 'auth' tags
```

### Resolving tags inside factories

Use `resolve.all()` inside factory functions:

```typescript
provide(PluginManager, (resolve) => {
  const plugins = resolve.all<Plugin>({ tags: 'plugin' });
  return new PluginManager(plugins);
})
```

## Validation

During `start()`, the container validates:

1. **Missing dependencies**: All declared deps must be registered
2. **Circular dependencies**: DFS-based cycle detection on the dependency graph
3. **Scope violations**: Singleton → scoped dependencies (auto-proxied, emits warning)

```typescript
class A {}
class B {}

const ctx = new ContainerContext('app');
ctx.register(provide(A, { deps: [B] }));
ctx.register(provide(B, { deps: [A] }));

await ctx.start();
// Throws ContainerValidationError:
//   - [circular-dependency] Circular dependency: A → B → A
```

## Debugging

### Inspecting the container

Use `describe()` to inspect the container tree:

```typescript
const description = ctx.describe();
// Returns: {
//   name: 'app',
//   closed: false,
//   providers: [
//     { token: 'Database', scope: 'singleton', visibility: 'public', resolved: true, ... },
//     { token: 'UserService', scope: 'singleton', deps: ['Database'], resolved: true, ... },
//   ],
//   children: [
//     { name: 'auth', providers: [...], children: [] },
//   ]
// }
```

Pass `{ validate: true }` to get the dependency graph and validation issues in a single call:

```typescript
const info = ctx.describe({ validate: true });
// info.issues — validation issues for the root container
// info.children[0].issues — issues for child containers

for (const issue of info.issues ?? []) {
  console.log(`[${issue.type}] ${issue.message}`);
}
```

### Debug mode

Enable debug logging to trace DI resolution, scope creation, and scope proxy creation:

```typescript
const ctx = new ContainerContext('app', { debug: true });
```

When `debug: true` (no effect in production):
- Logs every `get()` resolution with cached/new status
- Logs scope creation and cleanup
- Logs scope proxy creation once per singleton→scoped token pair

All debug output goes to the `putnami:di` logger.

## Error Types

All DI errors include actionable hints to guide you toward a fix.

| Error | When | Example hint |
|-------|------|-------------|
| `NotRegisteredError` | Token not found in container hierarchy | "Add .provide(X) to your application or module" |
| `CircularDependencyError` | Circular dependency detected at runtime | "Break the cycle by extracting shared logic" |
| `ContainerValidationError` | Validation fails during `start()` | Lists all issues with types |
| `ContainerClosedError` | `get()` called after `close()` | — |
| `DuplicateProviderError` | Same token registered twice in a container | "Use a different container or module" |
| `RequirementNotMetError` | Module requirement not met by parent | "Add .provide(X) before .use(module)" |
| `ScopeViolationError` | Singleton depends on scoped (informational) | — |

### Resolution chains

When a dependency fails to resolve during a chain of factory calls, the error includes the full resolution path:

```
NotRegisteredError: EmailService is not registered in container 'root' or its parents.
  Resolution chain: UserController → AuthService
  Hint: Add .provide(EmailService) to your application or module.
```

The `resolveInjection()` wrapper (used by `.inject()`) also includes the injection key:

```
Failed to resolve inject({ email: EmailService }): EmailService is not registered...
```

## Dynamic Providers

Providers marked with `{ dynamic: true }` can be refreshed at runtime via `context.refresh()`:

```typescript
const FeatureFlags = named<Record<string, boolean>>('feature-flags');

const ctx = new ContainerContext('app');
ctx.register(provide(FeatureFlags, () => loadFlags(), { dynamic: true }));

await ctx.start();

// When config changes:
await ctx.refresh(); // re-loads feature flags
```

**Caveat:** only the dynamic provider's own cached instance is replaced. Singletons that already hold a reference to the old instance keep it.

## Tracing

Enable tracing to automatically wrap all provider instances with a tracing proxy:

```typescript
const sink: TraceSink = {
  emit({ token, method, duration, error }) {
    console.log(`[${token}] ${String(method)} took ${duration}ms`);
  },
};

const ctx = new ContainerContext('app', {
  proxy: { tracing: true },
  traceSink: sink,
});
```

Opt out individual providers with `{ proxy: false }` or `{ proxy: { tracing: false } }`.

## Testing with fork()

Fork a `ContainerContext` and override specific providers with test doubles:

```typescript
const ctx = new ContainerContext('app');
ctx.register(provide(Database, async () => Database.connect(url)));
ctx.register(provide(UserService, { deps: [Database] }));

// Fork and override
await using testCtx = ctx.fork()
  .override(Database, () => new MockDatabase());

await testCtx.start();

const service = testCtx.get(UserService);
const user = await service.getUser('1');
```

`fork()` copies all root registrations and the mounted module-container structure, so `module()`-composed apps keep their module-scoped providers in the fork. `override()` replaces the factory for a specific token. The fork is fully isolated.

## Resolution Paths

There is one preferred way to resolve dependencies per context:

| Context | API | Import from |
|---------|-----|-------------|
| HTTP handlers | `.inject({...}).handle((deps, ctx) =>)` | `@putnami/application` |
| App-level code | `context.get(token)` / `context.list(filter)` | `@putnami/runtime` |
| Scope callbacks | `scope.get(token)` | callback parameter |
| Tests | `fork().override(token, factory)` | `@putnami/runtime` |

### Low-level scope helpers

`useContainer()`, `resolve()`, and `resolveInjection()` are available via `@putnami/runtime/inject` for advanced use cases (custom framework integration, middleware that needs DI):

```typescript
import { useContainer } from '@putnami/runtime/inject';
```

## Config Integration

Configuration definitions can be resolved through the DI container using config tokens.

### Config tokens

```typescript
import { configToken, provideConfig, Config, Default, Int } from '@putnami/runtime';

const DatabaseConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Int, 5432),
});

// Create a typed DI token
const dbToken = configToken(DatabaseConfig);

// Register in application
app.register(provideConfig(DatabaseConfig));

// Resolve via DI
const config = ctx.get(dbToken);
console.log(config.host); // typed as string
```

### Inject config in handlers

```typescript
endpoint()
  .inject({ db: configToken(DatabaseConfig) })
  .handle(async (ctx) => {
    console.log(ctx.deps.db.host, ctx.deps.db.port);
  });
```

### Auto-registration

`Application` automatically registers providers for all configs that have been tokenized via `configToken()`. You only need explicit `register(provideConfig(X))` when using custom params (e.g., path override).

### ConfigService

`ConfigService` is auto-registered by `Application` with lifecycle management (`onClose`). It provides:

- `get(config, params?)` — load and validate config (instance-scoped cache)
- `describe(config, params?)` — per-field origin tracking for debugging
- `invalidate()` — clear cache and force reload

See [Configuration](configuration.md#config-with-di) for details.

## Best Practices

1. **Use `.inject()` for handlers** — The only documented path for endpoint/loader/action DI
2. **Register everything explicitly** — No auto-instantiation. Every class must be `provide()`d
3. **Declare deps at registration** — Use `{ deps: [...] }` for class providers
4. **Prefer singleton for stateless services** — Use `'scoped'` only for request-specific state
5. **Use `ContainerHolder` for composition** — Group related providers into modules
6. **Use `'private'` for internals** — Hide implementation details
7. **Always `close()`** — Ensure resources are cleaned up via `onClose` hooks
8. **Test with `fork()`** — Fork the context and override for test isolation

## Next Steps

- Learn about [Context Management](context-management.md) for async context propagation
- Explore [Configuration](configuration.md) to inject configs into services
- Check the [API Reference](api-reference.md) for complete function signatures
