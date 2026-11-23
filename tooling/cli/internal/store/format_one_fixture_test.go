package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// writeFormatOneEntry writes the whole-output (format 1) entry an older CLI
// leaves under hash: meta.json, result.json, manifest.json and a files/ tree
// whose one file is a hardlink into the CAS. It is last used at lastUsed, and
// it returns the blob directory.
func writeFormatOneEntry(t *testing.T, s *LocalStore, hash, rel, content string, lastUsed time.Time) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := cache.DigestOf([]byte(content))
	blobDir := s.blobDir(hash)
	s.mu.Lock()
	err := s.ensureBlobLocked(src, digest)
	if err == nil {
		err = s.linkBlob(digest, filepath.Join(blobDir, "files", filepath.FromSlash(rel)), 0o644)
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"meta.json":   EntryMetadata{Hash: hash, Extension: "ext", Task: "build", Project: "pkg", Size: int64(len(content)), OutputFiles: []string{rel}},
		"result.json": EntryResult{Status: "success"},
		manifestFilename: cache.Manifest{Files: []cache.FileEntry{
			{Path: rel, Digest: digest, Mode: 0o644, Size: int64(len(content))},
		}},
	} {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(blobDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeLastUsed(blobDir, lastUsed, 1)
	return blobDir
}

// TestRunGC_EvictsAFormatOneEntry pins that GC treats a format-1 entry as any
// other blob directory: its bytes count toward the budget, an over-budget pass
// evicts it, and its CAS blob goes with it. A task-owned entry used inside the
// grace window survives the same pass.
func TestRunGC_EvictsAFormatOneEntry(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	blobDir := writeFormatOneEntry(t, s, hashA, "dist/main.js", "whole output", now.Add(-2*time.Hour))
	kept := ingestOne(t, s, "kept-key", "declared")
	writeLastUsed(s.blobDir(kept.Address), now, 1)

	casBlob := s.casBlobPath(cache.DigestOf([]byte("whole output")))
	if _, err := os.Stat(casBlob); err != nil {
		t.Fatalf("the entry holds no CAS blob: %v", err)
	}
	wantScanned := blobMetaBytes(blobDir) + blobMetaBytes(s.blobDir(kept.Address)) + dirBytes(filepath.Join(root, "cas"))

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.ScannedBytes != wantScanned {
		t.Errorf("ScannedBytes = %d, want %d: the format-1 entry's bytes were not counted", res.ScannedBytes, wantScanned)
	}
	if res.EvictedEntries != 1 || res.SweptBlobs != 1 {
		t.Errorf("EvictedEntries = %d, SweptBlobs = %d, want the format-1 entry and its CAS blob", res.EvictedEntries, res.SweptBlobs)
	}
	for _, path := range []string{blobDir, casBlob} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived GC: %v", path, err)
		}
	}
	if publishedEntry(s, "kept-key") == nil {
		t.Error("the task-owned entry in its grace window was evicted")
	}
}

// TestCleanStore_RemovesAFormatOneEntry pins that a clean counts and removes a
// format-1 entry with everything else.
func TestCleanStore_RemovesAFormatOneEntry(t *testing.T) {
	root := t.TempDir()
	blobDir := writeFormatOneEntry(t, NewLocalStore(root), hashA, "dist/main.js", "whole output", time.Now())

	files, size, err := CleanStore(root)
	if err != nil {
		t.Fatalf("CleanStore: %v", err)
	}
	if files == 0 || size < int64(len("whole output")) {
		t.Errorf("CleanStore counted files=%d bytes=%d, want the format-1 entry's", files, size)
	}
	if _, err := os.Stat(blobDir); !os.IsNotExist(err) {
		t.Errorf("the format-1 entry survived a clean: %v", err)
	}
}
