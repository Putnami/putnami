# Dependency Injection

Putnami provides a hierarchical DI container with explicit registration, lifecycle management, and scope support. No decorators or reflect-metadata required.

The DI primitives (`Container`, `ContainerContext`, `provide()`, tokens) live in `@putnami/runtime`. The application layer (`Application`, `Module`, plugins) lives in `@putnami/application`.

## Defining services

### Basic service

```ts
export class UserService {
  async getUser(id: string) {
    return { id, name: 'User' };
  }
}
```

### Service with dependencies

Dependencies are declared at registration time via `{ deps: [...] }`:

```ts
export class EmailService {
  async send(to: string, subject: string, body: string) {
    // Send email
  }
}

export class NotificationService {
  constructor(private emailService: EmailService) {}

  async notifyUser(userId: string, message: string) {
    await this.emailService.send('user@example.com', 'Notification', message);
  }
}
```

## Application setup

Build your application by registering providers and starting the lifecycle:

```ts
import { application } from '@putnami/application';
import { provide } from '@putnami/runtime';

const app = application()
  .provide(EmailService)
  .provide(NotificationService, { deps: [EmailService] });

await app.start();
// Application is now live — all singletons resolved
```

## Resolving services

### In HTTP handlers (recommended)

Use `.inject()` on `endpoint()`, `loader()`, or `action()` builders. This is the preferred path for handler-level DI — dependencies are resolved automatically before the handler runs and exposed on `ctx.deps`:

```ts
import { endpoint } from '@putnami/application';

export default endpoint()
  .inject({ users: UserService, notifications: NotificationService })
  .handle(async (ctx) => {
    const { users, notifications } = ctx.deps;
    const user = await users.getUser(ctx.params.id);
    await notifications.notifyUser(user.id, 'Welcome!');
    return { user };
  });
```

### Using context.get() (app-level)

For application-level resolution outside of HTTP handlers, use `app.context`:

```ts
const notifier = app.context.get(NotificationService);
await notifier.notifyUser('123', 'Welcome!');
```

### In scoped contexts (HTTP requests, events, jobs)

```ts
await app.context.scope(async (scope) => {
  const service = scope.get(UserService);
  const user = await service.getUser(ctx.params.id);
  return { user };
});
```

### Resolution path summary

| Context | API | When to use |
|---------|-----|-------------|
| HTTP handlers | `.inject({...}).handle((ctx) => ctx.deps...)` | Always — the primary path |
| Event handlers | `handler(topic).inject({...}).handle((deps, msg) =>)` | Preferred for typed event subscribers |
| App-level code | `context.get(token)` / `context.list(filter)` | Orchestration, startup |
| Scope callbacks | `scope.get(token)` | Inside `context.scope()` |

## Provider registration

### Class provider

```ts
// No dependencies
provide(AppConfig)

// With dependencies
provide(UserService, { deps: [Database, EmailService] })
```

### Factory provider

```ts
// Sync factory
provide(Database, () => new Database('postgres://localhost'))

// Async factory with dependency resolution
provide(Database, async (resolve) => {
  const config = resolve(AppConfig);
  const db = new Database(config.url);
  await db.connect();
  return db;
}, { onClose: (db) => db.disconnect() })
```

### Named token

For non-class values, use named tokens:

```ts
import { named } from '@putnami/runtime';

const ApiUrl = named<string>('api-url');
provide(ApiUrl, () => process.env.API_URL!)
```

### Provider options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `deps` | `Token[]` | `[]` | Constructor dependencies |
| `scope` | `'singleton' \| 'scoped'` | `'singleton'` | Instance lifecycle |
| `visibility` | `'private' \| 'public'` | `'public'` | Visibility in hierarchy |
| `tags` | `string[]` | `[]` | Tags for multi-resolution |
| `onClose` | `(instance) => void` | — | Cleanup hook |
| `proxy` | `boolean \| ProxyOptions` | — | Per-provider tracing override |
| `dynamic` | `boolean` | `false` | Allow refresh via `context.refresh()` |
| `lazy` | `boolean` | `false` | Defer resolution until first access |

## Scopes

### Singleton scope (default)

One instance shared across the entire application:

```ts
provide(ConfigService)
```

### Scoped

New instance for each scope invocation (HTTP request, job, event):

```ts
provide(RequestContext, { scope: 'scoped' })
```

### Using scoped services

Scoped services are resolved within `context.scope()`:

```ts
class RequestContext {
  requestId = crypto.randomUUID();
  startTime = Date.now();
}

const app = application()
  .provide(RequestContext, { scope: 'scoped' });

await app.start();

await app.context.scope(async (scope) => {
  const ctx = scope.get(RequestContext);
  console.log('Request ID:', ctx.requestId);
  // Same instance within this scope
  const ctx2 = scope.get(RequestContext);
  console.log(ctx === ctx2); // true
});

// Different scope = different instance
await app.context.scope(async (scope) => {
  const ctx = scope.get(RequestContext);
  console.log('Different ID:', ctx.requestId);
});
```

