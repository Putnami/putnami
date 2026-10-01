package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errHeldOpen is the failure these tests inject where Windows fails a rename
// or an open because another process holds the path.
var errHeldOpen = errors.New("held open by another process")

// loseBlobPublishRace replaces renameBlob for the rest of the test. Each
// rename first publishes the staged bytes itself, as a concurrent writer of
// the same digest does, and then fails the way Windows fails a rename over a
// blob another process holds. Not for parallel tests: it swaps a package
// variable.
func loseBlobPublishRace(t *testing.T) {
	t.Helper()
	saved := renameBlob
	renameBlob = func(staged, blob string) error {
		data, err := os.ReadFile(staged)
		if err != nil {
			return err
		}
		if err := os.WriteFile(blob, data, 0o644); err != nil {
			return err
		}
		return &os.LinkError{Op: "rename", Old: staged, New: blob, Err: errHeldOpen}
	}
	t.Cleanup(func() { renameBlob = saved })
}

// failBlobPublish replaces renameBlob for the rest of the test with a rename
// that fails the same way and publishes nothing.
func failBlobPublish(t *testing.T) {
	t.Helper()
	saved := renameBlob
	renameBlob = func(staged, blob string) error {
		return &os.LinkError{Op: "rename", Old: staged, New: blob, Err: errHeldOpen}
	}
	t.Cleanup(func() { renameBlob = saved })
}

// assertNoStagedBlobs fails when a staged blob (blob-* from a local ingest,
// fetch-* from a remote restore) is left in the CAS.
func assertNoStagedBlobs(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(root, "cas"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name := d.Name(); strings.HasPrefix(name, "blob-") || strings.HasPrefix(name, "fetch-") {
			t.Errorf("staged blob left in the CAS: %s", path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// TestPutSucceedsWhenAnotherWriterPublishedTheBlob pins F2: a blob rename that
// fails while a blob with that digest exists is a lost race, not an error. The
// digest names the bytes, so the entry links the blob the other writer
// published, and this handle counts no bytes it did not write.
func TestPutSucceedsWhenAnotherWriterPublishedTheBlob(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	loseBlobPublishRace(t)

	if err := s.Put(hashA, entryWith(makeSource(t, "built"))); err != nil {
		t.Fatalf("Put after a lost blob race: %v", err)
	}
	entry, err := s.Get(hashA)
	if err != nil || entry == nil {
		t.Fatalf("Get: %v (entry=%v)", err, entry)
	}
	data, err := os.ReadFile(filepath.Join(entry.FilesDir, "dist", "main.js"))
	if err != nil || string(data) != "built" {
		t.Errorf("published file = %q, %v; want built", data, err)
	}
	if got := s.added.Load(); got != 0 {
		t.Errorf("the handle counted %d bytes another writer published", got)
	}
	assertNoStagedBlobs(t, root)
}

// TestPutFailsWhenTheBlobPublishFailsAndNoBlobExists pins the other side of
// F2: without a blob at the digest, the failed rename is the error.
func TestPutFailsWhenTheBlobPublishFailsAndNoBlobExists(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	failBlobPublish(t)

	err := s.Put(hashA, entryWith(makeSource(t, "built")))
	if !errors.Is(err, errHeldOpen) {
		t.Fatalf("Put = %v, want the failed blob rename", err)
	}
	assertNoStagedBlobs(t, root)
}

// TestMaterializeSucceedsWhenAnotherWriterPublishedTheFetchedBlob pins F2 on
// the remote restore path (installBlobLocked).
func TestMaterializeSucceedsWhenAnotherWriterPublishedTheFetchedBlob(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	loseBlobPublishRace(t)

	content := "remote artifact bytes"
	entry, digest := hitEntry("dist/app.js", content)
	f := &inmemFetcher{blobs: map[string][]byte{digest: []byte(content)}}
	if err := s.Materialize(hashA, entry, f.fetch); err != nil {
		t.Fatalf("Materialize after a lost blob race: %v", err)
	}
	got, err := s.Get(hashA)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (entry=%v)", err, got)
	}
	data, err := os.ReadFile(filepath.Join(got.FilesDir, "dist", "app.js"))
	if err != nil || string(data) != content {
		t.Errorf("restored file = %q, %v; want %q", data, err, content)
	}
	if n := s.added.Load(); n != 0 {
		t.Errorf("the handle counted %d bytes another writer published", n)
	}
	assertNoStagedBlobs(t, root)
}

// TestMaterializeFailsWhenTheFetchedBlobPublishFailsAndNoBlobExists pins the
// error side of F2 on the remote restore path.
func TestMaterializeFailsWhenTheFetchedBlobPublishFailsAndNoBlobExists(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	failBlobPublish(t)

	content := "remote artifact bytes"
	entry, digest := hitEntry("dist/app.js", content)
	f := &inmemFetcher{blobs: map[string][]byte{digest: []byte(content)}}
	err := s.Materialize(hashA, entry, f.fetch)
	if !errors.Is(err, errHeldOpen) {
		t.Fatalf("Materialize = %v, want the failed blob rename", err)
	}
	assertNoStagedBlobs(t, root)
}
