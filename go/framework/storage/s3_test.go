package storage

import (
	"context"
	"encoding/xml"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

func TestS3Config_withDefaults(t *testing.T) {
	t.Run("empty config gets default region", func(t *testing.T) {
		cfg := S3Config{}.withDefaults()
		if cfg.Region != "us-east-1" {
			t.Errorf("Region = %q, want %q", cfg.Region, "us-east-1")
		}
	})
	t.Run("custom region is preserved", func(t *testing.T) {
		cfg := S3Config{Region: "eu-west-1"}.withDefaults()
		if cfg.Region != "eu-west-1" {
			t.Errorf("Region = %q, want %q", cfg.Region, "eu-west-1")
		}
	})
}

func TestS3Backend_objectURL(t *testing.T) {
	backend := NewS3Backend(S3Config{Endpoint: "https://s3.example.com"})
	got := backend.objectURL("mybucket", "path/to/file.txt")
	want := "https://s3.example.com/mybucket/path/to/file.txt"
	if got != want {
		t.Errorf("objectURL = %q, want %q", got, want)
	}
}

func TestS3Backend_objectURL_TrailingSlash(t *testing.T) {
	backend := NewS3Backend(S3Config{Endpoint: "https://s3.example.com/"})
	got := backend.objectURL("mybucket", "file.txt")
	want := "https://s3.example.com/mybucket/file.txt"
	if got != want {
		t.Errorf("objectURL = %q, want %q", got, want)
	}
}

// The configured RequestTimeout bounds control-plane operations but must
// not abort a streaming Get — its body read is governed only by the caller's
// context, so large/slow downloads are not truncated mid-stream.
func TestS3Backend_RequestTimeoutBoundsControlPlaneNotGet(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "s3-request-timeout-bounds-control-plane-not-get")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Headers immediately, then a body streamed slower than RequestTimeout.
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			for range 3 {
				_, _ = w.Write([]byte("chunk"))
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(40 * time.Millisecond)
			}
		case http.MethodHead:
			time.Sleep(200 * time.Millisecond) // exceed RequestTimeout before responding
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL, RequestTimeout: 50 * time.Millisecond})

	// Streaming Get must complete despite the body taking ~120ms (> 50ms cap).
	res, err := backend.Get(context.Background(), "bucket", "big.bin")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	data, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil {
		t.Fatalf("reading streamed body: %v", err)
	}
	if string(data) != "chunkchunkchunk" {
		t.Errorf("streamed body = %q, want full content (Get must not be capped by RequestTimeout)", data)
	}

	// Exists is control-plane: a response slower than RequestTimeout must fail.
	if _, err := backend.Exists(context.Background(), "bucket", "key"); err == nil {
		t.Error("expected Exists to fail under the RequestTimeout cap")
	}
}

