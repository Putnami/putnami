// Package cachepolicy owns the lifecycle of the two caches this extension
// creates.
//
//   - The ts-types SCRATCH under the worktree cache root: the .tsbuildinfo plus
//     the warm declaration mirror introduced with the warm mirror. Per-worktree, written by
//     this extension, meaningless to anyone else.
//   - Bun's machine-global PACKAGE cache: downloaded tarballs, extracted package
//     trees and registry metadata, shared by every repository on the host.
//
// Core now knows only that this extension declares the reserved `cache-clean`
// and `cache-gc` commands.
//
// Bun's native cache LOCATION is deliberately preserved. ~/.bun/install/cache is
// where every existing checkout's downloads already are, and relocating it as
// part of an ownership change would re-download the world once and orphan the
// old tree forever. Ownership moved; the bytes did not.
//
// clean is cold-reset semantics: the ts-types tree goes entirely, so the next
// build re-type-checks from scratch. Bun's cache is NOT wiped by clean — it is
// re-downloadable but expensive, it is shared with every other repository on the
// machine, and a `putnami cache clean` in one worktree has no business costing
// every other checkout its package downloads. It is bounded by gc instead.
package cachepolicy

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/cachepolicy"
)

const (
	// PhaseClean and PhaseGC are the subcommand names core invokes, declared in
	// the manifest's cache-clean-exec / cache-gc-exec tasks.
	PhaseClean = "cache-clean"
	PhaseGC    = "cache-gc"

	// scratchSubdir is the ts-types scratch root under the worktree cache root.
	scratchSubdir = "ts-types"

	// scratchGraceEnv overrides the ts-types grace window. The default is
	// generous because a ts-types entry is incremental build warmth: evicting a
	// recently-used one only forces a cold re-type-check.
	scratchGraceEnv     = "PUTNAMI_TS_CACHE_GC_GRACE"
	defaultScratchGrace = 14 * 24 * time.Hour

	// Bun cache settings. The names are unchanged from the CLI-owned collector,
	// so an environment that tuned the Bun cache keeps tuning it.
	bunCacheDirEnv     = "PUTNAMI_BUN_CACHE_DIR"
	bunNativeDirEnv    = "BUN_INSTALL_CACHE_DIR"
	bunMaxBytesEnv     = "PUTNAMI_BUN_CACHE_MAX_BYTES"
	bunGraceEnv        = "PUTNAMI_BUN_CACHE_GC_GRACE"
	defaultBunMaxBytes = int64(10 << 30)
	// Bun publishes no per-package access recency, so the only signal available
	// is download/extraction age. A long default grace is the honest response to
	// a weak signal: an old-but-hot package would otherwise be evicted on the
	// strength of a timestamp that never moves.
	defaultBunGrace = 24 * time.Hour

	// bunGlobalStoreDir holds the hardlink/clonefile virtual store Bun
	// materializes node_modules from. It is accounted but never evicted:
	// removing an entry there breaks the node_modules trees pointing at it.
	bunGlobalStoreDir = "links"

	// extensionCacheRootEnv is the generic per-extension machine root the C5
	// contract provides. Used only when Bun's own location cannot be resolved.
	extensionCacheRootEnv = "PUTNAMI_EXTENSION_CACHE_ROOT"
)

// Run executes one cache phase and writes the freed-bytes summary event to out.
//
// scratchRoot is the worktree cache root (PUTNAMI_CACHE_ROOT). An unrecognized
// phase frees nothing and still emits a zero summary, so the caller always reads
// a result rather than waiting on an empty stream.
func Run(phase, scratchRoot string, getenv func(string) string, out io.Writer) error {
	var freed int64
	switch phase {
	case PhaseClean:
		if scratchRoot != "" {
			freed += cleanScratch(filepath.Join(scratchRoot, scratchSubdir))
		}
	case PhaseGC:
		if scratchRoot != "" {
			freed += gcScratch(filepath.Join(scratchRoot, scratchSubdir), resolveScratchGrace(getenv))
		}
		freed += gcBunCache(resolveBunCacheRoot(getenv), resolveBunOptions(getenv))
	}
	return cachepolicy.WriteSummary(out, freed)
}

// cleanScratch removes the entire ts-types tree and returns the bytes
// reclaimed. A missing tree frees nothing.
func cleanScratch(root string) int64 {
	return cachepolicy.RemoveTree(root)
}

// gcScratch evicts ts-types files not modified within grace and prunes the
// directories left empty, returning the bytes reclaimed.
//
// One project's incremental set is written together on each build, so a stale
// project's files share an old mtime and are removed as a unit; a
// partially-removed set is harmless because tsc treats a missing or incomplete
// .tsbuildinfo as a cold start. That is why this one is age-based rather than
// budget-based: there is no correctness cliff here, only a warm/cold trade.
func gcScratch(root string, grace time.Duration) int64 {
	if grace < 0 {
		grace = 0
	}
	cutoff := time.Now().Add(-grace)
	var freed int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if os.Remove(path) == nil {
			freed += info.Size()
		}
		return nil
	})
	cachepolicy.PruneEmptyDirs(root)
	return freed
}

