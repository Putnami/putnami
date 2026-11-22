# Static Files

Serve static files with caching, compression, and automatic MIME type detection.

## Overview

The static files plugin automatically serves files from a directory with built-in caching, compression, and MIME type detection.

## Basic Setup

```typescript
import { application, http, staticFiles } from '@putnami/application';

const app = application()
  .use(http({ port: 3000 }))
  .use(staticFiles())
  .run(async () => {
    console.log('Static files served from /public');
  });

await app.start();
```

## Directory Structure

Place static files in the `public` folder:

```
public/
├── index.html
├── favicon.ico
├── styles.css
├── script.js
└── images/
    └── logo.png
```

Files are served at their relative paths:
- `public/index.html` → `/index.html`
- `public/styles.css` → `/styles.css`
- `public/images/logo.png` → `/images/logo.png`

## Where a static folder lives

The build stages a static folder into `.gen/public/` and caches the result. The
cache key of that step reads the `public/` folder of the workload and of every
project it depends on, and no other folder a scan could name. So a folder the
plugin scans lies under one of those `public/` folders: the workload's own, or a
library's, whose root holds a `putnami.json`. A change to any file there
rebuilds every workload that stages it.

A scan root anywhere else, such as a library's `fonts/` folder or a workload's
`assets/`, fails the build:

```
StaticPlugin: the scan root '../libs/fonts/fonts' lies outside every project's public/ folder, the only folders the build-generate cache key reads, so a change to its files would not rebuild the workload. Move it under '../libs/fonts/public'.
```

Move the folder under the `public/` the message names. A scan root that does
not exist stages nothing and fails nothing.

During `putnami build`, these routes are also included in
`.gen/schema/http-routes.json`. Root files are emitted as exact matches;
top-level asset directories and explicit non-root prefixes are emitted as
bounded prefixes such as `/images/`. The plugin never turns a root public
directory into a `/*` route.

## Configuration

### Basic Configuration

```typescript
import { staticFiles } from '@putnami/application';

const app = application()
  .use(http())
  .use(staticFiles({
    prefix: '/assets',
    publicFolder: 'public',
    cacheMaxAge: 604800,
    compress: 1024,
  }));
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

#### `publicFolder`

**Type:** `string`
**Default:** `'public'`

Folder the plugin scans when no `scanPath` is given, relative to the project,
and the name of the staged folder under `.gen/` and in the packaged workload.
A scanned folder must lie under a project's `public/` (see
[where a static folder lives](#where-a-static-folder-lives)), so keep the default
for a scanned folder.

```typescript
staticFiles({
  publicFolder: 'public', // Scan public/ and stage it into .gen/public/
})
```

#### `scanPath`

**Type:** `string`
**Default:** the project's `publicFolder`

Folder, or single file, the plugin stages at build time: absolute, or relative
to the project. It must lie under the `public/` folder of the workload or of a
workspace project (see [where a static folder lives](#where-a-static-folder-lives)).

```typescript
staticFiles({
  scanPath: 'public/site', // Serve only public/site/
})
```

#### `cacheMaxAge`

**Type:** `number`
**Default:** `86400` (1 day)

Cache duration in seconds. Controls the `Cache-Control: public, max-age=<value>` header.

```typescript
staticFiles({
  cacheMaxAge: 604800, // 1 week
})
```

#### `compress`

**Type:** `number`
**Default:** `1024`

Minimum file size in bytes to compress. Files larger than this are gzipped at build time. Set to `0` to disable compression.

```typescript
staticFiles({
  compress: 2048, // Only compress files > 2KB
})
```

### YAML Configuration

```yaml
static:
  prefix: '/assets'
  publicFolder: 'public'
  cacheMaxAge: 604800
  compress: 1024
