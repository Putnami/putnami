# Storage

The `storage` package provides object storage with pluggable backends. Buckets define containers with constraints (size limits, MIME types), and backends implement the actual storage operations.

## Buckets

Declare buckets with constraints using functional options:

```go
import "go.putnami.dev/storage"

var AvatarsBucket = storage.Bucket("avatars",
    storage.WithMaxFileSize(10*1024*1024),          // 10 MB
    storage.WithAllowedMimeTypes("image/png", "image/jpeg"),
    storage.WithPublic(true),
)
```

Buckets register in a global registry when created (similar to table definitions in `sql`).

### Bucket Options

| Option | Description |
|--------|-------------|
| `WithMaxFileSize(bytes)` | Maximum file size in bytes |
| `WithAllowedMimeTypes(types...)` | Allowed MIME types |
| `WithPublic(bool)` | Whether objects are publicly accessible |
| `WithAccess(access)` | Access the runtime identity is granted (`AccessRead`, `AccessWrite`, `AccessReadWrite`; defaults to `AccessReadWrite`) |

> The backend that `storage.NewPlugin()` provides enforces `WithMaxFileSize` and
> `WithAllowedMimeTypes`: `Put` rejects oversized or disallowed-MIME uploads to
> registered buckets with `storage.write`, and a rejected upload leaves the
> existing object intact. An allowlist also rejects an empty content type. Its
> signed PUT URLs enforce the MIME allowlist, but not the size limit.
>
> A backend you construct yourself stores raw bytes and enforces nothing. Wrap it
> with `storage.NewConstrainedBackend`. The wrapper keeps the wrapped backend's
> signed URLs:
>
> ```go
> backend := storage.NewConstrainedBackend(storage.NewFileBackend("/data/storage"))
> ```

### Bucket Registry

```go
// Access the global registry
reg := storage.GetRegistry()

// Get a bucket by name
bucket, ok := reg.Get("avatars")

// List all buckets
all := reg.All()
```

## Backend Interface

All backends implement the `Backend` interface:

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

It never lists: it reads the object's entry in memory, stats the object file
and reads its metadata file on the filesystem, sends one `HeadObject` on S3, and one
object metadata read on GCS, which bills as a Class B operation where a list
bills as Class A. `Stat` is the optional
`Stater` interface rather than a `Backend` method, so a backend you write
yourself keeps compiling. Every backend this package provides implements it,
and the constrained and binding backends forward it. On a backend without it,
`storage.Stat` fails with `storage.unsupported`.

## Memory Backend

In-memory backend for testing:

```go
backend := storage.NewMemoryBackend()
defer backend.Close()

result, _ := backend.Put(ctx, "avatars", "user-123/photo.png",
    bytes.NewReader(imageData),
    &storage.ObjectMetadata{ContentType: "image/png"},
)
```

## File Backend

Filesystem-backed storage for local development:

```go
backend := storage.NewFileBackend(".data/storage")
defer backend.Close()

// Objects stored at: .data/storage/{bucket}/{key}
// Metadata stored at: .data/storage/{bucket}/{key}.meta.json
```

A key segment ending in `.meta.json` or `.putnami-tmp`, in any letter case, is
reserved for metadata and temporary files: `Put` rejects the key with
`storage.write`.

### Directory Layout

```text
.data/storage/
  avatars/
    user-123/
      photo.png           # Object data
      photo.png.meta.json # Metadata sidecar
```

## S3 Backend

S3-compatible backend for production:

```go
backend := storage.NewS3Backend(storage.S3Config{
    Endpoint:  "https://s3.amazonaws.com",
    Region:    "us-east-1",
    AccessKey: os.Getenv("AWS_ACCESS_KEY"),
    SecretKey: os.Getenv("AWS_SECRET_KEY"),
    Bucket:    "my-app-storage",
})
defer backend.Close()
```

## GCS Backend

Google Cloud Storage backend for production. It talks to the native GCS JSON API
over `net/http` with **no third-party dependencies** — auth and V4 URL signing
are implemented directly against the wire format.

```go
// Application Default Credentials: Workload Identity via the metadata server on
// Cloud Run / GKE (keyless — no static secret to manage or rotate).
backend, err := storage.NewGCSBackend(ctx, storage.GCSConfig{})
defer backend.Close()

// Or authenticate with a service-account key file.
backend, err := storage.NewGCSBackend(ctx, storage.GCSConfig{
    CredentialsFile: "/secrets/service-account.json",
})
```

Signed URLs use GOOG4 V4 signing: locally when a service-account key is present,
otherwise keyless via the IAM SignBlob API under Workload Identity. If you only
have HMAC keys, `NewS3Backend` also works against GCS's S3-interoperable endpoint
(`https://storage.googleapis.com`).

## Operations

### Put

```go
result, err := backend.Put(ctx, "avatars", "user-123/photo.png",
    bytes.NewReader(data),
    &storage.ObjectMetadata{
        ContentType: "image/png",
        Custom:      map[string]string{"uploadedBy": "user-123"},
    },
)
// result.Key, result.Size, result.ETag
```

### Get

```go
result, err := backend.Get(ctx, "avatars", "user-123/photo.png")
if result == nil {
    // Object not found
}
defer result.Body.Close()
data, _ := io.ReadAll(result.Body)
// result.ContentType, result.Size, result.ETag, result.LastModified
```

### List

```go
result, err := backend.List(ctx, "avatars", &storage.ListOptions{
    Prefix:    "user-123/",
    Delimiter: "/",
    MaxKeys:   100,
})
// result.Objects, result.Prefixes, result.IsTruncated
```

### Copy

```go
err := backend.Copy(ctx, "avatars", "user-123/photo.png", "user-123/photo-backup.png")
```

## Runtime bucket bindings

Application code addresses buckets by **logical** name; a deploy target
provisions a real provider bucket per logical bucket and injects the mapping —
a document of `go.putnami.dev/protocol/storage` bindings plus provider params —
into the `storage` section of the resolved config, or via the legacy
`STORAGE_BINDINGS` environment variable (config wins when both are present).
Resolve a bound backend that transparently remaps logical → provider buckets:

```go
backend, err := storage.DiscoverBackend(ctx) // reads STORAGE_BINDINGS
```

An operation on a logical bucket with no injected binding fails closed
(`storage.unbound`) — it never silently targets a literal-named bucket. Signed
URLs are available only when the binding grants `signedUrls` and the resolved
backend implements `URLSigner`. See `AI.md` and `doc/getting-started.md` for the
full contract and key encoding.

## DI Integration

`storage.NewPlugin()` provides a `storage.Backend` into the container, resolved
from the injected managed bindings contract (above; config section first, env
fallback):

```go
app.New("my-service").Use(storage.NewPlugin())
```

Or provide a backend explicitly:

```go
app.ProvideFunc(func() storage.Backend {
    return storage.NewMemoryBackend() // or NewFileBackend, NewS3Backend
})
```

## Support and contract

The SDD owner is `go`. `go.putnami.dev/storage` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../RELEASE.md).

The durable backend and isolation contract is
[`go/object-storage`](specs/object-storage.json). Provider and bucket isolation
are recorded in [ADR 0001](doc/adr/0001-backend-and-bucket-isolation.md), and
constraint enforcement in
[ADR 0002](doc/adr/0002-enforce-constraints-on-the-plugin-backend.md). They are
protected by [`backend_test.go`](backend_test.go), [`constrain_test.go`](constrain_test.go),
[`file_test.go`](file_test.go), [`binding_test.go`](binding_test.go), and
[`stat_test.go`](stat_test.go).
