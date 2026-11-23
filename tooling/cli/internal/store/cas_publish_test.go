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

// TestIngestSucceedsWhenAnotherWriterPublishedTheBlob pins F2: a blob rename that
// fails while a blob with that digest exists is a lost race, not an error. The
// digest names the bytes, so the entry links the blob the other writer
// published, and this handle counts no bytes it did not write.
func TestIngestSucceedsWhenAnotherWriterPublishedTheBlob(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	loseBlobPublishRace(t)

	putWithFile(t, s, hashA, "main.js", "built")
	entry, err := s.LookupTaskEntry(hashA)
	if err != nil || entry == nil {
		t.Fatalf("LookupTaskEntry: %v (entry=%v)", err, entry)
	}
	if got := readDist(t, entry, "main.js"); got != "built" {
		t.Errorf("published file = %q; want built", got)
	}
	if got := s.added.Load(); got != 0 {
		t.Errorf("the handle counted %d bytes another writer published", got)
	}
	assertNoStagedBlobs(t, root)
}

// TestIngestFailsWhenTheBlobPublishFailsAndNoBlobExists pins the other side of
// F2: without a blob at the digest, the failed rename is the error, and no
// entry is published.
func TestIngestFailsWhenTheBlobPublishFailsAndNoBlobExists(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	failBlobPublish(t)

	staging := t.TempDir()
	out := dirOutput("dist", "dist")
	stage(t, staging, out, "main.js", "built")
	_, err := s.IngestTaskEntry(staging, taskSpec(hashA, out))
	if !errors.Is(err, errHeldOpen) {
		t.Fatalf("IngestTaskEntry = %v, want the failed blob rename", err)
	}
	if got := publishedEntry(s, hashA); got != nil {
		t.Errorf("a failed ingest published an entry: %+v", got)
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
	hit, f := distHit(t, hashA, map[string]string{"app.js": content})
	if _, err := s.MaterializeTaskEntry(hit, f.fetch); err != nil {
		t.Fatalf("MaterializeTaskEntry after a lost blob race: %v", err)
	}
	entry, err := s.LookupTaskEntry(hashA)
	if err != nil || entry == nil {
		t.Fatalf("LookupTaskEntry: %v (entry=%v)", err, entry)
	}
	if got := readDist(t, entry, "app.js"); got != content {
		t.Errorf("restored file = %q; want %q", got, content)
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

	hit, f := distHit(t, hashA, map[string]string{"app.js": "remote artifact bytes"})
	_, err := s.MaterializeTaskEntry(hit, f.fetch)
	if !errors.Is(err, errHeldOpen) {
		t.Fatalf("MaterializeTaskEntry = %v, want the failed blob rename", err)
	}
	assertNoStagedBlobs(t, root)
}
