// Package cachepolicy owns the lifecycle of the machine-global Go cache.
//
// An earlier migration moved this here from the CLI, which used to ship a Go
// collector, export the cache root into every job of every extension, and run
// that collector on the way out of every command. Core now knows only that this
// extension declares the reserved `cache-clean` and `cache-gc` commands; what
// they collect, and how, is decided in this file.
//
// GOCACHE is <root>/build and GOMODCACHE is <root>/mod, where root is resolved
// by toolchain.ResolveGoCacheRoot. The two halves are treated very differently:
//
//   - The BUILD cache is disposable, content-addressed compiler output. Losing
//     an entry turns the next lookup into a cache miss, so it is what both clean
//     and gc act on.
//   - The MODULE cache is immutable, broadly reusable, and exposes no reliable
//     per-module recency. It is ACCOUNTED against the budget — it is real disk —
//     but never evicted: re-downloading modules is pure churn, and removing part
//     of a module directory corrupts it.
//
// The dangerous case, and why gc is not just "clean with a filter": a concurrent
// `go` process does NOT uniformly tolerate a vanished build entry. An entry that
// is already absent at lookup is an ordinary miss, but a compiler that was
// handed the entry's path in its -importcfg FAILS when the file disappears
// underneath it. The collector's lock excludes other collectors, not the
// toolchain — so the grace window is what protects a running build, and it is
// widened below by Go's own mtime imprecision.
package cachepolicy

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/cachepolicy"
)

const (
	// PhaseClean and PhaseGC are the subcommand names core invokes, declared in
	// the manifest's cache-clean-exec / cache-gc-exec tasks.
	PhaseClean = "cache-clean"
	PhaseGC    = "cache-gc"

	// Budget and grace overrides. Both names are unchanged from the CLI-owned
	// collector, so an environment that tuned the Go cache keeps tuning it.
	maxBytesEnv = "PUTNAMI_GO_CACHE_MAX_BYTES"
	gcGraceEnv  = "PUTNAMI_GO_CACHE_GC_GRACE"

	defaultMaxBytes int64 = 10 << 30
	defaultGCGrace        = time.Hour

	// progDir is the directory the GOCACHEPROG helper keeps its action records
	// in (internal/gocacheprog), beside "build" and "mod" under the same root.
	// The helper keeps the compiled objects those records name in "build",
	// the go command's own directory, so one copy serves a go command with the
	// helper and one without it.
	//
	// It is collected exactly like the build cache. A clean that left it
	// would leave records naming objects that are gone, which read as misses
	// but still take disk. Helpers from before the objects moved into "build"
	// wrote "-d" files here too. This version never reads them, and an older
	// helper still running on the same machine (another workspace's pinned
	// extension) may, so they are not deleted on sight: the collector evicts
	// them oldest first like any other entry. The record format did not
	// change, and each version looks for a record's body only in its own data
	// directory, so a mix of versions costs misses, never a torn entry. The
	// helper writes Go's own entry layout (<shard>/<64 hex><-a|-d>), which is
	// what lets scanBuildCache read both trees without a second recognizer.
	progDir = "prog"

	// goMtimeInterval mirrors mtimeInterval in cmd/go/internal/cache: Go
	// refreshes an entry's mtime only once it is ALREADY this stale, so a file a
	// running compile is reading can present an mtime up to an hour old. A grace
	// window equal to that interval therefore has no margin at all. Go's own
	// trimmer subtracts the same interval on top of its age limit "to account
	// for the imprecision of our 'last used' mtimes"; this constant is that
	// correction.
	goMtimeInterval = time.Hour

	// maxDuration is the largest time.Duration, which the grace widening
	// saturates at instead of wrapping.
	maxDuration time.Duration = 1<<63 - 1
)

