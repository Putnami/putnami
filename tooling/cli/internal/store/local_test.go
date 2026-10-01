package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLocalStore_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	entry := &Entry{
		Result: &EntryResult{
			Status: "success",
			Data:   map[string]any{"output": "/dist"},
		},
		Metadata: &EntryMetadata{
			Extension:  "@putnami/typescript",
			Task:       "build~transpile",
			Project:    "my-project",
			DurationMs: 1500,
		},
	}

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

	// Put
	if err := s.Put(hash, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Get
	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Result.Status != "success" {
		t.Errorf("Result.Status = %q, want success", got.Result.Status)
	}
	if got.Result.Data["output"] != "/dist" {
		t.Errorf("Result.Data[output] = %v, want /dist", got.Result.Data["output"])
	}
	if got.Metadata.Extension != "@putnami/typescript" {
		t.Errorf("Metadata.Extension = %q, want @putnami/typescript", got.Metadata.Extension)
	}
	if got.Metadata.Task != "build~transpile" {
		t.Errorf("Metadata.Task = %q, want build~transpile", got.Metadata.Task)
	}
	if got.Metadata.Hash != hash {
		t.Errorf("Metadata.Hash = %q, want %s", got.Metadata.Hash, hash)
	}
}

func TestLocalStore_PutWithFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	// Create source files
	srcDir := filepath.Join(t.TempDir(), "output")
	os.MkdirAll(filepath.Join(srcDir, "dist"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "dist", "index.js"), []byte("console.log('hello');"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "dist", "index.d.ts"), []byte("export {};"), 0o644)

	entry := &Entry{
		Result: &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{
			Extension: "ext",
			Task:      "build",
			Project:   "pkg",
		},
		FilesDir: srcDir,
	}

	hash := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"

	if err := s.Put(hash, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.FilesDir == "" {
		t.Fatal("FilesDir is empty, want non-empty")
	}

	// Verify files were copied
	jsPath := filepath.Join(got.FilesDir, "dist", "index.js")
	data, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("read copied file: %v", err)
	}
	if string(data) != "console.log('hello');" {
		t.Errorf("file content = %q, want console.log('hello');", string(data))
	}

	// Verify metadata has output files
	if len(got.Metadata.OutputFiles) != 2 {
		t.Errorf("OutputFiles count = %d, want 2", len(got.Metadata.OutputFiles))
	}
	if got.Metadata.Size <= 0 {
		t.Errorf("Size = %d, want > 0", got.Metadata.Size)
	}
}

func TestLocalStore_PutExcludesThrowawayArtifacts(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	// Output dir mixes a real deliverable with throwaway lcov coverage shards.
	srcDir := filepath.Join(t.TempDir(), "output")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "lcov.info"), []byte("real coverage report"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "lcov.info.0.tmp"), []byte("shard 0"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "lcov.info.1.tmp"), []byte("shard 1"), 0o644)

	entry := &Entry{
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "test", Project: "pkg"},
		FilesDir: srcDir,
	}

	hash := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	if err := s.Put(hash, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// The real report is kept.
	if _, err := os.Stat(filepath.Join(got.FilesDir, "lcov.info")); err != nil {
		t.Errorf("expected lcov.info to be cached: %v", err)
	}
	// The temp shards are dropped.
	for _, shard := range []string{"lcov.info.0.tmp", "lcov.info.1.tmp"} {
		if _, err := os.Stat(filepath.Join(got.FilesDir, shard)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be excluded from cache, stat err = %v", shard, err)
		}
	}

	if len(got.Metadata.OutputFiles) != 1 {
		t.Errorf("OutputFiles count = %d, want 1 (temp shards excluded)", len(got.Metadata.OutputFiles))
	}
}

func TestLocalStore_Get_NotFound(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	entry, err := s.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry != nil {
		t.Error("Get returned non-nil for nonexistent hash")
	}
}

