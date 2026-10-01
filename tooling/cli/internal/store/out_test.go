package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutManager_Link(t *testing.T) {
	wsRoot := t.TempDir()
	storeRoot := filepath.Join(wsRoot, ".putnami", "store")
	om := NewOutManager(wsRoot, storeRoot)

	// Create a fake blob with files
	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	blobFilesDir := filepath.Join(storeRoot, "blobs", "ab", hash, "files")
	os.MkdirAll(blobFilesDir, 0o755)
	os.WriteFile(filepath.Join(blobFilesDir, "output.js"), []byte("built"), 0o644)

	// Create symlink
	err := om.Link("my-project", "build", "transpile", hash)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}

	// Verify symlink exists
	linkPath := filepath.Join(wsRoot, ".putnami", "out", "my-project", "build", "transpile")
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != blobFilesDir {
		t.Errorf("symlink target = %q, want %q", target, blobFilesDir)
	}

	// Verify we can read through the symlink
	data, err := os.ReadFile(filepath.Join(linkPath, "output.js"))
	if err != nil {
		t.Fatalf("read through symlink: %v", err)
	}
	if string(data) != "built" {
		t.Errorf("content through symlink = %q, want built", string(data))
	}
}

func TestOutManager_Link_NoFilesDir(t *testing.T) {
	wsRoot := t.TempDir()
	om := NewOutManager(wsRoot, filepath.Join(wsRoot, ".putnami", "store"))

	hash := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

	// No blob files directory → should not error, just no-op
	err := om.Link("pkg", "build", "", hash)
	if err != nil {
		t.Fatalf("Link with no files should not error: %v", err)
	}
}

func TestOutManager_AtomicUpdate(t *testing.T) {
	wsRoot := t.TempDir()
	storeRoot := filepath.Join(wsRoot, ".putnami", "store")
	om := NewOutManager(wsRoot, storeRoot)

	// Create two blobs
	hash1 := "aaaa000000000000000000000000000000000000000000000000000000000000"
	hash2 := "bbbb000000000000000000000000000000000000000000000000000000000000"

	for _, hash := range []string{hash1, hash2} {
		dir := filepath.Join(storeRoot, "blobs", hash[:2], hash, "files")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hash:"+hash[:4]), 0o644)
	}

	// Link to hash1
	om.Link("pkg", "build", "step", hash1)
	linkPath := filepath.Join(wsRoot, ".putnami", "out", "pkg", "build", "step")
	data, _ := os.ReadFile(filepath.Join(linkPath, "data.txt"))
	if string(data) != "hash:aaaa" {
		t.Errorf("after first link, content = %q", string(data))
	}

	// Atomically update to hash2
	om.Link("pkg", "build", "step", hash2)
	data, _ = os.ReadFile(filepath.Join(linkPath, "data.txt"))
	if string(data) != "hash:bbbb" {
		t.Errorf("after update, content = %q, want hash:bbbb", string(data))
	}
}

// LinkedBlobHash is the inverse of Link: it lets a later session recover the
// cache entry behind an out/ symlink instead of copying the tree blind. It must
// still answer for a link whose blob has since been reclaimed, since the hash is
// what tells the caller which entry to look for.
func TestLinkedBlobHashRoundTripsLink(t *testing.T) {
	root := t.TempDir()
	storeRoot := filepath.Join(root, ".putnami", "store")
	m := NewOutManager(root, storeRoot)

	hash := "abc123def4567890abc123def4567890abc123def4567890abc123def4567890"
	filesDir := filepath.Join(storeRoot, "blobs", "ab", hash, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Link("proj", "build", "", hash); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, ".putnami", "out", "proj", "build")

	if got := m.LinkedBlobHash(linkPath); got != hash {
		t.Errorf("LinkedBlobHash = %q, want %q", got, hash)
	}

	// A reclaimed blob must still resolve — the caller decides what a missing
	// entry means, and cannot ask without the hash.
	if err := os.RemoveAll(filepath.Join(storeRoot, "blobs")); err != nil {
		t.Fatal(err)
	}
	if got := m.LinkedBlobHash(linkPath); got != hash {
		t.Errorf("LinkedBlobHash after reclaim = %q, want %q", got, hash)
	}
}

func TestLinkedBlobHashRejectsNonBlobPaths(t *testing.T) {
	root := t.TempDir()
	m := NewOutManager(root, filepath.Join(root, ".putnami", "store"))
	outDir := filepath.Join(root, ".putnami", "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	realDir := filepath.Join(outDir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"foreign":         "/somewhere/else/files",
		"no-files-leaf":   "/store/blobs/ab/abc123def4567890abc123def4567890",
		"prefix-mismatch": "/store/blobs/zz/abc123def4567890abc123def4567890/files",
		"not-hex":         "/store/blobs/ab/abnot-a-hex-digest-at-all-here/files",
		"too-short":       "/store/blobs/ab/abcd/files",
	}
	for name, target := range cases {
		link := filepath.Join(outDir, name)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if got := m.LinkedBlobHash(link); got != "" {
			t.Errorf("%s: LinkedBlobHash = %q, want empty", name, got)
		}
	}

	if got := m.LinkedBlobHash(realDir); got != "" {
		t.Errorf("real directory: LinkedBlobHash = %q, want empty", got)
	}
	if got := m.LinkedBlobHash(filepath.Join(outDir, "missing")); got != "" {
		t.Errorf("missing path: LinkedBlobHash = %q, want empty", got)
	}
}
