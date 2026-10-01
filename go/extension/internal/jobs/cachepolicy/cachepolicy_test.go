package cachepolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/go/extension/internal/toolchain"
	sdkcache "go.putnami.dev/sdk/extension/cachepolicy"
)

// envFunc builds the lookup the cache commands read their settings through,
// so a test never depends on ambient process state.
func envFunc(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildEntry writes a file named the way Go names a build-cache entry, at the
// given age. Only these are eviction candidates.
func buildEntry(t *testing.T, buildRoot, name, suffix string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(buildRoot, name[:2], name+suffix)
	writeFile(t, path, size)
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

func entryName(seed int) string {
	return strings.Repeat(fmt.Sprintf("%02x", seed&0xff), 32)
}

func summaryFreed(t *testing.T, out []byte) int64 {
	t.Helper()
	var event struct {
		Data struct {
			FreedBytes int64 `json:"freedBytes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &event); err != nil {
		t.Fatalf("parse summary %q: %v", out, err)
	}
	return event.Data.FreedBytes
}

// TestRun_Clean removes the build cache (GOCACHE = <root>/build) and preserves
// the module cache (GOMODCACHE = <root>/mod).
func TestRun_Clean(t *testing.T) {
	root := t.TempDir()
	getenv := envFunc(map[string]string{"PUTNAMI_GO_CACHE_DIR": root})

	writeFile(t, filepath.Join(root, "build", "ab", "abc-d"), 200)
	writeFile(t, filepath.Join(root, "build", "trim.txt"), 40)
	modFile := filepath.Join(root, "mod", "cache", "download", "m")
	writeFile(t, modFile, 300)

	var out bytes.Buffer
	if err := Run(PhaseClean, getenv, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "build")); !os.IsNotExist(err) {
		t.Error("build cache should be removed for a cold rebuild")
	}
	if _, err := os.Stat(modFile); err != nil {
		t.Error("module cache must be preserved (no re-download churn)")
	}
	if freed := summaryFreed(t, out.Bytes()); freed != 240 {
		t.Errorf("freed = %d, want 240", freed)
	}
}

// TestRun_CleanMissing frees nothing when there is no build cache yet.
func TestRun_CleanMissing(t *testing.T) {
	var out bytes.Buffer
	if err := Run(PhaseClean, envFunc(map[string]string{"PUTNAMI_GO_CACHE_DIR": t.TempDir()}), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if freed := summaryFreed(t, out.Bytes()); freed != 0 {
		t.Errorf("freed = %d, want 0", freed)
	}
}

// TestRun_UnknownPhase is a no-op zero summary.
func TestRun_UnknownPhase(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "build", "ab", "abc-d"), 200)

	var out bytes.Buffer
	if err := Run("cache-something-else", envFunc(map[string]string{"PUTNAMI_GO_CACHE_DIR": root}), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "build")); err != nil {
		t.Error("an unknown phase must not touch the build cache")
	}
	if freed := summaryFreed(t, out.Bytes()); freed != 0 {
		t.Errorf("freed = %d, want 0", freed)
	}
}

// TestRun_GCEvictsOldBuildEntriesAndSparesModules is the collector's core
// contract: over budget, the oldest recognized BUILD entries go and the module
// cache is untouched.
func TestRun_GCEvictsOldBuildEntriesAndSparesModules(t *testing.T) {
	root := t.TempDir()
	buildRoot := filepath.Join(root, "build")
	stale := make([]string, 0, 10)
	for i := range 10 {
		stale = append(stale, buildEntry(t, buildRoot, entryName(i), "-d", 100, 72*time.Hour))
	}
	modFile := filepath.Join(root, "mod", "cache", "download", "m")
	writeFile(t, modFile, 100)

	getenv := envFunc(map[string]string{
		"PUTNAMI_GO_CACHE_DIR":       root,
		"PUTNAMI_GO_CACHE_MAX_BYTES": "500",
	})
	var out bytes.Buffer
	if err := Run(PhaseGC, getenv, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(modFile); err != nil {
		t.Fatalf("module cache was evicted: %v", err)
	}
	if freed := summaryFreed(t, out.Bytes()); freed == 0 {
		t.Fatal("an over-budget cache freed nothing")
	}
	survivors := 0
	for _, path := range stale {
		if _, err := os.Stat(path); err == nil {
			survivors++
		}
	}
	if survivors == len(stale) {
		t.Fatal("no build entry was evicted")
	}
}

// TestGC_GraceProtectsEntriesARunningCompileMayHold is the invariant that keeps
// a background collection from breaking a concurrent build: a compiler handed an
// entry's path in its -importcfg FAILS when the file vanishes, so a recent entry
// is never a victim — even when that leaves the cache over budget.
func TestGC_GraceProtectsEntriesARunningCompileMayHold(t *testing.T) {
	root := t.TempDir()
	buildRoot := filepath.Join(root, "build")
	fresh := make([]string, 0, 10)
	for i := range 10 {
		fresh = append(fresh, buildEntry(t, buildRoot, entryName(i), "-a", 100, time.Minute))
	}

	freed := gc(root, sdkcache.Options{MaxBytes: 200, Grace: widenGrace(time.Hour)})
	if freed != 0 {
		t.Fatalf("freed %d bytes from inside the grace window", freed)
	}
	for _, path := range fresh {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("grace-protected entry %s was evicted: %v", path, err)
		}
	}
}

// TestGC_LeavesUnrecognizedFilesAlone: anything that is not a Go build-cache
// entry counts toward the budget but is never removed. Deleting a file whose
// meaning is unknown is how a collector corrupts the cache it bounds.
func TestGC_LeavesUnrecognizedFilesAlone(t *testing.T) {
	root := t.TempDir()
	buildRoot := filepath.Join(root, "build")
	trim := filepath.Join(buildRoot, "trim.txt")
	writeFile(t, trim, 100)
	old := time.Now().Add(-500 * time.Hour)
	if err := os.Chtimes(trim, old, old); err != nil {
		t.Fatal(err)
	}
	buildEntry(t, buildRoot, entryName(1), "-d", 100, 500*time.Hour)

	gc(root, sdkcache.Options{MaxBytes: 50})

	if _, err := os.Stat(trim); err != nil {
		t.Fatalf("an unrecognized build-root file was removed: %v", err)
	}
}

// TestWidenGrace covers the three arms, including the saturation guard that
// keeps the LONGEST requested window from wrapping negative and meaning
// "protect nothing".
func TestWidenGrace(t *testing.T) {
	if got := widenGrace(0); got != 0 {
		t.Errorf("widenGrace(0) = %v, want 0 (a deliberate opt-out)", got)
	}
	if got := widenGrace(-time.Hour); got != 0 {
		t.Errorf("widenGrace(negative) = %v, want 0", got)
	}
	if got := widenGrace(5 * time.Minute); got != 5*time.Minute+goMtimeInterval {
		t.Errorf("widenGrace(5m) = %v, want 5m+%v", got, goMtimeInterval)
	}
	if got := widenGrace(maxDuration); got != maxDuration {
		t.Errorf("widenGrace(max) = %v, want saturation at %v", got, maxDuration)
	}
	if widenGrace(maxDuration) <= 0 {
		t.Fatal("the widest requestable grace wrapped to 'protect nothing'")
	}
}

// TestGoCacheRootLivesOutsideTheBuildStore pins that the root this collector
// bounds is NOT inside the CLI's build store, so the store's 10 GiB budget
// neither counts nor evicts the Go cache, and this package's own budget
// (resolveOptions, pinned by TestResolveOptions) is the one that applies
// to build, prog and mod. The store root is the one core hands every job:
// PUTNAMI_OCI_CACHE_ROOT is <store root>/oci (protocols/extension
// SharedOCILayerCacheRootEnv), and every store sits under ~/.putnami/store. The
// CLI's TestStoreBudget_LeavesExtensionMachineCachesAlone pins the other side.
func TestGoCacheRootLivesOutsideTheBuildStore(t *testing.T) {
	home := t.TempDir()
	storeParent := filepath.Join(home, ".putnami", "store")
	storeRoot := filepath.Join(storeParent, "0123456789abcdef")
	jobEnv := map[string]string{
		"HOME":                         home,
		"USERPROFILE":                  home,
		"PUTNAMI_OCI_CACHE_ROOT":       filepath.Join(storeRoot, "oci"),
		"PUTNAMI_EXTENSION_CACHE_ROOT": filepath.Join(home, ".putnami", "cache", "extensions", "@putnami-go"),
	}
	relocated := map[string]string{"PUTNAMI_HOME": filepath.Join(home, "relocated")}
	for key, value := range jobEnv {
		relocated[key] = value
	}

	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"default", jobEnv, filepath.Join(home, ".putnami", "cache", "go")},
		{"relocated putnami home", relocated, filepath.Join(home, "relocated", "cache", "go")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := toolchain.ResolveGoCacheRoot(envFunc(tc.env))
			if root != tc.want {
				t.Fatalf("Go cache root = %q, want %q", root, tc.want)
			}
			ociParent := filepath.Dir(tc.env["PUTNAMI_OCI_CACHE_ROOT"])
			for _, tree := range []string{"build", progDir, "mod"} {
				dir := filepath.Join(root, tree)
				for _, storeDir := range []string{ociParent, storeParent} {
					rel, err := filepath.Rel(storeDir, dir)
					if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						t.Errorf("%s lies inside the build store %s: the store budget would own it", dir, storeDir)
					}
				}
			}
		})
	}
}

func TestResolveOptions(t *testing.T) {
	opts := resolveOptions(envFunc(nil))
	if opts.MaxBytes != defaultMaxBytes {
		t.Errorf("default MaxBytes = %d, want %d", opts.MaxBytes, defaultMaxBytes)
	}
	if opts.Grace != defaultGCGrace+goMtimeInterval {
		t.Errorf("default Grace = %v, want %v", opts.Grace, defaultGCGrace+goMtimeInterval)
	}
	if opts.FloorBytes != defaultMaxBytes/5 {
		t.Errorf("FloorBytes = %d, want a fifth of the budget", opts.FloorBytes)
	}

	overridden := resolveOptions(envFunc(map[string]string{
		"PUTNAMI_GO_CACHE_MAX_BYTES": "1000",
		"PUTNAMI_GO_CACHE_GC_GRACE":  "0s",
	}))
	if overridden.MaxBytes != 1000 {
		t.Errorf("overridden MaxBytes = %d, want 1000", overridden.MaxBytes)
	}
	if overridden.Grace != 0 {
		t.Errorf("overridden Grace = %v, want 0 (an explicit opt-out is not widened)", overridden.Grace)
	}
}

// TestClean_EmptiesTheHelperCacheToo pins that a clean empties both trees the
// GOCACHEPROG helper writes: its compiled objects live in <root>/build beside
// the go command's own, and its action records in <root>/prog. Leaving
// <root>/prog would keep records, and on a tree written before the objects
// moved, whole objects, that a user asking for a cold rebuild expects gone.
func TestClean_EmptiesTheHelperCacheToo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "build", "aa", entryName(1)+"-d"), 100)
	writeFile(t, filepath.Join(root, progDir, "bb", entryName(2)+"-d"), 250)
	writeFile(t, filepath.Join(root, "mod", "cache", "download", "module.info"), 700)

	freed := Clean(root)
	if freed != 350 {
		t.Fatalf("Clean freed %d bytes, want 350 (both build trees)", freed)
	}
	if _, err := os.Stat(filepath.Join(root, progDir)); !os.IsNotExist(err) {
		t.Fatalf("the helper cache survived a clean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "mod")); err != nil {
		t.Fatalf("the module cache must survive a clean: %v", err)
	}
}

// TestGC_BoundsTheHelperCacheWithTheSameBudget pins the other half: the helper
// tree is accounted against the machine budget and evicted by the same age
// rule, with the same grace window protecting what a running compile holds.
func TestGC_BoundsTheHelperCacheWithTheSameBudget(t *testing.T) {
	root := t.TempDir()
	progRoot := filepath.Join(root, progDir)
	old := buildEntry(t, progRoot, entryName(3), "-d", 4096, 48*time.Hour)
	recent := buildEntry(t, progRoot, entryName(4), "-d", 4096, time.Minute)

	freed := gc(root, sdkcache.Options{MaxBytes: 1024, Grace: time.Hour})
	if freed == 0 {
		t.Fatal("gc freed nothing from the helper cache")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("a stale helper entry survived: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("a recently used helper entry was evicted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(progRoot, entryName(3)[:2])); !os.IsNotExist(err) {
		t.Fatalf("an emptied helper shard was left behind: %v", err)
	}
}

// TestGC_SweepsTheHelpersStaleTempFiles pins the orphan rule: a helper killed
// mid-write leaves put-*.tmp / rec-*.tmp files nobody will ever rename, and the
// collector removes them once they are older than the grace window — but not
// before, because a young one may still be under a live writer's pen.
func TestGC_SweepsTheHelpersStaleTempFiles(t *testing.T) {
	root := t.TempDir()
	progRoot := filepath.Join(root, progDir)
	stale := filepath.Join(progRoot, "ab", "put-123.tmp")
	fresh := filepath.Join(progRoot, "ab", "rec-456.tmp")
	elsewhere := filepath.Join(progRoot, "notashard", "put-789.tmp")
	writeFile(t, stale, 300)
	writeFile(t, fresh, 10)
	writeFile(t, elsewhere, 10)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age temp file: %v", err)
	}
	if err := os.Chtimes(elsewhere, old, old); err != nil {
		t.Fatalf("age temp file: %v", err)
	}

	// Well within budget: nothing is evicted, the sweep alone must act.
	freed := gc(root, sdkcache.Options{MaxBytes: 1 << 30, Grace: time.Hour})
	if freed != 300 {
		t.Fatalf("gc freed %d bytes, want 300 (the stale temp file only)", freed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the stale temp file survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a temp file inside the grace window was removed: %v", err)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatalf("a temp file outside the shard layout was removed: %v", err)
	}
}

// TestGC_SweepsTheHelpersStaleBodyTempFilesInTheBuildCache pins the sweep in
// the tree the helper shares with the go command. The helper writes a compiled
// object through put-*.tmp beside its final path in <root>/build, so an
// interrupted helper leaves its temp files there. Only the helper's own names
// are swept: <root>/build is the go command's directory too, and a file the
// helper did not name is not the helper's to delete.
func TestGC_SweepsTheHelpersStaleBodyTempFilesInTheBuildCache(t *testing.T) {
	root := t.TempDir()
	buildRoot := toolchain.GoBuildCacheDir(root)
	stale := filepath.Join(buildRoot, "cd", "put-123.tmp")
	fresh := filepath.Join(buildRoot, "cd", "put-456.tmp")
	notTheHelpers := filepath.Join(buildRoot, "cd", "other-789.tmp")
	trim := filepath.Join(buildRoot, "trim.txt")
	writeFile(t, stale, 300)
	writeFile(t, fresh, 10)
	writeFile(t, notTheHelpers, 10)
	writeFile(t, trim, 10)
	old := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{stale, notTheHelpers, trim} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}

	// Well within budget: nothing is evicted, the sweep alone must act.
	freed := gc(root, sdkcache.Options{MaxBytes: 1 << 30, Grace: time.Hour})
	if freed != 300 {
		t.Fatalf("gc freed %d bytes, want 300 (the stale helper temp file only)", freed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the helper's stale temp file in the build cache survived: %v", err)
	}
	for _, kept := range []string{fresh, notTheHelpers, trim} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s was removed: %v", kept, err)
		}
	}
}

// TestIsHelperTempFile pins the names the sweep treats as the helper's own.
func TestIsHelperTempFile(t *testing.T) {
	for name, want := range map[string]bool{
		"put-123.tmp":       true,
		"rec-456.tmp":       true,
		"put-123":           false,
		"blob-789.tmp":      false,
		"other.tmp":         false,
		entryName(1) + "-d": false,
	} {
		if got := isHelperTempFile(name); got != want {
			t.Errorf("isHelperTempFile(%q) = %t, want %t", name, got, want)
		}
	}
}
