package cachepolicy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkcache "go.putnami.dev/sdk/extension/cachepolicy"
)

// envFunc builds the lookup the cache commands read their settings through, so
// a test never depends on ambient process state.
func envFunc(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func writeFile(t *testing.T, path string, size int, modAge time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if modAge > 0 {
		mod := time.Now().Add(-modAge)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
}

// bunPackageBytes is the size of each package writeBunPackage materializes.
const bunPackageBytes = 100

// writeBunPackage materializes an extracted package directory (the shape Bun
// leaves in its cache) at the given age.
func writeBunPackage(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "package.json"), 2, age)
	writeFile(t, filepath.Join(dir, "index.js"), bunPackageBytes-2, age)
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
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

// TestRun_Clean wipes the whole ts-types tree and reports the reclaimed bytes.
func TestRun_Clean(t *testing.T) {
	dir := t.TempDir()
	// ts-types/<cmd>/<nested/project/path>/types.tsbuildinfo — the project path
	// segment can itself be nested, so exercise that.
	writeFile(t, filepath.Join(dir, "ts-types", "build", "ts", "framework", "app", "types.tsbuildinfo"), 100, 0)
	writeFile(t, filepath.Join(dir, "ts-types", "build-types", "ts", "app", "mirror", "a.d.ts"), 50, 0)

	var out bytes.Buffer
	if err := Run(PhaseClean, dir, envFunc(nil), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ts-types")); !os.IsNotExist(err) {
		t.Error("ts-types tree should be removed after clean")
	}
	if freed := summaryFreed(t, out.Bytes()); freed != 150 {
		t.Errorf("freed = %d, want 150", freed)
	}
}

// TestRun_CleanLeavesBunCacheAlone: a clean in ONE worktree must not cost every
// other checkout on the machine its package downloads.
func TestRun_CleanLeavesBunCacheAlone(t *testing.T) {
	scratch := t.TempDir()
	bunRoot := t.TempDir()
	writeBunPackage(t, filepath.Join(bunRoot, "left-pad@1.0.0"), 0)

	var out bytes.Buffer
	getenv := envFunc(map[string]string{"PUTNAMI_BUN_CACHE_DIR": bunRoot})
	if err := Run(PhaseClean, scratch, getenv, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bunRoot, "left-pad@1.0.0", "package.json")); err != nil {
		t.Fatalf("clean removed a Bun package the whole machine shares: %v", err)
	}
}

// TestRun_CleanEmpty frees nothing (and does not error) when there is no scratch.
func TestRun_CleanEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := Run(PhaseClean, t.TempDir(), envFunc(nil), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if freed := summaryFreed(t, out.Bytes()); freed != 0 {
		t.Errorf("freed = %d, want 0", freed)
	}
}

// TestGCScratch evicts entries untouched within grace and keeps warm ones.
func TestGCScratch(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ts-types")
	staleFile := filepath.Join(root, "build", "old-proj", "types.tsbuildinfo")
	warmFile := filepath.Join(root, "build", "new-proj", "types.tsbuildinfo")
	writeFile(t, staleFile, 200, 30*24*time.Hour) // 30 days old
	writeFile(t, warmFile, 60, 0)                 // fresh

	freed := gcScratch(root, 14*24*time.Hour)

	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Error("stale entry should be evicted")
	}
	if _, err := os.Stat(filepath.Join(root, "build", "old-proj")); !os.IsNotExist(err) {
		t.Error("emptied stale directory should be pruned")
	}
	if _, err := os.Stat(warmFile); err != nil {
		t.Error("warm entry should be preserved")
	}
	if freed != 200 {
		t.Errorf("freed = %d, want 200", freed)
	}
}

// TestRun_EmptyCacheRoot emits a zero summary without touching disk.
func TestRun_EmptyCacheRoot(t *testing.T) {
	var out bytes.Buffer
	if err := Run(PhaseClean, "", envFunc(nil), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if freed := summaryFreed(t, out.Bytes()); freed != 0 {
		t.Errorf("freed = %d, want 0", freed)
	}
}

func TestRun_UnknownPhase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "ts-types", "build", "p", "types.tsbuildinfo"), 100, 0)

	var out bytes.Buffer
	if err := Run("cache-something-else", dir, envFunc(nil), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ts-types")); err != nil {
		t.Fatalf("an unknown phase touched the scratch tree: %v", err)
	}
	if freed := summaryFreed(t, out.Bytes()); freed != 0 {
		t.Errorf("freed = %d, want 0", freed)
	}
}

