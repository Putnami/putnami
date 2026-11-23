package store

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// inmemFetcher serves blob bytes by digest and records which digests it served,
// so tests can assert that already-local blobs are not re-fetched (dedup). It is
// safe for concurrent use because materializeFiles fans out fetches across
// goroutines. blobs is read-only after construction.
type inmemFetcher struct {
	blobs map[string][]byte

	mu     sync.Mutex
	opened []string
}

func (f *inmemFetcher) fetch(digest string) (io.ReadCloser, error) {
	f.mu.Lock()
	f.opened = append(f.opened, digest)
	f.mu.Unlock()
	b, ok := f.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("no blob %s", digest)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// distinctOpened returns the set of digests fetched, collapsing the duplicate
// fetches two manifest files sharing a digest can race into.
func (f *inmemFetcher) distinctOpened() map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := make(map[string]bool, len(f.opened))
	for _, d := range f.opened {
		set[d] = true
	}
	return set
}

func TestMaterialize_CreatesAReadableEntry(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	content := "remote artifact bytes"
	hit, f := distHit(t, hashA, map[string]string{"app.js": content})

	if _, err := s.MaterializeTaskEntry(hit, f.fetch); err != nil {
		t.Fatalf("MaterializeTaskEntry: %v", err)
	}

	got, err := s.LookupTaskEntry(hashA)
	if err != nil || got == nil {
		t.Fatalf("LookupTaskEntry after materialize: %v (entry=%v)", err, got)
	}
	if got.Result.Status != "success" {
		t.Errorf("result status = %q, want success", got.Result.Status)
	}
	if got.Manifest == nil || len(got.Manifest.Files) != 1 {
		t.Fatalf("manifest not restored: %+v", got.Manifest)
	}
	if data := readDist(t, got, "app.js"); data != content {
		t.Errorf("materialized content = %q, want %q", data, content)
	}
	if n := countCASBlobs(t, dir); n != 1 {
		t.Errorf("expected 1 CAS blob, got %d", n)
	}
	// A fetched blob is store growth the in-run budget must see (gc_trigger.go).
	if got := s.added.Load(); got != int64(len(content)) {
		t.Errorf("added = %d, want the %d fetched bytes", got, len(content))
	}
}

func TestMaterialize_ReusesLocalBlobWithoutFetch(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	// Seed the CAS with the content via a local ingest.
	content := "shared bytes"
	putWithFile(t, s, hashA, "a.txt", content)

	// Materialize a different key referencing the same content. The fetcher
	// serves no payload blob: fetching it would fail.
	hit, f := distHit(t, hashB, map[string]string{"b.txt": content})
	digest := cache.DigestOf([]byte(content))
	delete(f.blobs, digest)

	if _, err := s.MaterializeTaskEntry(hit, f.fetch); err != nil {
		t.Fatalf("MaterializeTaskEntry with local dedup: %v", err)
	}
	if f.distinctOpened()[digest] {
		t.Errorf("expected no fetch for an already-local blob, fetched: %v", f.opened)
	}
	if n := countCASBlobs(t, dir); n != 1 {
		t.Errorf("expected the blob to be deduplicated to 1 CAS entry, got %d", n)
	}
	if got := s.added.Load(); got != int64(len(content)) {
		t.Errorf("added = %d, want only the ingest's %d bytes: a reused blob adds nothing", got, len(content))
	}
}

func TestMaterialize_ParallelMultiFileAndDedup(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	// A TypeScript-build-shaped manifest: many tiny declaration files. Two files
	// share identical content to exercise CAS dedup under the concurrent fan-out.
	want := map[string]string{}
	var distinctBytes int64
	for i := range 64 {
		content := fmt.Sprintf("export declare const x%d: number;", i)
		want[fmt.Sprintf("types/m%02d.d.ts", i)] = content
		distinctBytes += int64(len(content))
	}
	shared := "export {};"
	want["a.js"], want["b.js"] = shared, shared
	distinctBytes += int64(len(shared))
	const distinct = 65 // 64 declarations and the shared file

	hit, f := distHit(t, hashA, want)
	if _, err := s.MaterializeTaskEntry(hit, f.fetch); err != nil {
		t.Fatalf("MaterializeTaskEntry: %v", err)
	}

	got, err := s.LookupTaskEntry(hashA)
	if err != nil || got == nil {
		t.Fatalf("LookupTaskEntry after materialize: %v", err)
	}
	for rel, content := range want {
		if data := readDist(t, got, rel); data != content {
			t.Errorf("%s = %q, want %q", rel, data, content)
		}
	}

	// Shared content is stored once: 64 distinct + 1 shared = 65 CAS blobs, and
	// every distinct digest was fetched (the shared one possibly twice via the
	// publish race — storage still dedups it), plus the entry descriptor, which
	// never reaches the CAS.
	if n := countCASBlobs(t, dir); n != distinct {
		t.Errorf("CAS blobs = %d, want %d", n, distinct)
	}
	if opened := f.distinctOpened(); len(opened) != distinct+1 {
		t.Errorf("fetched %d distinct digests, want %d", len(opened), distinct+1)
	}
	// The racing loser's staged copy is discarded, not published, so the
	// shared blob counts once however the fan-out interleaved.
	if got := s.added.Load(); got != distinctBytes {
		t.Errorf("added = %d, want %d (each distinct blob once)", got, distinctBytes)
	}
}