func TestS3Backend_Put(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello world" {
			t.Errorf("expected body %q, got %q", "hello world", string(body))
		}
		if r.Header.Get("Content-Type") != "text/plain" {
			t.Errorf("expected Content-Type text/plain, got %q", r.Header.Get("Content-Type"))
		}
		w.Header().Set("ETag", `"etag123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	result, err := backend.Put(context.Background(), "bucket", "key.txt", strings.NewReader("hello world"), &ObjectMetadata{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Key != "key.txt" {
		t.Errorf("Key = %q, want %q", result.Key, "key.txt")
	}
	if result.Size != 11 {
		t.Errorf("Size = %d, want 11", result.Size)
	}
	if result.ETag != `"etag123"` {
		t.Errorf("ETag = %q, want %q", result.ETag, `"etag123"`)
	}
}

// Unsigned Put can stream an unsized body straight to the wire instead of
// buffering it with io.ReadAll. A buffered body would be sent as a sized
// *bytes.Reader (ContentLength set, no chunked encoding); an unsized streamed
// body is sent with generic HTTP chunked transfer.
func TestS3Backend_Put_StreamsBody(t *testing.T) {
	var (
		gotTransferEncoding []string
		gotContentLength    int64
		gotPayloadHash      string
		gotBody             string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTransferEncoding = r.TransferEncoding
		gotContentLength = r.ContentLength
		gotPayloadHash = r.Header.Get("X-Amz-Content-Sha256")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})

	// An io.Reader that is NOT a *bytes.Reader/*strings.Reader, so net/http
	// cannot infer a Content-Length and must stream chunked — unless Put
	// buffers it first (the bug this guards against).
	src := io.NopCloser(strings.NewReader("streamed payload"))
	result, err := backend.Put(context.Background(), "bucket", "key.bin", src, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody != "streamed payload" {
		t.Errorf("server body = %q, want %q", gotBody, "streamed payload")
	}
	// Chunked transfer encoding (ContentLength == -1) proves the body was not
	// buffered into a sized reader before sending.
	if gotContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1 (chunked streaming, not a buffered body)", gotContentLength)
	}
	if len(gotTransferEncoding) != 1 || gotTransferEncoding[0] != "chunked" {
		t.Errorf("TransferEncoding = %v, want [chunked]", gotTransferEncoding)
	}
	if gotPayloadHash != "" {
		t.Errorf("X-Amz-Content-Sha256 = %q, want empty unsigned request header", gotPayloadHash)
	}
	if result.Size != int64(len("streamed payload")) {
		t.Errorf("Size = %d, want %d", result.Size, len("streamed payload"))
	}
}

func TestS3Backend_Put_SignedKnownLengthUsesSingleUnsignedPayload(t *testing.T) {
	var (
		gotTransferEncoding []string
		gotContentLength    int64
		gotPayloadHash      string
		gotAuthorization    string
		gotBody             string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTransferEncoding = r.TransferEncoding
		gotContentLength = r.ContentLength
		gotPayloadHash = r.Header.Get("X-Amz-Content-Sha256")
		gotAuthorization = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{
		Endpoint:  server.URL,
		Region:    "us-east-1",
		AccessKey: "AKID",
		SecretKey: "SECRET",
	})

	result, err := backend.Put(context.Background(), "bucket", "key.bin", strings.NewReader("signed payload"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody != "signed payload" {
		t.Errorf("server body = %q, want %q", gotBody, "signed payload")
	}
	if gotContentLength != int64(len("signed payload")) {
		t.Errorf("ContentLength = %d, want %d", gotContentLength, len("signed payload"))
	}
	if len(gotTransferEncoding) != 0 {
		t.Errorf("TransferEncoding = %v, want none for signed single-payload PUT", gotTransferEncoding)
	}
	if gotPayloadHash != unsignedPayload {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", gotPayloadHash, unsignedPayload)
	}
	if !strings.HasPrefix(gotAuthorization, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization = %q, want SigV4 header", gotAuthorization)
	}
	if result.Size != int64(len("signed payload")) {
		t.Errorf("Size = %d, want %d", result.Size, len("signed payload"))
	}
}

func TestS3Backend_Put_SignedUnsizedReaderSpoolsToContentLength(t *testing.T) {
	var (
		gotTransferEncoding []string
		gotContentLength    int64
		gotPayloadHash      string
		gotBody             string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTransferEncoding = r.TransferEncoding
		gotContentLength = r.ContentLength
		gotPayloadHash = r.Header.Get("X-Amz-Content-Sha256")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{
		Endpoint:  server.URL,
		Region:    "us-east-1",
		AccessKey: "AKID",
		SecretKey: "SECRET",
	})

	// io.NopCloser hides strings.Reader's Len method, so Put cannot know the
	// size without a pre-pass. For signed S3 requests it must still avoid the
	// invalid combination of generic HTTP chunked transfer + UNSIGNED-PAYLOAD.
	src := io.NopCloser(strings.NewReader("spooled payload"))
	result, err := backend.Put(context.Background(), "bucket", "key.bin", src, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotBody != "spooled payload" {
		t.Errorf("server body = %q, want %q", gotBody, "spooled payload")
	}
	if gotContentLength != int64(len("spooled payload")) {
		t.Errorf("ContentLength = %d, want %d", gotContentLength, len("spooled payload"))
	}
	if len(gotTransferEncoding) != 0 {
		t.Errorf("TransferEncoding = %v, want none for signed spooled PUT", gotTransferEncoding)
	}
	if gotPayloadHash != unsignedPayload {
		t.Errorf("X-Amz-Content-Sha256 = %q, want %q", gotPayloadHash, unsignedPayload)
	}
	if result.Size != int64(len("spooled payload")) {
		t.Errorf("Size = %d, want %d", result.Size, len("spooled payload"))
	}
}

// Even a large body must not be fully buffered before the first wire byte. The
// trackingReader gates itself: once it has produced a small prefix it yields
// nothing more until the server has actually received a byte. A streaming Put
// already pushed that prefix onto the wire, so the handler drains it and the
// gate opens. A Put that buffers first (io.ReadAll) sends nothing until it has
// drained the reader, so the gate never opens and the read fails.
//
// The discriminator is causal, not volumetric: it asks "did anything reach the
// server before the body was fully read?" rather than measuring how many bytes
// were outstanding. That keeps the verdict independent of kernel socket-buffer
// sizing, loopback auto-tuning, and host speed — a read-ahead volume threshold
// is calibrated against numbers the test does not control and reds at random.
func TestS3Backend_Put_DoesNotBufferWholeBody(t *testing.T) {
	const (
		total     = 16 << 20 // 16 MiB of logical payload (no real allocation of this size)
		gateAfter = 1 << 20  // prefix a streaming Put must have put on the wire
		// Comfortably under the 30s default RequestTimeout, so a stalled body
		// reports itself instead of surfacing as a request-context deadline.
		gateTimeout = 10 * time.Second
	)
	src := newTrackingReader(total, gateAfter, gateTimeout)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain in small chunks; the first chunk to arrive opens the gate.
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			src.consume(n)
			if err != nil {
				break
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	result, err := backend.Put(context.Background(), "bucket", "big.bin", src, nil)
	if src.stalledAtGate() {
		t.Fatalf("Put read %d bytes without the server receiving any: body was buffered, not streamed", gateAfter)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Size != total {
		t.Errorf("Size = %d, want %d", result.Size, total)
	}
}

// errBodyBuffered aborts the upload when the gate times out, so a buffering Put
// fails fast with a diagnosis instead of hanging until the test binary deadline.
var errBodyBuffered = stderrors.New("gate timeout: body fully read before the server received anything")

// trackingReader emits `remaining` zero bytes, withholding everything past the
// first `gateAfter` of them until consume reports that the server received a
// byte. It allocates nothing proportional to the logical size.
type trackingReader struct {
	gateAfter   int64
	gateTimeout time.Duration
	consumed    chan struct{} // closed once the server receives a byte
	openGate    sync.Once

	mu        sync.Mutex
	remaining int
	produced  int64
	stalled   bool
}

func newTrackingReader(total int, gateAfter int64, gateTimeout time.Duration) *trackingReader {
	return &trackingReader{
		gateAfter:   gateAfter,
		gateTimeout: gateTimeout,
		consumed:    make(chan struct{}),
		remaining:   total,
	}
}

func (r *trackingReader) Read(p []byte) (int, error) {
	if err := r.awaitConsumer(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	r.produced += int64(n)
	return n, nil
}

// awaitConsumer holds the reader at the gate once `gateAfter` bytes have been
// produced with nothing received. It never holds the mutex while waiting, so
// consume can still open the gate.
func (r *trackingReader) awaitConsumer() error {
	select {
	case <-r.consumed:
		return nil // gate already open
	default:
	}

	r.mu.Lock()
	belowGate := r.produced < r.gateAfter
	r.mu.Unlock()
	if belowGate {
		return nil
	}

	timer := time.NewTimer(r.gateTimeout)
	defer timer.Stop()
	select {
	case <-r.consumed:
		return nil
	case <-timer.C:
		r.mu.Lock()
		r.stalled = true
		r.mu.Unlock()
		return errBodyBuffered
	}
}

// consume opens the gate on the first byte the server actually receives.
func (r *trackingReader) consume(n int) {
	if n <= 0 {
		return
	}
	r.openGate.Do(func() { close(r.consumed) })
}

func (r *trackingReader) stalledAtGate() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stalled
}

func TestS3Backend_Put_NilMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "" {
			t.Errorf("expected no Content-Type, got %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	_, err := backend.Put(context.Background(), "bucket", "key.txt", strings.NewReader("data"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestS3Backend_Put_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("access denied"))
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	_, err := backend.Put(context.Background(), "bucket", "key.txt", strings.NewReader("data"), nil)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestS3Backend_Get(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"etag456"`)
		_, _ = w.Write([]byte("file content"))
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	result, err := backend.Get(context.Background(), "bucket", "key.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	defer result.Body.Close()

	body, _ := io.ReadAll(result.Body)
	if string(body) != "file content" {
		t.Errorf("Body = %q, want %q", string(body), "file content")
	}
	if result.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", result.ContentType, "text/plain")
	}
	if result.ETag != `"etag456"` {
		t.Errorf("ETag = %q, want %q", result.ETag, `"etag456"`)
	}
}

