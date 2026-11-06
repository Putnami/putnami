package store

import (
	"os"
	"path/filepath"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// putWithFile stores an entry whose output is a single file at rel with the
// given content, and returns the store.
func putWithFile(t *testing.T, s *LocalStore, hash, rel, content string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "out")
	full := filepath.Join(src, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := &Entry{
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "build", Project: "pkg"},
		FilesDir: src,
	}
	if err := putFormatOneEntry(s, hash, entry); err != nil {
		t.Fatalf("Put(%s): %v", hash, err)
	}
}

func countCASBlobs(t *testing.T, root string) int {
	t.Helper()
	n := 0
	filepath.Walk(filepath.Join(root, "cas"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestCAS_DedupsIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	putWithFile(t, s, hashA, "data.txt", "identical bytes")
	putWithFile(t, s, hashB, "data.txt", "identical bytes")

	if n := countCASBlobs(t, dir); n != 1 {
		t.Errorf("expected 1 deduplicated CAS blob, got %d", n)
	}

	// Both entries' files are hardlinks to the same underlying inode.
	fa := filepath.Join(s.blobDir(hashA), "files", "data.txt")
	fb := filepath.Join(s.blobDir(hashB), "files", "data.txt")
	ia, err := os.Stat(fa)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := os.Stat(fb)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(ia, ib) {
		t.Error("expected the two entries' files to share one inode (hardlink dedup)")
	}
}

func TestCAS_ManifestRecordsDigests(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	content := "console.log('hi');"
	putWithFile(t, s, hashA, "dist/index.js", content)

	entry, err := s.Get(hashA)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Manifest == nil || len(entry.Manifest.Files) != 1 {
		t.Fatalf("expected manifest with 1 file, got %+v", entry.Manifest)
	}
	f := entry.Manifest.Files[0]
	if f.Path != "dist/index.js" {
		t.Errorf("path = %q, want dist/index.js", f.Path)
	}
	if want := cache.DigestOf([]byte(content)); f.Digest != want {
		t.Errorf("digest = %q, want %q", f.Digest, want)
	}
	if f.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", f.Size, len(content))
	}
	if !cache.ValidDigest(f.Digest) {
		t.Errorf("manifest digest is not well-formed: %q", f.Digest)
	}
}

func TestCAS_RestoreFilesFromManifestEntry(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)
	cm := NewCacheManager(s)

	putWithFile(t, s, hashA, "dist/index.js", "payload")

	entry, err := s.Get(hashA)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	restored, err := cm.RestoreFiles(entry, target)
	if err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}
	if !restored {
		t.Fatal("expected files to be restored")
	}
	got, err := os.ReadFile(filepath.Join(target, "dist", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("restored content = %q, want payload", got)
	}
}
