package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// LocalStore is a filesystem-backed content-addressed store. The root is
// resolved by ResolveStoreRoot to a machine-global, per-repo location
// (~/.putnami/store/<repo-id>) shared across every worktree of a repo, so it is
// guarded for cross-process access (see lock.go) in addition to the in-process
// mutex. Layout:
//
//	<root>/
//	  blobs/{hash[0:2]}/{hash}/   action-cache entries
//	    meta.json
//	    result.json
//	    manifest.json             per-file CAS digests (entries with outputs)
//	    files/                    output files (hardlinks into cas/)
//	    lastused                  last-hit timestamp sidecar (GC recency)
//	  cas/{digest[0:2]}/{digest}  raw deduplicated file bytes
//	  tmp/                        staging area for atomic writes
//	  leases/{hash[0:2]}/{hash}.lease
//	                              transient compute/restore leases (not metered)
//	  .lock                       advisory lock (publishers shared, GC exclusive)
type LocalStore struct {
	root string // absolute path to the store root
	mu   sync.Mutex

	// Lease timings use defaults when zero. Tests shorten them to exercise
	// heartbeat and crashed-owner takeover without slowing the suite.
	leaseTTL       time.Duration
	leaseHeartbeat time.Duration
	leasePoll      time.Duration
	// leaseClock replaces time.Now for lease records when set, so a test that
	// is not about expiry cannot see a lease expire on a slow host.
	leaseClock func() time.Time

	// gen is this build's store generation, set once (lazily, on the first
	// hit/store) via ensureGeneration; genOnce guards that bump. Stamped onto
	// entries' lastUsed sidecar so GC can measure idle reclaim in builds.
	gen     int64
	genOnce sync.Once

	// added counts the CAS bytes this handle has written to disk: a blob it
	// renamed into cas/, never one that was already there (dedup) and never
	// entry metadata. It is the in-run budget's cheap measure of how much this
	// run has grown the store (gc_trigger.go), so it is read without a walk.
	added atomic.Int64
}

// manifestFilename is the per-entry CAS file manifest written inside a blob.
const manifestFilename = "manifest.json"

// NewLocalStore creates a LocalStore rooted at the given directory.
func NewLocalStore(root string) *LocalStore {
	return &LocalStore{root: root}
}

// Root returns the store root directory.
func (s *LocalStore) Root() string {
	return s.root
}

// Get retrieves a LEGACY cache entry by hash. Returns nil, nil if not found.
//
// A blob carrying an entry descriptor belongs to the task-owned model
// (task_entry.go) and reads as a MISS here: its files/ tree is laid out by
// declared-output id, so interpreting it as a legacy capture would restore a
// wrongly shaped tree. The two models also occupy disjoint addresses, so this
// guard is defense in depth rather than the primary separation — it holds even
// if a task-owned address is reached by another route.
func (s *LocalStore) Get(hash string) (*Entry, error) {
	blobDir := s.blobDir(hash)
	if isTaskOwnedBlob(blobDir) {
		return nil, nil
	}
	metaPath := filepath.Join(blobDir, "meta.json")
	resultPath := filepath.Join(blobDir, "result.json")

	// Read metadata
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read meta.json: %w", err)
	}

	meta, err := UnmarshalMetadata(metaData)
	if err != nil {
		return nil, fmt.Errorf("parse meta.json: %w", err)
	}

	// Read result
	resultData, err := os.ReadFile(resultPath)
	if err != nil {
		return nil, fmt.Errorf("read result.json: %w", err)
	}

	result, err := UnmarshalResult(resultData)
	if err != nil {
		return nil, fmt.Errorf("parse result.json: %w", err)
	}

	entry := &Entry{
		Result:   result,
		Metadata: meta,
	}

	// Check for files directory
	filesDir := filepath.Join(blobDir, "files")
	if info, err := os.Stat(filesDir); err == nil && info.IsDir() {
		entry.FilesDir = filesDir
	}

	// Load the CAS manifest if present (absent for entries without output files).
	if manifestData, err := os.ReadFile(filepath.Join(blobDir, manifestFilename)); err == nil {
		var m cache.Manifest
		if err := json.Unmarshal(manifestData, &m); err == nil {
			entry.Manifest = &m
		}
	}

	return entry, nil
}