### Scope proxy (singleton → scoped)

When a singleton depends on a scoped provider, the DI system automatically creates a **scope proxy**. The proxy delegates every property access and method call to the scoped instance from the current active scope.

```ts
class RequestId {
  constructor(public id: string) {}
}

class AuditLogger {
  constructor(private requestId: RequestId) {}

  log(msg: string) {
    console.log(`[${this.requestId.id}] ${msg}`);
  }
}

const app = application()
  .provide(RequestId, () => new RequestId(crypto.randomUUID()), { scope: 'scoped' })
  .provide(AuditLogger, (resolve) => new AuditLogger(resolve(RequestId)), {
    deps: [RequestId],
  });

await app.start();

// AuditLogger is a singleton — constructed once.
// But its `requestId` field holds a scope proxy, not a direct instance.
// Inside a scope, the proxy resolves to that scope's RequestId:

await app.context.scope(() => {
  const logger = app.context.get(AuditLogger);
  logger.log('action performed'); // [<unique-uuid>] action performed
});
```

The proxy is transparent: method calls, property access, `in` operator, `Object.keys()`, and prototype checks all delegate to the current scope's instance. Outside a scope, accessing the proxy throws an error.

## Modules

Group related providers into modules with explicit requirements:

```ts
import { application, module } from '@putnami/application';

const authModule = module('auth')
  .require(Database)
  .provide(AuthService, { deps: [Database] })
  .provide(TokenValidator, { visibility: 'private' });

const storeModule = module('store')
  .provide(StoreService)
  .provide(CartService, { deps: [StoreService] });

const app = application()
  .provide(Database)
  .use(authModule)
  .use(storeModule);
```

### Module visibility

- `'public'` (default): Accessible from the app and sibling modules
- `'private'`: Only accessible within the module itself

### Module requirements

`require()` declares tokens that must be provided by the parent:

```ts
const authModule = module('auth')
  .require(Database) // Throws RequirementNotMetError if missing
  .provide(AuthService, { deps: [Database] });
```

Use `composeModules()` for a set of sibling feature modules that should satisfy
each other's requirements as one unit. The composed module still preserves the
child module tree for plugin lifecycle and discovery, but DI mounts the providers
in one container:

```ts
import { composeModules, module } from '@putnami/application';

const databaseModule = module('auth.db').provide(Database);
const usersModule = module('auth.users')
  .require(Database)
  .provide(UserService, { deps: [Database] });

const authModule = composeModules([databaseModule, usersModule], {
  name: 'auth.server',
});

application().use(authModule);
```

## Tags and multi-resolution

Tag providers for grouped resolution via `list()`:

```ts
import { tagged } from '@putnami/runtime';

const app = application()
  .provide(PluginA, { tags: ['plugin'] })
  .provide(PluginB, { tags: ['plugin'] });

await app.start();
const plugins = app.context.list<Plugin>({ tags: 'plugin' }); // [PluginA, PluginB]
```

`list()` accepts a `FilterOptions` object. When `tags` is an array, it matches providers that have **all** specified tags (AND logic):

```ts
// Matches providers tagged with both 'http' AND 'auth'
const secureMiddleware = app.context.list<Middleware>({ tags: ['http', 'auth'] });
```

### Resolving tags in factories

Use `resolve.all()` inside factory functions to collect all tagged providers:

```ts
provide(PluginManager, (resolve) => {
  const plugins = resolve.all<Plugin>({ tags: 'plugin' });
  return new PluginManager(plugins);
})
```

## Service patterns

### Repository pattern

```ts
import { Repository } from '@putnami/database';

export class UserRepository extends Repository<User> {
  constructor() {
    super(User);
  }

  async findByEmail(email: string) {
    return this.findOne({ email });
  }
}
```

### Service layer

```ts
export class UserService {
  constructor(
    private userRepo: UserRepository,
    private emailService: EmailService,
  ) {}

  async register(data: RegisterInput) {
    const existing = await this.userRepo.findByEmail(data.email);
    if (existing) {
      throw new ConflictException('Email already registered');
    }

    const user = await this.userRepo.save({
      id: crypto.randomUUID(),
      ...data,
      createdAt: new Date(),
    });

    await this.emailService.sendWelcome(user);
    return user;
  }
}

// Register with explicit deps
provide(UserService, { deps: [UserRepository, EmailService] })
```

### Logger injection

