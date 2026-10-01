# Configuration

Complete guide to the type-safe configuration system in `@putnami/runtime`.

## Overview

The configuration system provides:

- Type-safe configuration definitions with schema validation
- Automatic loading from YAML files
- Environment-specific configuration
- Built-in validation using schema primitives
- Environment variable binding via `Env()`
- Async secret resolution via `Resolve()`
- Sensitive field redaction in error messages
- Multi-datasource support (same config definition, different paths)
- Caching for performance

## Basic Usage

### Defining a Config

Create a configuration definition with the `Config()` function:

```typescript
import { Config, useConfig, Default, Optional } from '@putnami/runtime';

const DatabaseConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Number, 5432),
  name: Default(String, 'postgres'),
  password: Optional(String),
});
```

### Loading Configuration

Use `useConfig()` to load a validated configuration:

```typescript
const dbConfig = useConfig(DatabaseConfig);
console.log(dbConfig.host); // Value from YAML or default 'localhost'
console.log(dbConfig.port); // Value from YAML or default 5432
```

The first call loads and validates the config, then caches it. Subsequent calls return the cached instance.

### Type Annotations

Use `InferConfig` to extract the TypeScript type from a config definition:

```typescript
import type { InferConfig } from '@putnami/runtime';

type DbConfig = InferConfig<typeof DatabaseConfig>;
// { host: string; port: number; name: string; password?: string }
```

## Configuration Sources

Configuration is loaded from multiple sources in priority order (highest wins):

1. **Field-level `Env(...)`** descriptors (highest priority, applied during schema validation)
2. **`CONFIG_DATA` environment variable** — Inline YAML string for containers (priority 60)
3. **Registered extension sources** — for example `@putnami/cloud/runtime`
   contributes `/api/secrets/resolve` (priority 55) and
   `/api/configs/resolve` (priority 50) when it is installed and
   `CONFIG_SERVER_URL` is set
4. **`conf/.secrets.{env}.yaml`** — Local secrets file, gitignored by template default (priority 35)
5. **`.gen/conf/.env.{env}.yaml`** — Generated config files (priority 30)
6. **`conf/.env.{env}.yaml`** — Source config files in your project (priority 20)
7. **`conf/.env.yaml`** — Env-agnostic base config shared across all environments (priority 10)
8. **`confInit` values** — Programmatic defaults passed via `useConfig()`
9. **`Default()` values** in the schema (lowest priority)

### Remote sources

The remote source is contributed by an extension package, such as
`@putnami/cloud/runtime`. The framework activates it through an optional
dynamic `require` when `node_modules` are present, which covers `putnami serve`
and tests. A deployed workload is a bun-compiled binary, where that dynamic
`require` cannot resolve, so the deployed workload must **depend on the package
and activate it statically**: add an `activate.ts` that calls
`register({ registerSourceDiscoverer, registerConfigLoaderResetHook })` from
`@putnami/cloud/runtime`, and import it as the **first import of `main.ts`**.
The registrar dedupes by discoverer identity, so auto-activation and the static
import firing together is safe. The generated bundled entrypoint
(`.gen/src/serve.bundled.ts`) imports `main.ts` before the generated server
modules it activates, so this first import runs before any generated module
reads config — a `ReactApplication` reads config while its module evaluates.

```typescript
// src/activate.ts
import { register } from '@putnami/cloud/runtime';
import { registerConfigLoaderResetHook, registerSourceDiscoverer } from '@putnami/runtime';

register({ registerSourceDiscoverer, registerConfigLoaderResetHook });
```

```typescript
// src/main.ts
import './activate';
// ...the rest of the application imports
```

When `CONFIG_SERVER_URL` is set and no registered source serves it, startup
fails with one line naming this gap, instead of booting on schema defaults
while the control plane is never called. Without `CONFIG_SERVER_URL` (dev,
tests) the local file and env sources are the whole story and nothing is
reported.

Remote config, remote secrets, and their bearer-token resolution live in
`@putnami/cloud/runtime`; core `@putnami/runtime` only owns the source
registration seam. Bearer-token resolution checks these sources in order:

1. **Operator-provided bearer token** (highest) via env var. Three names accepted, checked in this order — most-specific wins:
   - `PUTNAMI_CLOUD_TOKEN` — preferred name for the cloud control-plane bearer.
   - `CONFIG_SERVER_TOKEN` — original framework name; kept working for existing setups.
   - `PUTNAMI_TOKEN` — generic Putnami token, broad fallback.

   Only mode usable from CI / scripts.
