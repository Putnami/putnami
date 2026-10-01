package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestBucketRegistration(t *testing.T) {
	reg := &Registry{buckets: make(map[string]*BucketDefinition)}
	b := &BucketDefinition{
		Name: "avatars",
		Options: BucketOptions{
			MaxFileSize:      10 * 1024 * 1024,
			AllowedMimeTypes: []string{"image/png", "image/jpeg"},
			Public:           true,
		},
	}
	reg.Register(b)

	got, ok := reg.Get("avatars")
	if !ok {
		t.Fatal("expected bucket to be registered")
	}
	if got.Name != "avatars" {
		t.Errorf("expected name 'avatars', got %q", got.Name)
	}
	if got.Options.MaxFileSize != 10*1024*1024 {
		t.Errorf("expected MaxFileSize 10MB, got %d", got.Options.MaxFileSize)
	}
	if !got.Options.Public {
		t.Error("expected Public to be true")
	}

	all := reg.All()
	if len(all) != 1 {
		t.Errorf("expected 1 bucket, got %d", len(all))
	}

	reg.Clear()
	all = reg.All()
	if len(all) != 0 {
		t.Errorf("expected 0 buckets after clear, got %d", len(all))
	}
}

func TestMemoryBackendPutGet(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "memory-backend-round-trip")
	ctx := context.Background()
	backend := NewMemoryBackend()

	data := []byte("hello world")
	meta := &ObjectMetadata{ContentType: "text/plain", Custom: map[string]string{"author": "test"}}

	result, err := backend.Put(ctx, "docs", "readme.txt", bytes.NewReader(data), meta)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if result.Key != "readme.txt" {
		t.Errorf("expected key 'readme.txt', got %q", result.Key)
	}
	if result.Size != int64(len(data)) {
		t.Errorf("expected size %d, got %d", len(data), result.Size)
	}

	got, err := backend.Get(ctx, "docs", "readme.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected object, got nil")
	}
	defer got.Body.Close()

	body, _ := io.ReadAll(got.Body)
	if string(body) != "hello world" {
		t.Errorf("expected 'hello world', got %q", body)
	}
	if got.ContentType != "text/plain" {
		t.Errorf("expected content type 'text/plain', got %q", got.ContentType)
	}
	if got.Metadata["author"] != "test" {
		t.Errorf("expected metadata author 'test', got %q", got.Metadata["author"])
	}
}

func TestMemoryBackendGetMissing(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "memory-missing-get-returns-no-object")
	ctx := context.Background()
	backend := NewMemoryBackend()

	got, err := backend.Get(ctx, "docs", "nonexistent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for missing object")
	}
}

