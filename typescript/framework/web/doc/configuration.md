# Configuration

Complete reference for configuring `@putnami/web` to match your project's needs.

## Configuration Options

The React plugin accepts configuration through the `react()` function:

```typescript
import { react } from '@putnami/web';

export const app = () => application().use(
  react({
    scanFolder: 'app',
    autoScan: true,
    // ... other options
  })
);
```

## Configuration Reference

### `scanFolder`

**Type:** `string`
**Default:** `'app'`

The folder name to scan for routes. The plugin looks for this folder relative to your entry point.

```typescript
react({
  scanFolder: 'routes', // Scans src/routes/ instead of src/app/
})
```

### `scanPath`

**Type:** `string | undefined`
**Default:** `undefined`

Explicit path to scan for routes. If not provided, the plugin auto-detects based on `scanFolder` and your entry point.

```typescript
react({
  scanPath: '/absolute/path/to/routes',
})
```

### `scanRoots`

**Type:** `Array<{ path: string; routePrefix?: string }> | undefined`  
**Default:** `undefined`

Scan multiple route roots and optionally mount each root under a URL prefix.

This is useful for module-owned UI folders such as `src/projects/web` and `src/tasks/web`.

```typescript
react({
  scanRoots: [
    { path: 'src/app' },
    { path: 'src/projects/web', routePrefix: '/projects' },
    { path: 'src/tasks/web', routePrefix: '/tasks' },
  ],
})
```

If `routePrefix` is omitted and the folder name is `web` (or `(web)`), the parent folder name is used as prefix.

### `autoScan`

**Type:** `boolean`
**Default:** `true`

Automatically scan for routes on startup. Set to `false` to disable automatic scanning.

```typescript
react({
  autoScan: false, // Disable automatic route scanning
})
```

### `buildEnable`

**Type:** `boolean`
**Default:** `true`

Enable building client bundles. Set to `false` to skip client bundle generation.

```typescript
react({
  buildEnable: false, // Skip client bundle building
})
```

### `sourcemap`

**Type:** `'none' | 'inline' | 'external'`
**Default:** `'external'`

Source map generation strategy:
- `'none'` - No source maps
- `'inline'` - Inline source maps in the bundle
- `'external'` - Separate `.map` files

```typescript
react({
  sourcemap: 'none', // No source maps (production)
})
```

### `minify`

**Type:** `boolean`
**Default:** `true`

Minify client bundles. Set to `false` for development.

```typescript
react({
  minify: false, // Don't minify (development)
})
```

### `splitting`

**Type:** `boolean`
**Default:** `false`

Enable code splitting for client bundles. When enabled, routes are split into separate chunks.

```typescript
react({
  splitting: true, // Enable code splitting
})
```

### `ssrTimeout`

**Type:** `number`
**Default:** `3000`

Timeout in milliseconds for SSR rendering. If rendering takes longer, the request is aborted.

```typescript
react({
  ssrTimeout: 5000, // 5 second timeout
})
```

### `clientFetchTimeout`

**Type:** `number`
**Default:** `30000`

Timeout in milliseconds for client-side loader and action data fetches. The value is
serialized to the browser during SSR, so navigations against slow backends can be tuned
without forking the framework.

```typescript
react({
  clientFetchTimeout: 10000, // abort client data fetches after 10s
})
```

### `moduleCacheSize`

**Type:** `number`
**Default:** `500`

Maximum number of entries in the server-side module and page-data LRU caches before the
least-recently-used entry is evicted. Increase it for large route tables.

```typescript
react({
  moduleCacheSize: 2000, // retain more lazily-loaded route modules
})
```

### `csrf`

**Type:** `boolean`
**Default:** `true`

Validate CSRF tokens on `action()` POST endpoints (double-submit cookie). Page GETs
issue a `_csrf` cookie; action POSTs must echo the token back — via the
`X-CSRF-Token` header (sent automatically by the client action handler) or a
`_csrf` form field (`<CsrfInput />`) — or they are rejected with `403`.

