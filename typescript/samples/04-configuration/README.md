# Configuration

Type-safe configuration with environment support.

## Features

- `Config()` builder for typed config schemas
- Multi-source config: `app.config.ts`, `database.config.ts`, `api.config.ts`
- Environment files: `.env.local.yaml`, `.env.test.yaml`, `.env.production.yaml`
- `getEnv()` environment detection
- `configToken()` + `.inject()` for typed config access in endpoints (multi-datasource pattern)
- `Default()` and `Int` schema types for validation
- `Sensitive()` + `Env()` for secrets (redacted in config dumps, sourced from env vars)
- Manifest-driven packaged activation: generated route loaders statically
  register routes and route-owned config definitions with no `.gen/src` tree at
  runtime

## Routes

| Method | Path | Description |
|--------|------|-------------|
| GET | [http://localhost:3904/](http://localhost:3904/) | All loaded configurations |
| GET | [http://localhost:3904/database](http://localhost:3904/database) | Primary database config |
| GET | [http://localhost:3904/database/replica](http://localhost:3904/database/replica) | Replica database config (multi-datasource) |
| GET | [http://localhost:3904/env](http://localhost:3904/env) | Environment detection info |

## Run

```bash
putnami serve .
```

`putnami build . --compile` selects the generated bundled entrypoint. Its
static imports come from `.gen/schema/capabilities.json`; after packaging, the
binary does not scan or import `.gen/src` at runtime. The sample test bundles
that exact entrypoint, runs it from an isolated directory where `.gen/src` is
absent, and verifies `/database` can resolve `DatabaseConfig` through DI.

## Try It

Once the server is running, open a new terminal:

**1. View all loaded configuration:**

```bash
curl http://localhost:3904/
```

Expected response:

```json
{
  "message": "Welcome to Config Sample (Test)!",
  "environment": "test",
  "app": { "name": "Config Sample (Test)", "version": "1.0.0", "debug": false },
  "features": { "cache": true, "metrics": true },
  "databases": {
    "primary": { "host": "localhost", "port": 5432, "name": "test_db" },
    "replica": { "host": "localhost", "port": 5432, "name": "test_db" }
  },
  "externalApi": { "baseUrl": "https://mock.weather.test", "timeout": 1000, "retries": 1 }
}
```

**2. View primary database config:**

```bash
curl http://localhost:3904/database
```

Expected response — database connection details for the primary datasource.

**3. View replica database config (multi-datasource pattern):**

```bash
curl http://localhost:3904/database/replica
```

Expected response — database connection details loaded from the `database.replica` config path. Demonstrates a separate `ReplicaDatabaseConfig` token bound to that path.

**4. Check the current environment:**

```bash
curl http://localhost:3904/env
```

Expected response — environment info including the detected env name (`local`, `test`, or `production`).

## Key Concepts

### Multi-Datasource Pattern

Define a `Config()` per datasource (each bound to its own path), then inject it
into an endpoint with `configToken()`:

```typescript
// src/config/database.config.ts
export const DatabaseConfig = Config('database.primary', { /* ... */ });
export const ReplicaDatabaseConfig = Config('database.replica', { /* ... */ });

// src/api/database/replica/get.ts
export const GET = endpoint()
  .inject({ config: configToken(ReplicaDatabaseConfig) })
  .handle((ctx) => ({ host: ctx.deps.config.host }));
```

### Environment Detection

```typescript
const env = getEnv(); // 'local' | 'test' | 'production'
```

### Secrets

Mark secret fields with `Sensitive()` so their values are redacted in config
dumps and error origins, and source them from environment variables with
`Env()` rather than committing literals:

```typescript
// src/config/database.config.ts
password: Sensitive(Env('DATABASE_PRIMARY_PASSWORD', Default(String, ''))),
```

The committed `conf/.env.*.yaml` files keep secret keys blank — the real value
is supplied at runtime via the env var (e.g. `DATABASE_PRIMARY_PASSWORD`).

`putnami build .` derives the declared secret **names** from the same schema and
writes them to the committed [`infra/requirements.json`](infra/requirements.json).
That sidecar is how infrastructure learns which secrets a workload needs; it
never carries a value.

## Test

```bash
putnami test .
```

## What this sample proves

The committed `conf/.env.*.yaml` files, the `Env()`-sourced secret, and the
per-datasource `Config()` blocks exercise the documented precedence order: files
supply the environment-specific values, `Env()` fills only what no file set, and
the `Sensitive()` password is redacted wherever the configuration is described.

Contract: [`@putnami/runtime`](../../framework/runtime/README.md) —
[typed-configuration specification](../../framework/runtime/specs/typed-configuration.json).
