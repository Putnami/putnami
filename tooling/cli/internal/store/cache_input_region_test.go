package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A region bounds every file its patterns can select, from the patterns alone,
// so a write at any path a digest may read overlaps it, and a write elsewhere
// does not.
func TestFileInputRegionBoundsEveryPatternShape(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "apps", "web")
	at := func(rel string) string { return filepath.Join(dir, filepath.FromSlash(rel)) }
	cases := []struct {
		name     string
		patterns []string
		reaches  []string
		misses   []string
	}{
		{
			name:     "no pattern is the whole project",
			patterns: nil,
			reaches:  []string{at(".gen/layers"), dir},
			misses:   []string{filepath.Join(ws, "apps", "api")},
		},
		{
			name:     "a recursive pattern stays under its static prefix",
			patterns: []string{"src/**/*.ts"},
			reaches:  []string{at("src/a.ts"), at("src"), dir},
			misses:   []string{at(".gen"), at("test/a.ts")},
		},
		{
			name:     "a leading recursive pattern reaches hidden output directories",
			patterns: []string{"**/*.go"},
			reaches:  []string{at(".gen/layers")},
			misses:   []string{filepath.Join(ws, "apps", "api")},
		},
		{
			name:     "a recursive pattern resolves its .. segments",
			patterns: []string{"../../**/putnami.json"},
			reaches:  []string{filepath.Join(ws, "libs", "core", "putnami.json")},
		},
		{
			name:     "a glob is bounded after the collector cleans it",
			patterns: []string{"a/*/../../x.txt"},
			reaches:  []string{at("x.txt")},
			misses:   []string{at("a/b")},
		},
		{
			name:     "a literal names one path",
			patterns: []string{"go.mod"},
			reaches:  []string{at("go.mod")},
			misses:   []string{at("go.sum")},
		},
		{
			name:     "an exclusion selects nothing",
			patterns: []string{"!**/*_test.go"},
			misses:   []string{dir, at("a_test.go")},
		},
		{
			name:     "a Go embed selector stays in the project",
			patterns: []string{"go-embed:build"},
			reaches:  []string{at("assets/logo.svg")},
			misses:   []string{filepath.Join(ws, "apps", "api")},
		},
		{
			name:     "an anchored Git pattern stays under its static prefix",
			patterns: []string{"git:src/**"},
			reaches:  []string{at("src/a.ts")},
			misses:   []string{at(".gen")},
		},
		{
			name:     "a literal Git pattern names one path",
			patterns: []string{"git:go.mod"},
			reaches:  []string{at("go.mod")},
			misses:   []string{at("go.sum")},
		},
		{
			name:     "a Git pattern whose wildcard matches .. is unbounded",
			patterns: []string{"git:**"},
			reaches:  []string{filepath.Join(ws, "apps", "api", "main.go"), filepath.Join(t.TempDir(), "elsewhere")},
		},
		{
			name:     "a Git pattern anchored only by .. is unbounded",
			patterns: []string{"git:../*/x"},
			reaches:  []string{filepath.Join(ws, "x")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			region := FileInputRegion(dir, tc.patterns)
			for _, path := range tc.reaches {
				if !region.Overlaps(PathsRegion([]string{path})) {
					t.Errorf("a write at %s does not overlap the region of %q", path, tc.patterns)
				}
			}
			for _, path := range tc.misses {
				if region.Overlaps(PathsRegion([]string{path})) {
					t.Errorf("a write at %s overlaps the region of %q", path, tc.patterns)
				}
			}
		})
	}
}

// Two spellings of one path that alias on a case-insensitive filesystem
// overlap, so a declared output never escapes a key that reads it under the
// other spelling.
func TestInputRegionFoldsCase(t *testing.T) {
	dir := t.TempDir()
	region := FileInputRegion(dir, []string{"Gen/**"})
	if !region.Overlaps(PathsRegion([]string{filepath.Join(dir, "gen", "layer.tar")})) {
		t.Fatal("a write under another case spelling of a keyed directory does not overlap it")
	}
}

