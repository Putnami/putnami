package storage

import (
	"context"
	"io"
	"net/http"
	"time"
)

// newStorageHTTPClient builds the http.Client used by a storage backend.
// It deliberately leaves Client.Timeout unset (0): that field bounds the whole
// exchange — including reading the response body — which would abort a streaming
// Get of a large object mid-download. Per-operation deadlines come from the
// request context.Context instead (see ctxutil.WithRequestTimeout).
//
// Each backend gets its OWN transport (a clone of http.DefaultTransport) rather
// than sharing the process-wide default. Otherwise Backend.Close() →
// Transport.CloseIdleConnections() would close idle keep-alive connections for
// every other consumer of DefaultTransport in the process, and backends could
// not tune their own pool. Close() is now scoped to this backend's transport.
func newStorageHTTPClient() *http.Client {
	client := &http.Client{}
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		client.Transport = dt.Clone()
	}
	return client
}

// Backend is the pluggable storage implementation interface.
type Backend interface {
	// Put stores an object in the bucket, replacing any existing object at key.
	// A Put whose data reader fails commits nothing: it returns an error and
	// leaves any existing object at key, and its metadata, intact.
	Put(ctx context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (*PutResult, error)

	// Get retrieves an object from the bucket.
	Get(ctx context.Context, bucket, key string) (*GetResult, error)

	// Delete removes an object from the bucket.
	Delete(ctx context.Context, bucket, key string) error

	// List returns objects matching the given options.
	List(ctx context.Context, bucket string, opts *ListOptions) (*ListResult, error)

	// Exists checks whether an object exists.
	Exists(ctx context.Context, bucket, key string) (bool, error)

	// Copy duplicates an object within the same bucket.
	Copy(ctx context.Context, bucket, source, destination string) error

	// Close releases any resources held by the backend.
	Close() error
}

// ObjectMetadata contains metadata for stored objects.
type ObjectMetadata struct {
	ContentType        string
	CacheControl       string
	ContentDisposition string
	Custom             map[string]string
}

// PutResult is returned after successfully storing an object.
type PutResult struct {
	Key  string
	Size int64
	ETag string
}

// GetResult is returned when retrieving an object.
type GetResult struct {
	Key          string
	Body         io.ReadCloser
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
	Metadata     map[string]string
}

// ListOptions configures object listing.
type ListOptions struct {
	// Prefix filters results to objects whose keys start with this value.
	Prefix string
	// Delimiter groups results by common prefixes (typically "/" for directory-like listing).
	Delimiter string
	// MaxKeys limits the number of objects returned per page. 0 uses backend default.
	MaxKeys int
	// ContinuationToken resumes listing from a previous truncated response.
	ContinuationToken string
}

// ListResult contains the results of a list operation.
type ListResult struct {
	Objects           []ObjectInfo
	Prefixes          []string // Common prefixes when delimiter is used.
	IsTruncated       bool
	ContinuationToken string
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

// URLSigner is an optional interface that backends can implement to provide
// pre-signed URLs for direct client access, bypassing the application server.
type URLSigner interface {
	// SignedGetURL returns a pre-signed URL for downloading an object.
	SignedGetURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error)

	// SignedPutURL returns a pre-signed URL for uploading an object.
	SignedPutURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error)
}

// SignedURLOptions configures signed URL generation.
type SignedURLOptions struct {
	// Expiry is how long the signed URL remains valid.
	Expiry time.Duration
	// ContentType restricts the upload to this content type (PUT URLs only).
	ContentType string
}
