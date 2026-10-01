# @putnami/storage

Object storage: declared buckets with per-bucket constraints, signed URLs for
browser-direct transfer, and backends that answer one shared contract.

```ts
import { application } from '@putnami/application';
import { Bucket, storage, storagePlugin } from '@putnami/storage';

export const AvatarsBucket = Bucket('avatars', {
  maxFileSize: '5mb',
  allowedMimeTypes: ['image/png', 'image/jpeg'],
  public: false,
});

export const app = () => application().use(storagePlugin());

const avatars = await storage(AvatarsBucket.bucketName);
await avatars.put('user-1.png', file);
const { url, expiresAt } = await avatars.signedDownloadUrl('user-1.png');
```

## Mental model

- A **bucket** is a declaration: name, access level, size limit, MIME allowlist,
  optional retention. Declaring it registers it and emits it to the project's
  infra requirements.
- A **client** is what application code uses. It resolves a backend for its
  bucket and enforces the bucket's constraints.
- A **backend** is the substitutable part: memory for tests, filesystem for local
  development, S3, or the remote storage service.

## One contract, four backends

Every backend answers the same questions the same way:

| Operation | Contract |
|-----------|----------|
| `get` of a missing object or bucket | returns `null` |
| `delete` of a missing key | no-op |
| `copy` from a missing source | throws `StorageError` with code `NOT_FOUND` |
| `put` over an existing key | replaces it |
| zero-byte object | round-trips with `size === 0` |
| `list` → `lastModified` | a `Date`, on every backend |

That table is not documentation of intent — it is the shared
`runStorageContract` suite, and it runs against the memory backend, the file
backend, and the remote backend over the HTTP shape the remote backend
documents.

The S3 backend does **not** run the shared suite yet. It has its own unit tests
plus a live integration suite gated behind `PUTNAMI_S3_TEST_ENDPOINT`, which
covers a subset of the table above by hand. Wiring it in needs the shared suite
to stop assuming a fixed bucket name and a destructive reset, so the gap is
recorded rather than papered over.

## Keys and signed URLs

Keys are validated before any I/O, on every key-taking method: empty keys, null
bytes, absolute paths, and any `..` segment are rejected, and each key segment is
percent-encoded when it becomes a URL. A signed URL is bound to its method,
bucket, and key, defaults to a 15-minute expiry, and is capped at 7 days — an
upload token does not authorize a download, and a forged or expired token is
refused.

## Backends are interfaces, not SDKs

No cloud SDK is a dependency. The S3 backend is built on the runtime's own S3
client; the remote backend is plain `fetch` with a bearer token, and the service
mints signed URLs server-side.

## Maintained contract

This package is `stable` in the workspace
[support catalog](../../../putnami.support.json). It owns one public promise:

- **[Object storage](specs/object-storage.json)**, with the
  [one-bucket-contract ADR](doc/adr/0001-one-bucket-contract-across-backends.md).
  Code written against a bucket behaves the same on memory, filesystem, S3, and
  the remote service; declared types are the runtime types on every backend; and
  a user-supplied key cannot reach an object the bucket does not own.

Before v1.0.0, follow the workspace
[compatibility policy](../../../RELEASE.md): a breaking change is allowed in a
minor `0.x` release when the migration is documented. Do not infer strict
compatibility between every `0.x` minor.

## Documentation

- [Getting started](./doc/01-getting-started.md)
- [Buckets](./doc/02-buckets.md)
- [Signed URLs](./doc/03-signed-urls.md)
- [Backends](./doc/04-backends.md)
- [Native S3 backend](./doc/05-s3-backend.md)

## See also

- [`@putnami/application`](../application/README.md) — the application the plugin composes into

## License

[FSL-1.1-MIT](../../../LICENSE.md)
