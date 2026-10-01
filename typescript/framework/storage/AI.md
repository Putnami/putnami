# @putnami/storage

Object storage for Putnami with bucket declarations, per-bucket validation, signed URLs, and pluggable backends.

Use this package when you need:

- file and blob storage
- bucket-level constraints
- browser-direct upload/download
- public URLs or signed URLs
- environment-adaptive backends

Do not use this package for:

- relational data: use `@putnami/database`
- document-shaped records: use `@putnami/document`
- large structured metadata queries: store that metadata elsewhere and keep storage for objects

## Setup

```ts
import { application } from '@putnami/application';
import { storagePlugin, storageServer } from '@putnami/storage';

export const app = () =>
  application()
    .use(storagePlugin())
    .use(storageServer());
```

Use `storageServer()` in local development when you want signed URLs against the file backend.

Configuration in `conf/.env.local.yaml`:

```yaml
storage:
  backend: file
  dataDir: .data/storage
```

Production usually uses the remote backend:

```yaml
storage:
  backend: remote
  endpoint: https://storage.putnami.cloud
  accessKey: ${STORAGE_ACCESS_KEY} # sent as `Authorization: Bearer <accessKey>`
```

The remote backend authenticates with a single bearer token (`accessKey`); the
storage service signs URLs server-side. There is no client-side secret-key
request signing, so no `secretKey` is used.

## Bucket Definitions

Buckets are declarative and register globally when the module is evaluated:

```ts
import { Bucket } from '@putnami/storage';

export const AvatarsBucket = Bucket('avatars', {
  maxFileSize: '10mb',
  allowedMimeTypes: ['image/png', 'image/jpeg', 'image/webp'],
  public: true,
});

export const InvoicesBucket = Bucket('invoices', {
  maxFileSize: '50mb',
  allowedMimeTypes: ['application/pdf'],
  public: false,
  storage: 'archive',
});
```

### Bucket Options

| Option | Purpose |
|---------|---------|
| `maxFileSize` | Maximum upload size, e.g. `'10mb'` |
| `allowedMimeTypes` | Allowed content types |
| `public` | Bucket-wide unauthenticated read. `true` serves **every** object in the bucket over `GET /_storage/{bucket}/{key}` with no token and no per-object ACL. Scope public assets into their own bucket; never enable it on a bucket that also holds private objects |
| `storage` | Named backend config path under `storage.<name>` |
| `retention` | Free-form retention hint, e.g. `'30d'`. Surfaced to deployers via the generated infra requirements manifest; not enforced at runtime |

## Using Storage

Get a `StorageClient` by bucket name:

```ts
import { storage } from '@putnami/storage';

const avatars = await storage('avatars');
```

### Built-in Methods

| Method | Returns | Notes |
|--------|---------|-------|
| `put(key, data, metadata?)` | `PutResult` | Validates bucket constraints first |
| `get(key)` | `GetResult \| null` | Returns `null` when missing |
| `delete(key)` | `void` | Removes object |
| `list(options?)` | `ListResult` | Prefix and delimiter aware |
| `exists(key)` | `boolean` | Existence check |
| `copy(source, destination)` | `void` | In-bucket copy |
| `url(key)` | `string` | Best for public buckets |
| `signedUploadUrl(key, options?)` | `SignedUrl` | Browser-direct uploads; `options.contentType` must be in the bucket's MIME allowlist when it has one |
| `signedDownloadUrl(key, options?)` | `SignedUrl` | Browser-direct downloads |

Example:

```ts
const avatars = await storage('avatars');

await avatars.put('user-123/photo.png', file, { contentType: 'image/png' });
const object = await avatars.get('user-123/photo.png');
const exists = await avatars.exists('user-123/photo.png');
const url = avatars.url('user-123/photo.png');
```

## Signed URLs

Signed URLs let browsers upload and download directly:

```ts
const avatars = await storage('avatars');

const upload = await avatars.signedUploadUrl('user-123/photo.png', {
  expiresIn: '15m',
  contentType: 'image/png',
});

const download = await avatars.signedDownloadUrl('user-123/photo.png', {
  expiresIn: '1h',
  contentDisposition: 'attachment; filename=\"photo.png\"',
});
```

Local behavior depends on backend:

- `file` → signed `/_storage/...` URLs served by `storageServer()`
- `remote` → signed remote endpoint URLs

Constraints (file backend):