Set to `false` only when CSRF is handled elsewhere (e.g. a reverse proxy or your
own `CsrfMiddleware`).

```typescript
react({
  csrf: false, // Disable built-in CSRF validation for action() POSTs
})
```

When the root HTTP plugin already enables token-based CSRF, React routes reuse
that single configured middleware. Its `CsrfOptions` remain authoritative:

```typescript
application()
  .use(http({ csrf: { sameSite: 'Lax', cookieMaxAge: 3600 } }))
  .use(react());
```

Do not set `react({ csrf: false })` merely to avoid duplicate middleware. That
option marks React routes as CSRF-exempt, including from the root HTTP
middleware; reserve it for applications whose protection is enforced outside
the Putnami request pipeline.

### `scriptsFolder`

**Type:** `string`
**Default:** `'scripts'`

Folder name for generated client scripts within the public folder.

```typescript
react({
  scriptsFolder: 'js', // Use .gen/public/js/ instead
})
```

### `isDevelopment`

**Type:** `boolean`
**Default:** `process.env.NODE_ENV !== 'production'`

Whether the app is in development mode. Affects error messages and debugging.

```typescript
react({
  isDevelopment: process.env.NODE_ENV === 'development',
})
```

### `page`

**Type:** `({ children }: { children?: ReactNode }) => ReactElement | undefined`
**Default:** `undefined`

Custom HTML page wrapper component. Overrides the default HTML template.

```typescript
react({
  page: ({ children }) => (
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <title>My App</title>
      </head>
      <body>
        <div id="root">{children}</div>
      </body>
    </html>
  ),
})
```

### `notFound`

**Type:** `({ children }: { children?: ReactNode }) => ReactElement | undefined`
**Default:** `undefined`

Custom HTML template for 404 pages.

```typescript
react({
  notFound: ({ children }) => (
    <html>
      <head>
        <title>404 - Not Found</title>
      </head>
      <body>
        <div id="root">{children}</div>
      </body>
    </html>
  ),
})
```

## Environment-Based Configuration

Use environment variables and conditions for different environments:

```typescript
// src/main.ts
import { react } from '@putnami/web';

const isProduction = process.env.NODE_ENV === 'production';

export const app = () => application().use(
  react({
    isDevelopment: !isProduction,
    minify: isProduction,
    sourcemap: isProduction ? 'none' : 'inline',
    ssrTimeout: isProduction ? 3000 : 10000, // Longer timeout in dev
  })
);
```

### Shared Configuration

The `publicFolder` and `skipLoading` settings are shared with `PutnamiConfig` from `@putnami/application`. They live under the `putnami:` YAML path (not under `putnami.react:`):

```yaml
putnami:
  publicFolder: assets    # shared: affects static files and React
  skipLoading: true       # shared: skip loading generated routes
  react:
    scanFolder: app       # react-specific
    ssrTimeout: 5000      # react-specific
```

You can still pass them via the `react()` constructor for convenience:

```typescript
react({
  publicFolder: 'assets', // Forwarded to PutnamiConfig
  skipLoading: true,      // Forwarded to PutnamiConfig
})
```

## Configuration File

The pre-build generator reads project settings from `putnami.json`. Shared
application settings stay directly in the hook block, while React-only settings
must be nested under `react`:

```json
{
  "options": {
    "@putnami/web:generate": {
      "publicFolder": "public",
      "react": {
        "scanRoots": [
          { "path": "src/app" },
          { "path": "src/projects/web", "routePrefix": "/projects" }
        ],
        "minify": true
      }
    }
  }
}
```

A known React-only option at the hook-block root is rejected before route
scanning or artifact writes. For example,
`options["@putnami/web:generate"].scanRoots` reports the corrected path
`options["@putnami/web:generate"].react.scanRoots`; it is not silently ignored.

Legacy `package.json#putnami` configuration remains supported, including its
historical flat React keys:

```json
{
  "putnami": {
    "publicFolder": "public",
    "scanFolder": "app",
    "autoScan": true,
    "minify": true,
    "sourcemap": "external",
    "ssrTimeout": 3000
  }
}
```

Nested `package.json#putnami.react` is also accepted. When both files configure
the same React setting, nested
`putnami.json#options["@putnami/web:generate"].react` wins. Shared settings and
React settings are resolved independently.

## Custom HTML Templates

### Default Template

The default template is minimal. Customize it with the `page` option:

```typescript
import { react } from '@putnami/web';

export const app = () => application().use(
  react({
    page: ({ children }) => (
      <html lang="en">
        <head>
          <meta charSet="utf-8" />
          <meta name="viewport" content="width=device-width, initial-scale=1" />
          <link rel="icon" href="/favicon.ico" />
          <title>My App</title>
        </head>
        <body>
          <div id="root">{children}</div>
        </body>
      </html>
    ),
  })
);
```

### Using Document Helpers in Template

```typescript
import { documentHelper } from '@putnami/web';

react({
  page: ({ children }) => {
    const doc = documentHelper();

    return (
      <html lang={doc.lang}>
        <head>
          <meta charSet="utf-8" />
          <title>{doc.title}</title>
          {doc.headHtml && <div dangerouslySetInnerHTML={{ __html: doc.headHtml }} />}
        </head>
        <body>
          <div id="root">{children}</div>
        </body>
      </html>
    );
  },
})
```

## Build Configuration

### Development

```typescript
react({
  isDevelopment: true,
  minify: false,
  sourcemap: 'inline',
  ssrTimeout: 10000, // Longer timeout for debugging
})
```

### Production

```typescript
react({
  isDevelopment: false,
  minify: true,
  sourcemap: 'none',
  ssrTimeout: 3000,
  splitting: true, // Enable code splitting
})
```

## Advanced Configuration

### Custom Route Generation

If you need custom route generation, you can disable auto-scanning and manually configure routes:

```typescript
import { ReactApplication } from '@putnami/web';

const reactApp = new ReactApplication();

// Manually configure routes
reactApp.reactPage('/', { page: HomePage, loader: homeLoader });
reactApp.reactLayout('/dashboard', DashboardLayout);

// Then use the HTTP plugin
export const app = () => application().use(reactApp.getHttpPlugin());
```

### Multiple Scan Paths

You can't directly configure multiple scan paths, but you can organize your routes:

```
src/
├── app/          # Main app routes
│   └── page.tsx
└── admin/         # Admin routes (if needed, use route groups)
    └── (admin)/
        └── page.tsx
```

### Custom Public Folder Structure

```typescript
react({
  publicFolder: 'static',
  scriptsFolder: 'bundles',
})
```

This creates:
```
.gen/
└── static/
    └── bundles/
        └── hydrate.main.js
```

## Troubleshooting Configuration

### Routes Not Found

If routes aren't being found:

1. Check `scanFolder` matches your folder structure
2. Verify `autoScan` is `true`
3. Check that files follow naming conventions (`page.tsx`, `layout.tsx`, etc.)

### Build Issues

If client bundles aren't building:

1. Ensure `buildEnable` is `true`
2. Check for TypeScript errors
3. Verify `publicFolder` is accessible

### SSR Timeout

If you're getting SSR timeouts:

1. Increase `ssrTimeout`
2. Optimize slow loaders
3. Add loading states for slow operations

## Best Practices

1. **Use environment-based config** - Different settings for dev/prod
2. **Keep defaults when possible** - Only override what you need
3. **Test configuration changes** - Verify routes and builds work
4. **Document custom config** - Note any non-standard settings
5. **Use TypeScript** - Get type safety for configuration options

## Next Steps

- Learn about [File-based Routing](file-based-routing.md) for route structure
- Explore [Advanced Patterns](advanced-patterns.md) for complex setups
- Check [Troubleshooting](troubleshooting.md) for configuration issues