```ts
import { useLogger } from '@putnami/runtime';

export class PaymentService {
  private logger = useLogger('PaymentService');

  async processPayment(orderId: string, amount: number) {
    this.logger.info('Processing payment', { orderId, amount });
    try {
      const result = await this.chargeCard(orderId, amount);
      this.logger.info('Payment successful', { orderId, transactionId: result.id });
      return result;
    } catch (error) {
      this.logger.error('Payment failed', { orderId, error });
      throw error;
    }
  }
}
```

### Lazy singletons

Providers marked with `{ lazy: true }` are not resolved during `start()`. Instead, they are instantiated on first access:

```ts
provide(ExpensiveService, () => new ExpensiveService(), { lazy: true })
```

This is useful for expensive services that may not be needed in every code path. The service is still a singleton — once instantiated, it is cached for subsequent accesses.

## Dynamic providers

Providers marked with `{ dynamic: true }` can be refreshed at runtime via `context.refresh()`. This re-executes their factory and replaces the cached instance:

```ts
const FeatureFlags = named<Record<string, boolean>>('feature-flags');

const app = application()
  .provide(FeatureFlags, () => loadFlags(), { dynamic: true });

await app.start();

// Configuration changes externally...
await app.context.refresh(); // re-loads feature flags
```

Non-dynamic providers are unaffected. The old instance's `onClose` hook runs before replacement.

**Caveat:** only the dynamic provider's own cached instance is replaced. Singletons that already hold a reference to the old instance keep it. If dependents need to observe the updated value, make them dynamic too, use scoped providers, or resolve the token lazily at call time.

## Tracing

Enable automatic method-call tracing across all providers:

```ts
import { application } from '@putnami/application';

const sink: TraceSink = {
  emit({ token, method, duration, error }) {
    console.log(`[${token}] ${String(method)} took ${duration}ms`);
  },
};

const app = application({
  proxy: { tracing: true },
  traceSink: sink,
});
```

Opt out individual providers with `{ proxy: false }` or `{ proxy: { tracing: false } }`.

## Auto-dispose

`ContainerContext` implements `Symbol.asyncDispose` for the `await using` pattern:

```ts
import { ContainerContext, provide } from '@putnami/runtime';

{
  await using ctx = new ContainerContext('app');
  ctx.register(provide(Database, async () => Database.connect(url), {
    onClose: (db) => db.disconnect(),
  }));

  await ctx.start();
  // ctx.close() is called automatically when the block exits
}
```

## Testing with DI

### Using `context.fork()` (recommended)

Fork a `ContainerContext` and override specific providers with test doubles:

```ts
import { describe, test, expect } from 'bun:test';
import { ContainerContext, provide } from '@putnami/runtime';

const ctx = new ContainerContext('app');
ctx.register(provide(Database, async () => Database.connect(url)));
ctx.register(provide(UserService, { deps: [Database] }));

describe('UserService', () => {
  test('registers a user', async () => {
    await using testCtx = ctx.fork()
      .override(EmailService, () => ({
        sendWelcome: async () => {},
        send: async () => {},
      }))
      .override(UserRepository, () => ({
        findByEmail: async () => null,
        save: async (data) => ({ id: '1', ...data }),
      }));

    await testCtx.start();

    const userService = testCtx.get(UserService);
    const user = await userService.register({
      name: 'Test User',
      email: 'test@example.com',
    });

    expect(user.email).toBe('test@example.com');
  });
});
```

`fork()` copies all registrations. `override()` replaces the factory for a specific token. The fork is fully isolated from the original.

### Manual test context

You can also create a standalone `ContainerContext` for tests:

```ts
test('registers a user', async () => {
  const ctx = new ContainerContext('test');
  ctx.register(provide(EmailService, () => ({
    sendWelcome: async () => {},
    send: async () => {},
  })));
  ctx.register(provide(UserRepository, () => ({
    findByEmail: async () => null,
    save: async (data) => ({ id: '1', ...data }),
  })));
  ctx.register(provide(UserService, { deps: [UserRepository, EmailService] }));

  await ctx.start();

  const userService = ctx.get(UserService);
  const user = await userService.register({
    name: 'Test User',
    email: 'test@example.com',
  });

  expect(user.email).toBe('test@example.com');

  await ctx.close();
});
```

### Testing scoped services

```ts
test('request context has unique ID', async () => {
  const ctx = new ContainerContext('test');
  ctx.register(provide(RequestContext, { scope: 'scoped' }));

  await ctx.start();

  let id1: string;
  let id2: string;

  await ctx.scope(async (scope) => {
    id1 = scope.get(RequestContext).requestId;
  });

  await ctx.scope(async (scope) => {
    id2 = scope.get(RequestContext).requestId;
  });

  expect(id1).not.toBe(id2);

  await ctx.close();
});
```

## Validation

