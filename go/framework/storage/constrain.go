package storage

import (
	"context"
	stderrors "errors"
	"io"
	"slices"

	"go.putnami.dev/errors"
)

// ConstrainedBackend wraps a Backend and enforces the constraints declared on
// registered buckets — maximum file size and allowed MIME types. Backends store
// raw bytes and never consult bucket definitions, so a backend you construct
// yourself enforces WithMaxFileSize and WithAllowedMimeTypes only behind this
// wrapper. The backend that Plugin provides is already wrapped. Objects written to
// buckets that are not registered pass through unconstrained.
//
// ConstrainedBackend implements Stater and URLSigner. Stat and signed URLs are
// delegated to the wrapped backend when it implements the matching interface,
// and fail with CodeStorageUnsupported otherwise. All other operations
// delegate unchanged.
type ConstrainedBackend struct {
	Backend
	registry *Registry
}

// Ensure ConstrainedBackend implements every interface at compile time.
var (
	_ Backend   = (*ConstrainedBackend)(nil)
	_ Stater    = (*ConstrainedBackend)(nil)
	_ URLSigner = (*ConstrainedBackend)(nil)
)

// NewConstrainedBackend wraps b so Put and SignedPutURL enforce the constraints
// of buckets registered in the global registry.
func NewConstrainedBackend(b Backend) *ConstrainedBackend {
	return &ConstrainedBackend{Backend: b, registry: GetRegistry()}
}

// Put enforces the registered bucket's MIME and size constraints before
// delegating to the wrapped backend. Unregistered buckets are not constrained.
//
// When the body length is known up front (see readerContentLength), a body over
// the limit is rejected before any backend I/O. A body within it is served
// through knownLengthReader, which reports that length to the backend and
// fails before the last byte if the source turns out longer. A body of unknown
// length is read through a guard that fails once the limit is crossed. Either
// rejection deletes nothing when the wrapped backend returns an error (see
// reject).
func (c *ConstrainedBackend) Put(ctx context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (*PutResult, error) {
	def, ok := c.definition(bucket)
	if !ok {
		return c.Backend.Put(ctx, bucket, key, data, meta)
	}

	contentType := ""
	if meta != nil {
		contentType = meta.ContentType
	}
	if err := checkContentType(def, bucket, key, contentType); err != nil {
		return nil, err
	}

	limit := def.Options.MaxFileSize
	if limit <= 0 {
		return c.Backend.Put(ctx, bucket, key, data, meta)
	}

	if size, known := readerContentLength(data); known {
		if size > limit {
			return nil, sizeError(nil, bucket, key, limit)
		}
		body := &knownLengthReader{r: data, size: size}
		result, err := c.Backend.Put(ctx, bucket, key, body, meta)
		if !body.grew {
			return result, err
		}
		return nil, c.reject(ctx, bucket, key, err, func(cause error) *errors.Error {
			return lengthError(cause, bucket, key, size)
		})
	}

	guard := &limitGuardReader{r: data, max: limit}
	result, err := c.Backend.Put(ctx, bucket, key, guard, meta)
	if !guard.over {
		return result, err
	}
	return nil, c.reject(ctx, bucket, key, err, func(cause error) *errors.Error {
		return sizeError(cause, bucket, key, limit)
	})
}

// reject builds the error for a Put its body reader refused. When the wrapped
// backend returned an error, that error becomes the cause and nothing is
// deleted: a conforming backend committed nothing, so an existing object stays
// intact. When the backend reported success anyway, it committed an object this
// Put wrote from a refused body, so reject deletes it best-effort and records a
// failed deletion as the cleanupError attribute.
func (c *ConstrainedBackend) reject(ctx context.Context, bucket, key string, backendErr error, rejection func(cause error) *errors.Error) error {
	if backendErr != nil {
		return rejection(backendErr)
	}
	err := rejection(nil)
	if derr := c.Backend.Delete(context.WithoutCancel(ctx), bucket, key); derr != nil {
		err = err.WithAttr(errors.String("cleanupError", derr.Error()))
	}
	return err
}

// Stat delegates to the wrapped backend. Constraints apply to writes only.
func (c *ConstrainedBackend) Stat(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	return Stat(ctx, c.Backend, bucket, key)
}

// SignedGetURL delegates to the wrapped backend.
func (c *ConstrainedBackend) SignedGetURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	signer, err := c.signer(bucket)
	if err != nil {
		return "", err
	}
	return signer.SignedGetURL(ctx, bucket, key, opts)
}

// SignedPutURL enforces the registered bucket's MIME allowlist against
// opts.ContentType, which the S3 and GCS signers bind into the signature, then
// delegates to the wrapped backend. The client uploads straight to the
// provider, so MaxFileSize is not enforced on a signed PUT URL.
func (c *ConstrainedBackend) SignedPutURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	if def, ok := c.definition(bucket); ok {
		if err := checkContentType(def, bucket, key, opts.ContentType); err != nil {
			return "", err
		}
	}
	signer, err := c.signer(bucket)
	if err != nil {
		return "", err
	}
	return signer.SignedPutURL(ctx, bucket, key, opts)
}

// signer returns the wrapped backend as a URLSigner, or a CodeStorageUnsupported
// error when it cannot sign URLs.
func (c *ConstrainedBackend) signer(bucket string) (URLSigner, error) {
	signer, ok := c.Backend.(URLSigner)
	if !ok {
		return nil, errors.New(CodeStorageUnsupported, "wrapped backend does not support signed URLs",
			errors.String("backend", "constrained"), errors.String("bucket", bucket))
	}
	return signer, nil
}

