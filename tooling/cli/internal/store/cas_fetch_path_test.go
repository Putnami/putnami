package store

import (
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
)

// fetchOne fetches the one-file manifest whose file sits at rel into an empty
// store. It returns the store root, the fetcher, so a test can tell whether any
// blob was fetched, and the error.
func fetchOne(t *testing.T, rel string) (string, *inmemFetcher, error) {
	t.Helper()
	content := []byte("remote artifact bytes")
	digest := cache.DigestOf(content)
	f := &inmemFetcher{blobs: map[string][]byte{digest: content}}
	root := t.TempDir()
	err := NewLocalStore(root).PrefetchBlobs(&cache.Manifest{Files: []cache.FileEntry{
		{Path: rel, Digest: digest, Mode: 0o644, Size: int64(len(content))},
	}}, f.fetch)
	return root, f, err
}

// TestMaterializeRefusesAManifestPathThatIsNotLocal pins the up-front path
// check of a remote fetch: a manifest path that leaves the files/ tree fails
// before any blob is fetched, and installs nothing.
func TestMaterializeRefusesAManifestPathThatIsNotLocal(t *testing.T) {
	for _, rel := range []string{"../escape", "dist/../../escape", "/abs/escape"} {
		t.Run(rel, func(t *testing.T) {
			root, f, err := fetchOne(t, rel)
			if err == nil || !strings.Contains(err.Error(), "is not a local path on this host") {
				t.Fatalf("PrefetchBlobs(%q) = %v, want the path refused", rel, err)
			}
			if fetched := f.distinctOpened(); len(fetched) != 0 {
				t.Errorf("fetched %v before refusing the manifest", fetched)
			}
			if n := countCASBlobs(t, root); n != 0 {
				t.Errorf("a refused manifest installed %d blobs", n)
			}
		})
	}
}

// TestMaterializeTaskEntryRefusesAPathThatLeavesItsOutput pins the same check
// on a whole restore: a payload path that is filed under a declared output but
// leaves the entry's files/ tree is refused before its blob is fetched, and
// publishes no entry.
func TestMaterializeTaskEntryRefusesAPathThatLeavesItsOutput(t *testing.T) {
	const content = "remote artifact bytes"
	s := NewLocalStore(t.TempDir())
	hit, f := distHit(t, hashA, map[string]string{"../../escape": content})
	digest := cache.DigestOf([]byte(content))

	_, err := s.MaterializeTaskEntry(hit, f.fetch)
	if err == nil || !strings.Contains(err.Error(), "is not a local path on this host") {
		t.Fatalf("MaterializeTaskEntry = %v, want the path refused", err)
	}
	if f.distinctOpened()[digest] {
		t.Error("the payload blob was fetched before the manifest was refused")
	}
	if got := publishedEntry(s, hashA); got != nil {
		t.Errorf("a refused restore published an entry: %+v", got)
	}
}
