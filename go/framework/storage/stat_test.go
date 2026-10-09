package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

// assertStatNotFound fails unless err carries CodeStorageNotFound.
func assertStatNotFound(t *testing.T, info *ObjectInfo, err error) {
	t.Helper()
	if info != nil {
		t.Errorf("Stat returned %+v for a missing key, want nil", info)
	}
	if !errors.Is(err, CodeStorageNotFound) {
		t.Errorf("Stat error code = %q (%v), want %q", errors.GetCode(err), err, CodeStorageNotFound)
	}
}

func TestMemoryBackendStat(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "memory-stat-reads-one-object")
	ctx := context.Background()
	b := NewMemoryBackend()
	before := time.Now()
	if _, err := b.Put(ctx, "bucket", "a/b.txt", strings.NewReader("hello"), &ObjectMetadata{ContentType: "text/plain"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	info, err := Stat(ctx, b, "bucket", "a/b.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != "a/b.txt" || info.Size != 5 || info.ContentType != "text/plain" || info.ETag == "" {
		t.Errorf("Stat = %+v", info)
	}
	if info.LastModified.Before(before) {
		t.Errorf("LastModified = %v, want at or after %v", info.LastModified, before)
	}

	info, err = b.Stat(ctx, "bucket", "a/missing.txt")
	assertStatNotFound(t, info, err)
	info, err = b.Stat(ctx, "no-such-bucket", "a/b.txt")
	assertStatNotFound(t, info, err)
}

func TestFileBackendStat(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "filesystem-stat-reads-one-object")
	ctx := context.Background()
	b := NewFileBackend(t.TempDir())
	if _, err := b.Put(ctx, "bucket", "dir/a.txt", strings.NewReader("hello"), &ObjectMetadata{ContentType: "text/plain"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	info, err := Stat(ctx, b, "bucket", "dir/a.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != "dir/a.txt" || info.Size != 5 || info.ContentType != "text/plain" || info.ETag == "" || info.LastModified.IsZero() {
		t.Errorf("Stat = %+v", info)
	}

	// A missing key, a directory and the metadata file hold no object.
	for _, key := range []string{"dir/missing.txt", "dir", "dir/a.txt" + fileMetaSuffix} {
		info, err := b.Stat(ctx, "bucket", key)
		assertStatNotFound(t, info, err)
	}
}

func TestS3BackendStat(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "s3-stat-reads-one-object")
	modified := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var (
		mu      sync.Mutex
		methods []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		switch r.URL.Path {
		case "/bucket/a.txt":
			w.Header().Set("Content-Length", "5")
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("ETag", `"abc"`)
			w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		case "/bucket/denied.txt":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	b := NewS3Backend(S3Config{Endpoint: server.URL})
	ctx := context.Background()

	info, err := Stat(ctx, b, "bucket", "a.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	want := ObjectInfo{Key: "a.txt", Size: 5, ContentType: "text/plain", ETag: `"abc"`, LastModified: modified}
	if *info != want {
		t.Errorf("Stat = %+v, want %+v", *info, want)
	}

	info, err = b.Stat(ctx, "bucket", "missing.txt")
	assertStatNotFound(t, info, err)

	_, err = b.Stat(ctx, "bucket", "denied.txt")
	if !errors.Is(err, CodeStorageRead) {
		t.Errorf("Stat(403) error code = %q, want %q", errors.GetCode(err), CodeStorageRead)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range methods {
		if m != http.MethodHead {
			t.Errorf("Stat sent %s, want HEAD only", m)
		}
	}
}

func TestGCSBackendStat(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "gcs-stat-reads-metadata-without-listing")
	f := newFakeGCS(t)
	b := f.backend(t)
	f.put("dir/a.txt", []byte("hello"))
	ctx := context.Background()

	info, err := Stat(ctx, b, "bucket", "dir/a.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != "dir/a.txt" || info.Size != 5 || info.ContentType != "application/octet-stream" ||
		info.ETag != "etag-dir/a.txt" || info.LastModified.IsZero() {
		t.Errorf("Stat = %+v", info)
	}

	// One metadata read: a GET on the object resource, without media and
	// without a list of the bucket.
	served := f.served()
	if len(served) != 1 {
		t.Fatalf("Stat sent %d requests %v, want 1", len(served), served)
	}
	if !strings.HasPrefix(served[0], "GET ") || !strings.Contains(served[0], "/o/dir%2Fa.txt?") ||
		strings.Contains(served[0], "alt=media") {
		t.Errorf("Stat request = %q, want one object metadata GET", served[0])
	}

	info, err = b.Stat(ctx, "bucket", "dir/missing.txt")
	assertStatNotFound(t, info, err)

	f.setFail()
	_, err = b.Stat(ctx, "bucket", "dir/a.txt")
	if !errors.Is(err, CodeStorageRead) {
		t.Errorf("Stat(403) error code = %q, want %q", errors.GetCode(err), CodeStorageRead)
	}
}

func TestStatForwardsThroughBindingAndConstraints(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "binding-and-constrained-stat-forward")
	ctx := context.Background()
	mem := NewMemoryBackend()
	if _, err := mem.Put(ctx, "provider-bucket", "tenant/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	b := NewConstrainedBackend(boundTo(map[string]boundTarget{
		"uploads": {backend: mem, bucket: "provider-bucket", prefix: "tenant/"},
	}, mem))

	info, err := Stat(ctx, b, "uploads", "a.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Key != "a.txt" || info.Size != 5 {
		t.Errorf("Stat = %+v, want the logical key a.txt and size 5", info)
	}

	info, err = Stat(ctx, b, "uploads", "missing.txt")
	assertStatNotFound(t, info, err)

	_, err = Stat(ctx, b, "unbound", "a.txt")
	if !errors.Is(err, CodeStorageUnbound) {
		t.Errorf("Stat(unbound) error code = %q, want %q", errors.GetCode(err), CodeStorageUnbound)
	}
}

func TestStatUnsupportedBackend(t *testing.T) {
	ctx := context.Background()
	// fakeBackend implements Backend but not Stater.
	fake := newFakeBackend()
	for name, b := range map[string]Backend{
		"direct":      fake,
		"constrained": NewConstrainedBackend(fake),
		"binding":     boundTo(map[string]boundTarget{"uploads": {backend: fake, bucket: "p"}}, fake),
	} {
		_, err := Stat(ctx, b, "uploads", "a.txt")
		if !errors.Is(err, CodeStorageUnsupported) {
			t.Errorf("%s: Stat error code = %q, want %q", name, errors.GetCode(err), CodeStorageUnsupported)
		}
	}
	if fake.callCount() != 0 {
		t.Errorf("an unsupported Stat reached the backend %d times", fake.callCount())
	}
}