func TestLocalStore_ConcurrentPut(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	var wg sync.WaitGroup
	hashes := []string{
		"a100000000000000000000000000000000000000000000000000000000000000",
		"a200000000000000000000000000000000000000000000000000000000000000",
		"a300000000000000000000000000000000000000000000000000000000000000",
		"a400000000000000000000000000000000000000000000000000000000000000",
	}

	for _, h := range hashes {
		wg.Add(1)
		go func(hash string) {
			defer wg.Done()
			entry := &Entry{
				Result:   &EntryResult{Status: "success"},
				Metadata: &EntryMetadata{Extension: "ext", Task: "t", Project: "p"},
			}
			if err := s.Put(hash, entry); err != nil {
				t.Errorf("concurrent Put %s: %v", hash[:8], err)
			}
		}(h)
	}

	wg.Wait()

	// All should exist
	for _, h := range hashes {
		got, err := s.Get(h)
		if err != nil {
			t.Errorf("Get %s: %v", h[:8], err)
		}
		if got == nil {
			t.Errorf("entry %s should exist after concurrent Put", h[:8])
		}
	}
}

func TestLocalStore_BlobDirLayout(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	hash := "ab12cd34ef56789012345678901234567890123456789012345678901234abcd"
	entry := &Entry{
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "t", Project: "p"},
	}

	s.Put(hash, entry)

	// Verify layout: blobs/ab/ab12cd34.../meta.json
	expectedDir := filepath.Join(dir, "blobs", "ab", hash)
	info, err := os.Stat(expectedDir)
	if err != nil {
		t.Fatalf("blob dir not at expected path: %v", err)
	}
	if !info.IsDir() {
		t.Error("blob dir should be a directory")
	}

	// Check meta.json exists
	metaPath := filepath.Join(expectedDir, "meta.json")
	if _, err := os.Stat(metaPath); err != nil {
		t.Errorf("meta.json missing: %v", err)
	}

	// Check result.json exists
	resultPath := filepath.Join(expectedDir, "result.json")
	if _, err := os.Stat(resultPath); err != nil {
		t.Errorf("result.json missing: %v", err)
	}
}

func TestLocalStore_PublishIsFirstWriterWins(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	hash := "ffff000000000000000000000000000000000000000000000000000000000000"

	// First put
	entry1 := &Entry{
		Result:   &EntryResult{Status: "success", Data: map[string]any{"v": float64(1)}},
		Metadata: &EntryMetadata{Extension: "ext", Task: "t", Project: "p"},
	}
	s.Put(hash, entry1)

	// A second put of the same hash must NOT clobber the first. The store is now
	// machine-global and shared across worktrees; entries are content-addressed
	// (byte-identical for a given hash), so the first writer's copy is kept and a
	// concurrent sibling never destroys an entry a reader may be using.
	entry2 := &Entry{
		Result:   &EntryResult{Status: "success", Data: map[string]any{"v": float64(2)}},
		Metadata: &EntryMetadata{Extension: "ext", Task: "t", Project: "p"},
	}
	s.Put(hash, entry2)

	got, err := s.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Result.Data["v"] != float64(1) {
		t.Errorf("Data[v] = %v, want 1 (first-writer-wins)", got.Result.Data["v"])
	}
}

func TestLocalStore_ConcurrentReadWrite(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	hash := "rw00000000000000000000000000000000000000000000000000000000000000"
	entry := &Entry{
		Result:   &EntryResult{Status: "success", Data: map[string]any{"key": "value"}},
		Metadata: &EntryMetadata{Extension: "ext", Task: "t", Project: "p"},
	}

	if err := s.Put(hash, entry); err != nil {
		t.Fatalf("initial Put: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			got, err := s.Get(hash)
			if err != nil {
				t.Errorf("concurrent Get: %v", err)
				return
			}
			if got != nil && got.Result.Status != "success" {
				t.Errorf("unexpected status: %s", got.Result.Status)
			}
		}()
		go func() {
			defer wg.Done()
			_, _ = s.Get(hash)
		}()
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
