# Configuration

Complete reference for configuring `@putnami/application` plugins and the application framework.

## Configuration Overview

The Application framework uses a plugin-based configuration system. Each plugin can be configured through:

1. **Constructor options** - Passed directly when creating the plugin
2. **YAML configuration files** - Located in `conf/.env.{env}.yaml`
3. **Environment variables** - Override any configuration value

## Shared Application Configuration (`PutnamiConfig`)

The `PutnamiConfig` holds settings shared across the HTTP server, static files, and React SSR plugins. It reads from the `putnami:` path in YAML:

```yaml
putnami:
  port: 3000
  publicFolder: public
  skipLoading: false
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `port` | `number` | `3000` | HTTP server port (overridden by `process.env.PORT` or `http({ port })`) |
| `publicFolder` | `string` | `'public'` | Base folder for static assets |
| `skipLoading` | `boolean` | `false` | Skip loading generated route files at startup |

## HTTP Plugin Configuration

The HTTP plugin accepts configuration through the `http()` function:

```typescript
import { http } from '@putnami/application';

export const app = () => application().use(
  http({
    port: 3000,
  })
);
```

### Configuration Options

#### `port`

**Type:** `number | string`
**Default:** `3000` or `process.env.PORT`

The port number to listen on. Set to `0` to use an available port. The port is read from `PutnamiConfig` (`putnami.port` in YAML) but can be overridden via constructor options or the `PORT` environment variable.

```typescript
http({
  port: 8080,
})
```

### YAML Configuration

You can also configure via `conf/.env.{env}.yaml`:

```yaml
putnami:
  port: 3000
```

## API Plugin Configuration

The API plugin supports file-based route discovery:

```typescript
import { api } from '@putnami/application';

export const app = () => application().use(
  api({
    prefix: '/api',
    scanFolder: 'api',
    autoScan: true,
  })
);
```

### Configuration Options

#### `prefix`

**Type:** `string`
**Default:** `undefined`

Prefix to add to all discovered routes.

```typescript
api({
  prefix: '/v1', // /users becomes /v1/users
})
```

#### `scanFolder`

**Type:** `string`
**Default:** `'api'`

The folder name to scan for routes. The plugin looks for this folder relative to your entry point.

```typescript
api({
  scanFolder: 'routes', // Scans src/routes/ instead of src/api/
})
```

#### `autoScan`

**Type:** `boolean`
**Default:** `true`

Automatically scan for routes on startup. Set to `false` to disable automatic scanning.

```typescript
api({
  autoScan: false, // Disable automatic route scanning
})
```

#### `openapi`

**Type:** `{ title: string; version: string; description?: string; servers?: { url: string; description?: string }[] } | undefined`
**Default:** `undefined`

Enable build-time OpenAPI 3.0.3 spec generation. When set, the plugin generates an `openapi.json` file in `.gen/` during the build step.

```typescript
api({
  openapi: {
    title: 'My API',
    version: '1.0.0',
    description: 'My service API',
    servers: [
      { url: 'https://api.example.com', description: 'Production' },
    ],
  },
})
```

Routes defined with the `route()` builder automatically contribute their `.params()`, `.query()`, `.body()`, and `.returns()` schemas to the generated spec.

### YAML Configuration

```yaml
api:
  prefix: '/api'
  scanFolder: 'api'
  autoScan: true
```

## Static Files Plugin Configuration

The static files plugin serves files from a directory. It uses shared settings from `PutnamiConfig` (`publicFolder`, `skipLoading`) and static-specific settings from `StaticConfig` at the `putnami.static` YAML path.

```typescript
import { staticFiles } from '@putnami/application';

export const app = () => application().use(
  staticFiles({
    prefix: '/assets',
    cacheMaxAge: 604800,
    compress: 1024,
  })
);
```

### Configuration Options

#### `prefix`

**Type:** `string`
**Default:** `undefined`

URL prefix for static files.

```typescript
staticFiles({
  prefix: '/static', // Files served at /static/...
})
```

#### `cacheMaxAge`

**Type:** `number`
**Default:** `86400` (1 day)

Cache duration in seconds. Set to `0` to disable caching.

```typescript
staticFiles({
  cacheMaxAge: 604800, // 1 week
})
```

#### `compress`

**Type:** `number`
**Default:** `1024`

Minimum file size in bytes to compress. Files larger than this are gzipped.

```typescript
staticFiles({
  compress: 2048, // Only compress files > 2KB
})
```

> **Note:** `publicFolder` and `skipLoading` are shared settings configured under `putnami:` in YAML. See [Shared Application Configuration](#shared-application-configuration-putnamiconfig) above.

### YAML Configuration

```yaml
putnami:
  publicFolder: public      # shared setting
  static:
    prefix: '/assets'
    cacheMaxAge: 604800
    compress: 1024
