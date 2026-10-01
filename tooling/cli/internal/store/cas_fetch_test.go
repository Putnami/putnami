package store

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// hitEntry builds a one-file remote hit (result + manifest) for content at rel.
func hitEntry(rel, content string) (*Entry, string) {
	digest := cache.DigestOf([]byte(content))
	entry := &Entry{
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "build", Project: "pkg"},
		Manifest: &cache.Manifest{Files: []cache.FileEntry{
			{Path: rel, Digest: digest, Mode: 0o644, Size: int64(len(content))},
		}},
	}
	return entry, digest
}

func TestMaterialize_CreatesGettableEntry(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	content := "remote artifact bytes"
	entry, digest := hitEntry("dist/app.js", content)
	f := &inmemFetcher{blobs: map[string][]byte{digest: []byte(content)}}

	if err := s.Materialize(hashA, entry, f.fetch); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	got, err := s.Get(hashA)
	if err != nil || got == nil {
		t.Fatalf("Get after materialize: %v (entry=%v)", err, got)
	}
	if got.Result.Status != "success" {
		t.Errorf("result status = %q, want success", got.Result.Status)
	}
	if got.Manifest == nil || len(got.Manifest.Files) != 1 {
		t.Fatalf("manifest not restored: %+v", got.Manifest)
	}
	data, err := os.ReadFile(filepath.Join(got.FilesDir, "dist", "app.js"))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(data) != content {
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

	// Seed the CAS with the content via a normal Put.
	content := "shared bytes"
	putWithFile(t, s, hashA, "a.txt", content)

	// Materialize a different key referencing the same content. The fetcher has
	// no blobs: if Materialize tried to fetch, it would fail.
	entry, _ := hitEntry("b.txt", content)
	f := &inmemFetcher{blobs: map[string][]byte{}}

	if err := s.Materialize(hashB, entry, f.fetch); err != nil {
		t.Fatalf("Materialize with local dedup: %v", err)
	}
	if len(f.opened) != 0 {
		t.Errorf("expected no fetch for an already-local blob, fetched: %v", f.opened)
	}
	if n := countCASBlobs(t, dir); n != 1 {
		t.Errorf("expected the blob to be deduplicated to 1 CAS entry, got %d", n)
	}
	if got := s.added.Load(); got != int64(len(content)) {
		t.Errorf("added = %d, want only the Put's %d bytes: a reused blob adds nothing", got, len(content))
	}
}

func TestMaterialize_ParallelMultiFileAndDedup(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	// A TypeScript-build-shaped manifest: many tiny declaration files. Two files
	// share identical content to exercise CAS dedup under the concurrent fan-out.
	manifest := &cache.Manifest{}
	blobs := map[string][]byte{}
	want := map[string]string{}
	for i := range 64 {
		rel := fmt.Sprintf("dist/types/m%02d.d.ts", i)
		content := fmt.Sprintf("export declare const x%d: number;", i)
		digest := cache.DigestOf([]byte(content))
		manifest.Files = append(manifest.Files, cache.FileEntry{Path: rel, Digest: digest, Mode: 0o644, Size: int64(len(content))})
		blobs[digest] = []byte(content)
		want[rel] = content
	}
	shared := "export {};"
	sharedDigest := cache.DigestOf([]byte(shared))
	for _, rel := range []string{"dist/a.js", "dist/b.js"} {
		manifest.Files = append(manifest.Files, cache.FileEntry{Path: rel, Digest: sharedDigest, Mode: 0o644, Size: int64(len(shared))})
		want[rel] = shared
	}
	blobs[sharedDigest] = []byte(shared)

	entry := &Entry{
		Result:   &EntryResult{Status: "success"},
		Metadata: &EntryMetadata{Extension: "ext", Task: "build", Project: "pkg"},
		Manifest: manifest,
	}
	f := &inmemFetcher{blobs: blobs}

	if err := s.Materialize(hashA, entry, f.fetch); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	got, err := s.Get(hashA)
	if err != nil || got == nil {
		t.Fatalf("Get after materialize: %v", err)
	}
	for rel, content := range want {
		data, err := os.ReadFile(filepath.Join(got.FilesDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read materialized %s: %v", rel, err)
		}
		if string(data) != content {
			t.Errorf("%s = %q, want %q", rel, data, content)
		}
	}

	// Shared content is stored once: 64 distinct + 1 shared = 65 CAS blobs, and
	// every distinct digest was fetched (the shared one possibly twice via the
	// publish race — storage still dedups it).
	distinct := len(blobs)
	if n := countCASBlobs(t, dir); n != distinct {
		t.Errorf("CAS blobs = %d, want %d", n, distinct)
	}
	if opened := f.distinctOpened(); len(opened) != distinct {
		t.Errorf("fetched %d distinct digests, want %d", len(opened), distinct)
	}
	// The racing loser's staged copy is discarded, not published, so the
	// shared blob counts once however the fan-out interleaved.
	var distinctBytes int64
	for _, b := range blobs {
		distinctBytes += int64(len(b))
	}
	if got := s.added.Load(); got != distinctBytes {
		t.Errorf("added = %d, want %d (each distinct blob once)", got, distinctBytes)
	}
}

func TestMaterialize_RejectsCorruptBlob(t *testing.T) {
	dir := t.TempDir()
	s := NewLocalStore(dir)

	entry, digest := hitEntry("dist/app.js", "expected content")
	// Fetcher returns bytes that do not match the manifest digest.
	f := &inmemFetcher{blobs: map[string][]byte{digest: []byte("tampered content")}}

	err := s.Materialize(hashA, entry, f.fetch)
	if err == nil {
		t.Fatal("expected an error for a blob that does not match its digest")
	}
	if !strings.Contains(err.Error(), "content-addresses to") {
		t.Errorf("error should explain the digest mismatch, got: %v", err)
	}
	if got, _ := s.Get(hashA); got != nil {
		t.Error("a failed materialize must not leave a committed entry")
	}
	if n := countCASBlobs(t, dir); n != 0 {
		t.Errorf("a corrupt blob must not be published to CAS, got %d", n)
	}
}

func TestMaterialize_RequiresFetcherForMissingBlob(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	entry, _ := hitEntry("x.txt", "bytes not in cas")

	err := s.Materialize(hashA, entry, nil)
	if err == nil {
		t.Fatal("expected an error materializing a missing blob without a fetcher")
	}
	if !strings.Contains(err.Error(), "no fetcher") {
		t.Errorf("error should mention the missing fetcher, got: %v", err)
	}
}

func TestMaterialize_RequiresManifest(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	err := s.Materialize(hashA, &Entry{Result: &EntryResult{Status: "success"}}, nil)
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
