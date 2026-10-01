# Storage

`go.putnami.dev/storage` provides object storage with pluggable backends for files, images, documents, and other binary data.

It is a **stable** public package owned by the Go SDD surface. Managed bindings
map logical buckets to provider buckets and prefixes; missing bindings and
unavailable capabilities fail closed instead of crossing an isolation boundary
or silently degrading. See the
object-storage specification and the accepted decision records next to the
package source: backend and bucket isolation, and constraint enforcement.

## Buckets

Declare storage buckets with constraints:

```go
import "go.putnami.dev/storage"

var Avatars = storage.Bucket("avatars",
    storage.WithMaxFileSize(10 * 1024 * 1024),            // 10 MB
    storage.WithAllowedMimeTypes("image/png", "image/jpeg"),
)

var Documents = storage.Bucket("documents",
    storage.WithMaxFileSize(50 * 1024 * 1024),            // 50 MB
    storage.WithAllowedMimeTypes("application/pdf", "text/plain"),
    storage.WithPublic(true),
)
```

### Bucket options

| Option | Description |
|--------|-------------|
| `WithMaxFileSize(bytes)` | Maximum file size in bytes |
| `WithAllowedMimeTypes(types...)` | Restrict to specific MIME types |
| `WithPublic(bool)` | Mark bucket as publicly accessible |

The backend that `storage.NewPlugin()` provides enforces `WithMaxFileSize` and
`WithAllowedMimeTypes`: `Put` rejects an oversized object or a disallowed
content type with `storage.write`, and a rejected upload leaves the existing
object intact. An allowlist also rejects an empty content type. Its signed PUT
URLs enforce the MIME allowlist, but not the size limit. A backend you construct
yourself enforces these options only when you wrap it with
`storage.NewConstrainedBackend`, which keeps the wrapped backend's signed URLs.

### Bucket registry

All declared buckets register in a global registry:

```go
registry := storage.GetRegistry()

bucket, ok := registry.Get("avatars")
all := registry.All()
```

## Backend interface

All backends implement the same interface:

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

Every operation carries the caller context. Non-streaming provider operations
may add a configured request timeout; a streaming `Get` remains bounded by the
caller context and the caller must close the returned body.

## Backends

### Memory backend

In-memory storage for testing:

```go
backend := storage.NewMemoryBackend()
```

### File backend

Filesystem storage for local development:

```go
backend := storage.NewFileBackend("./data")
// Objects stored at ./data/{bucket}/{key}
// Metadata sidecar at ./data/{bucket}/{key}.meta.json
```

A key segment ending in `.meta.json` or `.putnami-tmp`, in any letter case, is
reserved for metadata and temporary files: `Put` rejects the key with
`storage.write`.

### S3 backend

S3-compatible storage for production:

```go
backend := storage.NewS3Backend(storage.S3Config{
    Endpoint:  "https://s3.amazonaws.com",
    Region:    "us-east-1",
    AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
    SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
    Bucket:    "my-app-storage",
})
```

## Operations

### Put

```go
result, err := backend.Put(ctx, "avatars", "user-123/photo.png", file, &storage.ObjectMetadata{
    ContentType: "image/png",
    Custom:      map[string]string{"uploadedBy": "user-123"},
})
```

### Get

```go
obj, err := backend.Get(ctx, "avatars", "user-123/photo.png")
if err != nil {
    return err
}
defer obj.Body.Close()

// Read the object
data, err := io.ReadAll(obj.Body)
```

### Delete

```go
err := backend.Delete(ctx, "avatars", "user-123/photo.png")
```

### List

```go
result, err := backend.List(ctx, "avatars", &storage.ListOptions{
    Prefix: "user-123/",
})

for _, obj := range result.Objects {
    fmt.Printf("%s (%d bytes)\n", obj.Key, obj.Size)
}
```

### Exists

```go
exists, err := backend.Exists(ctx, "avatars", "user-123/photo.png")
```

### Copy

```go
err := backend.Copy(ctx, "avatars", "user-123/old.png", "user-123/new.png")
```

## Usage patterns

### Upload handler

```go
func uploadAvatar(backend storage.Backend) fhttp.Handler {
    return func(ctx *fhttp.Context) *fhttp.Response {
        body, err := ctx.RawBody()
        if err != nil {
            return fhttp.JSONStatus(400, map[string]string{"error": "invalid body"})
        }

        key := fmt.Sprintf("%s/avatar.png", ctx.Param("userId"))
        _, err = backend.Put(ctx.Context(), "avatars", key, bytes.NewReader(body), &storage.ObjectMetadata{
            ContentType: ctx.ContentType(),
        })
        if err != nil {
            return fhttp.InternalError(err.Error())
        }
        return fhttp.JSONStatus(201, map[string]string{"key": key})
    }
}
```

### Choosing backends per environment

```go
func newStorageBackend(env string) storage.Backend {
    switch env {
    case "test":
        return storage.NewMemoryBackend()
    case "development":
        return storage.NewFileBackend("./data")
    default:
        return storage.NewS3Backend(s3Config)
    }
}
```

## Related guides

- [HTTP & Middleware](/docs/frameworks/go/http) — file upload handlers
- [Configuration](/docs/frameworks/go/configuration) — backend configuration
- [TypeScript storage](/docs/frameworks/typescript/storage) — TypeScript equivalent
