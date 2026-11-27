package store

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// The index answers, for any write, at least every entry whose region
// overlaps it, so an invalidation that judges only those drops what a scan of
// every entry drops (ADR 0119).
func TestMemoRegionIndexFindsEveryOverlappingEntry(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	segments := []string{"a", "b", "ab", "a.b", "c"}
	randomPath := func() string {
		p := ""
		for range rng.IntN(4) {
			p += "/" + segments[rng.IntN(len(segments))]
		}
		if p == "" {
			return "/"
		}
		return p
	}
	randomRegion := func() InputRegion {
		switch rng.IntN(10) {
		case 0:
			return InputRegion{unbounded: true}
		case 1:
			return InputRegion{}
		}
		roots := make([]string, 1+rng.IntN(3))
		for i := range roots {
			roots[i] = randomPath()
		}
		if rng.IntN(5) == 0 {
			roots[0] += "/"
		}
		return InputRegion{roots: roots, spelled: len(roots)}
	}

	var index memoRegionIndex
	regions := map[string]InputRegion{}
	for step := range 2000 {
		key := fmt.Sprintf("k%d", rng.IntN(200))
		if previous, ok := regions[key]; ok {
			index.remove(key, previous)
			delete(regions, key)
		}
		if rng.IntN(4) != 0 {
			region := randomRegion()
			regions[key] = region
			index.add(key, region)
		}

		written := randomRegion()
		if written.unbounded {
			continue
		}
		found := index.overlapping(written)
		for key, region := range regions {
			if written.Overlaps(region) && !slices.Contains(found, key) {
				t.Fatalf("step %d: write %v overlaps %s's region %v, which the index does not name", step, written.roots, key, region.roots)
			}
		}
	}
}

// An invalidation drops a digest whose region the write reaches, and keeps
// one it does not, through the index.
func TestInvalidateJudgesTheIndexedEntries(t *testing.T) {
	var memo inputHashMemo
	memo.memoize("under", 0, "h1", PathsRegion([]string{"/w/p/src"}), nil)
	memo.memoize("above", 0, "h2", PathsRegion([]string{"/w"}), nil)
	memo.memoize("beside", 0, "h3", PathsRegion([]string{"/w/q"}), nil)
	memo.memoize("everywhere", 0, "h4", InputRegion{unbounded: true}, nil)
	memo.memoize("moved", 0, "h5", PathsRegion([]string{"/w/p/dist"}), nil)
	memo.memoize("moved", 0, "h5", PathsRegion([]string{"/w/q/dist"}), nil)

	memo.invalidate(PathsRegion([]string{"/w/p"}), nil, nil)
	for key, kept := range map[string]bool{"under": false, "above": false, "beside": true, "everywhere": false, "moved": true} {
		if _, _, ok := memo.lookup(key); ok != kept {
			t.Errorf("%s memoized after the write: %v, want %v", key, ok, kept)
		}
	}
	if len(memo.index.overlapping(PathsRegion([]string{"/w/p"}))) != 0 {
		t.Error("the index still names a dropped entry")
	}
}
