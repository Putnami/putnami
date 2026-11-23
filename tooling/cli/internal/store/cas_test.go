package store

import (
	"os"
	"path/filepath"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

func TestCAS_DedupsIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	a := putWithFile(t, s, hashA, "data.txt", "identical bytes")
	b := putWithFile(t, s, hashB, "data.txt", "identical bytes")

	if n := countCASBlobs(t, dir); n != 1 {
		t.Errorf("expected 1 deduplicated CAS blob, got %d", n)
	}

	// Both entries' files are hardlinks to the same underlying inode.
	ia, err := os.Stat(filepath.Join(a.FilesDir, "dist", "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	ib, err := os.Stat(filepath.Join(b.FilesDir, "dist", "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(ia, ib) {
		t.Error("expected the two entries' files to share one inode (hardlink dedup)")
	}
}

func TestCAS_ManifestRecordsDigests(t *testing.T) {
	s := NewLocalStore(t.TempDir())

	content := "console.log('hi');"
	putWithFile(t, s, hashA, "index.js", content)

	entry, err := s.LookupTaskEntry(hashA)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || entry.Manifest == nil || len(entry.Manifest.Files) != 1 {
		t.Fatalf("expected an entry with a 1-file manifest, got %+v", entry)
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
