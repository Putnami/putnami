# Configure your app

You will add type-safe configuration to an app, create environment-specific config files, and use config values in route handlers.

## Steps

### 1) Define a config schema

Create `src/config/app.config.ts`:

```typescript
import { Config, Default } from '@putnami/runtime';

export const AppConfig = Config('app', {
  name: Default(String, 'My App'),
  version: Default(String, '0.0.0'),
  debug: Default(Boolean, false),
});
```

`Config()` binds a YAML path (`app`) to a typed schema. `Default()` declares the type and fallback value.

### 2) Create environment files

Create `.env.local.yaml` for local development:

```yaml
app:
  name: "My App (Local)"
  version: "1.0.0"
  debug: true
```

Create `.env.production.yaml` for production:

```yaml
app:
  name: "My App"
  version: "1.0.0"
  debug: false
```

Putnami picks the right file based on the environment. Environment variables override file values.

### 3) Use config in a route handler

Create `src/app/api/config/get.ts`:

```typescript
import { endpoint } from '@putnami/application';
import { configToken } from '@putnami/runtime';
import { AppConfig } from '../../../config/app.config';

export const GET = endpoint()
  .inject({ config: configToken(AppConfig) })
  .handle((ctx) => ({
    name: ctx.deps.config.name,
    version: ctx.deps.config.version,
    debug: ctx.deps.config.debug,
  }));
```

`configToken()` creates a DI token from your config schema. The framework resolves it automatically.

### 4) Add a second config for a different concern

Create `src/config/database.config.ts`:

```typescript
import { Config, Default, Int } from '@putnami/runtime';

export const DatabaseConfig = Config('database.primary', {
  host: Default(String, 'localhost'),
  port: Default(Int, 5432),
  name: Default(String, 'mydb'),
  user: Default(String, 'postgres'),
  password: Default(String, ''),
});
```

Add the matching YAML to `.env.local.yaml`:

```yaml
app:
  name: "My App (Local)"
  version: "1.0.0"
  debug: true

database:
  primary:
    host: localhost
    port: 5432
    name: dev_db
    user: postgres
    password: localpass
```

Each config schema maps to a different subtree of the same YAML files.

### 5) Verify

```bash
putnami serve .
curl http://localhost:3000/api/config
```

You should see your local config values in the response.

## Result

You now have type-safe configuration with environment-specific overrides, typed defaults, and DI integration.

## Next steps

- [Add persistence](/docs/how-to/add-persistence) — use database config to connect to PostgreSQL
- [Add authentication](/docs/how-to/add-authentication) — configure OAuth2 providers
- [Configuration reference](/docs/frameworks/typescript/configuration) — advanced patterns: `Env()`, `Sensitive`, `Resolve()`, and config debugging

> **Companion sample:** [`typescript/samples/04-configuration`](https://github.com/putnami/putnami/tree/main/typescript/samples/04-configuration) — a runnable project covering typed config, environment files, and multi-datasource patterns. Run `putnami serve @example/configuration` from the workspace root.
