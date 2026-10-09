# Storage

`go.putnami.dev/storage` provides an object storage abstraction with pluggable backends. It ships four built-in backends -- memory, filesystem, S3-compatible, and Google Cloud Storage -- behind a single `Backend` interface so application code never couples to a specific provider. Every backend is implemented over the standard library with no third-party dependencies.

## Backend Interface

Every backend implements the `Backend` interface:

```go
type Backend interface {
    Put(ctx context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (*PutResult, error)
    Get(ctx context.Context, bucket, key string) (*GetResult, error)
    Delete(ctx context.Context, bucket, key string) error
    List(ctx context.Context, bucket string, opts *ListOptions) (*ListResult, error)
    Exists(ctx context.Context, bucket, key string) (bool, error)
    Copy(ctx context.Context, bucket, source, destination string) error
    Close() error
}
```

All operations are context-aware and return structured errors with codes defined in the `go.putnami.dev/errors` package (e.g. `storage.read`, `storage.write`, `storage.not_found`).

`Put` replaces any existing object at the key. A `Put` whose `data` reader fails commits nothing: it returns an error and leaves the existing object and its metadata intact.

### Reading one object's metadata

`storage.Stat` reads one object's size, write time, ETag and content type
without its bytes and without listing its prefix. A missing key fails with
`storage.not_found`:

```go
info, err := storage.Stat(ctx, backend, "avatars", "user-123/avatar.png")
if errors.Is(err, storage.CodeStorageNotFound) {
    // not found
}
// info.Size, info.LastModified, info.ETag, info.ContentType
```

It never lists: it reads the object's entry in memory, the object file and
its metadata file on the filesystem, sends one `HeadObject` on S3, and one
object metadata read on GCS, which bills as a Class B operation where a list
bills as Class A. `Stat` is the optional
`Stater` interface rather than a `Backend` method, so a backend you write
yourself keeps compiling. Every backend this package provides implements it,
and the constrained and binding backends forward it. On a backend without it,
`storage.Stat` fails with `storage.unsupported`.

### Object Metadata

Attach metadata when storing objects:

```go
meta := &storage.ObjectMetadata{
    ContentType:        "image/png",
    CacheControl:       "max-age=86400",
    ContentDisposition: "inline",
    Custom:             map[string]string{"author": "jane"},
}
```

### Result Types

`Put` returns a `PutResult` with the key, byte size, and ETag. `Get` returns a `GetResult` containing an `io.ReadCloser` body, size, content type, ETag, last-modified timestamp, and custom metadata. Always close the body after reading:

```go
result, err := backend.Get(ctx, "docs", "readme.txt")
if err != nil {
    return err
}
if result == nil {
    // Object does not exist.
    return nil
}
defer result.Body.Close()

data, err := io.ReadAll(result.Body)
```

When an object does not exist, `Get` returns `nil, nil` (no error).

### Listing Objects

Use `ListOptions` to filter and paginate:

```go
result, err := backend.List(ctx, "docs", &storage.ListOptions{
    Prefix:    "reports/2025/",
    Delimiter: "/",
    MaxKeys:   100,
})
```

| Field               | Purpose                                                     |
|---------------------|-------------------------------------------------------------|
| `Prefix`            | Return only keys starting with this string.                 |
| `Delimiter`         | Group keys by delimiter; common prefixes appear in `Prefixes`. |
| `MaxKeys`           | Maximum number of objects to return.                        |
| `ContinuationToken` | Resume a truncated listing from a previous `ListResult`.    |

The returned `ListResult` includes `Objects`, `Prefixes` (common prefixes when a delimiter is used), `IsTruncated`, and `ContinuationToken` for pagination.

## Backends

### Memory

An in-memory backend suitable for tests and short-lived processes. Data does not survive restarts.

```go
backend := storage.NewMemoryBackend()
defer backend.Close()

result, err := backend.Put(ctx, "uploads", "photo.jpg", reader, &storage.ObjectMetadata{
    ContentType: "image/jpeg",
})
```

The memory backend is goroutine-safe; all operations are guarded by a read-write mutex.

### Filesystem

Stores objects as files on disk. Metadata is persisted in sidecar `.meta.json` files next to each object. Nested key paths (e.g. `a/b/c/file.txt`) create the corresponding directory structure automatically.