```

## Features

### Automatic Compression

Files larger than the `compress` threshold are automatically gzipped at build time:

```typescript
staticFiles({
  compress: 1024, // Files > 1KB are compressed
})
```

The plugin:
1. Compresses files larger than the threshold during generation
2. Serves compressed files with `Content-Encoding: gzip` header

### Cache Headers

Responses include `Cache-Control` and `ETag` headers:

```
Cache-Control: public, max-age=86400
ETag: "abc123def456"
```

When a client sends an `If-None-Match` header matching the current ETag, the server responds with `304 Not Modified` instead of the full file body. This avoids unnecessary data transfer for unchanged files.

### MIME Type Detection

Automatic MIME type detection based on file extensions:

```
.css  → text/css
.js   → application/javascript
.png  → image/png
.jpg  → image/jpeg
.html → text/html
```

### HTML Conventions

Special handling for HTML files:

- `index.html` → Served at `/` (root)
- `about.html` → Served at `/about`

```
public/
├── index.html    → /
├── about.html    → /about
└── contact.html  → /contact
```

## Caching

### Cache Headers

Files are served with appropriate cache headers:

```
Cache-Control: public, max-age=86400
ETag: "abc123def456"
```

### Conditional Requests

The plugin handles conditional requests automatically. When a browser has a cached copy, it sends the stored ETag in an `If-None-Match` header. If the ETag matches, the server returns `304 Not Modified` with no body, saving bandwidth.

### Disable Caching

```typescript
staticFiles({
  cacheMaxAge: 0, // No caching
})
```

### Development vs Production

```typescript
const isProduction = process.env.NODE_ENV === 'production';

staticFiles({
  cacheMaxAge: isProduction ? 604800 : 0, // Cache in production only
})
```

## File Serving Examples

### Serve Images

```
public/
└── images/
    ├── logo.png
    └── banner.jpg
```

Access at:
- `/images/logo.png`
- `/images/banner.jpg`

### Serve CSS and JavaScript

```
public/
├── styles/
│   └── main.css
└── scripts/
    └── app.js
```

Access at:
- `/styles/main.css`
- `/scripts/app.js`

### Serve HTML Pages

```
public/
├── index.html
├── about.html
└── contact.html
```

Access at:
- `/` (index.html)
- `/about` (about.html)
- `/contact` (contact.html)

## Custom Routes

You can also register static files manually:

```typescript
import { StaticPlugin } from '@putnami/application';

const staticPlugin = staticFiles();

staticPlugin.routeStatic('custom.html', 'path/to/file.html', {
  mime: 'text/html',
  gzip: false,
});

const app = application()
  .use(http())
  .use(staticPlugin);
```

## Best Practices

1. **Use appropriate cache durations** - Long for assets, short for HTML
2. **Enable compression** - Reduces bandwidth for large files
3. **Organize files logically** - Group by type (images, styles, scripts)
4. **Use CDN in production** - Serve static files from a CDN for better performance
5. **Optimize file sizes** - Minify CSS/JS, compress images

## Integration with Other Plugins

### With API Plugin

```typescript
const app = application()
  .use(http())
  .use(api())      // API routes
  .use(staticFiles()); // Static files
```

Routes are matched in order:
1. API routes (if matched)
2. Static files (if matched)
3. 404 (if no match)

### With React Plugin

```typescript
const app = application()
  .use(http())
  .use(react())    // React SSR
  .use(staticFiles()); // Static assets
```

## Next Steps

- Learn about [HTTP Server](http-server.md) for routing
- Explore [Configuration](configuration.md) for plugin settings
- Check [API Reference](api-reference.md) for complete static files API

## Several Static Folders

A workload can use several StaticPlugins, each serving its own folder. A library
can ship one: it keeps its files in its own `public/` and exports the plugin.

```typescript
// libs/docs/src/server.ts, in a library whose root holds putnami.json
import { resolve } from 'node:path';
import { staticFiles } from '@putnami/application';

export const docsFiles = () =>
  staticFiles({ scanPath: resolve(import.meta.dir, '..', 'public'), prefix: '/docs' });

// the workload
application()
  .use(staticFiles()) // serves public/
  .use(docsFiles()); // serves libs/docs/public/ under /docs
```

Each plugin gets its own generated route loader and its own staging folder, so the packaged
workload serves every folder, and two folders can each hold an `index.html`. The first plugin
stages into `.gen/public/`; the others stage into `.gen/public/.static-<n>/`, which ships in the
same public folder.
