# go.putnami.dev/storage

Object storage with pluggable backends (memory, filesystem, S3, GCS), bucket
definitions, and constraint enforcement.

## Quick Start

Application code programs against the `Backend` interface; pick a backend at
composition time.

```go
import (
    "context"
    "strings"

    "go.putnami.dev/storage"
)

backend := storage.NewMemoryBackend()
defer backend.Close()

ctx := context.Background()
_, err := backend.Put(ctx, "avatars", "user-123/avatar.png",
    strings.NewReader("...bytes..."),
    &storage.ObjectMetadata{ContentType: "image/png"},
)
```

## Operations

```go
// Put — returns *PutResult{Key, Size, ETag}.
res, err := backend.Put(ctx, "avatars", "user-123/avatar.png", reader,
    &storage.ObjectMetadata{ContentType: "image/png"})

// Get — returns (nil, nil) when the object does not exist.
obj, err := backend.Get(ctx, "avatars", "user-123/avatar.png")
if err != nil {
    return err
}
if obj == nil {
    // not found
    return nil
}
defer obj.Body.Close()
data, err := io.ReadAll(obj.Body)

// Exists — cheap presence check (HEAD on S3).
ok, err := backend.Exists(ctx, "avatars", "user-123/avatar.png")

// Stat — one object's metadata without its bytes or a list. Fails with
// storage.not_found when missing, storage.unsupported on a backend that does
// not implement storage.Stater.
info, err := storage.Stat(ctx, backend, "avatars", "user-123/avatar.png")
// info.Size, info.LastModified, info.ETag, info.ContentType

// List — filter + paginate via ListOptions.
page, err := backend.List(ctx, "avatars", &storage.ListOptions{
    Prefix:    "user-123/",
    Delimiter: "/",
    MaxKeys:   100,
})
// page.Objects, page.Prefixes, page.IsTruncated, page.ContinuationToken

// Copy — duplicates within the same bucket.
err = backend.Copy(ctx, "avatars", "user-123/avatar.png", "user-123/avatar-backup.png")

// Delete
err = backend.Delete(ctx, "avatars", "user-123/avatar.png")
```

Paginate a large bucket by feeding the returned token back in:

```go
opts := &storage.ListOptions{MaxKeys: 1000}
for {
    page, err := backend.List(ctx, "avatars", opts)
    if err != nil {
        return err
    }
    for _, o := range page.Objects {
        // ...
    }
    if !page.IsTruncated {
        break
    }
    opts.ContinuationToken = page.ContinuationToken
}
```

## Backends

```go
// Memory (testing)
backend := storage.NewMemoryBackend()

// Filesystem. Key segments ending in .meta.json or .putnami-tmp are reserved.
backend := storage.NewFileBackend("/data/storage")

// S3-compatible
backend := storage.NewS3Backend(storage.S3Config{
    Endpoint:  "https://s3.us-east-1.amazonaws.com",
    Region:    "us-east-1",
    AccessKey: "AKIA...",
    SecretKey: "secret",
    Bucket:    "my-bucket",
})

// Google Cloud Storage (native JSON API over net/http; no GCP SDK).
// Auth is Application Default Credentials by default — Workload Identity via
// the metadata server on Cloud Run / GKE, needing no static secret. Set
// CredentialsFile (or GOOGLE_APPLICATION_CREDENTIALS) to use a service-account
// key instead.
backend, err := storage.NewGCSBackend(ctx, storage.GCSConfig{})
```

> Prefer the dedicated GCS backend (keyless under Workload Identity). If you only
> have HMAC keys, `NewS3Backend` also works against GCS's S3-interoperable
> endpoint (`Endpoint: "https://storage.googleapis.com"`).

## Buckets and constraints

Declare buckets with functional options. `Bucket` registers the definition in the
global registry.

```go
var Avatars = storage.Bucket("avatars",
    storage.WithMaxFileSize(5<<20),                       // 5 MiB
    storage.WithAllowedMimeTypes("image/png", "image/jpeg"),
    storage.WithPublic(true),
)
```

The backend that `storage.NewPlugin()` provides enforces these constraints (see
below). A backend you construct yourself, including one returned by
`DiscoverBackend` or `BackendFromBindings`, stores raw bytes and enforces
nothing; wrap it to enforce `WithMaxFileSize` / `WithAllowedMimeTypes`:

```go
backend := storage.NewConstrainedBackend(storage.NewFileBackend("/data/storage"))
// Put now rejects oversized or disallowed-MIME objects for registered buckets.
```

A rejected upload leaves the existing object intact. A body whose length is
known up front and exceeds the limit is rejected before any backend I/O.

## Pre-signed URLs

Backends that implement `URLSigner` can issue direct-access URLs. The S3 and GCS
backends support it. S3 uses SigV4 (requires credentials); GCS uses GOOG4 V4
signing — locally with a service-account key, or keyless via the IAM SignBlob
API under Workload Identity:

```go
if signer, ok := backend.(storage.URLSigner); ok {
    url, err := signer.SignedGetURL(ctx, "avatars", "user-123/avatar.png",
        storage.SignedURLOptions{Expiry: 15 * time.Minute})
}
```

A `ConstrainedBackend` always implements `URLSigner`. It delegates to the
wrapped backend and returns `storage.unsupported` when that backend cannot sign.
Its `SignedPutURL` first rejects a content type that the bucket's MIME allowlist
does not list, including an empty one. It cannot enforce the size limit.

## Infrastructure requirements

```go
storage.Bucket("audit-logs", storage.WithAccess(storage.AccessWrite), storage.WithRetention("90d"))
storage.Bucket("uploads") // access defaults to readwrite
```