// Put stores a cache entry under the given hash. Uses atomic rename for
// crash safety: writes to tmp/ first, then renames to blobs/.
//
// Cross-process safe: the whole CAS ingest + publish runs under the store's
// shared lock so a concurrent GC (exclusive) never sweeps a blob mid-ingest,
// and the publish is first-writer-wins — if a sibling worktree already
// published this (content-addressed, hence byte-identical) entry, theirs is
// kept rather than clobbered. The in-process mutex still serializes goroutines
// within this process.
func (s *LocalStore) Put(hash string, entry *Entry) error {
	s.ensureGeneration() // count this build before taking the store lock

	// Shared lock for the whole ingest+publish: coexists with sibling
	// publishers, excludes GC.
	releaseLock := s.lockShared()
	defer releaseLock()

	// Create staging directory (lock-free — each tmp dir is unique)
	tmpDir, err := s.createTmpDir()
	if err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) // cleanup on error

	// Write meta.json
	if entry.Metadata == nil {
		entry.Metadata = &EntryMetadata{Hash: hash, CreatedAt: time.Now()}
	}
	entry.Metadata.Hash = hash

	metaJSON, err := json.MarshalIndent(entry.Metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "meta.json"), metaJSON, 0o644); err != nil {
		return fmt.Errorf("write meta.json: %w", err)
	}

	// Write result.json
	if entry.Result != nil {
		resultJSON, err := json.MarshalIndent(entry.Result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "result.json"), resultJSON, 0o644); err != nil {
			return fmt.Errorf("write result.json: %w", err)
		}
	}

	// Ingest output files into the CAS and stage a files/ tree of hardlinks
	// plus a manifest of per-file digests. Throwaway intermediates (e.g. lcov
	// coverage temp shards) are filtered out so they never enter the cache.
	if entry.FilesDir != "" {
		destFiles := filepath.Join(tmpDir, "files")
		manifest, totalSize, err := s.ingestFiles(entry.FilesDir, destFiles)
		if err != nil {
			return fmt.Errorf("ingest files: %w", err)
		}

		// Write the manifest alongside the result.
		manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal manifest: %w", err)
		}
		if err := os.WriteFile(filepath.Join(tmpDir, manifestFilename), manifestJSON, 0o644); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}

		outputFiles := make([]string, 0, len(manifest.Files))
		for _, f := range manifest.Files {
			outputFiles = append(outputFiles, f.Path)
		}
		entry.Metadata.Size = totalSize
		entry.Metadata.OutputFiles = outputFiles

		// Re-write meta.json with updated size/files (best-effort — metadata is advisory)
		if updated, err := json.MarshalIndent(entry.Metadata, "", "  "); err == nil {
			os.WriteFile(filepath.Join(tmpDir, "meta.json"), updated, 0o644)
		}
	}

	// In-process lock for the atomic publish (the cross-process shared lock is
	// already held for the whole call).
	s.mu.Lock()
	defer s.mu.Unlock()

	// Atomic move: tmp → blobs/{hash[0:2]}/{hash}/
	blobDir := s.blobDir(hash)
	blobParent := filepath.Dir(blobDir)
	if err := os.MkdirAll(blobParent, 0o755); err != nil {
		return fmt.Errorf("create blob parent: %w", err)
	}

	if err := s.publishEntry(tmpDir, blobDir); err != nil {
		return err
	}
	// Stamp recency + this build's generation so a freshly-built entry has an
	// idle-reclaim baseline even if it is never hit again.
	writeLastUsed(blobDir, time.Now(), s.gen)
	return nil
}

// publishEntry commits a staged entry directory to blobDir with first-writer-
// wins semantics: the destination is never removed before the rename, so a
// sibling worktree that published the same (content-addressed) entry first is
// left intact. A rename onto an already-populated blobDir fails with ENOTEMPTY,
// which we treat as "a sibling won" since the entry is byte-identical. The
// cross-device fallback copies into a temp sibling then renames so the
// first-writer-wins check still holds across volumes.
func (s *LocalStore) publishEntry(tmpDir, blobDir string) error {
	if _, err := os.Stat(blobDir); err == nil {
		return nil // sibling already published this exact entry
	}

	if err := os.Rename(tmpDir, blobDir); err != nil {
		// A sibling may have won between the Stat and the Rename, or the store
		// is on a different volume than the staging dir.
		if _, statErr := os.Stat(blobDir); statErr == nil {
			return nil // sibling won the race; keep theirs
		}
		// Cross-device: copy into a temp sibling, then rename it into place.
		stagedCopy := blobDir + ".tmp-" + filepath.Base(tmpDir)
		if err := copyDir(tmpDir, stagedCopy); err != nil {
			return fmt.Errorf("move blob: %w", err)
		}
		if err := os.Rename(stagedCopy, blobDir); err != nil {
			os.RemoveAll(stagedCopy)
			if _, statErr := os.Stat(blobDir); statErr == nil {
				return nil // sibling won; discard our copy
			}
			return fmt.Errorf("publish blob: %w", err)
		}
	}
	return nil
}