func TestMaterialize_RejectsCorruptBlob(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	hit, f := distHit(t, hashA, map[string]string{"app.js": "expected content"})
	// Fetcher returns bytes that do not match the manifest digest.
	f.blobs[cache.DigestOf([]byte("expected content"))] = []byte("tampered content")

	_, err := s.MaterializeTaskEntry(hit, f.fetch)
	if err == nil {
		t.Fatal("expected an error for a blob that does not match its digest")
	}
	if !strings.Contains(err.Error(), "content-addresses to") {
		t.Errorf("error should explain the digest mismatch, got: %v", err)
	}
	if got := publishedEntry(s, hashA); got != nil {
		t.Error("a failed materialize must not leave a committed entry")
	}
	if n := countCASBlobs(t, dir); n != 0 {
		t.Errorf("a corrupt blob must not be published to CAS, got %d", n)
	}
}

func TestMaterialize_RequiresFetcherForMissingBlob(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	content := []byte("bytes not in cas")

	err := s.PrefetchBlobs(&cache.Manifest{Files: []cache.FileEntry{
		{Path: "x.txt", Digest: cache.DigestOf(content), Mode: 0o644, Size: int64(len(content))},
	}}, nil)
	if err == nil {
		t.Fatal("expected an error materializing a missing blob without a fetcher")
	}
	if !strings.Contains(err.Error(), "no fetcher") {
		t.Errorf("error should mention the missing fetcher, got: %v", err)
	}
}

func TestMaterialize_RequiresManifest(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	_, err := s.MaterializeTaskEntry(RemoteTaskEntryHit{Key: hashA, Result: &EntryResult{Status: "success"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "manifest required") {
		t.Fatalf("expected a manifest-required error, got: %v", err)
	}
}

func TestOpenBlob_RoundTripsAndRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	content := "blob source bytes"
	putWithFile(t, s, hashA, "f.txt", content)
	digest := cache.DigestOf([]byte(content))

	rc, err := s.OpenBlob(digest)
	if err != nil {
		t.Fatalf("OpenBlob: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != content {
		t.Errorf("OpenBlob bytes = %q, want %q", got, content)
	}

	missing := cache.DigestOf([]byte("never stored"))
	if _, err := s.OpenBlob(missing); err == nil {
		t.Error("expected an error opening a digest not in CAS")
	}
	if _, err := s.OpenBlob("not-a-digest"); err == nil {
		t.Error("expected an error opening an invalid digest")
	}
}

// A remote manifest's mode cannot loosen or lock the CAS blob that every entry
// holding the same bytes shares: linking chmods that inode through the hardlink.
func TestPrefetchBlobs_ManifestModeCannotLoosenSharedBlob(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)
	content := "trusted bytes"
	putWithFile(t, s, hashA, "a.txt", content)
	digest := cache.DigestOf([]byte(content))

	for _, mode := range []uint32{0, 0o777 | uint32(os.ModeSetuid)} {
		manifest := &cache.Manifest{Files: []cache.FileEntry{
			{Path: "hint.txt", Digest: digest, Mode: mode, Size: int64(len(content))},
		}}
		if err := s.PrefetchBlobs(manifest, nil); err != nil {
			t.Fatalf("PrefetchBlobs(mode %o): %v", mode, err)
		}
		info, err := os.Stat(s.casBlobPath(digest))
		if err != nil {
			t.Fatal(err)
		}
		got := info.Mode()
		if got&0o400 == 0 || got&0o022 != 0 || got&os.ModeSetuid != 0 {
			t.Errorf("manifest mode %o left the CAS blob at %v", mode, got)
		}
	}
}