// Run executes one cache phase and writes the freed-bytes summary event to out.
//
// An unrecognized phase frees nothing and still emits a zero summary, so the
// caller always reads a result rather than waiting on an empty stream.
func Run(phase string, getenv func(string) string, out io.Writer) error {
	root := toolchain.ResolveGoCacheRoot(getenv)
	var freed int64
	if root != "" {
		switch phase {
		case PhaseClean:
			freed = Clean(root)
		case PhaseGC:
			freed = gc(root, resolveOptions(getenv))
		}
	}
	return cachepolicy.WriteSummary(out, freed)
}

// Clean removes the two build-cache trees — <root>/build (GOCACHE, which also
// holds the GOCACHEPROG helper's compiled objects) and <root>/prog (the
// helper's action records) — and returns the bytes reclaimed, preserving
// <root>/mod (GOMODCACHE).
//
// This one DOES accept racing a sibling worktree's in-flight `go build`: an
// explicit clean is a user saying "make the next build cold", and honoring it
// halfway would not. The background collector must never make the same trade,
// which is what the grace window in gc is for.
func Clean(root string) int64 {
	freed := cachepolicy.RemoveTree(toolchain.GoBuildCacheDir(root))
	return freed + cachepolicy.RemoveTree(filepath.Join(root, progDir))
}

// resolveOptions returns the effective collection settings for this
// environment. The budget spans the WHOLE Go cache (build plus modules) because
// that is what occupies the disk.
func resolveOptions(getenv func(string) string) cachepolicy.Options {
	maxBytes := cachepolicy.EnvInt64(getenv, maxBytesEnv, defaultMaxBytes)
	return cachepolicy.Options{
		MaxBytes: maxBytes,
		Grace:    widenGrace(cachepolicy.EnvDuration(getenv, gcGraceEnv, defaultGCGrace)),
		// Keep a fifth of the budget compiled even when the module cache alone
		// exceeds the low watermark. Without it the target is unreachable, every
		// evictable entry is deleted on every pass, and the machine ends up with
		// a permanently cold Go build cache that is STILL over budget.
		FloorBytes: maxBytes / 5,
	}
}

// widenGrace adds Go's mtime imprecision to a positive grace so "used within the
// last hour" means it.
//
// A caller asking for no grace at all (0) is opting out deliberately and keeps
// that. The saturation guard is not theoretical bookkeeping: a grace within
// goMtimeInterval of the largest Duration would overflow NEGATIVE, and a
// non-positive grace means "protect nothing" — turning the longest window a
// caller can ask for into the exact behavior the window exists to prevent.
func widenGrace(grace time.Duration) time.Duration {
	if grace <= 0 {
		return 0
	}
	if grace > maxDuration-goMtimeInterval {
		return maxDuration
	}
	return grace + goMtimeInterval
}

// gc bounds the shared Go cache and returns the bytes reclaimed.
func gc(root string, opts cachepolicy.Options) int64 {
	buildRoot := toolchain.GoBuildCacheDir(root)
	progRoot := filepath.Join(root, progDir)
	candidates, buildBytes := scanBuildCache(buildRoot)
	progCandidates, progBytes := scanBuildCache(progRoot)
	candidates = append(candidates, progCandidates...)
	moduleBytes := cachepolicy.TreeSize(filepath.Join(root, "mod"))

	res, err := cachepolicy.Collect(root, buildBytes+progBytes+moduleBytes, candidates, opts)
	if err != nil || res == nil {
		return 0
	}
	// The helper writes a compiled object through a temp file beside its final
	// path in <root>/build, and a record through one in <root>/prog, so an
	// interrupted helper can leave its temp files in either tree.
	freed := res.FreedBytes +
		sweepStaleTempFiles(buildRoot, opts.Grace) +
		sweepStaleTempFiles(progRoot, opts.Grace)
	if res.EvictedEntries > 0 {
		removeEmptyBuildShards(buildRoot)
		removeEmptyBuildShards(progRoot)
	}
	return freed
}

