package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLocalStore_ConcurrentPublishThroughOneHandle(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	keys := []string{"key-1", "key-2", "key-3", "key-4"}

	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Go(func() {
			if _, err := s.IngestTaskEntry("", taskSpec(key)); err != nil {
				t.Errorf("concurrent ingest %s: %v", key, err)
			}
		})
	}
	wg.Wait()

	for _, key := range keys {
		got, err := s.LookupTaskEntry(key)
		if err != nil {
			t.Errorf("LookupTaskEntry %s: %v", key, err)
		}
		if got == nil {
			t.Errorf("entry %s should exist after a concurrent ingest", key)
		}
	}
}

func TestLocalStore_BlobDirLayout(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)
	entry := ingestOne(t, s, "layout-key", "built")

	// Verify layout: blobs/<h2>/<address>/meta.json
	expectedDir := filepath.Join(dir, "blobs", entry.Address[:2], entry.Address)
	info, err := os.Stat(expectedDir)
	if err != nil {
		t.Fatalf("blob dir not at expected path: %v", err)
	}
	if !info.IsDir() {
		t.Error("blob dir should be a directory")
	}
	for _, name := range []string{"meta.json", "result.json", manifestFilename, entryDescriptorFilename} {
		if _, err := os.Stat(filepath.Join(expectedDir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
}

func TestLocalStore_ConcurrentLookups(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	const key = "concurrent-lookup-key"
	ingestOne(t, s, key, "built")

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := s.LookupTaskEntry(key)
			if err != nil {
				t.Errorf("concurrent LookupTaskEntry: %v", err)
				return
			}
			if got == nil || got.Result.Status != "success" {
				t.Errorf("unexpected entry: %+v", got)
			}
		})
	}
	wg.Wait()
}

func TestLocalStore_Root(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)
	if s.Root() != dir {
		t.Errorf("Root() = %q, want %q", s.Root(), dir)
	}
}
