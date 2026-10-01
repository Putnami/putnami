package storage

import (
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.putnami.dev/errors"
	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

func newTestRegistry(defs ...*BucketDefinition) *Registry {
	r := &Registry{buckets: make(map[string]*BucketDefinition)}
	for _, d := range defs {
		r.buckets[d.Name] = d
	}
	return r
}

func TestConstrainedBackend_AllowedMimeTypes(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-enforces-mime-allowlist")
	reg := newTestRegistry(&BucketDefinition{
		Name:    "avatars",
		Options: BucketOptions{AllowedMimeTypes: []string{"image/png", "image/jpeg"}},
	})
	be := &ConstrainedBackend{Backend: NewMemoryBackend(), registry: reg}
	ctx := context.Background()

	if _, err := be.Put(ctx, "avatars", "ok.png", strings.NewReader("x"), &ObjectMetadata{ContentType: "image/png"}); err != nil {
		t.Fatalf("png should be allowed: %v", err)
	}
	if _, err := be.Put(ctx, "avatars", "bad.gif", strings.NewReader("x"), &ObjectMetadata{ContentType: "image/gif"}); err == nil {
		t.Fatal("gif should be rejected")
	}
	if _, err := be.Put(ctx, "avatars", "no-meta", strings.NewReader("x"), nil); err == nil {
		t.Fatal("nil content type should be rejected when an allow-list is set")
	}
	// Unregistered buckets are not constrained.
	if _, err := be.Put(ctx, "other", "anything.gif", strings.NewReader("x"), &ObjectMetadata{ContentType: "image/gif"}); err != nil {
		t.Fatalf("unregistered bucket should pass through: %v", err)
	}
}

func TestConstrainedBackend_MaxFileSize(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-enforces-size-limit")
	reg := newTestRegistry(&BucketDefinition{
		Name:    "uploads",
		Options: BucketOptions{MaxFileSize: 8},
	})
	mem := NewMemoryBackend()
	be := &ConstrainedBackend{Backend: mem, registry: reg}
	ctx := context.Background()

	if _, err := be.Put(ctx, "uploads", "ok", strings.NewReader("12345678"), nil); err != nil {
		t.Fatalf("8 bytes should be within the limit: %v", err)
	}
	_, err := be.Put(ctx, "uploads", "big", bytes.NewReader(make([]byte, 9)), nil)
	if err == nil {
		t.Fatal("9 bytes should exceed the 8-byte limit")
	}
	if errors.GetCode(err) != CodeStorageWrite {
		t.Errorf("expected code %q, got %q", CodeStorageWrite, errors.GetCode(err))
	}
	// The rejected object must not have been stored.
	if obj, _ := mem.Get(ctx, "uploads", "big"); obj != nil {
		t.Error("oversize object should not be stored")
	}
}

// readObject returns the body of bucket/key, or fails the test when it is absent.
func readObject(t *testing.T, b Backend, bucket, key string) (string, *GetResult) {
	t.Helper()
	obj, err := b.Get(context.Background(), bucket, key)
	if err != nil {
		t.Fatalf("Get %s/%s: %v", bucket, key, err)
	}
	if obj == nil {
		t.Fatalf("Get %s/%s: object is missing", bucket, key)
	}
	defer obj.Body.Close()
	data, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}
	return string(data), obj
}

