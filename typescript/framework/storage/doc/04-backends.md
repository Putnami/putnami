# Backends

`@putnami/storage` uses a pluggable backend architecture. All backends implement the `StorageBackend` interface, and the active backend is selected via configuration.

## StorageBackend Interface

```typescript
interface StorageBackend {
  put(bucket, key, data, metadata?): Promise<PutResult>;
  get(bucket, key): Promise<GetResult | null>;
  delete(bucket, key): Promise<void>;
  list(bucket, options?): Promise<ListResult>;
  exists(bucket, key): Promise<boolean>;
  copy(bucket, source, destination): Promise<void>;
  url(bucket, key): string;
  signedUploadUrl(bucket, key, options?): Promise<SignedUrl>;
  signedDownloadUrl(bucket, key, options?): Promise<SignedUrl>;
  close(): Promise<void>;
}
```

## Remote Backend

HTTP client for `storage.putnami.cloud` (or any compatible endpoint).

```yaml
storage:
  backend: remote
  endpoint: https://storage.putnami.cloud
  accessKey: ${STORAGE_ACCESS_KEY}
```

- Used in **production**
- All operations are HTTP requests to the configured endpoint
- Signed URLs point to the remote endpoint; the service mints them server-side
- Authentication is a single bearer token: `accessKey` is sent as
  `Authorization: Bearer <accessKey>`. There is no client-side secret-key
  request signing, so no `secretKey` is required or used.

## File Backend

Stores objects on the local filesystem with JSON metadata sidecars.

```yaml
storage:
  backend: file
  dataDir: .data/storage
```

- Used in **local development**
- Directory layout: `<dataDir>/<bucket>/<key>` + `<key>.meta.json`
- Signed URLs point to `/_storage/` routes (served by `storageServer()` plugin)
- Signed URLs are HMAC-signed with a per-process random secret
- Object keys are validated against path traversal attacks

## S3 Backend

Native S3-compatible backend built on Bun's `Bun.S3Client` — zero dependencies,
**client-side** SigV4 signing (no server-mediated `_sign/*` round-trip).

```yaml
storage:
  backend: s3
  region: us-east-1
  accessKeyId: ${S3_ACCESS_KEY_ID}
  secretAccessKey: ${S3_SECRET_ACCESS_KEY}
  s3Endpoint: http://localhost:9000 # omit for AWS; set for R2 / MinIO / GCS-S3
  virtualHostedStyle: false # path-style by default
```

- Works against **AWS S3** and any S3-compatible provider (Cloudflare R2, MinIO,
  GCS-S3) via `s3Endpoint`
- The putnami bucket name maps **1:1** to the S3 bucket name
- Presigned upload/download URLs are signed locally (no server round-trip)
- Credentials fall back to standard `S3_*`/`AWS_*` environment variables
- **Limitation:** Bun's native client cannot persist custom metadata
  (`x-amz-meta-*`) or `Cache-Control`; those fields are dropped on `put`

See [05-s3-backend.md](./05-s3-backend.md) for the full decision record,
config shape, and the live conformance suite.

## Memory Backend

Stores everything in memory. Zero I/O, zero setup.

```yaml
storage:
  backend: memory
```

- Used in **tests**
- Call `backend.clear()` between tests to reset state
- Provides test helpers: `objectCount(bucket)`, `bucketNames()`
- Signed URLs use `memory://` scheme (not usable over HTTP)

### Test Example

```typescript
import { describe, it, expect, beforeEach } from 'bun:test';
import { Bucket, MemoryBackend, StorageClient } from '@putnami/storage';

const TestBucket = Bucket('test-uploads', {
  maxFileSize: '5mb',
  allowedMimeTypes: ['image/png'],
  public: false,
});

const backend = new MemoryBackend();

beforeEach(() => {
  backend.clear();
});

it('should upload and retrieve an object', async () => {
  const client = new StorageClient(TestBucket, backend);
  const data = new Blob(['hello'], { type: 'image/png' });

  await client.put('test.png', data, { contentType: 'image/png' });
  const result = await client.get('test.png');

  expect(result).not.toBeNull();
  expect(result!.size).toBe(5);
});
```
