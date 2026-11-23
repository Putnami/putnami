package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	proto "go.putnami.dev/protocol/extension"
)

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// putWithFile publishes the task-owned entry of key, whose one directory
// output "dist" holds content at rel.
func putWithFile(t *testing.T, s *LocalStore, key, rel, content string) *TaskEntry {
	t.Helper()
	staging := t.TempDir()
	out := dirOutput("dist", "dist")
	stage(t, staging, out, rel, content)
	entry, err := s.IngestTaskEntry(staging, taskSpec(key, out))
	if err != nil {
		t.Fatalf("IngestTaskEntry(%s): %v", key, err)
	}
	return entry
}

// putFilled publishes an entry whose single output file is size bytes of fill,
// so entries with different fills hold different CAS blobs.
func putFilled(t *testing.T, s *LocalStore, key string, size int, fill byte) {
	t.Helper()
	putWithFile(t, s, key, "f.bin", string(bytes.Repeat([]byte{fill}, size)))
}

// putSized publishes an entry whose single output file is size zero bytes, so
// entries of one size share one CAS blob.
func putSized(t *testing.T, s *LocalStore, key string, size int) {
	t.Helper()
	putFilled(t, s, key, size, 0)
}

// taskBlobDir is the blob directory of key's task-owned entry.
func taskBlobDir(s *LocalStore, key string) string {
	return s.blobDir(TaskEntryAddress(key))
}

// publishedEntry reads key's task-owned entry from disk, or nil. Unlike
// LookupTaskEntry it takes no lock and stamps nothing, so a GC test can look
// while another handle holds the store or while recency is what it measures.
func publishedEntry(s *LocalStore, key string) *TaskEntry {
	address := TaskEntryAddress(key)
	return loadTaskEntry(s.blobDir(address), address)
}

// distHit builds a remote hit under key whose one directory output "dist" holds
// files, by path relative to it, and a fetcher that serves the hit's blobs.
func distHit(t *testing.T, key string, files map[string]string) (RemoteTaskEntryHit, *inmemFetcher) {
	t.Helper()
	blobs := map[string][]byte{}
	payload := &cache.Manifest{}
	var total int64
	for rel, content := range files {
		digest := cache.DigestOf([]byte(content))
		payload.Files = append(payload.Files, cache.FileEntry{
			Path: "dist/" + rel, Digest: digest, Mode: 0o644, Size: int64(len(content)),
		})
		blobs[digest] = []byte(content)
		total += int64(len(content))
	}
	descriptor, err := json.Marshal(&TaskEntry{
		Format: CurrentEntryFormat,
		Key:    key,
		Outputs: []TaskEntryOutput{{
			ID: "dist", Kind: proto.OutputKindDirectory, Root: proto.OutputRootProject, Path: "dist",
			State: TaskOutputPresent, Files: len(files), Size: total,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := NewTaskEntryTransfer(&TaskEntry{Key: key, Manifest: payload, Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	blobs[transfer.DescriptorDigest] = descriptor
	return hitFor(transfer, key), &inmemFetcher{blobs: blobs}
}

// readDist reads rel from the "dist" output of entry's blob.
func readDist(t *testing.T, entry *TaskEntry, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(entry.FilesDir, "dist", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read published %s: %v", rel, err)
	}
	return string(data)
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

// assertHoldsNoEntryFile fails when blobDir is absent or holds a file a reader
// of cache entries opens first: entry.json for this CLI, result.json and
// meta.json for an older one.
func assertHoldsNoEntryFile(t *testing.T, blobDir string) {
	t.Helper()
	if _, err := os.Stat(blobDir); err != nil {
		t.Fatalf("the record's blob directory: %v", err)
	}
	for _, name := range []string{entryDescriptorFilename, "result.json", "meta.json"} {
		if _, err := os.Stat(filepath.Join(blobDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s in %s can be read as a cache entry (stat err=%v)", name, blobDir, err)
		}
	}
}