`Put` writes the body to a temporary file in the object's directory and renames it over the object once the body is complete, then replaces the metadata file the same way. A `Put` without metadata removes the metadata file of the object it replaces. Object and metadata files get the mode `os.Create` gives: `0666` narrowed by the process umask.

A key segment ending in `.meta.json` or `.putnami-tmp`, in any letter case, is reserved for metadata and temporary files. `Put` rejects the key with `storage.write`.

```go
backend := storage.NewFileBackend("/var/data/storage")
defer backend.Close()

_, err := backend.Put(ctx, "attachments", "invoice.pdf", reader, &storage.ObjectMetadata{
    ContentType: "application/pdf",
    Custom:      map[string]string{"version": "2"},
})
```

The on-disk layout for a bucket named `attachments` with key `invoice.pdf` is:

```
/var/data/storage/
  attachments/
    invoice.pdf
    invoice.pdf.meta.json
```

### S3

Connects to any S3-compatible service (AWS S3, MinIO, Google Cloud Storage with S3 interop, etc.) using plain HTTP requests. Logical bucket names become key prefixes within a single S3 bucket.

```go
backend := storage.NewS3Backend(storage.S3Config{
    Endpoint:  "https://s3.us-east-1.amazonaws.com",
    Region:    "us-east-1",
    AccessKey: "AKIA...",
    SecretKey: "secret",
    Bucket:    "my-app-storage",
})
defer backend.Close()
```

| Config field | Purpose                                                     | Default      |
|-------------|-------------------------------------------------------------|--------------|
| `Endpoint`  | S3-compatible endpoint URL.                                 | (required)   |
| `Region`    | AWS region.                                                 | `us-east-1`  |
| `AccessKey` | Access key for authentication.                              | (required)   |
| `SecretKey` | Secret key for authentication.                              | (required)   |
| `Bucket`    | Physical S3 bucket name. Logical buckets map to key prefixes. | (required)   |

The S3 backend uses the ListObjectsV2 API for listing and HEAD requests for existence checks. `Close` drains idle HTTP connections.

### Google Cloud Storage

Connects to Google Cloud Storage via its native JSON API over `net/http`. Like the other backends it has no third-party dependencies: OAuth2 token acquisition and V4 URL signing are implemented directly against the wire format.

```go
// Application Default Credentials. On Cloud Run / GKE this resolves to Workload
// Identity via the metadata server, so no static secret is required.
backend, err := storage.NewGCSBackend(ctx, storage.GCSConfig{})
defer backend.Close()
```

| Config field      | Purpose                                                              | Default                          |
|-------------------|----------------------------------------------------------------------|----------------------------------|
| `ProjectID`       | GCP project ID. Optional; object operations do not require it.       | (none)                           |
| `CredentialsFile` | Path to a service-account JSON key file.                             | ADC (`GOOGLE_APPLICATION_CREDENTIALS`, else metadata server) |

Authentication resolves in this order: `CredentialsFile` if set, otherwise the path in `GOOGLE_APPLICATION_CREDENTIALS`, otherwise the GCP metadata server (Workload Identity). `Copy` uses the server-side rewrite API; uploads stream without buffering the whole object in memory.

Signed URLs use GOOG4 V4 signing — locally with a service-account key, or keyless via the IAM SignBlob API under Workload Identity. If you only have HMAC keys, the S3 backend also works against GCS's S3-interoperable endpoint (`https://storage.googleapis.com`).

## Buckets

Buckets define named storage containers with optional constraints. Register them at package initialization time using the `Bucket` helper:

```go
var AvatarsBucket = storage.Bucket("avatars",
    storage.WithMaxFileSize(5 * 1024 * 1024),          // 5 MB limit
    storage.WithAllowedMimeTypes("image/png", "image/jpeg"),
    storage.WithPublic(true),
)
```

### Bucket Options

| Option                | Purpose                                               |
|-----------------------|-------------------------------------------------------|
| `WithMaxFileSize(n)`  | Maximum file size in bytes. `0` means unlimited.      |
| `WithAllowedMimeTypes(...)` | Restrict uploads to specific MIME types. Empty means all types allowed. |
| `WithPublic(bool)`    | Mark objects as publicly accessible.                  |
| `WithAccess(access)`  | Access the deploy target grants the runtime identity: `AccessRead`, `AccessWrite`, or `AccessReadWrite`. Defaults to `AccessReadWrite`. Surfaced as the infra.storage requirement. |