func TestS3Backend_Get_NotFound(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "s3-missing-get-returns-no-object")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	result, err := backend.Get(context.Background(), "bucket", "missing.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil result for 404")
	}
}

func TestS3Backend_Get_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	_, err := backend.Get(context.Background(), "bucket", "key.txt")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestS3Backend_Delete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	err := backend.Delete(context.Background(), "bucket", "key.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestS3Backend_Delete_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	err := backend.Delete(context.Background(), "bucket", "missing.txt")
	if err != nil {
		t.Fatal("expected no error for 404 on delete (idempotent)")
	}
}

func TestS3Backend_Delete_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("access denied"))
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	err := backend.Delete(context.Background(), "bucket", "key.txt")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestS3Backend_Exists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	exists, err := backend.Exists(context.Background(), "bucket", "key.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected exists=true")
	}
}

func TestS3Backend_Exists_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	exists, err := backend.Exists(context.Background(), "bucket", "missing.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected exists=false for 404")
	}
}

func TestS3Backend_Exists_HTTPError(t *testing.T) {
	// A 403 (missing HeadObject grant) or 5xx must surface as an error, not be
	// masked as "absent" — an existence-gated write on a false negative is a
	// data-integrity risk.
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		backend := NewS3Backend(S3Config{Endpoint: server.URL})
		exists, err := backend.Exists(context.Background(), "bucket", "key.txt")
		server.Close()

		if err == nil {
			t.Errorf("status %d: expected an error, got exists=%v, err=nil", status, exists)
		}
		if exists {
			t.Errorf("status %d: expected exists=false on error", status)
		}
	}
}

