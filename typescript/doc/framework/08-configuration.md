# Configuration

Putnami uses YAML files for configuration combined with typed config definitions for validation. This provides both flexibility and type safety.

## Configuration files

### File hierarchy

Configuration is loaded from multiple sources in order of precedence (highest wins):

1. Environment variables via `Env()` descriptors (highest precedence)
2. `CONFIG_DATA` environment variable (inline YAML string)
3. `.gen/conf/.env.{env}.yaml` - Generated config files (created by build tools)
4. `conf/.env.{env}.yaml` - Source config files in your project
5. `confInit` values - Programmatic defaults from plugin constructors
6. `Default()` values in the schema (lowest precedence)

### Basic structure

```yaml
# .env.yaml
putnami:
  port: 3000
  publicFolder: public
  static:
    compress: 1024
    cacheMaxAge: 86400
  react:
    scanFolder: app
    ssrTimeout: 5000

database:
  host: localhost
  port: 5432
  database: myapp

logger:
  level: info
```

### Local overrides

```yaml
# .env.local.yaml (gitignored)
database:
  password: my-local-password

oauth:
  clientSecret: my-dev-secret
```

### Environment-specific

```yaml
# .env.production.yaml
putnami:
  port: 8080

database:
  host: prod-db.example.com
  ssl: true

logger:
  level: warn
```

## Typed configuration

### Defining a config

```ts
import { Config, useConfig, Default, Optional } from '@putnami/runtime';

export const HttpConfig = Config('http', {
  port: Default(Number, 3000),
  hostname: Default(String, '0.0.0.0'),
});

// Usage
const config = useConfig(HttpConfig);
console.log(config.port); // 3000
```

### Config path

The first argument to `Config()` is the dot-notation path in YAML:

```ts
const FeatureConfig = Config('myapp.feature', {
  enabled: Default(Boolean, false),
  maxItems: Default(Number, 100),
});
```

This loads from:

```yaml
myapp:
  feature:
    enabled: true
    maxItems: 100
```

### Schema primitives

```ts
import {
  Config,
  useConfig,
  Default,
  Optional,
  ArrayOf,
  OneOf,
  Min,
  Max,
  MinLength,
  MaxLength,
  Email,
  Url,
  Uuid,
  Int,
  Sensitive,
  Env,
  Resolve,
} from '@putnami/runtime';

const AppConfig = Config('app', {
  name: MinLength(1),
  port: Default(Min(1), 3000),
  debug: Default(Boolean, false),
  environment: Default(OneOf('development', 'staging', 'production'), 'development'),
  adminEmail: Optional(Email),
  apiEndpoint: Optional(Url),
});
```

### Nested configuration

Use plain objects for nested structures:

```ts
const ServicesConfig = Config('services', {
  database: {
    host: Default(String, 'localhost'),
    port: Default(Number, 5432),
    database: String,
    user: String,
    password: Sensitive(String),
  },
  cache: {
    host: Default(String, 'localhost'),
    port: Default(Number, 6379),
  },
});
```

```yaml
services:
  database:
    host: db.example.com
    port: 5432
    database: myapp
    user: postgres
    password: secret
  cache:
    host: cache.example.com
    port: 6379
```

### Type annotations

Use `InferConfig` to extract TypeScript types from config definitions:

```ts
import type { InferConfig } from '@putnami/runtime';

type AppCfg = InferConfig<typeof AppConfig>;
```

## Environment variables

### Direct access

```ts
import { getEnv } from '@putnami/runtime';

const env = getEnv(); // 'local', 'test', or 'production'
```

### Binding fields to env vars

Use `Env()` to bind a config field to an environment variable:

```ts
const ServerConfig = Config('server', {
  port: Env('PORT', Number),
  host: Default(String, '0.0.0.0'),
});
```

### Secret resolution

Use `Resolve()` for async secret resolution (e.g., from a secret manager):

```ts
const SecretConfig = Config('secrets', {
  apiKey: Resolve(async () => {
    return await fetchFromSecretManager('API_KEY');
  }, String),
  dbPassword: Resolve(async () => {
    return await fetchFromSecretManager('DB_PASSWORD');
  }, Sensitive(String)),
});

// At bootstrap, resolve all async values
await resolveConfigs(SecretConfig);

// Then use synchronously
const secrets = useConfig(SecretConfig);
```

### In YAML files

Reference environment variables using `${VAR_NAME}`:

```yaml
database:
  password: ${DATABASE_PASSWORD}

oauth:
  clientSecret: ${OAUTH_CLIENT_SECRET}

api:
  key: ${API_KEY:default-key}  # With default value
```

## Using configuration

### In services

```ts
import { useConfig } from '@putnami/runtime';
import { AppConfig } from './config/app.config';

export class MyService {
  private config = useConfig(AppConfig);

  doSomething() {
    if (this.config.debug) {
      console.log('Debug mode enabled');
    }
  }
}
```

### In route handlers

```ts
import { endpoint } from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { FeatureConfig } from '../config/feature.config';

export default endpoint(() => {
  const config = useConfig(FeatureConfig);

  return {
    feature: config.name,
    enabled: config.enabled,
  };
});
```

