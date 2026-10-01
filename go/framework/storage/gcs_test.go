package storage

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

func TestGCSBackendCompileCheck(t *testing.T) {
	cfg := GCSConfig{
		ProjectID:       "test-project",
		CredentialsFile: "/tmp/creds.json",
	}
	// Verify config field access compiles. No real GCS call.
	if cfg.ProjectID != "test-project" {
		t.Error("unexpected ProjectID")
	}
	// Interface satisfaction is checked by var _ Backend = (*GCSBackend)(nil) in gcs.go.
}

// --- Signing tests (URLSigner): the security-relevant contract. ---

// newSigningBackend builds a GCSBackend with a generated RSA service-account
// key, so V4 SignedURL works entirely offline (no network).
func newSigningBackend(t *testing.T) *GCSBackend {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return &GCSBackend{
		endpoint:  gcsDefaultEndpoint,
		signEmail: "test@test.iam.gserviceaccount.com",
		signKey:   key,
	}
}

func TestGCSBackend_SignedGetURL(t *testing.T) {
	b := newSigningBackend(t)

	got, err := b.SignedGetURL(context.Background(), "mybucket", "path/to/key.txt", SignedURLOptions{Expiry: time.Hour})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}

	u, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("parse url: %v", perr)
	}
	if u.Host != "storage.googleapis.com" {
		t.Errorf("host = %q, want storage.googleapis.com", u.Host)
	}
	if u.Path != "/mybucket/path/to/key.txt" {
		t.Errorf("path = %q, want /mybucket/path/to/key.txt", u.Path)
	}
	q := u.Query()
	if q.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		t.Errorf("X-Goog-Algorithm = %q, want GOOG4-RSA-SHA256", q.Get("X-Goog-Algorithm"))
	}
	if q.Get("X-Goog-Signature") == "" {
		t.Error("expected non-empty X-Goog-Signature")
	}
	assertExpiresAbout(t, q.Get("X-Goog-Expires"), 3600)
	if !strings.Contains(q.Get("X-Goog-Credential"), "test@test.iam.gserviceaccount.com") {
		t.Errorf("X-Goog-Credential = %q, want it to contain the service account email", q.Get("X-Goog-Credential"))
	}
}

func TestGCSBackend_SignedGetURL_DefaultExpiry(t *testing.T) {
	b := newSigningBackend(t)

	// Expiry left zero -> backend defaults to 1h (3600s).
	got, err := b.SignedGetURL(context.Background(), "bucket", "key", SignedURLOptions{})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, _ := url.Parse(got)
	assertExpiresAbout(t, u.Query().Get("X-Goog-Expires"), 3600)
}

func TestGCSBackend_SignedPutURL(t *testing.T) {
	b := newSigningBackend(t)

	got, err := b.SignedPutURL(context.Background(), "mybucket", "upload.bin", SignedURLOptions{
		Expiry:      30 * time.Minute,
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("SignedPutURL: %v", err)
	}

	u, perr := url.Parse(got)
	if perr != nil {
		t.Fatalf("parse url: %v", perr)
	}
	if u.Host != "storage.googleapis.com" {
		t.Errorf("host = %q, want storage.googleapis.com", u.Host)
	}
	if u.Path != "/mybucket/upload.bin" {
		t.Errorf("path = %q, want /mybucket/upload.bin", u.Path)
	}
	q := u.Query()
	if q.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		t.Errorf("X-Goog-Algorithm = %q, want GOOG4-RSA-SHA256", q.Get("X-Goog-Algorithm"))
	}
	if q.Get("X-Goog-Signature") == "" {
		t.Error("expected non-empty X-Goog-Signature")
	}
	assertExpiresAbout(t, q.Get("X-Goog-Expires"), 1800)
	// ContentType must be a signed header for PUT URLs.
	if !strings.Contains(q.Get("X-Goog-SignedHeaders"), "content-type") {
		t.Errorf("X-Goog-SignedHeaders = %q, want it to include content-type", q.Get("X-Goog-SignedHeaders"))
	}
}

func TestGCSBackend_SignedPutURL_DefaultExpiry(t *testing.T) {
	b := newSigningBackend(t)
	got, err := b.SignedPutURL(context.Background(), "bucket", "key", SignedURLOptions{})
	if err != nil {
		t.Fatalf("SignedPutURL: %v", err)
	}
	u, _ := url.Parse(got)
	assertExpiresAbout(t, u.Query().Get("X-Goog-Expires"), 3600)
}