func TestMemoryBackendDelete(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()

	backend.Put(ctx, "docs", "temp.txt", bytes.NewReader([]byte("temp")), nil)

	if err := backend.Delete(ctx, "docs", "temp.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	exists, _ := backend.Exists(ctx, "docs", "temp.txt")
	if exists {
		t.Error("expected object to be deleted")
	}
}

func TestMemoryBackendExists(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()

	exists, _ := backend.Exists(ctx, "docs", "file.txt")
	if exists {
		t.Error("expected false for non-existent object")
	}

	backend.Put(ctx, "docs", "file.txt", bytes.NewReader([]byte("data")), nil)

	exists, _ = backend.Exists(ctx, "docs", "file.txt")
	if !exists {
		t.Error("expected true for existing object")
	}
}

func TestMemoryBackendList(t *testing.T) {
	ctx := context.Background()
	backend := NewMemoryBackend()

	backend.Put(ctx, "docs", "a/1.txt", bytes.NewReader([]byte("1")), nil)
	backend.Put(ctx, "docs", "a/2.txt", bytes.NewReader([]byte("2")), nil)
	backend.Put(ctx, "docs", "b/3.txt", bytes.NewReader([]byte("3")), nil)
	backend.Put(ctx, "docs", "root.txt", bytes.NewReader([]byte("root")), nil)

	// List all.
	result, err := backend.List(ctx, "docs", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(result.Objects) != 4 {
		t.Errorf("expected 4 objects, got %d", len(result.Objects))
	}

	// List with prefix.
	result, err = backend.List(ctx, "docs", &ListOptions{Prefix: "a/"})
	if err != nil {
		t.Fatalf("list with prefix: %v", err)
	}
	if len(result.Objects) != 2 {
		t.Errorf("expected 2 objects with prefix 'a/', got %d", len(result.Objects))
	}

	// List with delimiter.
	result, err = backend.List(ctx, "docs", &ListOptions{Delimiter: "/"})
	if err != nil {
		t.Fatalf("list with delimiter: %v", err)
	}
	if len(result.Objects) != 1 {
		t.Errorf("expected 1 root object, got %d", len(result.Objects))
	}
	if len(result.Prefixes) != 2 {
		t.Errorf("expected 2 prefixes, got %d: %v", len(result.Prefixes), result.Prefixes)
	}

	// List with max keys.
	result, err = backend.List(ctx, "docs", &ListOptions{MaxKeys: 2})
	if err != nil {
		t.Fatalf("list with max keys: %v", err)
	}
	if len(result.Objects) != 2 {
		t.Errorf("expected 2 objects with MaxKeys=2, got %d", len(result.Objects))
	}
	if !result.IsTruncated {
		t.Error("expected IsTruncated=true")
	}
}

func TestMemoryBackendCopy(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "memory-backend-copy")
	ctx := context.Background()
	backend := NewMemoryBackend()

	backend.Put(ctx, "docs", "original.txt", bytes.NewReader([]byte("data")), &ObjectMetadata{ContentType: "text/plain"})

	if err := backend.Copy(ctx, "docs", "original.txt", "copy.txt"); err != nil {
		t.Fatalf("copy: %v", err)
	}

	got, err := backend.Get(ctx, "docs", "copy.txt")
	if err != nil {
		t.Fatalf("get copy: %v", err)
	}
	if got == nil {
		t.Fatal("expected copy to exist")
	}
	defer got.Body.Close()

	body, _ := io.ReadAll(got.Body)
	if string(body) != "data" {
		t.Errorf("expected 'data', got %q", body)
	}
}

func TestFileBackendPutGet(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "filesystem-backend-round-trip")
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	data := []byte("file content")
	meta := &ObjectMetadata{ContentType: "text/plain"}

	result, err := backend.Put(ctx, "bucket1", "test.txt", bytes.NewReader(data), meta)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if result.Size != int64(len(data)) {
		t.Errorf("expected size %d, got %d", len(data), result.Size)
	}

	got, err := backend.Get(ctx, "bucket1", "test.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected object, got nil")
	}
	defer got.Body.Close()

	body, _ := io.ReadAll(got.Body)
	if string(body) != "file content" {
		t.Errorf("expected 'file content', got %q", body)
	}
	if got.ContentType != "text/plain" {
		t.Errorf("expected content type 'text/plain', got %q", got.ContentType)
	}
}

func TestFileBackendDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	backend.Put(ctx, "bucket1", "temp.txt", bytes.NewReader([]byte("temp")), nil)

	if err := backend.Delete(ctx, "bucket1", "temp.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	exists, _ := backend.Exists(ctx, "bucket1", "temp.txt")
	if exists {
		t.Error("expected object to be deleted")
	}
}

func TestFileBackendList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	backend.Put(ctx, "bucket1", "dir/a.txt", bytes.NewReader([]byte("a")), nil)
	backend.Put(ctx, "bucket1", "dir/b.txt", bytes.NewReader([]byte("b")), nil)
	backend.Put(ctx, "bucket1", "root.txt", bytes.NewReader([]byte("root")), nil)

	result, err := backend.List(ctx, "bucket1", &ListOptions{Prefix: "dir/"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(result.Objects) != 2 {
		t.Errorf("expected 2 objects, got %d", len(result.Objects))
	}
}

func TestFileBackendCopy(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "filesystem-backend-copy")
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	backend.Put(ctx, "bucket1", "src.txt", bytes.NewReader([]byte("source")), nil)

	if err := backend.Copy(ctx, "bucket1", "src.txt", "dst.txt"); err != nil {
		t.Fatalf("copy: %v", err)
	}

	exists, _ := backend.Exists(ctx, "bucket1", "dst.txt")
	if !exists {
		t.Error("expected copy to exist")
	}
}

func TestFileBackendGetMissing(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "filesystem-missing-get-returns-no-object")
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	got, err := backend.Get(ctx, "bucket1", "nonexistent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for missing object")
	}
}

// Ensure backends implement the Backend interface.
var _ Backend = (*MemoryBackend)(nil)
var _ Backend = (*FileBackend)(nil)
var _ Backend = (*S3Backend)(nil)

func TestFileBackendListEmptyBucket(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	result, err := backend.List(ctx, "empty-bucket", nil)
	if err != nil {
		t.Fatalf("list empty bucket: %v", err)
	}
	if len(result.Objects) != 0 {
		t.Errorf("expected 0 objects, got %d", len(result.Objects))
	}
}

func TestS3BackendCompileCheck(t *testing.T) {
	// Just verify the S3 backend can be created.
	backend := NewS3Backend(S3Config{
		Endpoint:  "http://localhost:9000",
		Region:    "us-east-1",
		AccessKey: "test",
		SecretKey: "test",
		Bucket:    "test",
	})
	if backend == nil {
		t.Fatal("expected non-nil S3 backend")
	}
	defer backend.Close()
}

