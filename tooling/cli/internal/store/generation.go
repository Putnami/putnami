package store

import (
	"os"
	"path/filepath"
	"strconv"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// The store keeps a monotonic "generation" counter, bumped once per cache-using
// build (a putnami run that touches this store). Each cache hit/store records
// the generation it occurred in, so GC can evict entries "not hit in the last N
// builds" (idle reclaim) — a staleness measure in terms of activity rather than
// wall-clock, so an idle machine doesn't reap a still-valued cache.

const (
	// generationFile holds the store's current build generation (decimal int64).
	generationFile = "generation"
	// genLockFile serializes generation bumps independently of the publish/GC
	// lock, so bumping never contends with an in-flight ingest or sweep.
	genLockFile = ".genlock"
)

// ensureGeneration bumps this store's generation exactly once for the lifetime
// of the LocalStore (i.e. once per build process), the first time the store is
// mutated by a hit or store. GC and clean never call it, so they don't advance
// the counter.
func (s *LocalStore) ensureGeneration() {
	s.genOnce.Do(func() { s.gen = s.bumpGeneration() })
}

// bumpGeneration atomically increments and returns the store's generation under
// a dedicated lock. Best-effort: on any lock/IO error it returns the read value
// without persisting, which only makes idle reclaim more conservative.
func (s *LocalStore) bumpGeneration() int64 {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return 0
	}
	if l, err := flock.Acquire(filepath.Join(s.root, genLockFile), true, false); err == nil {
		defer func() { _ = l.Release() }()
	}
	next := readGenerationFile(s.root) + 1
	writeGenerationFile(s.root, next)
	return next
}

// currentGeneration reads a store's generation without bumping it — GC uses it
// as the reference point for "builds since last hit".
func currentGeneration(root string) int64 {
	return readGenerationFile(root)
}

func readGenerationFile(root string) int64 {
	data, err := os.ReadFile(filepath.Join(root, generationFile))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(string(trimSpaceBytes(data)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func writeGenerationFile(root string, gen int64) {
	tmp, err := os.CreateTemp(root, "generation-")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.WriteString(strconv.FormatInt(gen, 10))
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, filepath.Join(root, generationFile)); err != nil {
		os.Remove(tmpName)
	}
}