// The index names exactly the owners whose region overlaps: an owner at, above
// or below a root, and an unbounded owner, and none beside it.
func TestRegionIndexNamesOverlappingOwners(t *testing.T) {
	ws := t.TempDir()
	at := func(rel string) string { return filepath.Join(ws, filepath.FromSlash(rel)) }
	var index RegionIndex
	index.Add("gen", PathsRegion([]string{at("web/.gen")}))
	index.Add("project", PathsRegion([]string{at("web")}))
	index.Add("sibling", PathsRegion([]string{at("web-admin")}))
	index.Add("other", PathsRegion([]string{at("api/.gen")}))
	index.Add("anywhere", InputRegion{unbounded: true})

	cases := []struct {
		region InputRegion
		want   []string
	}{
		{FileInputRegion(at("web"), []string{"**/*.go"}), []string{"anywhere", "gen", "project"}},
		{FileInputRegion(at("web"), []string{"src/**"}), []string{"anywhere", "project"}},
		{PathsRegion([]string{at("web/.gen/layers/a.tar")}), []string{"anywhere", "gen", "project"}},
		{FileInputRegion(ws, nil), []string{"anywhere", "gen", "other", "project", "sibling"}},
		{InputRegion{unbounded: true}, []string{"anywhere", "gen", "other", "project", "sibling"}},
		{FileInputRegion(at("web"), []string{"!**"}), nil},
	}
	for _, tc := range cases {
		got := index.Overlapping(tc.region)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Overlapping(%+v) = %v, want %v", tc.region, got, tc.want)
		}
	}
}

// A write drops the memoized digests whose region it reaches and only those:
// a key computed after the write reads the written bytes, and a digest the
// write cannot move keeps its entry.
func TestInvalidateFileHashesUnderDropsTheDigestsAWriteCanMove(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "src", "main.ts")
	layer := filepath.Join(dir, ".gen", "layers", "base.tar")
	writeRegionFile(t, source, "v1")
	writeRegionFile(t, layer, "old layer")

	cm := NewCacheManager(nil)
	digest := func(patterns ...string) string {
		t.Helper()
		hash, err := cm.HashFiles(dir, patterns, ProjectConfigScope{})
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	sources, layers := digest("src/**"), digest(".gen/layers/**")
	assets := cm.lookupExtraFilesHash(dir, []string{filepath.Join(dir, ".gen", "layers")})

	writeRegionFile(t, source, "v2")
	writeRegionFile(t, layer, "new layer")
	if digest(".gen/layers/**") != layers {
		t.Fatal("the memo did not hold the digest of the old layer, so this test cannot prove anything")
	}

	cm.InvalidateFileHashesUnder([]string{filepath.Join(dir, ".gen", "layers")})

	if digest(".gen/layers/**") == layers {
		t.Error("a digest of the written output survived the invalidation that named it")
	}
	if cm.lookupExtraFilesHash(dir, []string{filepath.Join(dir, ".gen", "layers")}) == assets {
		t.Error("an extra-file digest of the written output survived the invalidation that named it")
	}
	if digest("src/**") != sources {
		t.Error("a digest the write cannot reach was dropped: the invalidation is not targeted")
	}
}

// A digest computed while an invalidation lands may hold the bytes from before
// the write that caused it, so it is never memoized: the next lookup reads the
// tree again.
func TestDigestSpanningAnInvalidationIsNotMemoized(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, ".gen", "base.tar")
	writeRegionFile(t, layer, "old layer")

	cm := NewCacheManager(nil)
	original := memoizedHashFiles
	t.Cleanup(func() { memoizedHashFiles = original })
	memoizedHashFiles = func(dir string, patterns []string, scope ProjectConfigScope) (string, error) {
		hash, err := original(dir, patterns, scope)
		// The writer lands its output and invalidates while this digest of the
		// old bytes is still in flight.
		writeRegionFile(t, layer, "new layer")
		cm.InvalidateFileHashesUnder([]string{filepath.Dir(layer)})
		memoizedHashFiles = original
		return hash, err
	}

	stale, err := cm.HashFiles(dir, []string{".gen/**"}, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := cm.HashFiles(dir, []string{".gen/**"}, ProjectConfigScope{})
	if err != nil {
		t.Fatal(err)
	}
	if fresh == stale {
		t.Fatal("a digest computed across an invalidation was memoized and served after the write")
	}
}

func writeRegionFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