// attrValue returns the value of the named structured attribute of err.
func attrValue(err error, key string) (any, bool) {
	for _, a := range errors.GetAttrs(err) {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

func TestConstrainedBackend_RejectedOverwriteKeepsObject(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-rejected-overwrite-keeps-object")
	for name, newBackend := range map[string]func(t *testing.T) Backend{
		"memory": func(*testing.T) Backend { return NewMemoryBackend() },
		"file":   func(t *testing.T) Backend { return NewFileBackend(t.TempDir()) },
	} {
		t.Run(name, func(t *testing.T) {
			reg := newTestRegistry(&BucketDefinition{
				Name:    "uploads",
				Options: BucketOptions{MaxFileSize: 8},
			})
			inner := newBackend(t)
			be := &ConstrainedBackend{Backend: inner, registry: reg}
			ctx := context.Background()

			if _, err := be.Put(ctx, "uploads", "doc", strings.NewReader("original"), &ObjectMetadata{ContentType: "text/plain"}); err != nil {
				t.Fatalf("seed Put: %v", err)
			}
			_, err := be.Put(ctx, "uploads", "doc", unsized(bytes.Repeat([]byte("x"), 64)), &ObjectMetadata{ContentType: "application/octet-stream"})
			if err == nil {
				t.Fatal("64 bytes should exceed the 8-byte limit")
			}
			if errors.GetCode(err) != CodeStorageWrite {
				t.Errorf("code = %q, want %q", errors.GetCode(err), CodeStorageWrite)
			}
			if v, ok := attrValue(err, "maxFileSize"); !ok || v != int64(8) {
				t.Errorf("maxFileSize attr = %v (present %v), want 8", v, ok)
			}
			if !hasAttr(err, "backend", name) {
				t.Errorf("size error %v should keep the %s backend's error as its cause", err, name)
			}

			body, obj := readObject(t, inner, "uploads", "doc")
			if body != "original" {
				t.Errorf("object after rejected overwrite = %q, want %q", body, "original")
			}
			if obj.ContentType != "text/plain" {
				t.Errorf("content type after rejected overwrite = %q, want %q", obj.ContentType, "text/plain")
			}
		})
	}
}

// unsized returns a reader over data that hides its length, so the constrained
// backend must enforce the size limit while streaming.
func unsized(data []byte) io.Reader {
	return io.MultiReader(bytes.NewReader(data))
}

// hasAttr reports whether any error in err's tree carries the attribute key=value.
func hasAttr(err error, key string, value any) bool {
	for _, a := range errors.GetAttrs(err) {
		if a.Key == key && a.Value == value {
			return true
		}
	}
	return false
}

// swallowingBackend violates the Backend Put contract: it commits whatever its
// reader yielded before failing and reports success.
type swallowingBackend struct {
	*MemoryBackend
	deleteErr error
	deletes   int
}

func (s *swallowingBackend) Put(ctx context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (*PutResult, error) {
	buf, _ := io.ReadAll(data)
	return s.MemoryBackend.Put(ctx, bucket, key, bytes.NewReader(buf), meta)
}

func (s *swallowingBackend) Delete(ctx context.Context, bucket, key string) error {
	s.deletes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.MemoryBackend.Delete(ctx, bucket, key)
}

func TestConstrainedBackend_DeletesTruncatedObjectCommittedPastLimit(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-deletes-truncated-commit")
	reg := newTestRegistry(&BucketDefinition{
		Name:    "uploads",
		Options: BucketOptions{MaxFileSize: 8},
	})
	ctx := context.Background()

	inner := &swallowingBackend{MemoryBackend: NewMemoryBackend()}
	be := &ConstrainedBackend{Backend: inner, registry: reg}
	_, err := be.Put(ctx, "uploads", "big", unsized(make([]byte, 9)), nil)
	if errors.GetCode(err) != CodeStorageWrite {
		t.Fatalf("err = %v, want code %q", err, CodeStorageWrite)
	}
	if inner.deletes != 1 {
		t.Errorf("deletes = %d, want 1", inner.deletes)
	}
	if obj, _ := inner.Get(ctx, "uploads", "big"); obj != nil {
		t.Error("the truncated object the backend committed should be deleted")
	}
	if _, ok := attrValue(err, "cleanupError"); ok {
		t.Error("a successful cleanup should not report cleanupError")
	}

	failing := &swallowingBackend{MemoryBackend: NewMemoryBackend(), deleteErr: stderrors.New("delete refused")}
	be = &ConstrainedBackend{Backend: failing, registry: reg}
	_, err = be.Put(ctx, "uploads", "big", unsized(make([]byte, 9)), nil)
	if errors.GetCode(err) != CodeStorageWrite {
		t.Fatalf("err = %v, want code %q", err, CodeStorageWrite)
	}
	if v, ok := attrValue(err, "cleanupError"); !ok || v != "delete refused" {
		t.Errorf("cleanupError attr = %v (present %v), want the delete error", v, ok)
	}
}

// closeCountingSigner is a signing backend that counts Close calls.
type closeCountingSigner struct {
	*signingFakeBackend
	mu     sync.Mutex
	closes int
}

func (c *closeCountingSigner) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return c.signingFakeBackend.Close()
}

func TestConstrainedBackend_SignedURLsEnforceMimeAllowlist(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "signed-put-url-enforces-mime-allowlist")
	reg := newTestRegistry(
		&BucketDefinition{Name: "avatars", Options: BucketOptions{AllowedMimeTypes: []string{"image/png"}, MaxFileSize: 8}},
		&BucketDefinition{Name: "sized", Options: BucketOptions{MaxFileSize: 8}},
	)
	inner := &closeCountingSigner{signingFakeBackend: &signingFakeBackend{fakeBackend: newFakeBackend()}}
	be := &ConstrainedBackend{Backend: inner, registry: reg}
	ctx := context.Background()

	url, err := be.SignedPutURL(ctx, "avatars", "a.png", SignedURLOptions{ContentType: "image/png"})
	if err != nil || url != "signed:put:avatars:a.png" {
		t.Errorf("allowed SignedPutURL = %q, %v; want the delegated URL", url, err)
	}
	for _, contentType := range []string{"image/gif", ""} {
		if _, err := be.SignedPutURL(ctx, "avatars", "a", SignedURLOptions{ContentType: contentType}); errors.GetCode(err) != CodeStorageWrite {
			t.Errorf("SignedPutURL content type %q: err = %v, want code %q", contentType, err, CodeStorageWrite)
		}
	}
	// A size limit cannot bind a signed PUT, so it does not refuse one.
	if _, err := be.SignedPutURL(ctx, "sized", "k", SignedURLOptions{}); err != nil {
		t.Errorf("SignedPutURL on a size-only bucket: %v", err)
	}
	if _, err := be.SignedPutURL(ctx, "unregistered", "k", SignedURLOptions{}); err != nil {
		t.Errorf("SignedPutURL on an unregistered bucket: %v", err)
	}
	if url, err := be.SignedGetURL(ctx, "avatars", "a.png", SignedURLOptions{}); err != nil || url != "signed:get:avatars:a.png" {
		t.Errorf("SignedGetURL = %q, %v; want the delegated URL", url, err)
	}
	if got := strings.Join(inner.calls, ","); got != "signput avatars a.png,signput sized k,signput unregistered k,signget avatars a.png" {
		t.Errorf("signer calls = %q; a rejected content type must not reach the signer", got)
	}

	if err := be.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if inner.closes != 1 {
		t.Errorf("inner Close calls = %d, want 1", inner.closes)
	}

	// NewConstrainedBackend keeps the wrapped backend's signed URLs.
	var signer URLSigner = NewConstrainedBackend(&signingFakeBackend{fakeBackend: newFakeBackend()})
	if url, err := signer.SignedGetURL(ctx, "b", "k", SignedURLOptions{}); err != nil || url != "signed:get:b:k" {
		t.Errorf("NewConstrainedBackend SignedGetURL = %q, %v; want the delegated URL", url, err)
	}

	// A wrapped backend that cannot sign fails with a typed error, after the
	// allowlist check.
	plain := &ConstrainedBackend{Backend: NewMemoryBackend(), registry: reg}
	if _, err := plain.SignedPutURL(ctx, "avatars", "a", SignedURLOptions{ContentType: "image/gif"}); errors.GetCode(err) != CodeStorageWrite {
		t.Errorf("non-signing SignedPutURL with a disallowed type: err = %v, want code %q", err, CodeStorageWrite)
	}
	if _, err := plain.SignedPutURL(ctx, "avatars", "a", SignedURLOptions{ContentType: "image/png"}); errors.GetCode(err) != CodeStorageUnsupported {
		t.Errorf("non-signing SignedPutURL: err = %v, want code %q", err, CodeStorageUnsupported)
	}
	if _, err := plain.SignedGetURL(ctx, "avatars", "a", SignedURLOptions{}); errors.GetCode(err) != CodeStorageUnsupported {
		t.Errorf("non-signing SignedGetURL: err = %v, want code %q", err, CodeStorageUnsupported)
	}
}

func TestConstrainedBackend_KnownOversizedBodyCausesNoBackendIO(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-rejects-known-oversized-body-without-io")
	reg := newTestRegistry(&BucketDefinition{Name: "uploads", Options: BucketOptions{MaxFileSize: 8}})
	inner := newFakeBackend()
	be := &ConstrainedBackend{Backend: inner, registry: reg}

	_, err := be.Put(context.Background(), "uploads", "big", bytes.NewReader(make([]byte, 9)), nil)
	if errors.GetCode(err) != CodeStorageWrite {
		t.Fatalf("err = %v, want code %q", err, CodeStorageWrite)
	}
	if n := inner.callCount(); n != 0 {
		t.Errorf("backend calls = %d (%v), want 0 for a body of known oversized length", n, inner.calls)
	}
}

// Without credentials, S3Backend.Put streams a body of unknown length with
// chunked transfer encoding; with credentials it spools that body to a
// temporary file. A Content-Length on the request therefore proves the
// constrained backend kept the body's length visible.
func TestConstrainedBackend_KnownLengthBodyReachesS3WithContentLength(t *testing.T) {
	var (
		gotContentLength    int64
		gotTransferEncoding []string
		gotBody             string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		gotTransferEncoding = r.TransferEncoding
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	reg := newTestRegistry(&BucketDefinition{Name: "uploads", Options: BucketOptions{MaxFileSize: 64}})
	be := &ConstrainedBackend{Backend: NewS3Backend(S3Config{Endpoint: server.URL}), registry: reg}
	if _, err := be.Put(context.Background(), "uploads", "k", bytes.NewReader([]byte("known length")), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotBody != "known length" {
		t.Errorf("server body = %q, want %q", gotBody, "known length")
	}
	if gotContentLength != int64(len("known length")) || len(gotTransferEncoding) != 0 {
		t.Errorf("ContentLength = %d, TransferEncoding = %v; want %d and none", gotContentLength, gotTransferEncoding, len("known length"))
	}
}

func TestPluginBackendEnforcesBucketConstraints(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "plugin-backend-enforces-bucket-constraints")
	t.Setenv("CONFIG_DATA", "")
	t.Setenv(EnvBindings, `{"protocolVersion":1,"bindings":[{"name":"uploads","backend":"memory","bucket":"prov-uploads","prefix":"p/"}]}`)
	p := &Plugin{registry: newTestRegistry(&BucketDefinition{
		Name:    "uploads",
		Options: BucketOptions{MaxFileSize: 8, AllowedMimeTypes: []string{"text/plain"}},
	})}
	c := inject.NewContainer("test", nil)
	for _, reg := range p.Provides() {
		if err := c.Register(reg); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	v, err := c.Get(inject.TokenOf[Backend]())
	if err != nil {
		t.Fatalf("resolve storage.Backend: %v", err)
	}
	backend, ok := v.(Backend)
	if !ok {
		t.Fatalf("resolved value is %T, want storage.Backend", v)
	}
	ctx := context.Background()
	plain := &ObjectMetadata{ContentType: "text/plain"}

	if _, err := backend.Put(ctx, "uploads", "note", strings.NewReader("original"), plain); err != nil {
		t.Fatalf("Put within the constraints: %v", err)
	}
	for name, body := range map[string]io.Reader{
		"known length":   strings.NewReader("123456789"),
		"unknown length": unsized([]byte("123456789")),
	} {
		if _, err := backend.Put(ctx, "uploads", "note", body, plain); errors.GetCode(err) != CodeStorageWrite {
			t.Errorf("oversized Put with a body of %s: err = %v, want code %q", name, err, CodeStorageWrite)
		}
	}
	if _, err := backend.Put(ctx, "uploads", "pic", strings.NewReader("png"), &ObjectMetadata{ContentType: "image/png"}); errors.GetCode(err) != CodeStorageWrite {
		t.Errorf("disallowed MIME Put: err = %v, want code %q", err, CodeStorageWrite)
	}
	if body, _ := readObject(t, backend, "uploads", "note"); body != "original" {
		t.Errorf("object after rejected overwrite = %q, want %q", body, "original")
	}
	if obj, _ := backend.Get(ctx, "uploads", "pic"); obj != nil {
		t.Error("a Put with a disallowed MIME type should store nothing")
	}

	signer, ok := backend.(URLSigner)
	if !ok {
		t.Fatalf("provided backend %T should still implement URLSigner", backend)
	}
	if _, err := signer.SignedPutURL(ctx, "uploads", "pic", SignedURLOptions{ContentType: "image/png"}); errors.GetCode(err) != CodeStorageWrite {
		t.Errorf("SignedPutURL with a disallowed MIME type: err = %v, want code %q", err, CodeStorageWrite)
	}
	// An allowed content type reaches the binding layer, which refuses it
	// because this binding does not grant signed URLs.
	if _, err := signer.SignedPutURL(ctx, "uploads", "note", SignedURLOptions{ContentType: "text/plain"}); errors.GetCode(err) != CodeStorageUnsupported {
		t.Errorf("SignedPutURL with an allowed MIME type: err = %v, want code %q", err, CodeStorageUnsupported)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close container: %v", err)
	}
	if p.backend != nil {
		t.Error("plugin backend was not cleared after container close")
	}
}

// understatedReader reports a Len smaller than the bytes it yields, like a
// file that grows after its length was read.
type understatedReader struct {
	io.Reader
	len int
}

func (u *understatedReader) Len() int { return u.len }

func TestConstrainedBackend_GrowingKnownLengthBodyStaysBounded(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-bounds-growing-known-length-body")
	reg := newTestRegistry(&BucketDefinition{Name: "uploads", Options: BucketOptions{MaxFileSize: 8}})
	mem := NewMemoryBackend()
	be := &ConstrainedBackend{Backend: mem, registry: reg}
	ctx := context.Background()

	body := &understatedReader{Reader: bytes.NewReader(make([]byte, 64)), len: 4}
	_, err := be.Put(ctx, "uploads", "grown", body, nil)
	if err == nil {
		t.Fatal("a body that grows past the limit after its length check must be rejected")
	}
	if errors.GetCode(err) != CodeStorageWrite {
		t.Errorf("code = %q, want %q", errors.GetCode(err), CodeStorageWrite)
	}
	if obj, _ := mem.Get(ctx, "uploads", "grown"); obj != nil {
		t.Error("the grown body must not be stored")
	}
}

func TestConstrainedBackend_GrowingKnownLengthBodyKeepsS3Object(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-growing-body-keeps-s3-object")
	const reported = 256 << 10
	reg := newTestRegistry(&BucketDefinition{Name: "uploads", Options: BucketOptions{MaxFileSize: 2 * reported}})
	for name, cfg := range map[string]S3Config{
		"streaming": {},
		"signed":    {AccessKey: "AKID", SecretKey: "SECRET"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg.Endpoint = fakeS3Objects(t).URL
			s3 := NewS3Backend(cfg)
			be := &ConstrainedBackend{Backend: s3, registry: reg}
			ctx := context.Background()
			if _, err := s3.Put(ctx, "uploads", "doc", strings.NewReader("original"), nil); err != nil {
				t.Fatalf("seed Put: %v", err)
			}

			// The reader reports 256 KiB and yields 1 MiB: S3 receives a
			// Content-Length of 256 KiB, so a complete request would commit
			// that prefix over the original object.
			body := &understatedReader{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 4*reported)), len: reported}
			_, err := be.Put(ctx, "uploads", "doc", body, nil)
			if errors.GetCode(err) != CodeStorageWrite {
				t.Fatalf("err = %v, want code %q", err, CodeStorageWrite)
			}
			if got, _ := readObject(t, s3, "uploads", "doc"); got != "original" {
				t.Errorf("object after rejected Put has %d bytes, want the original", len(got))
			}
		})
	}
}