The DI system validates the entire dependency graph during `start()`:

- **Missing dependencies**: All declared `deps` must be registered
- **Circular dependencies**: Detected via DFS-based graph analysis
- **Scope violations**: Singleton → scoped deps auto-proxied (warning emitted)
- **Requirement validation**: Modules' `require()` tokens must be available in the parent

```ts
class A {}
class B {}

const ctx = new ContainerContext('app');
ctx.register(provide(A, { deps: [B] }));
ctx.register(provide(B, { deps: [A] }));

await ctx.start();
// Throws ContainerValidationError:
//   [circular-dependency] Circular dependency: A → B → A
```

## Debugging

### Inspecting the container

Use `describe()` to inspect the container tree at any time:

```ts
const description = app.describeContainer();
// Returns: { name, closed, providers: [...], children: [...] }
// Each provider includes: token, scope, visibility, async, lazy, dynamic, tags, deps, resolved
```

Pass `{ validate: true }` to get graph + validation issues in one call:

```ts
const info = app.context.describe({ validate: true });
for (const issue of info.issues ?? []) {
  console.log(`[${issue.type}] ${issue.message}`);
}
```

### Debug mode

Enable debug logging to trace DI resolution, scope lifecycle, and scope proxy creation:

```ts
import { application } from '@putnami/application';

const app = application({ debug: true });
```

When `debug: true` (ignored in production):
- Logs every `get()` resolution (cached vs new instance)
- Logs scope creation and cleanup
- Logs scope proxy creation once per singleton→scoped token pair

All output goes to the `putnami:di` logger.

### Error messages

All DI errors include actionable hints:

```
NotRegisteredError: EmailService is not registered in container 'root' or its parents.
  Resolution chain: UserController → AuthService
  Hint: Add .provide(EmailService) to your application or module.
```

When using `.inject()`, the error includes which key failed:

```
Failed to resolve inject({ email: EmailService }): EmailService is not registered...
```

## Lifecycle

```
idle → start() → started → close() → closed
         │
         ├─ Validate dependency graph
         ├─ Resolve non-lazy singletons
         ├─ Cache scoped providers
         └─ Apply tracing (if enabled)
```

`onClose` hooks run in reverse registration order. Children close before parents.

## Config integration

Configuration definitions can be resolved through the DI container using config tokens. This bridges the config system with DI for type-safe injection.

### Config tokens

```ts
import { configToken } from '@putnami/runtime';
import { DatabaseConfig } from './database.config';

// Inject config in handlers
endpoint()
  .inject({ db: configToken(DatabaseConfig) })
  .handle(async (ctx) => {
    const { db } = ctx.deps;
    console.log(db.host, db.port); // typed
  });
```

### Auto-registration

`Application` automatically registers `ConfigService` and providers for all configs that have been tokenized via `configToken()`. No explicit registration is needed.

### ConfigService

`ConfigService` is a DI-managed singleton that provides:

- `get(config, params?)` — load and validate config with instance-scoped caching
- `describe(config, params?)` — per-field origin tracking for debugging
- `invalidate()` — clear cache and force reload

```ts
const configService = app.context.get(ConfigService);
const desc = configService.describe(DatabaseConfig);

for (const origin of desc.origins) {
  console.log(`${origin.field}: ${origin.value} (from ${origin.source})`);
}
```

See [Configuration](/docs/frameworks/typescript/configuration#config-with-di) for details.

## Best practices

1. **Use `.inject()` for handlers** — The only documented path for endpoint/loader/action DI
2. **Register everything explicitly** — No auto-instantiation
3. **Declare deps at registration** — `{ deps: [Database, Config] }`
4. **Use `'scoped'` sparingly** — Only for per-request/per-job state
5. **Group with modules** — Use `module()` for domain boundaries
6. **Use `'private'` for internals** — Hide implementation details
7. **Always `close()`** — Clean up resources via `onClose` hooks
8. **Test with `fork()`** — Fork the context and override for test isolation

### Low-level scope helpers

`useContainer()`, `resolve()`, and `resolveInjection()` are available via `@putnami/runtime/inject` for advanced use cases (custom framework code, middleware that needs DI):

```ts
import { useContainer } from '@putnami/runtime/inject';
```

## Related guides

- [Configuration](/docs/frameworks/typescript/configuration)
- [Testing](/docs/frameworks/typescript/testing)

## Support and compatibility

`@putnami/runtime` is a public, documented, maintained package classified
`stable`. Its dependency-injection specification and its accepted
composition-time lifetimes ADR live next to the package source. The promise is
that lifetimes, visibility, and correctness are decided when the container is
composed: starting a context validates the whole graph before constructing
anything, a `private` provider never leaks past the container that declared it,
and `close()` disposes in reverse construction order.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
