package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// storeLockFile is the advisory lock file at the store root that serializes
// publishers and lease-record operations (shared) against garbage collection
// and cleanup (exclusive). It lives inside the store so each per-repo store has
// its own lock.
const storeLockFile = ".lock"

// lastUsedFile is a per-entry sidecar holding the unix-nano timestamp of the
// last cache hit. GC evicts by ascending lastUsed; the sidecar is written
// atomically and never mutates the otherwise-immutable entry payload (meta /
// result / manifest), so a hit cannot corrupt an entry a sibling is reading.
const lastUsedFile = "lastused"

// lockShared takes the store-wide lock in shared mode for a publish/ingest or a
// lease-record operation, so a concurrent GC (which takes it exclusively) can
// never sweep a CAS blob this publisher just linked or unlink a lease's flock
// inode while it is open. The returned func releases it. On any lock error the
// release is a no-op and the caller proceeds best-effort, relying on rename
// atomicity / lease TTL fallback (correct, just unprotected against concurrent
// GC cleanup).
func (s *LocalStore) lockShared() func() {
	release, _ := s.acquireLock(false, false)
	return release
}

// lockExclusive takes the store-wide lock in exclusive mode for GC / clean, so
// it waits for in-flight publishers and lease-record operations to drain and
// blocks new ones for the duration. Under this lock no CAS blob is mid-ingest
// and no lease flock inode is open, so orphan sweeping and expired-lease
// reaping are sound.
func (s *LocalStore) lockExclusive() func() {
	release, _ := s.acquireLock(true, false)
	return release
}

// tryLockShared / tryLockExclusive are the non-blocking variants used by
// opportunistic GC so a finishing build never stalls behind a busy store: they
// return ok=false (and a no-op release) when the lock is currently contended.
func (s *LocalStore) tryLockShared() (func(), bool)    { return s.acquireLock(false, true) }
func (s *LocalStore) tryLockExclusive() (func(), bool) { return s.acquireLock(true, true) }

// acquireLock acquires the store-wide advisory lock. In blocking mode a lock
// error degrades to a no-op release with ok=true (proceed best-effort). In
// non-blocking mode a contended/erroring lock returns ok=false so the caller
// skips the operation.
func (s *LocalStore) acquireLock(exclusive, nonBlocking bool) (func(), bool) {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return func() {}, !nonBlocking
	}
	l, err := flock.Acquire(filepath.Join(s.root, storeLockFile), exclusive, nonBlocking)
	if err != nil {
		return func() {}, !nonBlocking
	}
	return func() { _ = l.Release() }, true
}

// markUsed stamps the entry's lastUsed sidecar with the current time and this
// build's generation so GC's recency/grace and idle reclaim see this hit.
// Best-effort: it takes the shared lock (mutually exclusive with GC) and
// silently no-ops if the entry has already been evicted.
func (s *LocalStore) markUsed(hash string) {
	s.ensureGeneration()
	release := s.lockShared()
	defer release()

	blobDir := s.blobDir(hash)
	if _, err := os.Stat(blobDir); err != nil {
		return // gone (evicted) — nothing to stamp
	}
	writeLastUsed(blobDir, time.Now(), s.gen)
}

// getAndTouch returns the entry for hash AND stamps its lastUsed time +
// generation atomically under the shared lock, so a concurrent GC (exclusive)
// cannot evict the entry in the window between the read and the stamp. This is
// the cache-HIT read path; plain Get is for GC enumeration and tests, which must
// not refresh recency.
func (s *LocalStore) getAndTouch(hash string) (*Entry, error) {
	s.ensureGeneration()
	release := s.lockShared()
	defer release()

	entry, err := s.Get(hash)
	if err != nil || entry == nil {
		return entry, err
	}
	writeLastUsed(s.blobDir(hash), time.Now(), s.gen)
	return entry, nil
}

// lastUsedRecord is the JSON payload of the lastUsed sidecar: when the entry was
// last hit and the store generation at that time.
type lastUsedRecord struct {
	UsedAtNano int64 `json:"usedAt"`
	Generation int64 `json:"gen"`
}

// writeLastUsed atomically writes the lastUsed timestamp and generation into
// blobDir.
func writeLastUsed(blobDir string, t time.Time, gen int64) {
	data, err := json.Marshal(lastUsedRecord{UsedAtNano: t.UnixNano(), Generation: gen})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(blobDir, "lastused-")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, filepath.Join(blobDir, lastUsedFile)); err != nil {
		os.Remove(tmpName)
	}
}

// readLastUsed returns the entry's last-used time and the generation it was last
// hit in. The time falls back to CreatedAt (meta.json) then the blob dir mtime
// so legacy entries still have a sane recency; the generation is -1 when unknown
// (legacy entries / never stamped), which excludes them from idle reclaim.
func readLastUsed(blobDir string) (time.Time, int64) {
	if data, err := os.ReadFile(filepath.Join(blobDir, lastUsedFile)); err == nil {
		trimmed := trimSpaceBytes(data)
		var rec lastUsedRecord
		if json.Unmarshal(trimmed, &rec) == nil && rec.UsedAtNano != 0 {
			return time.Unix(0, rec.UsedAtNano), rec.Generation
		}
		// Legacy format: a bare unix-nano integer (no generation).
		if n, err := strconv.ParseInt(string(trimmed), 10, 64); err == nil {
			return time.Unix(0, n), -1
		}
	}
	if data, err := os.ReadFile(filepath.Join(blobDir, "meta.json")); err == nil {
		if m, err := UnmarshalMetadata(data); err == nil && !m.CreatedAt.IsZero() {
			return m.CreatedAt, -1
		}
	}
	if fi, err := os.Stat(blobDir); err == nil {
		return fi.ModTime(), -1
	}
	return time.Time{}, -1
}

// trimSpaceBytes trims surrounding ASCII whitespace without importing strings.
func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