// TestResolveBunCacheRoot pins the resolution order. Bun's NATIVE default stays
// ahead of the contract-provided extension root: relocating it would re-download
// every package on the machine once and orphan the old tree forever.
func TestResolveBunCacheRoot(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "putnami override wins",
			env: map[string]string{
				"PUTNAMI_BUN_CACHE_DIR":        "/putnami/bun",
				"BUN_INSTALL_CACHE_DIR":        "/bun/native",
				"HOME":                         "/home/dev",
				"PUTNAMI_EXTENSION_CACHE_ROOT": "/machine/@putnami-typescript",
			},
			want: "/putnami/bun",
		},
		{
			name: "bun's own override next",
			env: map[string]string{
				"BUN_INSTALL_CACHE_DIR": "/bun/native",
				"HOME":                  "/home/dev",
			},
			want: "/bun/native",
		},
		{
			name: "bun's native default next",
			env:  map[string]string{"HOME": "/home/dev"},
			want: filepath.Join("/home/dev", ".bun", "install", "cache"),
		},
		{
			name: "contract root only when nothing else resolves",
			env:  map[string]string{"PUTNAMI_EXTENSION_CACHE_ROOT": "/machine/@putnami-typescript"},
			want: filepath.Join("/machine/@putnami-typescript", "bun"),
		},
		{name: "nothing resolvable", env: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBunCacheRoot(envFunc(tt.env)); got != tt.want {
				t.Errorf("resolveBunCacheRoot = %q, want %q", got, tt.want)
			}
		})
	}
	if got := resolveBunCacheRoot(nil); got != "" {
		t.Errorf("resolveBunCacheRoot(nil) = %q, want empty", got)
	}
}

// A Bun release that Putnami installed keeps its package cache under its
// install, toolchains/bun/bun-<version>/install/cache in the Putnami home. gc
// bounds each of those caches with the budget of Bun's native cache, and
// touches nothing else under the toolchains directory.
func TestRun_GCBoundsTheCacheOfEveryInstalledBun(t *testing.T) {
	home := t.TempDir()
	toolchains := filepath.Join(home, "toolchains", "bun")
	caches := []string{
		filepath.Join(toolchains, "bun-1.4.0", "install", "cache"),
		filepath.Join(toolchains, "bun-1.5.0", "install", "cache"),
	}
	for _, cache := range caches {
		for _, name := range []string{"a@1", "b@1", "c@1"} {
			writeBunPackage(t, filepath.Join(cache, name), 72*time.Hour)
		}
	}
	program := filepath.Join(toolchains, "bun-1.4.0", "bin", "bun")
	writeFile(t, program, 10, 72*time.Hour)
	stray := filepath.Join(toolchains, "notes", "install", "cache", "a@1")
	writeBunPackage(t, stray, 72*time.Hour)

	env := envFunc(map[string]string{
		"PUTNAMI_HOME":                home,
		"PUTNAMI_BUN_CACHE_DIR":       filepath.Join(t.TempDir(), "native"),
		"PUTNAMI_BUN_CACHE_MAX_BYTES": "250",
		"PUTNAMI_BUN_CACHE_GC_GRACE":  "1h",
	})
	if got := installedBunCacheRoots(env); len(got) != 2 || got[0] != caches[0] || got[1] != caches[1] {
		t.Fatalf("installedBunCacheRoots = %v, want %v", got, caches)
	}

	var out bytes.Buffer
	if err := Run(PhaseGC, "", env, &out); err != nil {
		t.Fatal(err)
	}
	// Each cache holds three packages over a budget of two and a half: gc
	// evicts one package of each.
	if freed := summaryFreed(t, out.Bytes()); freed != 2*bunPackageBytes {
		t.Errorf("freed = %d, want %d: one package of each installed Bun's cache", freed, 2*bunPackageBytes)
	}
	for _, cache := range caches {
		packages := 0
		for _, name := range []string{"a@1", "b@1", "c@1"} {
			if _, err := os.Stat(filepath.Join(cache, name, "package.json")); err == nil {
				packages++
			}
		}
		if packages != 2 {
			t.Errorf("%s holds %d packages after gc, want 2", cache, packages)
		}
	}
	for _, kept := range []string{program, filepath.Join(stray, "package.json")} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("gc removed %s: %v", kept, err)
		}
	}
}

