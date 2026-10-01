# Static Files

Use the `staticFiles()` plugin to serve static assets like images, CSS, JavaScript, and other files with caching and compression support.

## Basic setup

```ts
import { application, http, staticFiles } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(staticFiles());
```

## Configuration

### Plugin options

```ts
staticFiles({
  publicFolder: 'public',   // Folder containing static files (default: 'public')
  prefix: '/',              // URL prefix for static files (optional)
  cacheMaxAge: 86400,       // Cache duration in seconds (default: 86400 / 1 day)
  compress: 1024,           // Min file size in bytes to gzip (default: 1024)
})
```

### URL prefix

Serve static files under a specific path:

```ts
staticFiles({
  prefix: '/assets',
  publicFolder: 'public',
})
```

Files in `public/` will be available at `/assets/`:

- `public/logo.png` → `/assets/logo.png`
- `public/css/style.css` → `/assets/css/style.css`

## Folder structure

```text
my-app/
  public/
    favicon.ico
    robots.txt
    images/
      logo.png
      hero.jpg
    css/
      style.css
    js/
      app.js
  src/
    main.ts
```

## Caching

### Cache-Control and ETag headers

Static files are served with both `Cache-Control` and `ETag` headers:

```
Cache-Control: public, max-age=86400
ETag: "a1b2c3d4e5f60718"
```

The `cacheMaxAge` option controls the `max-age` value in seconds:

```ts
staticFiles({
  cacheMaxAge: 604800, // 1 week
})
```

### Conditional requests (304 Not Modified)

The plugin handles conditional requests automatically. When a browser has a cached copy of a file, it sends the stored ETag back in an `If-None-Match` header. If the ETag matches, the server responds with `304 Not Modified` and no body, saving bandwidth.

### Cache strategies

For development:

```ts
staticFiles({
  cacheMaxAge: 0, // No caching
})
```

For production with versioned assets:

```ts
staticFiles({
  cacheMaxAge: 31536000, // 1 year
})
```

For frequently updated files:

```ts
staticFiles({
  cacheMaxAge: 3600, // 1 hour
})
```

## Compression

Files larger than the `compress` threshold are gzipped at build time and served with the `Content-Encoding: gzip` header:

```ts
staticFiles({
  compress: 1024, // Gzip files > 1KB (default)
})
```

Set to `0` to disable compression:

```ts
staticFiles({
  compress: 0,
})
```

## Content types

The plugin automatically detects content types based on file extensions:

- **.html** - `text/html`
- **.css** - `text/css`
- **.js** - `application/javascript`
- **.json** - `application/json`
- **.png** - `image/png`
- **.jpg**, **.jpeg** - `image/jpeg`
- **.gif** - `image/gif`
- **.svg** - `image/svg+xml`
- **.webp** - `image/webp`
- **.woff** - `font/woff`
- **.woff2** - `font/woff2`
- **.pdf** - `application/pdf`

## Multiple static folders

Serve files from multiple locations:

```ts
import { application, http, staticFiles } from '@putnami/application';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(staticFiles({ publicFolder: 'public' }))
    .use(staticFiles({ publicFolder: 'uploads', prefix: '/uploads' }));
```

## Build-time assets

### Configuring assets in package.json

```json
{
  "putnami": {
    "application": {
      "assets": [
        {
          "from": "/apps/my-app/doc",
          "to": "public/docs"
        },
        {
          "from": "/LICENSE.md",
          "to": "public/LICENSE.md"
        }
      ]
    }
  }
}
```

### Generated assets

Assets generated during build (like compiled CSS or bundled JS) are typically placed in `.gen/public/`:

```text
.gen/
  public/
    css/
      main.css
    js/
      bundle.js
```

## React integration

When using `@putnami/web`, static files work alongside SSR:

```ts
import { application, http, staticFiles } from '@putnami/application';
import { react } from '@putnami/web';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(react())
    .use(staticFiles({ publicFolder: 'public' }));
```

### Referencing static files in React

```tsx
export default function Page() {
  return (
    <div>
      <img src="/images/logo.png" alt="Logo" />
      <link rel="stylesheet" href="/css/style.css" />
    </div>
  );
}
```

### Public folder in React config

```ts
react({
  publicFolder: 'public',  // Static files folder
})
```

## Security considerations

### Prevent directory traversal

The staticFiles plugin automatically prevents directory traversal attacks. Requests like `/../../etc/passwd` are blocked.

### Hide sensitive files

Don't place sensitive files in the public folder:

```text
public/           # Served to users
  images/
  css/
private/          # Not served
  secrets.json
  .env
```

### Exclude files

Use `.gitignore` patterns in your public folder if needed:

```text
public/
  .gitignore      # Contains patterns to ignore
  images/
```

## Performance tips

### Use CDN for production

In production, consider serving static files from a CDN:

```ts
const isProduction = process.env.NODE_ENV === 'production';

// In React components
const assetPrefix = isProduction ? 'https://cdn.example.com' : '';

<img src={`${assetPrefix}/images/logo.png`} />
```

### Compress files

Enable build-time compression for large assets:

```ts
staticFiles({
  compress: 1024, // Gzip files > 1KB
})
```

### Image optimization

Optimize images before deployment:

```bash
# Using imagemagick
mogrify -strip -quality 85 public/images/*.jpg

# Using optipng
optipng -o7 public/images/*.png
```

## Common patterns

### Favicon

```text
public/
  favicon.ico
  apple-touch-icon.png
  favicon-32x32.png
  favicon-16x16.png
```

```tsx
// In layout.tsx
<head>
  <link rel="icon" href="/favicon.ico" />
  <link rel="apple-touch-icon" href="/apple-touch-icon.png" />
</head>
```

### Robots.txt

```text
# public/robots.txt
User-agent: *
Allow: /
Sitemap: https://example.com/sitemap.xml
```

### Manifest for PWA

```json
// public/manifest.json
{
  "name": "My App",
  "short_name": "App",
  "start_url": "/",
  "display": "standalone",
  "icons": [
    {
      "src": "/icons/icon-192.png",
      "sizes": "192x192",
      "type": "image/png"
    }
  ]
}
```

## Related guides

- [Web](/docs/frameworks/typescript/web)
- [Configuration](/docs/frameworks/typescript/configuration)