// growsAfterEOFReader returns io.EOF once at the end of its first part, then
// yields its second part, like a file appended to after a reader hit its end.
type growsAfterEOFReader struct {
	first, later io.Reader
	sawEOF       bool
	len          int
}

func (g *growsAfterEOFReader) Len() int { return g.len }

func (g *growsAfterEOFReader) Read(p []byte) (int, error) {
	if !g.sawEOF {
		n, err := g.first.Read(p)
		if err == io.EOF {
			g.sawEOF = true
		}
		return n, err
	}
	return g.later.Read(p)
}

func TestConstrainedBackend_KnownLengthEndIsSticky(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "constraints", "constrained-backend-known-length-end-is-sticky")
	reg := newTestRegistry(&BucketDefinition{Name: "uploads", Options: BucketOptions{MaxFileSize: 64}})
	s3 := NewS3Backend(S3Config{Endpoint: fakeS3Objects(t).URL})
	be := &ConstrainedBackend{Backend: s3, registry: reg}
	ctx := context.Background()

	// Go's HTTP/1.1 transport reads once more after Content-Length bytes. The
	// source has ended by then, so the growth must not turn the committed
	// upload into a reported failure.
	body := &growsAfterEOFReader{first: strings.NewReader("abcd"), later: strings.NewReader("efgh"), len: 4}
	if _, err := be.Put(ctx, "uploads", "doc", body, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, _ := readObject(t, s3, "uploads", "doc"); got != "abcd" {
		t.Errorf("object = %q, want %q", got, "abcd")
	}
}