// The GOCACHEPROG helper writes through temp files named put-*.tmp (a body, in
// <root>/build) and rec-*.tmp (a record, in <root>/prog) before renaming them
// into place (internal/gocacheprog).
const (
	helperBodyTempPrefix   = "put-"
	helperRecordTempPrefix = "rec-"
	helperTempSuffix       = ".tmp"
)

// isHelperTempFile reports whether name is one of the helper's own temp file
// names. The sweep matches these names exactly because <root>/build is the go
// command's directory too: a file the helper did not name is not the helper's
// to delete.
func isHelperTempFile(name string) bool {
	return strings.HasSuffix(name, helperTempSuffix) &&
		(strings.HasPrefix(name, helperBodyTempPrefix) || strings.HasPrefix(name, helperRecordTempPrefix))
}

// sweepStaleTempFiles removes the helper's temp files an interrupted process
// left in a shard directory of one tree — a helper killed mid-write never gets
// to rename or unlink them — and returns the bytes reclaimed.
//
// The sweep is UNCONDITIONAL rather than budgeted: a temp file is never read
// by anyone once its writer is gone, so keeping it has no value at any budget.
// It is still gated by the grace window, because a temp file younger than that
// may belong to a helper that is writing it right now. A zero grace — an
// explicit clean — removes the whole tree anyway (Clean), so this only ever
// runs with the widened background window.
func sweepStaleTempFiles(treeRoot string, grace time.Duration) int64 {
	if grace <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-grace)
	var freed int64
	_ = filepath.WalkDir(treeRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !isHelperTempFile(entry.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(treeRoot, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 || len(parts[0]) != 2 || !isLowerHex(parts[0]) {
			return nil // only the shard layout the helper writes
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			return nil
		}
		if os.Remove(path) == nil {
			freed += info.Size()
		}
		return nil
	})
	return freed
}

// scanBuildCache returns the evictable entries and the total bytes beneath one
// build-cache tree. It is called once for <root>/build and once for
// <root>/prog, which share the entry layout.
//
// Only files matching Go's own build-cache entry layout — a 64-character hex
// name with a "-a"/"-d" suffix, under a two-character hex shard — are eviction
// candidates. Everything else under the build root (Go's trim log, its lock
// file, anything a future Go release adds) is counted against the budget and
// deliberately left alone: deleting a file whose meaning is unknown is how a
// collector corrupts the cache it was supposed to bound.
func scanBuildCache(buildRoot string) ([]cachepolicy.Entry, int64) {
	var candidates []cachepolicy.Entry
	var totalBytes int64
	_ = filepath.WalkDir(buildRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		totalBytes += info.Size()
		if info.Mode().IsRegular() && isBuildCacheEntry(buildRoot, path) {
			candidates = append(candidates, cachepolicy.Entry{
				Path:     path,
				Size:     info.Size(),
				LastUsed: info.ModTime(),
			})
		}
		return nil
	})
	return candidates, totalBytes
}

func isBuildCacheEntry(buildRoot, path string) bool {
	rel, err := filepath.Rel(buildRoot, path)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || len(parts[0]) != 2 || !isLowerHex(parts[0]) {
		return false
	}
	name := parts[1]
	return len(name) == 66 && isLowerHex(name[:64]) && (name[64:] == "-a" || name[64:] == "-d")
}

func isLowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return value != ""
}

// removeEmptyBuildShards drops the two-character shard directories a collection
// emptied, in one build-cache tree. Only recognized shard names are removed,
// for the same reason only recognized entries are evicted.
func removeEmptyBuildShards(buildRoot string) {
	entries, err := os.ReadDir(buildRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 2 || !isLowerHex(entry.Name()) {
			continue
		}
		path := filepath.Join(buildRoot, entry.Name())
		if children, readErr := os.ReadDir(path); readErr == nil && len(children) == 0 {
			_ = os.Remove(path)
		}
	}
}
