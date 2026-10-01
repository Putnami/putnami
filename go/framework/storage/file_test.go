package storage

import (
	"context"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

// dirNames returns the sorted entry names of dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// failingReader yields prefix and then fails.
func failingReader(prefix string) io.Reader {
	return io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(stderrors.New("reader failed")))
}

func TestFileBackendFailedPutKeepsObject(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "backend-contract", "filesystem-backend-failed-put-keeps-object")
	dir := t.TempDir()
	backend := NewFileBackend(dir)
	ctx := context.Background()

	if _, err := backend.Put(ctx, "docs", "report", strings.NewReader("original"), &ObjectMetadata{ContentType: "text/plain"}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	_, err := backend.Put(ctx, "docs", "report", failingReader("partial replacement"), &ObjectMetadata{ContentType: "application/json"})
	if err == nil {
		t.Fatal("a Put whose reader fails should return an error")
	}
	if errors.GetCode(err) != CodeStorageWrite {
		t.Errorf("code = %q, want %q", errors.GetCode(err), CodeStorageWrite)
	}

	body, obj := readObject(t, backend, "docs", "report")
	if body != "original" {
		t.Errorf("object after failed Put = %q, want %q", body, "original")
	}
	if obj.ContentType != "text/plain" {
		t.Errorf("content type after failed Put = %q, want %q", obj.ContentType, "text/plain")
	}

	// A failed Put of a new key stores nothing.
	if _, err := backend.Put(ctx, "docs", "fresh", failingReader("abc"), nil); err == nil {
		t.Fatal("a Put whose reader fails should return an error")
	}
	if exists, err := backend.Exists(ctx, "docs", "fresh"); err != nil || exists {
		t.Errorf("Exists(fresh) = %v, %v; want false, nil", exists, err)
	}

	// No temporary file is left behind.
	if got, want := dirNames(t, filepath.Join(dir, "docs")), []string{"report", "report.meta.json"}; !slices.Equal(got, want) {
		t.Errorf("bucket directory = %v, want %v", got, want)
	}
}