// assertExpiresAbout checks the signed X-Goog-Expires (in seconds) is within a
// couple of seconds of want. The V4 signer derives the value from (Expires-now)
// at signing time, so a sub-second delay can shave one second off the nominal value.
func assertExpiresAbout(t *testing.T, got string, want int) {
	t.Helper()
	n, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("X-Goog-Expires = %q, not an integer: %v", got, err)
	}
	if n > want || n < want-2 {
		t.Errorf("X-Goog-Expires = %d, want ~%d", n, want)
	}
}

// Error path: a backend with neither a signing key nor a token source has no
// credentials to sign with, so the V4 signer must return an error.
func TestGCSBackend_SignedGetURL_NoCredentials(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "signed-access", "gcs-signed-get-url-requires-credentials")
	b := &GCSBackend{endpoint: gcsDefaultEndpoint}

	if _, err := b.SignedGetURL(context.Background(), "bucket", "key", SignedURLOptions{Expiry: time.Hour}); err == nil {
		t.Fatal("expected error signing without credentials")
	}
}

func TestGCSBackend_SignedPutURL_NoCredentials(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "signed-access", "gcs-signed-put-url-requires-credentials")
	b := &GCSBackend{endpoint: gcsDefaultEndpoint}

	if _, err := b.SignedPutURL(context.Background(), "bucket", "key", SignedURLOptions{}); err == nil {
		t.Fatal("expected error signing without credentials")
	}
}

// --- Data-plane tests against the in-memory fake GCS JSON API. ---

func TestGCSBackend_Put(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)

	res, err := b.Put(context.Background(), "bucket", "dir/key.txt", strings.NewReader("hello world"),
		&ObjectMetadata{ContentType: "text/plain", Custom: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Key != "dir/key.txt" {
		t.Errorf("Key = %q, want dir/key.txt", res.Key)
	}
	if res.Size != 11 {
		t.Errorf("Size = %d, want 11", res.Size)
	}
	if res.ETag == "" {
		t.Error("expected non-empty ETag")
	}

	// Stored bytes and metadata are observable.
	f.mu.Lock()
	obj := f.objects["bucket/dir/key.txt"]
	f.mu.Unlock()
	if obj == nil {
		t.Fatal("object not stored in fake")
	}
	if string(obj.Data) != "hello world" {
		t.Errorf("stored data = %q, want hello world", string(obj.Data))
	}
	if obj.ContentType != "text/plain" {
		t.Errorf("stored content-type = %q, want text/plain", obj.ContentType)
	}
	if obj.Metadata["k"] != "v" {
		t.Errorf("stored metadata[k] = %q, want v", obj.Metadata["k"])
	}
}

func TestGCSBackend_Put_NilMetadata(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)

	res, err := b.Put(context.Background(), "bucket", "key", strings.NewReader("data"), nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Size != 4 {
		t.Errorf("Size = %d, want 4", res.Size)
	}
}

func TestGCSBackend_Get_Hit(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("file.txt", []byte("file content"))
	f.mu.Lock()
	f.objects["bucket/file.txt"].ContentType = "text/plain"
	f.objects["bucket/file.txt"].Metadata = map[string]string{"owner": "alice"}
	f.mu.Unlock()

	res, err := b.Get(context.Background(), "bucket", "file.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil result on hit")
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	if string(body) != "file content" {
		t.Errorf("Body = %q, want file content", string(body))
	}
	if res.Size != int64(len("file content")) {
		t.Errorf("Size = %d, want %d", res.Size, len("file content"))
	}
	if res.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want text/plain", res.ContentType)
	}
	if res.ETag == "" {
		t.Error("expected non-empty ETag")
	}
	if res.Metadata["owner"] != "alice" {
		t.Errorf("Metadata[owner] = %q, want alice", res.Metadata["owner"])
	}
}

