# Native S3 Backend (spike)

`S3Backend` is an S3-compatible `StorageBackend` built on Bun's native S3 client
(`Bun.S3Client`) — zero dependencies, no AWS SDK. Unlike the
[Remote backend](./04-backends.md#remote-backend), **signing happens
client-side**: presigned URLs are computed locally with AWS SigV4 and every
operation talks straight to the bucket, with no `_sign/*` server round-trip.

This document records the design of the spike and what it does not yet promise.

## Status

Prototype / spike. The implementation lives in
`src/backend/s3.backend.ts` and is wired into the storage factory as the
`s3` backend. Unit tests run in CI without network; a live conformance suite
runs against MinIO/R2/AWS when `PUTNAMI_S3_TEST_ENDPOINT` is set.

## Configuration

```yaml
storage:
  backend: s3
  region: us-east-1
  accessKeyId: ${S3_ACCESS_KEY_ID}
  secretAccessKey: ${S3_SECRET_ACCESS_KEY}
  # S3-compatible providers (Cloudflare R2, MinIO, GCS-S3): set the endpoint.
  s3Endpoint: http://localhost:9000
  # Path-style addressing is the default; flip on virtual-hosted style for AWS.
  virtualHostedStyle: false
  # Optional canonical ACL applied to writes.
  s3Acl: private
  # Optional CDN/website base for url() on public buckets.
  publicBaseUrl: https://cdn.example.com
```

Credentials are also read from the standard `S3_*`/`AWS_*` environment variables
when omitted, so role-based/instance credentials work without putting secrets in
config. `secretAccessKey` and `sessionToken` are marked `Sensitive` so they are
redacted from validation errors.

### Bucket mapping

The putnami bucket name maps **1:1** to the S3 bucket name. A `Bucket('avatars')`
declaration reads and writes the `avatars` S3 bucket. The S3 client carries
credentials once and receives the bucket per operation, so a single backend
instance serves every registered bucket.

### Endpoint / addressing

- **AWS S3** — omit `s3Endpoint`; the region-derived host is used.
- **Cloudflare R2 / MinIO / GCS-S3** — set `s3Endpoint` to the provider URL.
- `virtualHostedStyle: true` switches from `https://<host>/<bucket>/<key>` to
  `https://<bucket>.<host>/<key>`. Path-style (the default) is what MinIO and
  most self-hosted gateways expect.

## Contract coverage

| Operation | Notes |
|-----------|-------|
| `put` | `contentType` → `Content-Type`, `contentDisposition` honored. ETag recovered via a follow-up `stat`. |
| `get` | `stat` + `stream`; returns `null` on a 404 (`NoSuchKey`). |
| `delete` | Idempotent (S3 returns success for a missing key). |
| `list` | ListObjectsV2 — prefix, delimiter, `maxKeys`, and opaque `continuationToken` pagination. |
| `exists` | HEAD request. |
| `copy` | Streams the source object to the destination through Bun's S3 client; throws when the source is missing. |
| `url` | Unsigned object URL from `publicBaseUrl`, the endpoint, or the AWS region host. |
| `signedUploadUrl` / `signedDownloadUrl` | Local SigV4 presign. Expiry reuses the shared `resolveExpiry` (default `15m`, hard cap `7d`). |

## Known limitations

Bun's native S3 client (retested on Bun 1.4.0) does **not** expose:

- **Custom user metadata** (`x-amz-meta-*`) — `metadata.custom` is dropped on
  `put` and never surfaced by `get`.
- **`Cache-Control`** — `metadata.cacheControl` is dropped on `put`.

Buckets that depend on custom metadata or cache-control headers should stay on
the Remote backend until Bun adds these. Everything else in the `StorageBackend`
contract is covered.

## Relationship to the Remote backend

The two coexist:

- **`S3Backend`** — self-hosted / first-party S3-compatible buckets (AWS, R2,
  MinIO, GCS-S3) where the app holds credentials and signs client-side.
- **`RemoteBackend`** — `storage.putnami.cloud`, where signing is delegated to
  the service via a single bearer token.

`S3Backend` does not migrate any existing data paths; it is purely additive.

## Running the live conformance suite

```bash
docker run -p 9000:9000 -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server /data
# create the bucket once (mc / console), then:
PUTNAMI_S3_TEST_ENDPOINT=http://localhost:9000 \
PUTNAMI_S3_TEST_BUCKET=putnami-test \
S3_ACCESS_KEY_ID=minioadmin S3_SECRET_ACCESS_KEY=minioadmin \
bun test test/s3-backend.test.ts
```

Without `PUTNAMI_S3_TEST_ENDPOINT` the integration cases are skipped and only the
hermetic unit tests run.