### In plugins

```ts
import type { Plugin } from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import { MyPluginConfig } from './config';

export function myPlugin(): Plugin {
  const config = useConfig(MyPluginConfig);

  return {
    async warmup() {
      console.log(`Plugin initialized with ${config.setting}`);
    },
  };
}
```

## Framework configuration

### Putnami application

Shared settings for the HTTP server, static files, and React SSR:

```yaml
putnami:
  port: 3000
  publicFolder: public
  skipLoading: false
  static:                # Static files plugin
    compress: 1024
    cacheMaxAge: 86400
    prefix: '/assets'
  react:                 # React SSR plugin
    scanFolder: app
    autoScan: true
    ssrTimeout: 5000
```

### Database

```yaml
database:
  host: localhost
  port: 5432
  database: myapp
  user: postgres
  password: secret
  ssl: false
  queryProfiling: false
```

### Sessions

```yaml
session:
  store: 'cookie'
  cookieName: 'session'
  cookieSecret: 'your-32-character-secret-key!!!'
  ttl: 604800
  secure: true
  sameSite: 'lax'
```

### OAuth

```yaml
oauth:
  clientId: 'your-client-id'
  clientSecret: 'your-client-secret'
  authorizeUri: 'https://auth.example.com/authorize'
  tokenUri: 'https://auth.example.com/token'
  scopes:
    - 'openid'
    - 'profile'
```

### Logging

```yaml
logging:
  level: 'info'  # 'debug', 'info', 'warn', 'error'
```

## Configuration patterns

### Feature flags

```ts
const FeatureFlags = Config('features', {
  newDashboard: Default(Boolean, false),
  betaFeatures: Default(Boolean, false),
  maintenanceMode: Default(Boolean, false),
});
```

```yaml
features:
  newDashboard: true
  betaFeatures: false
  maintenanceMode: false
```

### API clients

```ts
const StripeConfig = Config('integrations.stripe', {
  apiKey: Sensitive(String),
  webhookSecret: Sensitive(String),
  mode: Default(OneOf('test', 'live'), 'test'),
});
```

```yaml
integrations:
  stripe:
    apiKey: ${STRIPE_API_KEY}
    webhookSecret: ${STRIPE_WEBHOOK_SECRET}
    mode: test
```

### Multi-tenant configuration

```ts
const TenantConfig = Config('tenants', {
  defaultTenant: Default(String, 'default'),
  multiTenantEnabled: Default(Boolean, false),
});
```

## Config with DI

When running inside a DI container (via `Application`), `useConfig()` automatically delegates to `ConfigService`, which provides instance-scoped caching and per-field origin tracking.

### Config tokens

Create typed DI tokens for config definitions using `configToken()`:

```ts
import { configToken, provideConfig } from '@putnami/runtime';
import { DatabaseConfig } from './database.config';

// In endpoint handlers:
endpoint()
  .inject({ db: configToken(DatabaseConfig) })
  .handle(async (ctx) => {
    console.log(ctx.deps.db.host); // typed as string
  });
```

Config tokens that have been created via `configToken()` are auto-registered by `Application` at container build time.

### Debugging config origins

`ConfigService` can show where each field value comes from:

```ts
const configService = ctx.get(ConfigService);
const desc = configService.describe(DatabaseConfig);

for (const origin of desc.origins) {
  console.log(`${origin.field}: ${origin.value} (from ${origin.source})`);
}
// host: prod.db (from CONFIG_DATA)
// port: 5432 (from default)
// password: *** (from env:DB_PASSWORD)
```

### Custom config sources

Provide alternative config backends for testing or production:

```ts
import type { ConfigSource } from '@putnami/runtime';

const vaultSource: ConfigSource = {
  name: 'vault',
  priority: 90,
  load: () => fetchVaultSecrets(),
};
```

## Testing with configuration

### Override in tests

Use `confInit` to provide programmatic defaults. These are overridden by YAML files, so in tests you may combine them with `resetConfigLoader()` and `CONFIG_DATA`:

```ts
import { resetConfigLoader } from '@putnami/runtime';

beforeEach(() => {
  // Reset config cache between tests
  resetConfigLoader();
});

test('uses confInit as programmatic default', () => {
  const config = useConfig(DatabaseConfig, {
    confInit: { host: 'test-db' },
  });
  expect(config.host).toBe('test-db');
});
```

### Test configuration file

Create `.env.test.yaml` for test-specific values:

```yaml
database:
  host: localhost
  database: myapp_test

logging:
  level: error
```

## Related guides

- [Configure your app](/docs/how-to/configure-your-app)

## Support and compatibility

`@putnami/runtime` is a public, documented, maintained package classified
`stable`. Its typed-configuration specification and its accepted precedence and
secret-handling ADR live next to the package source. Two rules define the
promise: source precedence is fixed — base file, environment file, build-merged
file, local secrets, registered extension sources, then `CONFIG_DATA`, with a
field-level `Env()` filling only the keys nothing else supplied — and a field
declared `Sensitive()` contributes its name, never its value, to validation
errors, config descriptions, and infrastructure manifests.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