2. **GCP workload identity** — when `K_SERVICE` or `GOOGLE_CLOUD_PROJECT` is set, `@putnami/cloud/runtime` mints an ID token from the metadata server, audience-bound to `CONFIG_SERVER_AUDIENCE` when set, otherwise `CONFIG_SERVER_URL`. Cached and refreshed ~5min before expiry.
3. **Unauthenticated** — the server rejects the call; `@putnami/cloud/runtime` logs a startup warning.

The same identity is presented to both `/api/configs/resolve` and `/api/secrets/resolve` (scopes are bound to the identity, not the endpoint).

On Cloud Run, `CONFIG_SERVER_URL` should be the exact full
`/api/configs/resolve?appName=...&environment=...&secretsMode=reveal` URL
provided by the control plane, and `CONFIG_SERVER_AUDIENCE` should be the
control-plane origin. `@putnami/cloud/runtime` fetches that URL with `GET`, reads
`response.config`, and does not call `/api/secrets/resolve`; `secretsMode=reveal`
already merges config and secrets. Remote fetch failures are startup errors on
Cloud Run/prod.

Required remote config and secrets sources retry transient network, timeout,
and 5xx/429 failures before giving up. `CONFIG_SERVER_TIMEOUT` controls the
per-attempt timeout (default `5s`), and `CONFIG_SERVER_RETRY_BUDGET` controls
the total retry budget (default `30s`; set `0ms` to disable retries).

For local `putnami serve`, set the same exact `CONFIG_SERVER_URL` plus a user or
session bearer in `PUTNAMI_CLOUD_TOKEN` (or `CONFIG_SERVER_TOKEN` /
`PUTNAMI_TOKEN`). `@putnami/cloud/runtime` uses the token in memory for the fetch, does not
require `CONFIG_DATA`, and does not persist plaintext secrets.

To plug in a custom resolver (Workload Identity Federation, OIDC client creds, custom secret-manager-minted JWTs), import `RemoteConfigSource` / `RemoteSecretsSource` from `@putnami/cloud/runtime` and pass `tokenSource` directly when constructing them.

> `CONFIG_DATA` sits at priority 60. Field-level `Env(...)` resolves at
> 80, so it wins over `CONFIG_DATA` when both set the same field.

### Environment Detection

The environment is automatically detected:

| Condition | Environment |
|-----------|-------------|
| `NODE_ENV === 'test'` | `'test'` |
| `NODE_ENV === 'production'` or `'prod'` | `'production'` |
| `K_SERVICE` is set (GCP Cloud Run) | `'production'` |
| Otherwise | `'local'` |

You can also check the current environment:

```typescript
import { getEnv } from '@putnami/runtime';

const env = getEnv(); // 'local', 'test', or 'production'
```

### Config File Structure

Create environment-specific config files:

```
src/
└── conf/
    ├── .env.local.yaml    # Local development
    ├── .env.test.yaml     # Test environment
    └── .env.prod.yaml     # Production
```

Example `conf/.env.local.yaml`:

```yaml
database:
  host: localhost
  port: 5432
  name: myapp_db
  password: secret123
```

## Schema Primitives

### Base Types

Use JavaScript constructors for basic types:

| Schema | TypeScript Type | Description |
|--------|----------------|-------------|
| `String` | `string` | String value |
| `Number` | `number` | Numeric value |
| `Boolean` | `boolean` | Boolean value |

### Combinators

| Combinator | Example | TypeScript Type |
|------------|---------|----------------|
| `Optional(T)` | `Optional(String)` | `string \| undefined` |
| `ArrayOf(T)` | `ArrayOf(String)` | `string[]` |
| `Default(T, value)` | `Default(Number, 3000)` | `number` (optional in input, required in output) |
| `OneOf(...values)` | `OneOf('a', 'b', 'c')` | `'a' \| 'b' \| 'c'` |

### Constrained Types

