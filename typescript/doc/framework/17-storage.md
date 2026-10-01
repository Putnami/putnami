# Storage

Use `@putnami/storage` for object storage with pluggable backends, bucket declarations, presigned URLs, and a local development server. Store and serve files like avatars, documents, and media with type-safe constraints and environment-adaptive configuration.

## Getting started

### Installation

```bash
bunx putnami deps add @putnami/storage
```

### Enable the storage plugin

```ts
import { application, http } from '@putnami/application';
import { storagePlugin, storageServer } from '@putnami/storage';

export const app = () =>
  application()
    .use(http({ port: 3000 }))
    .use(storagePlugin())
    .use(storageServer()); // local dev server for signed URLs
```

### Storage configuration

Configure the backend in your environment files:

```yaml
# .env.local.yaml — local development
storage:
  backend: file
  dataDir: .data/storage

# .env.production.yaml — production
storage:
  backend: remote
  endpoint: https://storage.putnami.cloud
  accessKey: ${STORAGE_ACCESS_KEY}
  secretKey: ${STORAGE_SECRET_KEY}

# .env.test.yaml — tests
storage:
  backend: memory
```

### Configuration options

```yaml
storage:
  backend: file                # Backend type: 'remote', 'file', or 'memory'
  endpoint: https://storage.putnami.cloud  # Remote backend URL
  accessKey: ''                # Access key for remote auth
  secretKey: ''                # Secret key for remote auth
  dataDir: .data/storage       # Data directory for file backend
  slowOperationThresholdMs: 0  # Warn on slow operations (ms, 0 = disabled)
```

## Declaring buckets

Each project can declare any number of buckets. Buckets register globally when their module is evaluated, like `Table()` in `@putnami/database`.

### Basic bucket

```ts
import { Bucket } from '@putnami/storage';

export const AvatarsBucket = Bucket('avatars', {
  maxFileSize: '10mb',
  allowedMimeTypes: ['image/png', 'image/jpeg', 'image/webp'],
  public: true,
});
```

### Private bucket

```ts
import { Bucket } from '@putnami/storage';

export const InvoicesBucket = Bucket('invoices', {
  maxFileSize: '50mb',
  allowedMimeTypes: ['application/pdf'],
  public: false,
});
```

### Bucket options

- **maxFileSize** — Maximum file size (e.g., `'10mb'`, `'1.5gb'`). Validated on upload.
- **allowedMimeTypes** — Allowed content types. Validated on upload.
- **public** — Whether objects are publicly readable (default: `false`).
- **storage** — Named storage backend for per-bucket config override.

### File size format

Supported units: `b`, `kb`, `mb`, `gb`, `tb`. Decimals supported (e.g., `'1.5gb'`).

## Using the client

Get a `StorageClient` for a bucket by name. The client wraps the active backend with validation and observability.

### Basic operations

```ts
import { storage } from '@putnami/storage';

const avatars = await storage('avatars');

// Upload
await avatars.put('user-123/photo.png', file, { contentType: 'image/png' });

// Download
const result = await avatars.get('user-123/photo.png');

// Check existence
const exists = await avatars.exists('user-123/photo.png');

// Copy
await avatars.copy('user-123/photo.png', 'user-123/photo-backup.png');

// List objects
const list = await avatars.list({ prefix: 'user-123/', delimiter: '/' });

// Delete
await avatars.delete('user-123/photo.png');
```

### Public URL

For public buckets, get a direct URL:

```ts
const url = avatars.url('user-123/photo.png');
// Production: https://storage.putnami.cloud/avatars/user-123/photo.png
// Local dev:  .data/storage/avatars/user-123/photo.png
```

### Upload metadata

Attach content type, cache headers, and custom metadata:

```ts
await avatars.put('user-123/photo.png', file, {
  contentType: 'image/png',
  cacheControl: 'public, max-age=31536000',
  custom: { uploadedBy: 'user-123', originalName: 'selfie.png' },
});
```

### Connection lifecycle

```ts
import { storage, closeStorage, closeAllStorage } from '@putnami/storage';

// Get or create client
const avatars = await storage('avatars');

// Close a specific client
await closeStorage('avatars');

// Close all clients (on shutdown — handled by the plugin)
await closeAllStorage();
```

## Signed URLs

Signed URLs let browsers upload to and download from storage directly, bypassing the application server.

### Upload flow

```
Browser  ─── 1. Request signed URL ──▸  App Server
         ◂── 2. Return signed URL ───
         ─── 3. PUT directly ────────▸  storage.putnami.cloud
```

```ts
import { storage } from '@putnami/storage';

// Server-side: generate signed upload URL
const avatars = await storage('avatars');
const { url, headers, expiresAt } = await avatars.signedUploadUrl('user-123/photo.png', {
  expiresIn: '15m',
  contentType: 'image/png',
});

// Return url + headers to the browser
```

```ts
// Client-side: upload directly
await fetch(url, { method: 'PUT', headers, body: file });
```

### Download flow

```ts
// Server-side: generate signed download URL
const invoices = await storage('invoices');
const { url } = await invoices.signedDownloadUrl('inv-2025-001.pdf', {
  expiresIn: '1h',
  contentDisposition: 'attachment; filename="invoice.pdf"',
});

// Redirect or return the URL to the browser
```

### Sign options

- **expiresIn** — URL validity duration (e.g., `'15m'`, `'1h'`, `'7d'`)
- **maxFileSize** — Max upload size
- **allowedMimeTypes** — Restrict upload content types
- **contentType** — Expected content type for upload; when the bucket declares `allowedMimeTypes`, it must be one of them
- **contentDisposition** — Disposition header for download
- **metadata** — Custom metadata to attach to uploaded object

### Security

