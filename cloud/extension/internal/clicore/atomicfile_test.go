package clicore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// errPartialRead marks a read that returned a JSON object missing the required
// workspace_id — a truncation-adjacent failure the concurrency test guards against.
var errPartialRead = errors.New("read a cloud-link with no workspace_id")

// TestWriteFileAtomicWritesContentAndPerm asserts the happy path: the requested
// bytes land at the target, the requested perm is applied, and the parent dir is
// created if absent.
func TestWriteFileAtomicWritesContentAndPerm(t *testing.T) {
	t.Parallel()
	// Nested path so MkdirAll has to create the parent directory.
	path := filepath.Join(t.TempDir(), "sub", "dir", "cloud-link.json")
	want := []byte("{\"workspace_id\":\"w\"}\n")

	if err := WriteFileAtomic(path, want, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("perm = %o, want 644", perm)
	}
}

// TestWriteFileAtomicOverwrites confirms a second write replaces the file and
// leaves no *.tmp residue in the directory.
func TestWriteFileAtomicOverwrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	if err := WriteFileAtomic(path, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("second\n"), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "second\n" {
		t.Fatalf("content = %q, want %q", got, "second\n")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "cache.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir entries = %v, want [cache.json] (no *.tmp residue)", names)
	}
}

// TestWriteFileAtomicConcurrentReadNeverTruncated reproduces the install race:
// a killed writer must never leave a partial cloud-link.json for a concurrent
// reader (the cache provider's token command) to choke on. With the atomic
// temp+rename write, every read observes either the old or a complete new JSON,
// never a truncated one.
func TestWriteFileAtomicConcurrentReadNeverTruncated(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cloud-link.json")

	payload := func(marker string) []byte {
		data, err := json.Marshal(map[string]any{
			"workspace_id":      "ws-" + marker,
			"control_plane_url": "https://api.putnami.cloud",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return append(data, '\n')
	}

	// Seed a valid file so readers always have a complete state to observe.
	if err := WriteFileAtomic(path, payload("seed"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	const writers, readers, iterations = 4, 8, 300

	var writersWg, readersWg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, writers+readers)

	for id := range writers {
		writersWg.Go(func() {
			for range iterations {
				if err := WriteFileAtomic(path, payload(string(rune('a'+id))), 0o644); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	for range readers {
		readersWg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(path)
				if err != nil {
					errs <- err
					return
				}
				// Every observed file must be a complete, non-empty JSON object
				// with the required fields — a truncated read fails Unmarshal or
				// yields an empty workspace_id.
				var link map[string]any
				if err := json.Unmarshal(data, &link); err != nil {
					errs <- err
					return
				}
				if StringValue(link["workspace_id"]) == "" {
					errs <- errPartialRead
					return
				}
			}
		})
	}

	writersWg.Wait()
	close(stop)
	readersWg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("reader observed a partial/invalid write: %v", err)
	}
}
