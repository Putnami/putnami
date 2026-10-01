package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// makeSource writes a small output tree and returns its path, for use as an
// entry's FilesDir (the ingest source).
func makeSource(t *testing.T, content string) string {
	t.Helper()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dist", "main.js"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

func entryWith(filesDir string) *Entry {
	return &Entry{
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "build", Project: "pkg"},
		FilesDir: filesDir,
	}
}

// TestPut_ConcurrentSameHash models many worktrees (separate LocalStore handles
// on one shared store root, each with its own flock fd and its own in-process
// mutex) publishing the SAME content-addressed entry at once. The result must be
// exactly one intact entry — no corruption, no partial directory.
func TestPut_ConcurrentSameHash(t *testing.T) {
	root := t.TempDir()
	hash := "aaaa000000000000000000000000000000000000000000000000000000000000"

	const writers = 12
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := NewLocalStore(root) // distinct handle per "process"
			errs[i] = s.Put(hash, entryWith(makeSource(t, "built")))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: Put failed: %v", i, err)
		}
	}

	// Exactly one intact, readable entry.
	s := NewLocalStore(root)
	entry, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry == nil {
		t.Fatal("entry missing after concurrent publish")
	}
	data, err := os.ReadFile(filepath.Join(entry.FilesDir, "dist", "main.js"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(data) != "built" {
		t.Errorf("published content = %q, want built", string(data))
	}
	// No staging dirs left behind.
	tmpEntries, _ := os.ReadDir(filepath.Join(root, "tmp"))
	for _, e := range tmpEntries {
		t.Errorf("leftover staging dir: %s", e.Name())
	}
}

// TestPut_ConcurrentDifferentHashes publishes many distinct entries at once and
// asserts they all land — the shared lock must not serialize them to death or
// drop any.
func TestPut_ConcurrentDifferentHashes(t *testing.T) {
	root := t.TempDir()
	const writers = 16
	hashes := make([]string, writers)
	var wg sync.WaitGroup
	for i := range writers {
		hashes[i] = fmt.Sprintf("%064x", i+1)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := NewLocalStore(root)
			if err := s.Put(hashes[i], entryWith(makeSource(t, fmt.Sprintf("c%d", i)))); err != nil {
				t.Errorf("Put %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	s := NewLocalStore(root)
	for i, h := range hashes {
		entry, err := s.Get(h)
		if err != nil || entry == nil {
			t.Errorf("entry %d (%s) missing: err=%v", i, h, err)
		}
	}
}

// TestPut_FirstWriterWins verifies an existing entry is never clobbered by a
// later publish of the same hash (content-addressed → byte-identical, so the
// first writer's copy is authoritative).
func TestPut_FirstWriterWins(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	hash := "bbbb000000000000000000000000000000000000000000000000000000000000"

	first := entryWith(makeSource(t, "first"))
	first.Result = &EntryResult{Status: "success", Data: map[string]any{"who": "first"}}
	if err := s.Put(hash, first); err != nil {
		t.Fatalf("first Put: %v", err)
	}

	// A second publish of the same hash must be a no-op clobber-wise.
	second := entryWith(makeSource(t, "second"))
	second.Result = &EntryResult{Status: "success", Data: map[string]any{"who": "second"}}
	if err := s.Put(hash, second); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	entry, err := s.Get(hash)
	if err != nil || entry == nil {
		t.Fatalf("Get: %v", err)
	}
	if entry.Result.Data["who"] != "first" {
		t.Errorf("first-writer-wins violated: result = %v, want first", entry.Result.Data["who"])
	}
	data, _ := os.ReadFile(filepath.Join(entry.FilesDir, "dist", "main.js"))
	if string(data) != "first" {
		t.Errorf("files clobbered: %q, want first", string(data))
	}
}