Signed URLs for the file backend are HMAC-SHA256 signed with a per-process random secret. Tokens cannot be forged, reused across processes, or applied to a different bucket/key. Object keys are validated against path traversal attacks (`../` sequences, absolute paths, null bytes). Upload size is validated against the actual request body.

## Backends

### Remote backend

HTTP client for `storage.putnami.cloud`. Used in production.

```yaml
storage:
  backend: remote
  endpoint: https://storage.putnami.cloud
  accessKey: ${STORAGE_ACCESS_KEY}
  secretKey: ${STORAGE_SECRET_KEY}
```

### File backend

Stores objects on the local filesystem. Used in local development.

```yaml
storage:
  backend: file
  dataDir: .data/storage
```

Directory layout:

```text
.data/storage/
  avatars/
    user-123/
      photo.png           # Object data
      photo.png.meta.json # Metadata sidecar
  invoices/
    inv-2025-001.pdf
    inv-2025-001.pdf.meta.json
```

### Memory backend

In-memory storage with zero I/O. Used in tests.

```yaml
storage:
  backend: memory
```

```ts
import { MemoryBackend, StorageClient, Bucket } from '@putnami/storage';

const TestBucket = Bucket('test-uploads', {
  maxFileSize: '5mb',
  allowedMimeTypes: ['image/png'],
});

const backend = new MemoryBackend();
const client = new StorageClient(TestBucket, backend);

// Use in tests
await client.put('test.png', new Blob(['data'], { type: 'image/png' }), {
  contentType: 'image/png',
});

// Reset between tests
backend.clear();
```

## Local development server

The `storageServer()` plugin mounts `/_storage/*` routes on the application's HTTP server. This handles signed URL uploads and downloads when using the file backend locally.

```ts
import { storageServer } from '@putnami/storage';

app.use(storageServer({ dataDir: '.data/storage' }));
```

In production, signed URLs point to `storage.putnami.cloud`. In local dev, they point to `/_storage/*` on localhost, where `storageServer()` handles them.

| Environment | Signed URL target |
|-------------|-------------------|
| Production  | `https://storage.putnami.cloud/avatars/user-123/photo.png?signature=...` |
| Local dev   | `http://localhost:3000/_storage/avatars/user-123/photo.png?token=...` |
| Tests       | `memory://avatars/user-123/photo.png` (not HTTP, direct client use) |

## Named storage backends

Like `database.analytics` in `@putnami/database`, buckets can use named backends for per-bucket configuration:

```ts
const ArchiveBucket = Bucket('archive', {
  storage: 'cold', // Uses 'storage.cold' config path
});
```

```yaml
storage:
  backend: remote
  endpoint: https://storage.putnami.cloud

storage.cold:
  backend: remote
  endpoint: https://archive.putnami.cloud
```

## Bucket validation

The `StorageClient` validates constraints before uploading:

```ts
import { bucketHelper } from '@putnami/storage';

const helper = bucketHelper(AvatarsBucket);

// Programmatic validation
const errors = helper.validate({ size: 20_000_000, mimeType: 'image/gif' });
// ['File size 20000000 bytes exceeds maximum 10mb',
//  'MIME type "image/gif" is not allowed. Allowed: image/png, image/jpeg, image/webp']
```

Validation runs automatically on `client.put()` — invalid uploads throw `StorageValidationError`.

## Observability

All storage operations emit structured logs and telemetry metrics. When the `telemetry()` plugin is active, storage metrics are collected alongside HTTP and SQL metrics.

### Metrics emitted

| Metric | Type | Description |
|---|---|---|
| `storage.{operation}.{bucket}` | Counter | Operation count per bucket |
| `storage.{operation}.{bucket}.duration` | Histogram | Operation duration (ms) |
| `storage.operation.duration` | Histogram | Aggregate across all buckets |
| `storage.{operation}.bytes` | Histogram | Bytes transferred |
| `storage.operation.error` | Counter | Aggregate error count |
| `storage.operation.slow` | Counter | Operations exceeding slow threshold |
| `storage.client.created` | Counter | Client creation events |
| `storage.client.closed` | Counter | Client close events |
| `storage.client.count` | Gauge | Current active client count |

### Structured logging

```text
[storage] put avatars { operation: "put", bucket: "avatars", duration: 45, bytes: 102400, key: "user-123/photo.png" }
[storage] get invoices { operation: "get", bucket: "invoices", duration: 12, key: "inv-2025-001.pdf" }
```

### Slow operation detection

```yaml
storage:
  slowOperationThresholdMs: 500
```

```text
[storage] Slow storage operation: put avatars (820ms >= 500ms)
```

## Related guides

- [Persistence](/docs/frameworks/typescript/persistence)
- [Plugins & lifecycle](/docs/frameworks/typescript/plugins-and-lifecycle)
- [Configuration](/docs/frameworks/typescript/configuration)
- [Telemetry](/docs/frameworks/typescript/telemetry)
- [Testing](/docs/frameworks/typescript/testing)

## Support and compatibility

`@putnami/storage` is a public, documented, maintained package classified
`stable`. Its object-storage specification and its accepted one-bucket-contract
decision record live next to the package source.

The promise is that a bucket behaves the same on every backend. `get` of a
missing object or bucket returns `null`, `delete` of a missing key is a no-op,
`copy` from a missing source throws `StorageError` with code `NOT_FOUND`, and a
listed object's `lastModified` is a `Date` — including on the remote backend,
where it arrives as JSON. That table is a shared contract suite run against the
memory, file, and remote backends, not a statement of intent. Keys are validated
before any I/O on every key-taking method, and signed URLs are bound to their
method, bucket, and key with a bounded expiry.

Before v1.0.0, minor `0.x` releases may still contain documented breaking
changes; strict compatibility across every pre-1.0 minor is not promised.