func TestFileBackendPutReplacesObjectWithCreateMode(t *testing.T) {
	// A restrictive umask makes the expected mode differ from any fixed mode.
	setUmask(t, 0o077)
	dir := t.TempDir()
	backend := NewFileBackend(dir)
	ctx := context.Background()

	ref, err := os.Create(filepath.Join(dir, "reference"))
	if err != nil {
		t.Fatalf("create reference file: %v", err)
	}
	refInfo, err := ref.Stat()
	if err != nil {
		t.Fatalf("stat reference file: %v", err)
	}
	if err := ref.Close(); err != nil {
		t.Fatalf("close reference file: %v", err)
	}
	want := refInfo.Mode().Perm()

	for _, content := range []string{"first version", "second"} {
		res, err := backend.Put(ctx, "docs", "a/b.txt", strings.NewReader(content), &ObjectMetadata{ContentType: "text/plain"})
		if err != nil {
			t.Fatalf("Put %q: %v", content, err)
		}
		if res.Size != int64(len(content)) {
			t.Errorf("Size = %d, want %d", res.Size, len(content))
		}
		if body, _ := readObject(t, backend, "docs", "a/b.txt"); body != content {
			t.Errorf("object = %q, want %q", body, content)
		}
	}
	if got, wantNames := dirNames(t, filepath.Join(dir, "docs", "a")), []string{"b.txt", "b.txt.meta.json"}; !slices.Equal(got, wantNames) {
		t.Errorf("object directory = %v, want %v", got, wantNames)
	}
	for _, name := range []string{"b.txt", "b.txt.meta.json"} {
		info, err := os.Stat(filepath.Join(dir, "docs", "a", name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm != want {
			t.Errorf("%s permissions = %o, want %o as os.Create gives", name, perm, want)
		}
	}
}

func TestFileBackendPutRejectsReservedKeySuffixes(t *testing.T) {
	spectest.Proves(t, "go/object-storage", "isolation", "filesystem-reserved-key-suffix-refused")
	dir := t.TempDir()
	backend := NewFileBackend(dir)
	ctx := context.Background()

	if _, err := backend.Put(ctx, "docs", "x", strings.NewReader("object"), &ObjectMetadata{ContentType: "text/plain"}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	for _, key := range []string{"x.meta.json", "x.META.JSON", "export" + fileTempSuffix, "a/b.Putnami-Tmp", "x.meta.json/y", "d.putnami-tmp/e"} {
		_, err := backend.Put(ctx, "docs", key, strings.NewReader("{}"), nil)
		if errors.GetCode(err) != CodeStorageWrite {
			t.Errorf("Put %q: err = %v, want code %q", key, err, CodeStorageWrite)
		}
	}
	if _, obj := readObject(t, backend, "docs", "x"); obj.ContentType != "text/plain" {
		t.Errorf("metadata of x = %q, want the seeded %q", obj.ContentType, "text/plain")
	}
	if err := backend.Copy(ctx, "docs", "x", "copy.meta.json"); errors.GetCode(err) != CodeStorageCopy {
		t.Errorf("Copy to a reserved key: err = %v, want code %q", err, CodeStorageCopy)
	}
	if got, want := dirNames(t, filepath.Join(dir, "docs")), []string{"x", "x.meta.json"}; !slices.Equal(got, want) {
		t.Errorf("bucket directory = %v, want %v", got, want)
	}
}

func TestFileBackendFailedRenameRemovesTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	backend := NewFileBackend(dir)
	ctx := context.Background()

	if _, err := backend.Put(ctx, "docs", "a/b", strings.NewReader("nested"), nil); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	// The object path of key "a" is the directory holding "a/b".
	_, err := backend.Put(ctx, "docs", "a", strings.NewReader("collides"), nil)
	if errors.GetCode(err) != CodeStorageWrite || !hasAttr(err, "op", "rename") {
		t.Fatalf("err = %v, want code %q from the rename", err, CodeStorageWrite)
	}
	if got, want := dirNames(t, filepath.Join(dir, "docs")), []string{"a"}; !slices.Equal(got, want) {
		t.Errorf("bucket directory = %v, want %v", got, want)
	}
	if body, _ := readObject(t, backend, "docs", "a/b"); body != "nested" {
		t.Errorf("nested object = %q, want %q", body, "nested")
	}
}

func TestFileBackendPutWithoutMetadataRemovesReplacedMetadata(t *testing.T) {
	dir := t.TempDir()
	backend := NewFileBackend(dir)
	ctx := context.Background()

	if _, err := backend.Put(ctx, "docs", "k", strings.NewReader("v1"), &ObjectMetadata{ContentType: "text/plain", Custom: map[string]string{"v": "1"}}); err != nil {
		t.Fatalf("Put with metadata: %v", err)
	}
	if _, err := backend.Put(ctx, "docs", "k", strings.NewReader("v2"), nil); err != nil {
		t.Fatalf("Put without metadata: %v", err)
	}
	body, obj := readObject(t, backend, "docs", "k")
	if body != "v2" || obj.ContentType != "" || obj.Metadata != nil {
		t.Errorf("object = %q (content type %q, metadata %v), want %q with no metadata", body, obj.ContentType, obj.Metadata, "v2")
	}
	if got, want := dirNames(t, filepath.Join(dir, "docs")), []string{"k"}; !slices.Equal(got, want) {
		t.Errorf("bucket directory = %v, want %v", got, want)
	}
}

func TestFileBackendListSkipsTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	backend := NewFileBackend(dir)
	ctx := context.Background()

	if _, err := backend.Put(ctx, "docs", "kept", strings.NewReader("x"), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	abandoned := filepath.Join(dir, "docs", ".put-123"+fileTempSuffix)
	if err := os.WriteFile(abandoned, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write abandoned temporary file: %v", err)
	}
	res, err := backend.List(ctx, "docs", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Objects) != 1 || res.Objects[0].Key != "kept" {
		t.Errorf("List objects = %+v, want only %q", res.Objects, "kept")
	}
}