func TestS3Backend_Copy(t *testing.T) {
	// Server that handles GET (source) and PUT (destination)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("source content"))
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			if string(body) != "source content" {
				t.Errorf("copied body = %q, want %q", string(body), "source content")
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	err := backend.Copy(context.Background(), "bucket", "source.txt", "dest.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestS3Backend_Copy_SourceNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	err := backend.Copy(context.Background(), "bucket", "missing.txt", "dest.txt")
	if err == nil {
		t.Fatal("expected error when source doesn't exist")
	}
}

func TestS3Backend_Close(t *testing.T) {
	backend := NewS3Backend(S3Config{Endpoint: "https://s3.example.com"})
	err := backend.Close()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestS3Backend_List(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	tests := []struct {
		name       string
		xmlBody    listBucketResult
		opts       *ListOptions
		wantCount  int
		wantPrefix []string
		wantTrunc  bool
		wantToken  string
	}{
		{
			name: "basic list",
			xmlBody: listBucketResult{
				Contents: []listBucketObject{
					{Key: "file1.txt", Size: 100, ETag: `"abc"`, LastModified: now.Format(time.RFC3339)},
					{Key: "file2.txt", Size: 200, ETag: `"def"`, LastModified: now.Format(time.RFC3339)},
				},
			},
			opts:      nil,
			wantCount: 2,
		},
		{
			name: "with prefix filter",
			xmlBody: listBucketResult{
				Contents: []listBucketObject{
					{Key: "docs/readme.md", Size: 50, ETag: `"aaa"`, LastModified: now.Format(time.RFC3339)},
				},
			},
			opts:      &ListOptions{Prefix: "docs/"},
			wantCount: 1,
		},
		{
			name: "with delimiter and common prefixes",
			xmlBody: listBucketResult{
				Contents: []listBucketObject{
					{Key: "root.txt", Size: 10, ETag: `"bbb"`, LastModified: now.Format(time.RFC3339)},
				},
				CommonPrefixes: []listCommonPrefix{
					{Prefix: "docs/"},
					{Prefix: "images/"},
				},
			},
			opts:       &ListOptions{Delimiter: "/"},
			wantCount:  1,
			wantPrefix: []string{"docs/", "images/"},
		},
		{
			name: "truncated with continuation token",
			xmlBody: listBucketResult{
				IsTruncated: true,
				Contents: []listBucketObject{
					{Key: "a.txt", Size: 1, ETag: `"111"`, LastModified: now.Format(time.RFC3339)},
				},
				NextContinuationToken: "token-abc",
			},
			opts:      &ListOptions{MaxKeys: 1},
			wantCount: 1,
			wantTrunc: true,
			wantToken: "token-abc",
		},
		{
			name:      "empty result",
			xmlBody:   listBucketResult{},
			opts:      nil,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("expected GET, got %s", r.Method)
				}
				if r.URL.Query().Get("list-type") != "2" {
					t.Error("expected list-type=2 query param")
				}
				if tt.opts != nil {
					if tt.opts.Prefix != "" && r.URL.Query().Get("prefix") != tt.opts.Prefix {
						t.Errorf("expected prefix=%s, got %s", tt.opts.Prefix, r.URL.Query().Get("prefix"))
					}
					if tt.opts.Delimiter != "" && r.URL.Query().Get("delimiter") != tt.opts.Delimiter {
						t.Errorf("expected delimiter=%s, got %s", tt.opts.Delimiter, r.URL.Query().Get("delimiter"))
					}
				}

				w.Header().Set("Content-Type", "application/xml")
				body, _ := xml.Marshal(tt.xmlBody)
				_, _ = w.Write(body)
			}))
			defer server.Close()

			backend := NewS3Backend(S3Config{Endpoint: server.URL})
			result, err := backend.List(context.Background(), "test-bucket", tt.opts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(result.Objects) != tt.wantCount {
				t.Errorf("expected %d objects, got %d", tt.wantCount, len(result.Objects))
			}
			if result.IsTruncated != tt.wantTrunc {
				t.Errorf("expected IsTruncated=%v, got %v", tt.wantTrunc, result.IsTruncated)
			}
			if result.ContinuationToken != tt.wantToken {
				t.Errorf("expected ContinuationToken=%q, got %q", tt.wantToken, result.ContinuationToken)
			}
			if len(result.Prefixes) != len(tt.wantPrefix) {
				t.Errorf("expected %d prefixes, got %d", len(tt.wantPrefix), len(result.Prefixes))
			}
			for i, p := range tt.wantPrefix {
				if i < len(result.Prefixes) && result.Prefixes[i] != p {
					t.Errorf("expected prefix[%d]=%q, got %q", i, p, result.Prefixes[i])
				}
			}
		})
	}
}

