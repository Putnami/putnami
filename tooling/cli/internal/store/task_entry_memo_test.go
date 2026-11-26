package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// lookupReads looks key up in s and returns the entry and the number of
// entries the lookup read from disk.
func lookupReads(t *testing.T, s *LocalStore, key string) (*TaskEntry, int64) {
	t.Helper()
	before := taskEntryReads.Load()
	entry, err := s.LookupTaskEntry(key)
	if err != nil {
		t.Fatal(err)
	}
	return entry, taskEntryReads.Load() - before
}

// A handle reads a published entry's files once: a second lookup answers from
// what the first one loaded while each file the entry was read from keeps its
// stat. A rewrite of one of them, even one that keeps its size and
// modification time, is read again; so is an evicted entry, and an entry
// written within the file index's margin.
func TestTaskEntry_AHandleReadsAnEntryOnce(t *testing.T) {
	if _, ok := fileindex.Lstat(os.Args[0]); !ok {
		t.Skip("the file index does not decode this platform's stat: every lookup reads the entry")
	}
	root := t.TempDir()
	putWithFile(t, NewLocalStore(root), hashB, "main.js", "bundle")
	s := NewLocalStore(root)
	blobDir := taskBlobDir(s, hashB)

	// Written less than the margin ago: read on every lookup.
	for range 2 {
		if entry, reads := lookupReads(t, s, hashB); entry == nil || reads != 1 {
			t.Fatalf("a fresh entry's lookup = %v, %d reads; want the entry, read once", entry, reads)
		}
	}

	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	first, reads := lookupReads(t, s, hashB)
	if first == nil || reads != 1 {
		t.Fatalf("first settled lookup = %v, %d reads; want the entry, read", first, reads)
	}
	second, reads := lookupReads(t, s, hashB)
	if second == nil || reads != 0 {
		t.Fatalf("second lookup = %v, %d reads; want the entry the first one loaded", second, reads)
	}
	if second == first || second.Key != first.Key || second.FilesDir != first.FilesDir {
		t.Fatalf("second lookup = %+v, want a copy of %+v", second, first)
	}

	// A result rewritten in place, with its size and modification time kept:
	// its status-change time moved, and the entry is read again.
	result := filepath.Join(blobDir, "result.json")
	info, err := os.Stat(result)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(result, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if entry, reads := lookupReads(t, s, hashB); entry == nil || reads != 1 {
		t.Fatalf("a lookup after a rewrite of the result = %v, %d reads; want the entry, read again", entry, reads)
	}

	// An evicted entry is a miss, even while the handle holds it.
	time.Sleep(fileindex.Margin + 100*time.Millisecond)
	if entry, reads := lookupReads(t, s, hashB); entry == nil || reads != 1 {
		t.Fatalf("a settled lookup after the rewrite = %v, %d reads; want the entry, read", entry, reads)
	}
	if entry, reads := lookupReads(t, s, hashB); entry == nil || reads != 0 {
		t.Fatalf("the lookup before the eviction = %v, %d reads; want the entry the handle holds", entry, reads)
	}
	if err := os.RemoveAll(blobDir); err != nil {
		t.Fatal(err)
	}
	if entry, _ := lookupReads(t, s, hashB); entry != nil {
		t.Fatal("a lookup of an evicted entry answered")
	}
}