```

## Config Plugin Configuration

The config plugin discovers and merges YAML configuration from dependencies:

```typescript
import { config } from '@putnami/application';

export const app = () => application().use(
  config({ env: 'local' })
);
```

### Configuration Options

#### `env`

**Type:** `string`
**Default:** `process.env.NODE_ENV || 'local'`

Environment name to load configuration for. The plugin looks for `conf/.env.{env}.yaml` files.

```typescript
config({
  env: 'production',
})
```

### How It Works

1. Walks your dependency tree
2. Loads `conf/.env.{env}.yaml` from each dependency
3. Merges configs (later dependencies override earlier)
4. Writes merged config to `.gen/conf/.env.{env}.yaml`

### Module Config Structure

```
packages/my-module/
├── conf/
│   ├── .env.local.yaml   # Local development
│   ├── .env.test.yaml    # Test config
│   └── .env.prod.yaml    # Production
└── src/
```

## OAuth Plugin Configuration

The OAuth plugin handles OAuth2 authentication flows:

```typescript
import { oAuth2 } from '@putnami/application';

export const app = () => application().use(
  oAuth2({
    loginRoute: '/login',
    logoutRoute: '/logout',
  })
);
```

### Configuration Options

#### `loginRoute`

**Type:** `string`
**Default:** `'/login'`

Route path for login.

#### `logoutRoute`

**Type:** `string`
**Default:** `'/logout'`

Route path for logout.

### YAML Configuration

```yaml
oauth:
  clientId: 'your-client-id'
  clientSecret: 'your-client-secret'
  loginRoute: '/login'
  logoutRoute: '/logout'
  defaultLoginRedirectUrl: '/'
  authorizeUri: 'https://auth.example.com/authorize'
  tokenUri: 'https://auth.example.com/token'
  keysUri: 'https://auth.example.com/.well-known/jwks.json'
  signoutUri: 'https://auth.example.com/logout'
  issuer: 'https://auth.example.com'    # Optional: validate token issuer globally
  audience: 'my-api'                     # Optional: validate token audience globally
  scopes:
    - 'openid'
    - 'profile'
```

## Session Configuration

Session management is configured via YAML:

```yaml
session:
  store: 'cookie'           # 'cookie' | 'memory' | 'database'
  cookieName: 'session'
  cookieSecret: '...'       # Required: crypto.randomBytes(32).toString('hex')
  ttl: 604800               # 1 week in seconds
```

### Store Types

| Store | Description |
|-------|-------------|
| `cookie` | Encrypted client-side storage (default) |
| `memory` | Server-side with TTL expiration |
| `database` | PostgreSQL backend (requires `@putnami/database`) |

## Environment-Based Configuration

Use environment variables and conditions for different environments:

```typescript
// src/main.ts
import { application, http, api } from '@putnami/application';

const isProduction = process.env.NODE_ENV === 'production';

export const app = () => application()
  .use(http({
    port: process.env.PORT || 3000,
  }))
  .use(api({
    prefix: isProduction ? '/api' : undefined,
  }))
  .run(async () => {
    console.log(`Server running in ${process.env.NODE_ENV || 'development'} mode`);
  });
```

## Configuration Priority

Configuration values are resolved in this order (highest to lowest priority):

1. **Constructor options** - Passed directly to plugin
2. **Environment variables** - `process.env.*`
3. **YAML configuration** - From `conf/.env.{env}.yaml`
4. **Default values** - Plugin defaults

## Best Practices

1. **Use environment-based config** - Different settings for dev/prod
2. **Keep defaults when possible** - Only override what you need
3. **Store secrets in environment variables** - Never commit secrets to YAML
4. **Use the config plugin** - Automatically merge configs from dependencies
5. **Document custom config** - Note any non-standard settings

## Next Steps

- Learn about [Plugins](plugins.md) for plugin architecture
- Explore [HTTP Server](http-server.md) for routing configuration
- Check [API Reference](api-reference.md) for complete configuration options