func TestS3Backend_List_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("access denied"))
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	_, err := backend.List(context.Background(), "test-bucket", nil)
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestS3Backend_List_InvalidXML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not xml"))
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	_, err := backend.List(context.Background(), "test-bucket", nil)
	if err == nil {
		t.Fatal("expected error for invalid XML response")
	}
}

func TestS3Backend_List_ObjectOrdering(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		body, _ := xml.Marshal(listBucketResult{
			Contents: []listBucketObject{
				{Key: "c.txt", Size: 3, ETag: `"ccc"`, LastModified: now.Format(time.RFC3339)},
				{Key: "a.txt", Size: 1, ETag: `"aaa"`, LastModified: now.Format(time.RFC3339)},
				{Key: "b.txt", Size: 2, ETag: `"bbb"`, LastModified: now.Format(time.RFC3339)},
			},
		})
		_, _ = w.Write(body)
	}))
	defer server.Close()

	backend := NewS3Backend(S3Config{Endpoint: server.URL})
	result, err := backend.List(context.Background(), "bucket", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Objects) != 3 {
		t.Fatalf("expected 3 objects, got %d", len(result.Objects))
	}
	if result.Objects[0].Key != "a.txt" || result.Objects[1].Key != "b.txt" || result.Objects[2].Key != "c.txt" {
		t.Errorf("objects not sorted: %v", result.Objects)
	}
}