Add `storage.NewPlugin()` to an application so the build's describe phase
emits a per-project infra-requirements scratch fragment at
`.gen/infra/storage.json` listing one `infra.storage` entry per registered
bucket. Each entry carries `access` (the role the deploy target grants the
runtime identity — defaults to `readwrite`), `public`, and `retention`
(omitted when empty). The entries are projected from the canonical storage
`Manifest` through `infra.StoragesFromManifest`, the same bridge the database
plugin uses. The Go generator syncs that fragment into committed
`infra/requirements.json`, and `putnami build` merges committed requirements
into the workload's ephemeral `.gen/requirements.json` — so a deployer grants
bucket access from the declared requirements instead of by hand.

## Runtime bucket bindings

A workload registers buckets by **logical** name (`storage.Bucket("uploads")`)
and calls `backend.Put(ctx, "uploads", ...)`. At deploy time the control plane
provisions a real, globally-unique provider bucket per logical bucket and must
tell the workload which provider bucket backs each logical name. That mapping is
the **runtime binding contract** — the storage analogue of how config/secrets
resolve from an injected `CONFIG_SERVER_URL`.

### The contract: the managed bindings document

A deploy target injects one bindings document. It arrives over one of two
transports: merged into the `storage` section of the workload's resolved config
(operator keys in the section are preserved, managed keys win), or as the
legacy `STORAGE_BINDINGS` environment variable holding the document as JSON.
Config wins when both are present. Each entry is a
`go.putnami.dev/protocol/storage` **Binding**:

```json
{
  "$schema": "https://putnami.dev/schemas/putnami-storage-bindings.json",
  "protocolVersion": 1,
  "bindings": [
    { "name": "uploads", "backend": "gcs", "bucket": "pn-acme-prod-uploads-a1b2c3", "identity": "workload", "signedUrls": true },
    { "name": "exports", "backend": "gcs", "bucket": "acme-shared-prod", "prefix": "preview-42/exports/" }
  ],
  "providers": { "gcs": { "projectId": "acme-prod" } }
}
```

| Field | Meaning |
|-------|---------|
| `protocolVersion` | Envelope version. Must be `1`. |
| `bindings[].name` | The **logical** bucket the workload registered (`storage.Bucket(name)`). |
| `bindings[].backend` | Backend kind: `gcs`, `s3`, or `memory`. |
| `bindings[].bucket` | The **provider** bucket the logical name resolves to. |
| `bindings[].prefix` | Optional key prefix within `bucket` (shared-bucket isolation). |
| `bindings[].identity` | `workload` (ambient identity, no secret) or `static`. |
| `bindings[].signedUrls` | Whether signed-URL capability was granted. |
| `providers.gcs` | `{ projectId?, credentialsFile? }` — omit under Workload Identity. |
| `providers.s3` | `{ endpoint?, region?, accessKey?, secretKey? }`. |

`bindings` is the protocol Binding shape verbatim; `providers` carries the
per-backend construction params a Binding does not convey. The document is
strict-parsed — an unknown field, a duplicate logical name, an unsupported
backend kind, or a binding missing its bucket fails startup loudly.

### Resolving a bound backend

```go
// Discover from STORAGE_BINDINGS (nil, nil when unset — fall back to a manual backend).
backend, err := storage.DiscoverBackend(ctx)

// Or build from an already-parsed document.
doc, err := storage.ParseBindings(raw)
backend, err := storage.BackendFromBindings(ctx, doc)
```

Or just add `storage.NewPlugin()`: it provides a `storage.Backend` into the DI
container, resolved from the managed bindings document (the `storage` config
section first, `STORAGE_BINDINGS` as fallback), so an endpoint can inject it:

```go
endpoint().
  inject({ store: storage.Backend }). // pseudo: resolve storage.Backend from DI
  handle(...)
```

The resolved backend transparently remaps each logical bucket to its bound
provider bucket (and prepends the key prefix). **Fail-closed:** an operation on a
logical bucket with no injected binding returns `storage.unbound` rather than
silently targeting a literal-named bucket. Signed URLs are available only for
bindings whose `signedUrls` grant is true and whose backend implements
`URLSigner`. The backend that `storage.NewPlugin()` provides also enforces the
registered bucket constraints: `Put` rejects oversized or disallowed-MIME uploads
with `storage.write`, and `SignedPutURL` rejects a content type the bucket's MIME
allowlist does not list. A signed PUT URL cannot enforce the size limit.
`DiscoverBackend` and `BackendFromBindings` return an unconstrained backend.
Direct `NewGCSBackend` / `NewMemoryBackend` / `NewS3Backend` usage is unchanged —
the binding layer is additive. Wrap any of these backends with
`NewConstrainedBackend` to enforce constraints; the wrapper keeps their signed
URLs.

See `doc/getting-started.md` for the full reference.

## Contract invariants

- Logical bucket bindings fail closed when absent and never cross provider,
  bucket, or prefix boundaries.
- A `Put` whose body reader fails commits nothing and leaves any existing
  object intact.
- MIME constraints are checked before backend I/O. A body of known length over
  the limit is rejected before backend I/O; other bodies are bounded during
  streaming. A size rejection never deletes the previous object. Unavailable
  capabilities return typed errors instead of silently degrading.
- Operations carry caller cancellation and streaming reads keep bodies bounded
  by explicit ownership and close semantics.

Support is **stable** in `../../../putnami.support.json`; the normative feature
contract is `specs/object-storage.json`, with the decision in
`doc/adr/0001-backend-and-bucket-isolation.md` and
`doc/adr/0002-enforce-constraints-on-the-plugin-backend.md`.
