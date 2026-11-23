package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// LocalStore is a filesystem-backed content-addressed store. The root is
// resolved by ResolveStoreRoot to a machine-global, per-repo location
// (~/.putnami/store/<repo-id>) shared across every worktree of a repo, so it is
// guarded for cross-process access (see lock.go) in addition to the in-process
// mutex. Layout:
//
//	<root>/
//	  blobs/{hash[0:2]}/{hash}/   action-cache entries
//	    entry.json                declared-output descriptor (task_entry.go)
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
