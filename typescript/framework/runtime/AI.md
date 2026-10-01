# @putnami/runtime

Core runtime: DI container, configuration, validation, logging. No HTTP dependency — used by all framework packages.

## Dependency Injection

Explicit registration, hierarchical containers, no decorators.

### Provider Registration

```ts
import { ContainerContext, provide, named, tagged } from '@putnami/runtime';

// Class (no deps)
provide(AppConfig)

// Class with dependencies
provide(UserService, { deps: [Database, EmailService] })

// Factory
provide(Database, () => new Database(process.env.DB_URL))

// Async factory with resolve
provide(Database, async (resolve) => {
  const config = resolve(AppConfig);
  const db = new Database(config.url);
  await db.connect();
  return db;
}, { onClose: (db) => db.disconnect() })

// Named token (for non-class values)
const ApiUrl = named<string>('api-url');
provide(ApiUrl, () => process.env.API_URL!)

// Tagged (for multi-resolution)
provide(PluginA, { tags: ['plugin'] })
provide(PluginB, { tags: ['plugin'] })
```

### Container Lifecycle

```ts
const ctx = new ContainerContext('app');
ctx.register(provide(Database));
ctx.register(provide(UserService, { deps: [Database] }));

await ctx.start();     // Validate graph + resolve singletons
const svc = ctx.get(UserService);
await ctx.close();     // Dispose in reverse order
```

### Scoped Providers

New instance per scope (HTTP request, job, event):

```ts
provide(RequestContext, { scope: 'scoped' })

await ctx.scope(async (scope) => {
  const reqCtx = scope.get(RequestContext); // Fresh per scope
});
```

Scope proxies: when a singleton depends on a scoped provider, the DI system auto-creates a proxy that delegates to the current scope.

### Provider Options

| Option | Default | Description |
|--------|---------|-------------|
| `deps` | `[]` | Constructor dependencies |
| `scope` | `'singleton'` | `'singleton'` or `'scoped'` |
| `visibility` | `'public'` | `'public'` or `'private'` |
| `tags` | `[]` | For `list()` multi-resolution |
| `onClose` | — | Cleanup on `close()` |
| `lazy` | `false` | Defer until first `get()` |
| `dynamic` | `false` | Allow `refresh()` |

### Testing

```ts
// Fork and override for test isolation
await using testCtx = ctx.fork()
  .override(Database, () => mockDb)
  .override(EmailService, () => mockEmail);
await testCtx.start();
const svc = testCtx.get(UserService);
```

### Validation

At `start()`, the DI system validates:
- Missing dependencies
- Circular dependencies (DFS detection)
- Scope violations (singleton -> scoped auto-proxied with warning)
- Module requirements

## Configuration

YAML-based, type-safe, environment-aware:

```ts
import { Config, useConfig, Default, Optional, Env, Sensitive } from '@putnami/runtime';

const DatabaseConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Number, 5432),
  password: Sensitive(Env('DB_PASSWORD', Optional(String))),
});

const config = useConfig(DatabaseConfig);
config.host; // string
config.port; // number
```

### Raw section access

`useRawConfigSection(path)` reads a section of the merged config tree without
schema validation — the escape hatch for contracts whose shape a field schema
cannot express, such as the managed binding documents a deploy target merges
into the `database`/`storage` sections (keyed by arbitrary logical names and
validated by their own protocol parsers). It honors the active `ConfigService`
(container-scoped sources) and returns `undefined` when the path is absent or
not an object. Typed operator-facing config should keep using `Config()` +
`useConfig()`.

### Client / Browser build

The client entrypoint (`src/index.browser.ts`, used via the package `browser`
export condition) ships browser-safe **stubs** for server-only facilities. The
browser bundle has no `AsyncLocalStorage` request context or config sources —
no YAML files, no `CONFIG_DATA`, no remote resolution, and no `Env()` lookups:

- `tryContext()` returns `undefined`; request context exists only in the server
  runtime backed by Node's `AsyncLocalStorage`.
- `useConfig` applies the schema's `Default()` values (and merges any
  `confInit` overrides on top) so typed reads are not `undefined`, but it does
  **not** run validation and never throws on missing required fields. Fields
  with neither a `Default()` nor a `confInit` value stay absent.
- `getEnv()` always returns `'browser'`.
- `resetConfigLoader()` is a no-op (there is no client-side config cache).

These stubs exist because the listed facilities have a meaningful client
semantic, not as a containment device. Server-only APIs with **no** browser
counterpart (`useContext`, `runInContext`, `useLogger`, ...) deliberately have
no stub: they stay off the browser entry, and the packaging tooling keeps them
out of browser-reachable chunks by transpiling browser-condition entrypoints in
their own build graph. Do not add a stub to make an accidental client import
link — fix the import.

### Config Files

```
conf/.env.yaml             # Shared defaults (all envs)
conf/.env.local.yaml       # Local development
conf/.env.test.yaml        # Test environment
conf/.secrets.local.yaml   # Local secrets (gitignored)
```

### Config Priority

1. Field-level `Env(...)` (80, highest, applied during schema validation)
2. `CONFIG_DATA` env var (60)
3. Registered extension sources, such as `@putnami/cloud/runtime`
   (`/api/secrets/resolve` priority 55 and `/api/configs/resolve` priority 50
   when installed and enabled by `CONFIG_SERVER_URL`)
4. `conf/.secrets.{env}.yaml` (35, gitignored local secrets)
5. `.gen/conf/.env.{env}.yaml` (30, generated, merged from deps)
6. `conf/.env.{env}.yaml` (20, env-specific)
7. `conf/.env.yaml` (10, env-agnostic base)

