package store

import (
	"strings"
	"testing"
)

// materializeOne restores a one-file remote hit whose file sits at rel. It
// returns the fetcher, so a test can tell whether any blob was fetched, and
// the restore's error.
func materializeOne(t *testing.T, s *LocalStore, rel string) (*inmemFetcher, error) {
	t.Helper()
	const content = "remote artifact bytes"
	entry, digest := hitEntry(rel, content)
	f := &inmemFetcher{blobs: map[string][]byte{digest: []byte(content)}}
	return f, s.Materialize(hashA, entry, f.fetch)
}

// TestMaterializeRefusesAManifestPathThatIsNotLocal pins the up-front path
// check of a remote restore: a manifest path that leaves the entry's files/
// tree fails the restore before any blob is fetched, and publishes nothing.
func TestMaterializeRefusesAManifestPathThatIsNotLocal(t *testing.T) {
	for _, rel := range []string{"../escape", "dist/../../escape", "/abs/escape"} {
		t.Run(rel, func(t *testing.T) {
			s := NewLocalStore(t.TempDir())
			f, err := materializeOne(t, s, rel)
			if err == nil || !strings.Contains(err.Error(), "is not a local path on this host") {
				t.Fatalf("Materialize(%q) = %v, want the path refused", rel, err)
			}
			if fetched := f.distinctOpened(); len(fetched) != 0 {
				t.Errorf("fetched %v before refusing the manifest", fetched)
			}
			if got, _ := s.Get(hashA); got != nil {
				t.Errorf("a refused restore published an entry: %+v", got)
			}
		})
	}
}