func TestGCSBackend_Get_MetadataUsesRequestTimeout(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "deadlines-and-streams", "gcs-get-metadata-honors-request-timeout")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"slow","size":"1"}`)
	}))
	defer srv.Close()

	b := &GCSBackend{
		httpClient:     newStorageHTTPClient(),
		endpoint:       srv.URL,
		tokens:         staticTokenSource(""),
		requestTimeout: 50 * time.Millisecond,
	}

	started := time.Now()
	_, err := b.Get(context.Background(), "bucket", "slow")
	if err == nil {
		t.Fatal("expected Get to fail when the metadata request exceeds RequestTimeout")
	}
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("metadata request was not bounded by RequestTimeout; elapsed %s", elapsed)
	}
}

func TestGCSBackend_Get_Miss(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "gcs-missing-get-returns-no-object")
	f := newFakeGCS(t)
	b := f.backend(t)

	res, err := b.Get(context.Background(), "bucket", "missing.txt")
	if err != nil {
		t.Fatalf("Get miss should not error, got: %v", err)
	}
	if res != nil {
		t.Errorf("expected nil result on miss, got %+v", res)
	}
}

// Get where metadata (Attrs) succeeds but opening the reader fails, covering
// the NewReader error branch.
func TestGCSBackend_Get_ReaderError(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("file.txt", []byte("data"))
	f.setFailDownload(true)

	_, err := b.Get(context.Background(), "bucket", "file.txt")
	if err == nil {
		t.Fatal("expected error when reader cannot be opened")
	}
	if !errors.Is(err, CodeStorageRead) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageRead)
	}
}

func TestGCSBackend_Delete(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("key.txt", []byte("x"))

	if err := b.Delete(context.Background(), "bucket", "key.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.mu.Lock()
	_, exists := f.objects["bucket/key.txt"]
	f.mu.Unlock()
	if exists {
		t.Error("object should have been deleted")
	}
}

func TestGCSBackend_Delete_Missing(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)

	// Deleting a non-existent object is a no-op (nil error).
	if err := b.Delete(context.Background(), "bucket", "missing.txt"); err != nil {
		t.Fatalf("Delete of missing should be no-op, got: %v", err)
	}
}

func TestGCSBackend_Exists(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("present.txt", []byte("x"))

	ok, err := b.Exists(context.Background(), "bucket", "present.txt")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("expected exists=true")
	}

	ok, err = b.Exists(context.Background(), "bucket", "absent.txt")
	if err != nil {
		t.Fatalf("Exists(absent): %v", err)
	}
	if ok {
		t.Error("expected exists=false for absent object")
	}
}

func TestGCSBackend_List_Prefix(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("docs/a.txt", []byte("a"))
	f.put("docs/b.txt", []byte("bb"))
	f.put("images/c.png", []byte("ccc"))

	res, err := b.List(context.Background(), "bucket", &ListOptions{Prefix: "docs/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Objects) != 2 {
		t.Fatalf("expected 2 objects under docs/, got %d (%+v)", len(res.Objects), res.Objects)
	}
	for _, o := range res.Objects {
		if !strings.HasPrefix(o.Key, "docs/") {
			t.Errorf("unexpected key %q outside prefix", o.Key)
		}
		if o.ETag == "" {
			t.Errorf("object %q missing ETag", o.Key)
		}
	}
}

func TestGCSBackend_List_Delimiter(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("root.txt", []byte("r"))
	f.put("docs/a.txt", []byte("a"))
	f.put("images/c.png", []byte("c"))

	res, err := b.List(context.Background(), "bucket", &ListOptions{Delimiter: "/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Top-level object only.
	if len(res.Objects) != 1 || res.Objects[0].Key != "root.txt" {
		t.Errorf("expected only root.txt at top level, got %+v", res.Objects)
	}
	// Two common prefixes.
	gotPrefixes := map[string]bool{}
	for _, p := range res.Prefixes {
		gotPrefixes[p] = true
	}
	if !gotPrefixes["docs/"] || !gotPrefixes["images/"] {
		t.Errorf("expected prefixes docs/ and images/, got %v", res.Prefixes)
	}
}

func TestGCSBackend_List_Empty(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)

	res, err := b.List(context.Background(), "bucket", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Objects) != 0 {
		t.Errorf("expected no objects, got %d", len(res.Objects))
	}
}

func TestGCSBackend_Copy(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("source.txt", []byte("source content"))

	if err := b.Copy(context.Background(), "bucket", "source.txt", "dest.txt"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	f.mu.Lock()
	dst, ok := f.objects["bucket/dest.txt"]
	f.mu.Unlock()
	if !ok {
		t.Fatal("destination not created")
	}
	if string(dst.Data) != "source content" {
		t.Errorf("copied data = %q, want source content", string(dst.Data))
	}
}

func TestGCSBackend_Copy_SourceNotFound(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)

	err := b.Copy(context.Background(), "bucket", "missing.txt", "dest.txt")
	if err == nil {
		t.Fatal("expected error when source not found")
	}
	if !errors.Is(err, CodeStorageNotFound) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageNotFound)
	}
}

func TestGCSBackend_Close(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestNewGCSBackend_CredentialsFileMissing(t *testing.T) {
	// A non-existent credentials file makes client construction fail, exercising
	// the error-wrapping branch in NewGCSBackend.
	_, err := NewGCSBackend(t.Context(), GCSConfig{CredentialsFile: "/nonexistent/creds.json"})
	if err == nil {
		t.Fatal("expected error for missing credentials file")
	}
	if !errors.Is(err, CodeStorageRequest) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageRequest)
	}
}

// Put with all optional metadata fields set, covering the CacheControl and
// ContentDisposition branches.
func TestGCSBackend_Put_FullMetadata(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)

	_, err := b.Put(context.Background(), "bucket", "key", strings.NewReader("body"), &ObjectMetadata{
		ContentType:        "text/plain",
		CacheControl:       "max-age=60",
		ContentDisposition: "attachment; filename=key",
		Custom:             map[string]string{"a": "1"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	f.mu.Lock()
	obj := f.objects["bucket/key"]
	f.mu.Unlock()
	if obj == nil || string(obj.Data) != "body" {
		t.Fatalf("object not stored correctly: %+v", obj)
	}
}

// List with MaxKeys forces client-side truncation and a continuation token, then
// a follow-up call resumes past that token (covering the pagination branches).
func TestGCSBackend_List_Pagination(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("a.txt", []byte("1"))
	f.put("b.txt", []byte("2"))
	f.put("c.txt", []byte("3"))

	first, err := b.List(context.Background(), "bucket", &ListOptions{MaxKeys: 2})
	if err != nil {
		t.Fatalf("List page 1: %v", err)
	}
	if len(first.Objects) != 2 {
		t.Fatalf("page 1: expected 2 objects, got %d", len(first.Objects))
	}
	if !first.IsTruncated {
		t.Error("page 1: expected IsTruncated=true")
	}
	if first.ContinuationToken == "" {
		t.Fatal("page 1: expected a continuation token")
	}

	second, err := b.List(context.Background(), "bucket", &ListOptions{ContinuationToken: first.ContinuationToken})
	if err != nil {
		t.Fatalf("List page 2: %v", err)
	}
	// Everything after the token; the first page's keys must not reappear.
	for _, o := range second.Objects {
		if o.Key <= first.ContinuationToken {
			t.Errorf("page 2 contains key %q at/under token %q", o.Key, first.ContinuationToken)
		}
	}
	if len(second.Objects) != 1 {
		t.Errorf("page 2: expected 1 remaining object, got %d (%+v)", len(second.Objects), second.Objects)
	}
}

// --- Error-path tests: a failing server exercises the error-wrapping branches. ---

func TestGCSBackend_Get_Error(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.setFail()

	_, err := b.Get(context.Background(), "bucket", "key")
	if err == nil {
		t.Fatal("expected error from failing server")
	}
	if !errors.Is(err, CodeStorageRead) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageRead)
	}
}

func TestGCSBackend_Delete_Error(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.setFail()

	err := b.Delete(context.Background(), "bucket", "key")
	if err == nil {
		t.Fatal("expected error from failing server")
	}
	if !errors.Is(err, CodeStorageDelete) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageDelete)
	}
}

func TestGCSBackend_Exists_Error(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.setFail()

	_, err := b.Exists(context.Background(), "bucket", "key")
	if err == nil {
		t.Fatal("expected error from failing server")
	}
	if !errors.Is(err, CodeStorageRead) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageRead)
	}
}

func TestGCSBackend_List_Error(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.setFail()

	_, err := b.List(context.Background(), "bucket", nil)
	if err == nil {
		t.Fatal("expected error from failing server")
	}
	if !errors.Is(err, CodeStorageList) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageList)
	}
}

func TestGCSBackend_Copy_Error(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.setFail()

	err := b.Copy(context.Background(), "bucket", "src", "dst")
	if err == nil {
		t.Fatal("expected error from failing server")
	}
	if !errors.Is(err, CodeStorageCopy) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageCopy)
	}
}

func TestGCSBackend_Put_Error(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)
	f.setFail()

	_, err := b.Put(context.Background(), "bucket", "key", strings.NewReader("data"), nil)
	if err == nil {
		t.Fatal("expected error from failing server")
	}
}

// errReader always fails, exercising Put's io.Copy error branch.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestGCSBackend_Put_CopyError(t *testing.T) {
	f := newFakeGCS(t)
	b := f.backend(t)

	_, err := b.Put(context.Background(), "bucket", "key", errReader{}, nil)
	if err == nil {
		t.Fatal("expected error when the data reader fails")
	}
	if !errors.Is(err, CodeStorageWrite) {
		t.Errorf("error code = %q, want %q", errors.GetCode(err), CodeStorageWrite)
	}
}