### Enforcing Constraints

The backend that `storage.NewPlugin()` provides enforces the size and MIME
options above (see [Runtime bucket bindings](#runtime-bucket-bindings)).

A backend you construct yourself stores raw bytes and does not consult bucket
definitions, so the options are inert on it. Wrap it with
`NewConstrainedBackend` to enforce them: `Put` looks up the bucket in the
registry and rejects oversized objects (`storage.write`) or disallowed content
types before delegating. Buckets that are not registered pass through
unconstrained. A rejected upload leaves the existing object intact.

A body whose length is known up front (`*bytes.Reader`, `*strings.Reader`, a
seekable file) is rejected before any backend I/O when it exceeds the limit.
Otherwise the S3 backend still sends its `Content-Length`. The wrapper holds
back the last byte until the source is proven to end at that length, so a
source that grows before that proof never completes the request and the
existing object stays intact. Once the end is proven, the upload holds that
many bytes even if the source grows later. Other bodies are bounded while they
stream.

The wrapper implements `URLSigner`. `SignedGetURL` and `SignedPutURL` delegate
to the wrapped backend and return `storage.unsupported` when it cannot sign.
`SignedPutURL` first rejects a content type the bucket's MIME allowlist does not
list.

```go
backend := storage.NewConstrainedBackend(storage.NewFileBackend("/var/data/storage"))

// Rejected: exceeds WithMaxFileSize, or ContentType not in WithAllowedMimeTypes.
_, err := backend.Put(ctx, "avatars", "huge.png", bigReader, &storage.ObjectMetadata{
    ContentType: "image/png",
})
```

### Bucket Registry

`Bucket()` automatically registers the definition in the global registry. Query the registry at runtime:

```go
reg := storage.GetRegistry()

// Look up a single bucket.
def, ok := reg.Get("avatars")

// Iterate all registered buckets.
for _, def := range reg.All() {
    fmt.Println(def.Name, def.Options.MaxFileSize)
}
```

## Runtime bucket bindings

Application code registers buckets by **logical** name and addresses them by
that name: `storage.Bucket("uploads")` then `backend.Put(ctx, "uploads", ...)`.
A deploy target (e.g. Putnami Cloud), however, provisions a real, globally-unique
provider bucket per logical bucket (`pn-<workspace>-<env>-uploads-<hash>`). The
**runtime binding contract** is how the workload learns which provider bucket
backs each logical name — mirroring how config and secrets resolve from an
injected `CONFIG_SERVER_URL`.

### The `STORAGE_BINDINGS` contract

A deploy target injects one environment variable, `STORAGE_BINDINGS`, whose value
is a JSON document. Each `bindings` entry is a `go.putnami.dev/protocol/storage`
**Binding**; `providers` carries the per-backend construction parameters a
Binding does not itself convey.

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

| Field | Type | Meaning |
|-------|------|---------|
| `protocolVersion` | int | Envelope version. Must be `1`. |
| `bindings[].name` | string | The **logical** bucket the workload registered. Required. |
| `bindings[].backend` | string | Backend kind: `gcs`, `s3`, or `memory`. Required. |
| `bindings[].bucket` | string | The **provider** bucket the logical name resolves to. Required. |
| `bindings[].prefix` | string | Optional key prefix within `bucket` (shared-bucket isolation). |
| `bindings[].identity` | string | `workload` (ambient identity, no static secret) or `static`. |
| `bindings[].signedUrls` | bool | Whether signed-URL capability was granted. |
| `providers.gcs` | object | `{ projectId?, credentialsFile? }`. Omit entirely under Workload Identity. |
| `providers.s3` | object | `{ endpoint?, region?, accessKey?, secretKey? }`. |

The document is strict-parsed: an unknown field, a `protocolVersion` other than
`1`, a duplicate logical name, an unsupported backend kind, or a binding missing
its `bucket` fails startup loudly (`storage.request`). A per-binding
`protocolVersion` is defaulted to the current protocol version when omitted, so a
deployer need not repeat it on every entry.

### Resolving a bound backend

`DiscoverBackend` reads `STORAGE_BINDINGS` and returns a backend whose logical
bucket names transparently resolve to the bound provider buckets:

```go
backend, err := storage.DiscoverBackend(ctx)
if err != nil {
    return err // malformed contract — fail loudly
}
if backend == nil {
    // STORAGE_BINDINGS unset: fall back to a manually constructed backend
    backend = storage.NewMemoryBackend()
}
```

To go through a parsed document instead, use `ParseBindings` + `BackendFromBindings`.

The idiomatic path is `storage.NewPlugin()`, which provides a `storage.Backend`
into the DI container resolved from `STORAGE_BINDINGS`. A workload that registered
buckets and added the plugin gets a working, correctly-bound backend with **no**
per-workload bucket env:

```go
app.New("my-service").Use(storage.NewPlugin())
// elsewhere: resolve storage.Backend from the container and Put/Get/List by logical name.
```

### Fail-closed, key prefixes, and signed URLs

The bound backend remaps each logical bucket to its provider bucket on every
operation. Three guarantees:

- **Fail-closed.** An operation on a logical bucket with no injected binding
  returns `storage.unbound`; it never silently targets a bucket literally named
  after the logical name.
- **Prefix isolation.** When a binding sets `prefix`, every key is transparently
  stored under that prefix in the shared provider bucket, and the prefix is
  stripped from keys and common prefixes returned by `List`, so application code
  only ever sees logical keys.
- **Capability enforcement.** Signed URLs are available only when the binding's
  `signedUrls` grant is true and the resolved backend implements `URLSigner`.

The plugin-provided backend also enforces the registered bucket constraints on
logical bucket names:

- `Put` rejects an oversized object or a disallowed content type with
  `storage.write`. A rejected upload leaves the existing object intact.
- `SignedPutURL` rejects a content type the bucket's MIME allowlist does not
  list, including an empty one. The S3 and GCS signers bind the content type
  into the URL. A signed PUT goes straight to the provider, so the size limit
  does not apply to it.

`DiscoverBackend` and `BackendFromBindings` return an unconstrained backend.
Direct `NewGCSBackend` / `NewMemoryBackend` / `NewS3Backend` usage is unchanged —
the binding layer is purely additive. Wrap any of these backends with
`NewConstrainedBackend` to enforce bucket constraints; the wrapper keeps their
signed URLs.

## Error Codes

All errors are wrapped with structured codes from `go.putnami.dev/errors`:

| Code                  | Meaning                                  |
|-----------------------|------------------------------------------|
| `storage.read`        | Failed to read an object.                |
| `storage.write`       | Failed to write an object.               |
| `storage.delete`      | Failed to delete an object.              |
| `storage.list`        | Failed to list objects.                  |
| `storage.not_found`   | Object or bucket not found.              |
| `storage.request`     | HTTP request error, or malformed `STORAGE_BINDINGS`. |
| `storage.copy`        | Failed to copy an object.                |
| `storage.unbound`     | Operation targeted a logical bucket with no injected binding (fail-closed). |
| `storage.unsupported` | Operation unsupported by the resolved backend (e.g. signed URLs). |

## Best Practices

- **Program against `Backend`, not a concrete type.** Accept `storage.Backend` in constructors and function parameters so backends can be swapped without code changes.
- **Use `MemoryBackend` in tests.** It requires no setup, no cleanup, and runs entirely in-process.
- **Always close `GetResult.Body`.** The body is an `io.ReadCloser`. Failing to close it leaks resources (file descriptors for filesystem, HTTP connections for S3).
- **Use `Exists` instead of `Get` when you only need to check presence.** The S3 backend uses a lightweight HEAD request, avoiding a full object download.
- **Use `storage.Stat` instead of `List` to read one object's size or write time.** It reads that object's metadata alone; a list with the key as prefix costs a Class A operation on GCS.
- **Paginate large listings.** Set `MaxKeys` and use `ContinuationToken` from the `ListResult` to iterate through large buckets without loading everything into memory.
- **Define bucket constraints early.** Register buckets with `Bucket()` at package init time so constraints are available before any upload logic runs.
- **Call `Close` on shutdown.** This releases file handles and drains idle HTTP connections.