| Schema | TypeScript Type | Validation |
|--------|----------------|------------|
| `Uuid` | `string` | UUID v4 format |
| `Email` | `string` | Valid email address |
| `Int` | `number` | Integer (no decimals) |
| `Url` | `string` | Valid URL |
| `DateIso` | `string` | ISO 8601 date |
| `Min(n)` | `number` | Value >= n |
| `Max(n)` | `number` | Value <= n |
| `MinLength(n)` | `string` | String length >= n |
| `MaxLength(n)` | `string` | String length <= n |
| `Pattern(regex)` | `string` | Must match regex |

### Environment Variables

Bind a field to an environment variable with `Env()`:

```typescript
const ServerConfig = Config('server', {
  port: Env('PORT', Number),
  host: Default(String, '0.0.0.0'),
});
```

### Secret Resolution

Use `Resolve()` for async secret resolution (e.g., from a secret manager):

```typescript
const AppConfig = Config('app', {
  apiKey: Resolve(async () => {
    return await fetchSecret('API_KEY');
  }, String),
});

// At bootstrap, resolve all async values
await resolveConfigs(AppConfig);

// Then use synchronously
const config = useConfig(AppConfig);
```

### Sensitive Fields

Mark fields as sensitive to redact values in error messages:

```typescript
const DbConfig = Config('database', {
  host: String,
  password: Sensitive(String),
});
```

## Multi-Datasource Pattern

Load the same config definition from different paths. Useful for multiple database connections, API endpoints, etc.

### Example: Multiple Databases

```typescript
const DatabaseConfig = Config('database', {
  host: Default(String, 'localhost'),
  port: Default(Number, 5432),
  name: Default(String, 'postgres'),
});

// Load from different paths
const authDb = useConfig(DatabaseConfig, { path: 'database.auth' });
const mainDb = useConfig(DatabaseConfig, { path: 'database.main' });
```

Config file structure:

```yaml
database:
  auth:
    host: auth-db.example.com
    port: 5432
    name: auth_db
  main:
    host: main-db.example.com
    port: 5432
    name: main_db
```

## Validation

Configuration is automatically validated against the schema when loaded:

```typescript
const ApiConfig = Config('api', {
  baseUrl: Url,
  apiKey: String,
  timeout: Default(Min(1), 30),
  version: Optional(String),
});
```

### Validation Errors

Validation errors show property names and constraints:

```
Config validation failed at 'api': field.timeout must be of type number
```

Sensitive fields have their values redacted in error messages.

## Programmatic Defaults

You can provide programmatic defaults when loading config. These act as fallback values — YAML files and environment variables override them:

```typescript
const config = useConfig(DatabaseConfig, {
  confInit: { host: 'fallback-host' },
});
```

This is commonly used by framework plugins to set sensible defaults that can be overridden by YAML config files:

```typescript
// Plugin sets defaults in code
const config = useConfig(OAuthConfig, {
  confInit: { authorizeUri: 'https://auth.putnami.cloud/authorize' },
});

// Users can override in conf/.env.local.yaml:
// oauth:
//   authorizeUri: 'https://custom-provider.com/authorize'
```

## Environment Variables

### `CONFIG_DATA`

Provide configuration as an inline YAML string:

```bash
CONFIG_DATA="database:\n  host: prod-db.example.com\n  port: 5432" bun run src/main.ts
```

This is useful for production deployments where you don't want to commit config files.

### `CONFIG_FILE`

Override the config file path (advanced usage):

```bash
CONFIG_FILE=/path/to/custom/config.yaml bun run src/main.ts
```

## Resetting Configuration

For testing, you can reset the configuration loader:

```typescript
import { resetConfigLoader } from '@putnami/runtime';

beforeEach(() => {
  resetConfigLoader();
  process.env.CONFIG_DATA = JSON.stringify({ database: { host: 'test-db' } });
});
```

This clears the cache and forces a reload on the next `useConfig()` call.

## Nested Configuration

Schema properties can be plain objects to define nested structures:

```typescript
const AppConfig = Config('app', {
  name: Default(String, 'My App'),
  database: {
    host: Default(String, 'localhost'),
    port: Default(Number, 5432),
  },
});
```

```yaml
app:
  name: My App
  database:
    host: localhost
    port: 5432
```

## Config with DI

When running inside a DI container (via `Application`), configuration automatically integrates with the DI system through `ConfigService`.

### How It Works

`ConfigService` is automatically registered by `Application` in the DI container. When active, all `useConfig()` calls delegate to `ConfigService`, providing:

- **Instance-scoped caching** — each `ConfigService` instance has its own cache (not module-level globals)
- **Per-field origin tracking** — `describe()` tells you where each config value came from
- **Custom sources** — inject alternative `ConfigSource` implementations for testing

No code changes are needed in existing `useConfig()` calls — the delegation is transparent.

### Config Tokens

Create a typed DI token for a config definition:

```typescript
import { configToken, provideConfig } from '@putnami/runtime';

const DbConfigToken = configToken(DatabaseConfig);
// Type: NamedToken<{ host: string; port: number; ... }>
```

Tokens are interned: calling `configToken(X)` with the same `ConfigDefinition` always returns the same token object (reference equality), which is required for `Map<Token, Provider>` lookups.

### Registering Config Providers

Use `provideConfig()` to create a DI registration that resolves config via `ConfigService`:

```typescript
import { provideConfig } from '@putnami/runtime';

// In your application setup:
app.register(provideConfig(DatabaseConfig));

// Then resolve via DI:
const config = ctx.get(configToken(DatabaseConfig));
```

Config tokens that have been created via `configToken()` are automatically registered by `Application` at container build time.

For packaged workloads, the generated bundled entrypoint statically imports
the server loader modules declared by `capabilities.json` before it constructs
the application. Route-owned `configToken()` calls therefore populate this
registry without runtime filesystem scanning. It also registers composed
`ConfigContributor` definitions and fails startup if any manifest config path
is still absent. The manifest's field summary is intentionally not converted
back into a `ConfigDefinition`, because it does not contain executable schema
descriptors, defaults, or validators.

### Injecting Config in Handlers

Use `.inject()` on endpoint builders to resolve config tokens:

```typescript
import { configToken } from '@putnami/runtime';

endpoint()
  .inject({ db: configToken(DatabaseConfig) })
  .handle(async (ctx) => {
    console.log(ctx.deps.db.host); // typed as string
  });
```

### Debugging with describe()

`ConfigService` provides per-field origin tracking:

```typescript
const configService = ctx.get(ConfigService);
const desc = configService.describe(DatabaseConfig);

for (const origin of desc.origins) {
  console.log(`${origin.field}: ${origin.value} (from ${origin.source})`);
}
// host: prod.db (from CONFIG_DATA)
// port: 5432 (from default)
// password: *** (from env:DB_PASSWORD)
```

Sources include (highest priority first): `env:VAR_NAME`, `CONFIG_DATA`, `.gen/conf`, `conf`, `confInit`, `default`.

### Custom Config Sources

Provide custom `ConfigSource` implementations for testing or alternative backends:

```typescript
import type { ConfigSource } from '@putnami/runtime';

const vaultSource: ConfigSource = {
  name: 'vault',
  priority: 90, // Higher than CONFIG_DATA (60)
  load: () => fetchVaultSecrets(),
};

// Inject into ConfigService
const service = new ConfigService([vaultSource, ...getDefaultSources()]);
```

## Best Practices

1. **Use defaults**: Always provide default values with `Default()` where possible
2. **Validate with schema**: Use constrained types (`Url`, `Email`, `Min`, etc.) for type safety
3. **Environment-specific files**: Keep different configs for different environments
4. **Never commit secrets**: Use `Env()`, `Resolve()`, or secure config management
5. **Mark secrets sensitive**: Use `Sensitive()` to prevent values appearing in logs
6. **Use multi-datasource**: Reuse config definitions for similar configurations
7. **Cache awareness**: Configs are cached, so changes require restart or `resetConfigLoader()`

## Troubleshooting

### Config Not Found

If config values are missing, check:

1. The `path` in your `Config()` call matches your YAML structure
2. The config file exists for the current environment
3. Default values are provided for optional fields with `Default()` or `Optional()`

### Validation Errors

If validation fails:

1. Check that your schema types match the data in your YAML
2. Verify your YAML file has the correct structure
3. Ensure default values match validation constraints

### Config Not Updating

Configs are cached. To see changes:

1. Restart your application
2. Use `resetConfigLoader()` in tests
3. Check that you're modifying the correct environment file

## Next Steps

- Learn about [Dependency Injection](dependency-injection.md) to inject configs into services
- Explore [Context Management](context-management.md) for request-scoped configuration
- Check the [API Reference](api-reference.md) for complete function signatures
