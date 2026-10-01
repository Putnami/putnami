# Buckets

Buckets are the top-level organizational unit in `@putnami/storage`. Each bucket is an independent namespace for objects with its own constraints and access settings.

## Declaring a Bucket

Use the `Bucket()` builder to declare a bucket. Buckets register themselves globally at definition time (like `Table()` in `@putnami/database`):

```typescript
import { Bucket } from '@putnami/storage';

export const DocumentsBucket = Bucket('documents', {
  maxFileSize: '100mb',
  allowedMimeTypes: ['application/pdf', 'application/msword'],
  public: false,
  storage: 'primary',
});
```

## Bucket Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `maxFileSize` | `string` | — | Maximum file size (e.g., `'10mb'`, `'1gb'`). Validated on upload. |
| `allowedMimeTypes` | `string[]` | — | Allowed MIME types. Validated on upload. |
| `public` | `boolean` | `false` | **Bucket-wide unauthenticated read.** When `true`, *every* object in the bucket is served over `GET /_storage/{bucket}/{key}` with no token. See the warning below. |
| `storage` | `string` | — | Named storage backend for config override (e.g., `'primary'`, `'archive'`). |
| `retention` | `string` | — | Free-form retention hint (e.g., `'30d'`). Emitted to the per-project infra requirements manifest for deployers; not enforced at runtime. |

> **`public: true` is a bucket-wide switch, not a per-object one.** The
> `storageServer()` plugin serves *every* object in a public bucket over
> `GET /_storage/{bucket}/{key}` with no token and no per-object ACL — enabling
> it for a single asset type exposes the whole bucket. The default (`false`,
> private) is the safe choice. When you need public reads, put those assets in a
> dedicated public bucket and keep private objects in a separate private bucket;
> use `signedDownloadUrl()` for temporary access to private objects.

## Infra Requirements

During the build's `generate()` phase the storage plugin discovers declared buckets — both in dependency packages and local `src/**` route or library files — and writes a per-project infra scratch fragment to `.gen/infra/storage.json`. The TypeScript generator syncs that fragment into committed `infra/requirements.json`; `putnami build` merges committed requirements into the workload's ephemeral `.gen/requirements.json`. Each bucket contributes a `{ name, retention? }` entry. Buckets without a `retention` omit the field.

## File Size Format

Supported units: `b`, `kb`, `mb`, `gb`, `tb`. Decimal values are supported (e.g., `'1.5gb'`).

## Bucket Registry

All declared buckets are available through the `bucketRegistry`:

```typescript
import { bucketRegistry } from '@putnami/storage';

// List all registered bucket names
const names = bucketRegistry.getNames();

// Look up a specific bucket
const bucket = bucketRegistry.getByName('avatars');
```

## Bucket Helper

The `BucketHelper` provides convenience methods for reading bucket metadata and validating files:

```typescript
import { bucketHelper } from '@putnami/storage';

const helper = bucketHelper(AvatarsBucket);

helper.bucketName;        // 'avatars'
helper.isPublic;          // true
helper.maxFileSize;       // '10mb'
helper.maxFileSizeBytes;  // 10485760
helper.allowedMimeTypes;  // ['image/png', 'image/jpeg', 'image/webp']

// Validate a file against bucket constraints
const errors = helper.validate({ size: 20_000_000, mimeType: 'image/gif' });
// ['File size 20000000 bytes exceeds maximum 10mb', 'MIME type "image/gif" is not allowed...']
```