- `expiresIn` is capped at `7d`. A longer value is rejected at signing time —
  long-lived signed URLs are a credential-leak amplifier. Default is `15m`.
- Tokens are HMAC-signed with `storage.tokenSecret`. **Set a stable, shared
  `tokenSecret`** in any horizontally-scaled or persistent deployment: when it
  is omitted the backend falls back to a per-process random secret (and warns),
  so URLs minted by one replica fail validation on another and after a restart.
- `memory` → `memory://` URLs for tests, not real browser downloads

## Named Backends

Buckets can opt into a named backend config:

```yaml
storage:
  backend: file
  dataDir: .data/storage

  archive:
    backend: remote
    endpoint: https://storage.putnami.cloud
    accessKey: ${ARCHIVE_ACCESS_KEY}
```

```ts
const ArchiveBucket = Bucket('archive', {
  maxFileSize: '250mb',
  allowedMimeTypes: ['application/pdf'],
  public: false,
  storage: 'archive',
});
```

## Backends

### File

- local development default
- stores objects on disk under `dataDir`
- signed URLs require `storageServer()`

### Remote

- production backend
- talks to `storage.putnami.cloud` or another compatible endpoint
- signed URLs target the remote service

### S3 (native)

- S3-compatible backend on Bun's native `Bun.S3Client` (no AWS SDK)
- client-side SigV4 signing — presigned URLs are minted locally, no server round-trip
- works with AWS S3, Cloudflare R2, MinIO, GCS-S3 (set `s3Endpoint`)
- putnami bucket name maps 1:1 to the S3 bucket name
- cannot persist custom metadata or `Cache-Control` (Bun client limitation)

```yaml
storage:
  backend: s3
  region: us-east-1
  accessKeyId: ${S3_ACCESS_KEY_ID}
  secretAccessKey: ${S3_SECRET_ACCESS_KEY}
  s3Endpoint: http://localhost:9000 # omit for AWS
```

### Memory

- best for tests
- no I/O
- signed URLs are not usable over HTTP

## Lifecycle

The package caches `StorageClient` and backend instances.

```ts
import { closeAllStorage, closeStorage, storage } from '@putnami/storage';

const avatars = await storage('avatars');

await closeStorage('avatars');
await closeAllStorage();
```

`storagePlugin()` calls `closeAllStorage()` on shutdown.

## Common Pitfalls

- Do not set `public: true` on a bucket that holds anything private — it serves the **entire** bucket unauthenticated. Put public assets in a dedicated public bucket
- Do not use `url()` for private buckets when you need temporary access; use `signedDownloadUrl()`
- Do not forget `storageServer()` in local dev if you expect file-backend signed URLs to work in a browser
- Do not assume bucket names create directories automatically in your source tree; they are runtime declarations
- Do not put large queryable metadata into storage object metadata and expect database-like querying
- Do not bypass bucket constraints by uploading through raw backends unless you explicitly want to skip validation

## Detailed Documentation

See `doc/`:

- `01-getting-started.md`
- `02-buckets.md`
- `03-signed-urls.md`
- `04-backends.md`
- `05-s3-backend.md`

## Maintained contract

This public package is `stable` in the workspace [support
catalog](../../../putnami.support.json). It owns the
[object-storage specification](specs/object-storage.json) with its
[one-bucket-contract ADR](doc/adr/0001-one-bucket-contract-across-backends.md).

Facts to rely on when generating code:

- Every backend answers the same contract: `get` of a missing object or bucket
  returns `null`, `delete` of a missing key is a no-op, `copy` from a missing
  source throws `StorageError` with code `NOT_FOUND`, and a listed object's
  `lastModified` is a `Date` on every backend including the remote one.
- Declare buckets with `Bucket(name, options)`; resolve a client with
  `await storage(bucketName)`. `public: true` makes every object in the bucket
  world-readable and logs a startup warning.
- Keys are validated before any I/O on every key-taking method. Empty keys, null
  bytes, absolute paths, and `..` segments are rejected.
- Signed URLs are bound to method, bucket, and key, default to 15 minutes, and
  are capped at 7 days.
- No cloud SDK is a dependency; the S3 backend uses the runtime's own client and
  the remote backend uses `fetch`.

Before v1.0, follow the workspace
[migration-based compatibility policy](../../../RELEASE.md); do not infer strict
compatibility between every `0.x` minor.