// Without a Putnami home and without a workspace root there is no install to
// look for, and a home that holds no Bun lists none.
func TestInstalledBunCacheRoots_WithoutAnInstall(t *testing.T) {
	if got := installedBunCacheRoots(nil); got != nil {
		t.Errorf("no environment = %v, want none", got)
	}
	if got := installedBunCacheRoots(envFunc(nil)); got != nil {
		t.Errorf("empty environment = %v, want none", got)
	}
	if got := installedBunCacheRoots(envFunc(map[string]string{"PUTNAMI_HOME": t.TempDir()})); got != nil {
		t.Errorf("a home without Bun = %v, want none", got)
	}

	// With no home, the Putnami home is .putnami under the workspace root.
	workspace := t.TempDir()
	cache := filepath.Join(workspace, ".putnami", "toolchains", "bun", "bun-1.4.0", "install", "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	got := installedBunCacheRoots(envFunc(map[string]string{"PUTNAMI_WORKSPACE_ROOT": workspace}))
	if len(got) != 1 || got[0] != cache {
		t.Errorf("workspace home = %v, want [%s]", got, cache)
	}
}

// TestGCBunCache_EvictsOldestPackages is the collector's core contract.
func TestGCBunCache_EvictsOldestPackages(t *testing.T) {
	root := t.TempDir()
	names := []string{"a@1", "b@1", "c@1", "d@1", "e@1"}
	for _, name := range names {
		writeBunPackage(t, filepath.Join(root, name), 72*time.Hour)
	}

	freed := gcBunCache(root, sdkcache.Options{MaxBytes: 200, Grace: time.Hour})
	if freed == 0 {
		t.Fatal("an over-budget Bun cache freed nothing")
	}
	remaining := 0
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			remaining++
		}
	}
	if remaining == len(names) {
		t.Fatal("no package was evicted")
	}
}

// TestGCBunCache_SparesTheVirtualStore: `links` is the hardlink store every
// materialized node_modules points at. Evicting from it breaks trees on disk, so
// it is accounted against the budget and never removed.
func TestGCBunCache_SparesTheVirtualStore(t *testing.T) {
	root := t.TempDir()
	linked := filepath.Join(root, bunGlobalStoreDir, "left-pad", "index.js")
	writeFile(t, linked, 1000, 500*time.Hour)
	writeBunPackage(t, filepath.Join(root, "left-pad@1.0.0"), 500*time.Hour)

	gcBunCache(root, sdkcache.Options{MaxBytes: 100})

	if _, err := os.Stat(linked); err != nil {
		t.Fatalf("the virtual store was evicted: %v", err)
	}
}

// TestGCBunCache_LeavesUnrecognizedEntriesAlone: Bun does not take this
// collector's lock, so an entry whose layout is not recognized is one whose
// safety cannot be reasoned about.
func TestGCBunCache_LeavesUnrecognizedEntriesAlone(t *testing.T) {
	root := t.TempDir()
	mystery := filepath.Join(root, "something-new", "payload.bin")
	writeFile(t, mystery, 1000, 500*time.Hour)
	writeFile(t, filepath.Join(root, "notes.txt"), 500, 500*time.Hour)

	gcBunCache(root, sdkcache.Options{MaxBytes: 10})

	if _, err := os.Stat(mystery); err != nil {
		t.Fatalf("an unrecognized cache subtree was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err != nil {
		t.Fatalf("an unrecognized root file was removed: %v", err)
	}
}

// TestGCBunCache_GraceProtectsRecentDownloads: Bun publishes no access recency,
// so age is the only signal and a fresh download must never be a victim.
func TestGCBunCache_GraceProtectsRecentDownloads(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a@1", "b@1", "c@1"} {
		writeBunPackage(t, filepath.Join(root, name), time.Minute)
	}

	if freed := gcBunCache(root, sdkcache.Options{MaxBytes: 50, Grace: time.Hour}); freed != 0 {
		t.Fatalf("freed %d bytes from inside the grace window", freed)
	}
}

func TestGCBunCache_MissingRoot(t *testing.T) {
	if freed := gcBunCache("", sdkcache.Options{MaxBytes: 10}); freed != 0 {
		t.Errorf("unresolved root freed %d bytes", freed)
	}
	if freed := gcBunCache(filepath.Join(t.TempDir(), "absent"), sdkcache.Options{MaxBytes: 10}); freed != 0 {
		t.Errorf("missing root freed %d bytes", freed)
	}
}

func TestIsEvictableBunFile(t *testing.T) {
	for _, name := range []string{"bun-darwin-arm64", "abc123-def456.npm", "0a1b.npm"} {
		if !isEvictableBunFile(name) {
			t.Errorf("isEvictableBunFile(%q) = false", name)
		}
	}
	for _, name := range []string{"notes.txt", "package.json", ".npm", "zz-yy.npm", ""} {
		if isEvictableBunFile(name) {
			t.Errorf("isEvictableBunFile(%q) = true", name)
		}
	}
}