// resolveBunCacheRoot returns Bun's machine-global package cache, or "" when no
// location can be resolved.
//
// Order: the Putnami override, Bun's own standard override, Bun's native
// default, then the contract-provided extension cache root. Bun's native default
// stays ahead of the contract root on purpose — see the package comment.
func resolveBunCacheRoot(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	if dir := strings.TrimSpace(getenv(bunCacheDirEnv)); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(getenv(bunNativeDirEnv)); dir != "" {
		return dir
	}
	if home := strings.TrimSpace(getenv("HOME")); home != "" {
		return filepath.Join(home, ".bun", "install", "cache")
	}
	if extensionCache := strings.TrimSpace(getenv(extensionCacheRootEnv)); extensionCache != "" {
		return filepath.Join(extensionCache, "bun")
	}
	return ""
}

func resolveScratchGrace(getenv func(string) string) time.Duration {
	return cachepolicy.EnvDuration(getenv, scratchGraceEnv, defaultScratchGrace)
}

func resolveBunOptions(getenv func(string) string) cachepolicy.Options {
	return cachepolicy.Options{
		MaxBytes: cachepolicy.EnvInt64(getenv, bunMaxBytesEnv, defaultBunMaxBytes),
		Grace:    cachepolicy.EnvDuration(getenv, bunGraceEnv, defaultBunGrace),
	}
}

// gcBunCache bounds Bun's package cache and returns the bytes reclaimed.
func gcBunCache(root string, opts cachepolicy.Options) int64 {
	if root == "" {
		return 0
	}
	candidates, totalBytes, err := scanBunCache(root)
	if err != nil {
		return 0
	}
	res, collectErr := cachepolicy.Collect(root, totalBytes, candidates, opts)
	if collectErr != nil || res == nil {
		return 0
	}
	if res.EvictedEntries > 0 {
		removeEmptyBunContainers(root)
	}
	return res.FreedBytes
}

// scanBunCache counts every byte beneath root while discovering ONLY entries
// whose layout Bun documents or has emitted in supported releases: direct
// package directories, one-level scope/package containers, .npm registry
// metadata, and cached Bun compiler binaries.
//
// Everything else — the `links` virtual store, and any directory shape a future
// Bun release introduces — counts against the budget but is never removed. Bun
// does not participate in this collector's lock, so an unrecognized entry is one
// whose safety cannot be reasoned about, and evicting it is how a collector
// breaks the node_modules trees it was supposed to leave alone.
func scanBunCache(root string) ([]cachepolicy.Entry, int64, error) {
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}

	var candidates []cachepolicy.Entry
	var total int64
	for _, entry := range rootEntries {
		name := entry.Name()
		if name == cachepolicy.LockFileName {
			continue
		}
		path := filepath.Join(root, name)
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			if isEvictableBunFile(name) {
				candidates = append(candidates, cachepolicy.Entry{
					Path:     path,
					Size:     info.Size(),
					LastUsed: info.ModTime(),
				})
			}
			continue
		}
		if !entry.IsDir() {
			continue
		}
		if name == bunGlobalStoreDir {
			total += cachepolicy.TreeSize(path)
			continue
		}
		if hasPackageManifest(path) {
			size := cachepolicy.TreeSize(path)
			total += size
			candidates = append(candidates, cachepolicy.Entry{
				Path:     path,
				Size:     size,
				LastUsed: info.ModTime(),
				Dir:      true,
			})
			continue
		}
		// A scope container (@scope/package) or a legacy one-level layout.
		children, readErr := os.ReadDir(path)
		if readErr != nil {
			continue
		}
		for _, child := range children {
			childInfo, childErr := child.Info()
			if childErr != nil {
				continue
			}
			childPath := filepath.Join(path, child.Name())
			if childInfo.Mode().IsRegular() {
				total += childInfo.Size()
				continue
			}
			if !child.IsDir() {
				continue
			}
			size := cachepolicy.TreeSize(childPath)
			total += size
			if hasPackageManifest(childPath) {
				candidates = append(candidates, cachepolicy.Entry{
					Path:     childPath,
					Size:     size,
					LastUsed: childInfo.ModTime(),
					Dir:      true,
				})
			}
		}
	}
	return candidates, total, nil
}

func hasPackageManifest(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "package.json"))
	return err == nil && info.Mode().IsRegular()
}

// isEvictableBunFile recognizes the two flat file shapes Bun writes at the cache
// root: cached Bun compiler binaries (bun-*) and registry metadata
// (<hex>-<hex>….npm).
func isEvictableBunFile(name string) bool {
	if strings.HasPrefix(name, "bun-") {
		return true
	}
	base := strings.TrimSuffix(name, ".npm")
	if base == name || base == "" {
		return false
	}
	for _, part := range strings.Split(base, "-") {
		if part == "" || !isLowerHex(part) {
			return false
		}
	}
	return true
}

func isLowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return value != ""
}

// removeEmptyBunContainers drops one-level scope/legacy containers a successful
// eviction emptied. Unrecognized empty directories are left exactly as Bun or
// the user created them.
func removeEmptyBunContainers(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == bunGlobalStoreDir {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if hasPackageManifest(path) {
			continue
		}
		if children, readErr := os.ReadDir(path); readErr == nil && len(children) == 0 {
			_ = os.Remove(path)
		}
	}
}