The remote source is contributed by an extension package such as
`@putnami/cloud/runtime`. The framework auto-activates it via an optional
dynamic `require` when `node_modules` are present (dev, tests). A bun-compiled
binary cannot resolve that require, so a deployed workload must depend on the
package and activate it statically: an `activate.ts` that calls
`register({ registerSourceDiscoverer, registerConfigLoaderResetHook })` from
`@putnami/cloud/runtime`, imported as the first import of `main.ts` (the
registrar dedupes by discoverer identity, so both paths firing is safe). When
`CONFIG_SERVER_URL` is set and no registered source serves it, startup fails
with one line naming that gap rather than booting on schema defaults. See
[`doc/configuration.md` § Remote sources](doc/configuration.md).

Remote config, remote secrets, and bearer-token resolution live in
`@putnami/cloud/runtime`; core `@putnami/runtime` owns only the config-source
registration seam. Bearer-token resolution checks these sources in order:

1. **Operator-provided bearer token** (highest) via env var. Three names accepted, checked in this order: `PUTNAMI_CLOUD_TOKEN` > `CONFIG_SERVER_TOKEN` > `PUTNAMI_TOKEN`. Most-specific wins. Only mode usable from CI / scripts.
2. Otherwise, on GCP (detected via `K_SERVICE` or `GOOGLE_CLOUD_PROJECT`), `@putnami/cloud/runtime` mints an ID token via the metadata server, audience-bound to `CONFIG_SERVER_AUDIENCE` when set, otherwise `CONFIG_SERVER_URL`. Cached and refreshed ~5min before `exp`.
3. Otherwise `@putnami/cloud/runtime` logs a startup warning and resolve calls go out unauthenticated.

To plug in a custom resolver, import `RemoteConfigSource` / `RemoteSecretsSource`
from `@putnami/cloud/runtime` and pass `tokenSource` directly when constructing
them. The same identity is presented to both endpoints. The protocol's
[ADR 0001](../../../protocols/config/doc/adr/0001-sensitive-decides-the-store.md)
states which store each field belongs to.

Cloud Run deploys set `CONFIG_SERVER_URL` to the exact
`/api/configs/resolve?...&secretsMode=reveal` URL and
`CONFIG_SERVER_AUDIENCE` to the control-plane origin. `@putnami/cloud/runtime` fetches the
exact URL with `GET`, reads `response.config`, and skips `/api/secrets/resolve`
because secrets are already merged. Cloud Run/prod fetch failures are startup
errors. Local `putnami serve` can use the same URL with a user/session token in
`PUTNAMI_CLOUD_TOKEN`, `CONFIG_SERVER_TOKEN`, or `PUTNAMI_TOKEN`; no
`CONFIG_DATA` handoff or plaintext local cache is required.

Required remote config and secrets sources retry transient network, timeout,
and 5xx/429 failures before failing startup. `CONFIG_SERVER_TIMEOUT` controls
the per-attempt timeout (default `5s`), and `CONFIG_SERVER_RETRY_BUDGET`
controls the total retry budget (default `30s`; set `0ms` to disable retries).

> **Breaking change** (TS framework only): `CONFIG_DATA` priority moved
> from 80 → 60 to align with Go. Field-level `Env(...)` now wins over
> `CONFIG_DATA` when both are set for the same field. Remove the
> conflicting `Env(...)` declaration if you relied on the old order.

### DI Integration

```ts
import { configToken } from '@putnami/runtime';

endpoint()
  .inject({ db: configToken(DatabaseConfig) })
  .handle(async (ctx) => {
    console.log(ctx.deps.db.host); // typed
  });
```

### Contributing config from a library

A library that owns a config block publishes it through any workload that
composes it by implementing `ConfigContributor` on its plugin:

```ts
import type { ConfigContributor, ConfigDefinition } from '@putnami/runtime';

export const CoreConfig = Config('core', { auth: { clientSecret: Sensitive(String) } });

class CorePlugin implements Plugin, ConfigContributor {
  name = 'core';
  configDefinitions(): ConfigDefinition[] {
    return [CoreConfig];
  }
}
```

Config extraction walks the composed plugin tree, aggregates every
`ConfigContributor`'s blocks into the workload's published `schema/config.json`
(and the secrets their `Sensitive` fields declare), and rejects a path the
workload already defines — the same way migration sources aggregate from deps.

## Logging

```ts
import { useLogger } from '@putnami/runtime';

const logger = useLogger('MyService');
logger.info('Starting', { port: 3000 });
logger.error('Failed', { error });
logger.debug('Query', { sql, params });
```

## Error Handling

```ts
import { HttpException } from '@putnami/runtime';

throw new HttpException(404, 'User not found');
throw new HttpException(400, 'Invalid input', { fields: ['email'] });
```

## Detailed Documentation

See `doc/` folder:
- `dependency-injection.md` — full DI guide
- `configuration.md` — config system
- `logging.md` — structured logging
- `error-handling.md` — error types
- `context-management.md` — async context

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It owns two specifications: the
[dependency-injection specification](specs/dependency-injection.json) with its
[composition-time lifetimes ADR](doc/adr/0001-lifetimes-and-visibility-are-composition-time-decisions.md),
and the [typed-configuration specification](specs/typed-configuration.json) with
its [precedence and secrets ADR](doc/adr/0002-configuration-precedence-and-name-only-secrets.md).
A dependency graph is validated when the container starts, not on first
resolution, and a `Sensitive()` field is name-only everywhere it is reported.
The schema vocabulary re-exported here is owned by
[`@putnami/utils`](../utils/README.md). Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