func TestFileBackendNestedDirectories(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	// Create nested structure.
	backend.Put(ctx, "docs", "a/b/c/file.txt", bytes.NewReader([]byte("deep")), nil)

	got, err := backend.Get(ctx, "docs", "a/b/c/file.txt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected object")
	}
	defer got.Body.Close()

	body, _ := io.ReadAll(got.Body)
	if string(body) != "deep" {
		t.Errorf("expected 'deep', got %q", body)
	}
}

func TestBucketCreationHelpers(t *testing.T) {
	// Save and restore global registry.
	saved := bucketRegistry
	bucketRegistry = &Registry{buckets: make(map[string]*BucketDefinition)}
	defer func() { bucketRegistry = saved }()

	b := Bucket("images",
		WithMaxFileSize(5*1024*1024),
		WithAllowedMimeTypes("image/png", "image/jpeg"),
		WithPublic(true),
	)

	if b.Name != "images" {
		t.Errorf("expected name 'images', got %q", b.Name)
	}
	if b.Options.MaxFileSize != 5*1024*1024 {
		t.Errorf("expected MaxFileSize 5MB, got %d", b.Options.MaxFileSize)
	}
	if len(b.Options.AllowedMimeTypes) != 2 {
		t.Errorf("expected 2 MIME types, got %d", len(b.Options.AllowedMimeTypes))
	}
	if !b.Options.Public {
		t.Error("expected Public to be true")
	}

	// Should be in global registry.
	got, ok := GetRegistry().Get("images")
	if !ok {
		t.Fatal("expected bucket in registry")
	}
	if got.Name != "images" {
		t.Errorf("expected 'images', got %q", got.Name)
	}
}

func TestFileBackendMetadataPersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend := NewFileBackend(dir)

	meta := &ObjectMetadata{
		ContentType: "application/json",
		Custom:      map[string]string{"version": "1"},
	}
	backend.Put(ctx, "data", "config.json", bytes.NewReader([]byte(`{"key":"value"}`)), meta)

	// Create a new backend instance to verify persistence.
	backend2 := NewFileBackend(dir)
	got, err := backend2.Get(ctx, "data", "config.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected object")
	}
	defer got.Body.Close()

	if got.ContentType != "application/json" {
		t.Errorf("expected content type 'application/json', got %q", got.ContentType)
	}
	if got.Metadata["version"] != "1" {
		t.Errorf("expected metadata version '1', got %q", got.Metadata["version"])
	}

	// Verify the meta file exists on disk.
	metaPath, perr := backend.metaPath("data", "config.json")
	if perr != nil {
		t.Fatalf("metaPath error: %v", perr)
	}
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		t.Error("expected metadata file to exist on disk")
	}
}

// --- Path traversal protection ---

func TestFileBackendPathTraversal(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "filesystem-path-traversal-refused")
	tmp := t.TempDir()
	backend := NewFileBackend(tmp)
	ctx := context.Background()

	traversals := []struct {
		bucket string
		key    string
	}{
		{"../etc", "passwd"},
		{"data", "../../etc/passwd"},
		{"..", "file"},
	}

	for _, tt := range traversals {
		t.Run(tt.bucket+"/"+tt.key, func(t *testing.T) {
			_, err := backend.Put(ctx, tt.bucket, tt.key, bytes.NewReader([]byte("x")), nil)
			if err == nil {
				t.Error("expected error for path traversal, got nil")
			}

			_, err = backend.Get(ctx, tt.bucket, tt.key)
			if err == nil {
				t.Error("expected error for path traversal Get, got nil")
			}

			err = backend.Delete(ctx, tt.bucket, tt.key)
			if err == nil {
				t.Error("expected error for path traversal Delete, got nil")
			}

			_, err = backend.Exists(ctx, tt.bucket, tt.key)
			if err == nil {
				t.Error("expected error for path traversal Exists, got nil")
			}
		})
	}
}

func TestFileBackendValidPaths(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "filesystem-valid-paths-accepted")
	tmp := t.TempDir()
	backend := NewFileBackend(tmp)
	ctx := context.Background()

	// Valid paths should work
	_, err := backend.Put(ctx, "mybucket", "myfile.txt", bytes.NewReader([]byte("hello")), nil)
	if err != nil {
		t.Fatalf("unexpected error for valid Put: %v", err)
	}

	result, err := backend.Get(ctx, "mybucket", "myfile.txt")
	if err != nil {
		t.Fatalf("unexpected error for valid Get: %v", err)
	}
	if result == nil {
		t.Fatal("expected result, got nil")
	}
	result.Body.Close()

	// Nested keys should work
	_, err = backend.Put(ctx, "mybucket", "sub/dir/file.txt", bytes.NewReader([]byte("nested")), nil)
	if err != nil {
		t.Fatalf("unexpected error for nested Put: %v", err)
	}
}