// definition returns the registered definition of bucket, if any.
func (c *ConstrainedBackend) definition(bucket string) (*BucketDefinition, bool) {
	if c.registry == nil {
		return nil, false
	}
	def, ok := c.registry.Get(bucket)
	if !ok || def == nil {
		return nil, false
	}
	return def, true
}

// checkContentType rejects contentType when def declares a MIME allowlist that
// does not list it. An empty contentType is rejected by any allowlist.
func checkContentType(def *BucketDefinition, bucket, key, contentType string) error {
	allowed := def.Options.AllowedMimeTypes
	if len(allowed) == 0 || slices.Contains(allowed, contentType) {
		return nil
	}
	return errors.New(CodeStorageWrite, "content type not allowed for bucket",
		errors.String("backend", "constrained"), errors.String("bucket", bucket),
		errors.String("key", key), errors.String("contentType", contentType))
}

// sizeError reports an object over the bucket's limit. A non-nil cause is the
// wrapped backend's error and stays reachable through the returned error.
func sizeError(cause error, bucket, key string, limit int64) *errors.Error {
	attrs := []errors.Attr{
		errors.String("backend", "constrained"), errors.String("bucket", bucket),
		errors.String("key", key), errors.Int64("maxFileSize", limit),
	}
	if cause != nil {
		return errors.Wrapf(cause, CodeStorageWrite, "object exceeds bucket max file size", attrs...)
	}
	return errors.New(CodeStorageWrite, "object exceeds bucket max file size", attrs...)
}

// lengthError reports a body that yielded more bytes than the length it
// reported when Put started, with cause as the backend's error when it has one.
func lengthError(cause error, bucket, key string, size int64) *errors.Error {
	attrs := []errors.Attr{
		errors.String("backend", "constrained"), errors.String("bucket", bucket),
		errors.String("key", key), errors.Int64("reportedLength", size),
	}
	if cause != nil {
		return errors.Wrapf(cause, CodeStorageWrite, "object body is longer than its reported length", attrs...)
	}
	return errors.New(CodeStorageWrite, "object body is longer than its reported length", attrs...)
}

// errLimitExceeded stops the wrapped backend from reading past the size limit.
// The caller detects the condition via limitGuardReader.over and returns a
// structured error, so this sentinel value never surfaces to users.
var errLimitExceeded = errors.New(CodeStorageWrite, "object exceeds bucket max file size")

// limitGuardReader fails the read once more than max bytes have been consumed,
// so size enforcement works on streaming uploads without buffering the body.
type limitGuardReader struct {
	r     io.Reader
	max   int64
	count int64
	over  bool
}

func (l *limitGuardReader) Read(p []byte) (int, error) {
	remaining := l.max - l.count
	if remaining <= 0 {
		var probe [1]byte
		n, err := l.r.Read(probe[:])
		if n > 0 {
			l.over = true
			return 0, errLimitExceeded
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := l.r.Read(p)
	l.count += int64(n)
	return n, err
}

// errBodyOutgrewLength stops the wrapped backend before the last byte of a
// body that turned out longer than its reported length. The caller detects the
// condition via knownLengthReader.grew and returns lengthError, so this
// sentinel value never surfaces to users.
var errBodyOutgrewLength = errors.New(CodeStorageWrite, "object body is longer than its reported length")

// knownLengthReader serves a body whose length was known, and within the
// limit, when Put started. Len reports the unread part of that length, so a
// backend that needs a Content-Length (S3) streams the body instead of
// spooling it. It yields at most size bytes and holds back the last one until
// the source is proven to end there. A source that grew fails the read before
// the backend has the whole advertised body: an HTTP backend then never
// completes the request, so the provider commits nothing over the existing
// object. Once the end is proven, every later read returns io.EOF without
// touching the source, because the backend may already hold the whole body.
type knownLengthReader struct {
	r      io.Reader
	size   int64
	served int64
	grew   bool
	ended  bool
}

// Len returns the number of bytes of the reported length not served yet.
func (k *knownLengthReader) Len() int {
	return int(k.size - k.served)
}

func (k *knownLengthReader) Read(p []byte) (int, error) {
	if k.ended {
		return 0, io.EOF
	}
	remaining := k.size - k.served
	if remaining == 0 {
		return 0, k.proveEnd()
	}
	if len(p) == 0 {
		return 0, nil
	}
	if remaining > 1 {
		n, err := k.r.Read(p[:min(int64(len(p)), remaining-1)])
		k.served += int64(n)
		return n, err
	}
	n, err := io.ReadFull(k.r, p[:1])
	if n == 0 {
		return 0, err
	}
	if err := k.proveEnd(); !stderrors.Is(err, io.EOF) {
		return 0, err
	}
	k.served++
	return 1, io.EOF
}

// proveEnd reads one byte past the reported length. It returns io.EOF, and
// records the end, when the source ends there; errBodyOutgrewLength when it
// does not; and the source's own error otherwise.
func (k *knownLengthReader) proveEnd() error {
	var probe [1]byte
	n, err := io.ReadFull(k.r, probe[:])
	if n > 0 {
		k.grew = true
		return errBodyOutgrewLength
	}
	k.ended = stderrors.Is(err, io.EOF)
	return err
}
