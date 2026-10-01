package artifactstore

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

// GC knobs are deliberately separate from the build store's. Binaries are static
// and expensive to re-fetch, so the budget is smaller, and idleness is measured
// in WALL-CLOCK time — a flat, repo-independent store has no build "generations".
const (
	artifactMaxBytesEnv = "PUTNAMI_ARTIFACT_MAX_BYTES"
	artifactGCGraceEnv  = "PUTNAMI_ARTIFACT_GC_GRACE"
	artifactMaxIdleEnv  = "PUTNAMI_ARTIFACT_MAX_IDLE"

	defaultArtifactMaxBytes int64         = 5 << 30         // 5 GiB
	defaultArtifactGCGrace  time.Duration = time.Hour       // protects in-flight links/execs
	defaultArtifactMaxIdle  time.Duration = 720 * time.Hour // 30 days unused → reclaimable

	gcStampFile             = ".gc-stamp"
	opportunisticGCInterval = 6 * time.Hour // binaries churn far less than build outputs
)

// GCOptions configures a pass. Zero/negative MaxBytes falls back to the default;
// Grace and MaxIdle of exactly 0 disable that mechanism.
type GCOptions struct {
	MaxBytes    int64
	Grace       time.Duration
	MaxIdle     time.Duration
	Now         time.Time // injectable clock for tests; defaults to time.Now()
	NonBlocking bool      // opportunistic callers set this to skip a busy store
}

// GCResult summarizes a pass.
type GCResult struct {
	Scanned       int // digest dirs seen
	Live          int // entries a registered workspace links to; never evicted
	EvictedIdle   int // evicted for exceeding MaxIdle
	EvictedBudget int // evicted to get under MaxBytes
	SweptStaging  int // stale tmp/staging-* dirs removed
	FreedBytes    int64
	TotalBytes    int64 // total before eviction
}

// ResolveGCOptions builds GCOptions from the PUTNAMI_ARTIFACT_* env vars, falling
// back to the defaults.
func ResolveGCOptions() GCOptions {
	return GCOptions{
		MaxBytes: envInt64(artifactMaxBytesEnv, defaultArtifactMaxBytes),
		Grace:    envDuration(artifactGCGraceEnv, defaultArtifactGCGrace),
		MaxIdle:  envDuration(artifactMaxIdleEnv, defaultArtifactMaxIdle),
	}
}

type gcVictim struct {
	dir      string
	lastUsed time.Time
	bytes    int64
}

// GC reclaims space in the flat artifact store. It never evicts an entry a
// registered workspace links to (see roots.go), whatever its age. Every other
// entry is evicted as a whole dir by recency — wall-clock idle past MaxIdle
// first, then oldest-first to get under MaxBytes — protected by a grace window
// and an under-lock recency re-check. The whole pass holds the store's EXCLUSIVE
// lock, so no admit is in flight and the store holds no transients. An evicted
// entry a worktree still needs re-materializes on that worktree's next command
// (self-healing).
func (s *Store) GC(opts GCOptions) (*GCResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultArtifactMaxBytes
	}
	grace := opts.Grace
	if grace < 0 {
		grace = 0
	}
	maxIdle := opts.MaxIdle
	if maxIdle < 0 {
		maxIdle = 0
	}

	res := &GCResult{}

	release, ok := s.lockExclusive(opts.NonBlocking)
	if !ok {
		// The lock is held (a publisher / another GC) or could not be acquired.
		// Either way we do NOT hold it exclusively, so skip rather than run the
		// destructive RemoveAll passes unprotected against an in-flight admit.
		return res, nil
	}
	defer release()

	// Phase A — enumerate reclaimable dirs, their recency, and bytes.
	var all []gcVictim
	add := func(dir string) {
		last, found := readUsed(dir)
		if !found {
			return
		}
		b := dirBytes(dir)
		all = append(all, gcVictim{dir: dir, lastUsed: last, bytes: b})
		res.Scanned++
		res.TotalBytes += b
	}

	shaRoot := filepath.Join(s.root, shaDirName)
	prefixes, _ := os.ReadDir(shaRoot)
	for _, p := range prefixes {
		if !p.IsDir() {
			continue
		}
		shard := filepath.Join(shaRoot, p.Name())
		digests, _ := os.ReadDir(shard)
		for _, d := range digests {
			if !d.IsDir() {
				continue
			}
			add(filepath.Join(shard, d.Name()))
		}
	}

	// Also reclaim the CLI blobs putnamiw publishes outside the verified sha256/
	// digest tree: prebuilt ones (cli/<sha>/, re-downloaded) and from-source ones
	// (cli-source/<key>/, rebuilt). A source workspace publishes one per source
	// state, so without this they leak unbounded in the shared root the store is
	// meant to GC. (The managed Go toolchain under ~/.putnami/toolchains is a
	// separate root and is NOT collected here.)
	for _, name := range []string{cliDirName, cliSourceDirName} {
		for _, dir := range blobDirs(filepath.Join(s.root, name)) {
			add(dir)
		}
	}

	live := s.liveEntries()
	for _, v := range all {
		if live[v.dir] {
			res.Live++
		}
	}
	protected := func(v gcVictim) bool {
		return live[v.dir] || (grace > 0 && now.Sub(v.lastUsed) < grace)
	}

	// Phase B1 — idle reclaim (independent of budget): evict dirs unused longer
	// than MaxIdle so abandoned binaries from deleted worktrees do not linger.
	var remaining []gcVictim
	for _, v := range all {
		if maxIdle > 0 && now.Sub(v.lastUsed) > maxIdle && !protected(v) && s.evict(v, now, grace) {
			res.EvictedIdle++
			res.FreedBytes += v.bytes
			res.TotalBytes -= v.bytes
			continue
		}
		remaining = append(remaining, v)
	}

	// Phase B2 — budget reclaim: if still over budget, evict oldest-first to an
	// 80% low watermark, skipping grace-protected dirs.
	if res.TotalBytes > maxBytes {
		sort.Slice(remaining, func(i, j int) bool { return remaining[i].lastUsed.Before(remaining[j].lastUsed) })
		watermark := maxBytes * 8 / 10
		for _, v := range remaining {
			if res.TotalBytes <= watermark {
				break
			}
			if protected(v) || !s.evict(v, now, grace) {
				continue
			}
			res.EvictedBudget++
			res.FreedBytes += v.bytes
			res.TotalBytes -= v.bytes
		}
	}

	res.SweptStaging = s.sweepStaging(now, grace)
	removeEmptyShards(shaRoot)
	s.pruneDigestLocks()
	return res, nil
}

// Clean reclaims entries from the artifact store — extension/template digest dirs
// under sha256/, CLI blobs under cli/ and cli-source/, and staging — EXCEPT entries used
// within the grace window. Those an in-flight sibling worktree may be mid-exec on:
// the link→exec window of a running job is NOT under the store lock, and a binary
// it pins may not be re-downloadable (e.g. @putnami/cloud), so reaping it strands
// that run with an opaque fork/exec failure instead of a self-heal. An
// active run keeps its pinned binaries warm (Touch under the shared lock), so a
// recent lastused stamp means a worktree likely still needs the entry. Pass
// grace<=0 for an unconditional wipe.
//
// It preserves the root and its lock file so cross-process lock identity stays
// intact. Returns the count of dirs removed and the bytes freed. It runs under the
// EXCLUSIVE lock and errors (touching nothing) if that lock cannot be held, so a
// wipe can never race an in-flight admit's rename — and, holding it exclusive, it
// drains any in-flight keep-warm Touch (shared) so each lastused read reflects the
// last completed stamp. The managed Go toolchain (~/.putnami/toolchains) is a
// separate root and is NOT removed here.
func (s *Store) Clean(grace time.Duration) (dirs int, freed int64, err error) {
	release, ok := s.lockExclusive(false)
	if !ok {
		return 0, 0, fmt.Errorf("artifactstore: cannot acquire exclusive lock to clean %s", s.root)
	}
	defer release()

	now := time.Now()
	// inUse spares an entry kept warm within the grace window; grace<=0 disables
	// the protection (unconditional wipe).
	inUse := func(dir string) bool {
		if grace <= 0 {
			return false
		}
		last, found := readUsed(dir)
		return found && now.Sub(last) < grace
	}

	shaRoot := filepath.Join(s.root, shaDirName)
	prefixes, _ := os.ReadDir(shaRoot)
	for _, p := range prefixes {
		shard := filepath.Join(shaRoot, p.Name())
		digests, _ := os.ReadDir(shard)
		for _, d := range digests {
			dir := filepath.Join(shard, d.Name())
			if inUse(dir) {
				continue
			}
			b := dirBytes(dir)
			if os.RemoveAll(dir) == nil {
				dirs++
				freed += b
			}
		}
		// Drop the shard only when no in-use entry kept it populated.
		if sub, rerr := os.ReadDir(shard); rerr == nil && len(sub) == 0 {
			_ = os.Remove(shard)
		}
	}
	for _, name := range []string{cliDirName, cliSourceDirName} {
		for _, dir := range blobDirs(filepath.Join(s.root, name)) {
			if inUse(dir) {
				continue
			}
			sz := dirBytes(dir)
			if os.RemoveAll(dir) == nil {
				dirs++
				freed += sz
			}
		}
	}
	_ = os.RemoveAll(filepath.Join(s.root, tmpDirName))
	s.pruneDigestLocks()
	return dirs, freed, nil
}

// pruneDigestLocks removes ownership-lock inodes whose content digest no
// longer exists in either artifact namespace. Callers hold the store-exclusive
// lock, so no publisher can still be flocking one of these files; unlinking
// without that exclusion could split future contenders across two inodes.
func (s *Store) pruneDigestLocks() {
	root := filepath.Join(s.root, digestLocks)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		digest := strings.TrimSuffix(entry.Name(), ".lock")
		if !isHexDigest(digest) {
			continue
		}
		if s.Has(digest) || s.HasCLI(digest) {
			continue
		}
		_ = os.Remove(filepath.Join(root, entry.Name()))
	}
	if remaining, err := os.ReadDir(root); err == nil && len(remaining) == 0 {
		_ = os.Remove(root)
	}
}

// evict removes a digest dir, re-reading its recency under the exclusive lock
// immediately before deletion so a dir freshly stamped since enumeration (a
// worktree just linked it) is spared. Returns whether it was removed.
func (s *Store) evict(v gcVictim, now time.Time, grace time.Duration) bool {
	if grace > 0 {
		if last, found := readUsed(v.dir); found && now.Sub(last) < grace {
			return false // touched since enumeration; spare it
		}
	}
	if err := os.RemoveAll(v.dir); err != nil {
		// A partial RemoveAll (EACCES on a child, a busy inode, transient I/O)
		// can leave the dir present but incomplete — manifest-less — which Has()
		// still reports as a hit and Admit short-circuits to, wedging the
		// artifact machine-wide. Demote the remnant out of the canonical tree
		// into tmp/ (a directory rename, which succeeds where the recursive
		// delete did not, and is safe under the exclusive lock we hold) so Has()
		// stops reporting it, the next install re-materializes cleanly, and a
		// later sweepStaging reclaims the bytes.
		if !s.trashDir(v.dir) {
			return false
		}
	}
	slog.Info("artifact GC: evicted",
		"bytes", v.bytes, "idleFor", now.Sub(v.lastUsed).Round(time.Second).String())
	return true
}

// trashDir atomically renames a (possibly partially-deleted) entry dir into the
// store's tmp/ area, so it leaves the canonical tree immediately even when a
// recursive delete could not finish; a later sweepStaging reclaims the bytes.
// Sound only under the exclusive lock, where no admit is mid-rename. Returns
// whether the dir is no longer at its original path.
func (s *Store) trashDir(dir string) bool {
	tmpRoot := filepath.Join(s.root, tmpDirName)
	if err := os.MkdirAll(tmpRoot, dirPerm); err != nil {
		return false
	}
	dest, err := os.MkdirTemp(tmpRoot, "evicted-")
	if err != nil {
		return false
	}
	// rename(2) needs the target absent; MkdirTemp created it to reserve a unique
	// name, so drop it first (still under the exclusive lock, so no race).
	_ = os.Remove(dest)
	return os.Rename(dir, dest) == nil
}

// sweepStaging removes tmp/staging-* dirs older than grace (orphaned by an
// interrupted admit). Sound only under the exclusive lock, where no admit is live.
func (s *Store) sweepStaging(now time.Time, grace time.Duration) int {
	entries, err := os.ReadDir(filepath.Join(s.root, tmpDirName))
	if err != nil {
		return 0
	}
	swept := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if grace > 0 && now.Sub(info.ModTime()) < grace {
			continue
		}
		if os.RemoveAll(filepath.Join(s.root, tmpDirName, e.Name())) == nil {
			swept++
		}
	}
	return swept
}

// lockExclusive takes the store-wide lock in EXCLUSIVE mode for the destructive
// GC / Clean passes. ok is true ONLY when the lock is genuinely held: a
// non-blocking caller that finds the store busy, OR any caller that hits a real
// lock error (cannot create the root, OpenFile fails, a blocking Acquire
// errors), gets ok=false and MUST NOT proceed — running the RemoveAll passes
// without the exclusive lock could reap a staging dir or digest tree a sibling
// admit is mid-rename into.
func (s *Store) lockExclusive(nonBlocking bool) (func(), bool) {
	if err := os.MkdirAll(s.root, dirPerm); err != nil {
		return func() {}, false
	}
	l, err := flock.Acquire(filepath.Join(s.root, lockFile), true, nonBlocking)
	if err != nil {
		return func() {}, false
	}
	return func() { _ = l.Release() }, true
}

// MaybeOpportunisticGC runs a throttled, non-blocking, best-effort GC over the
// flat artifact store at root, mirroring the build store's trigger but on a
// longer interval. Errors are swallowed: it must never fail or slow a build.
func MaybeOpportunisticGC(root string) {
	if root == "" {
		return
	}
	stamp := filepath.Join(root, gcStampFile)
	if !gcThrottleElapsed(stamp, opportunisticGCInterval) {
		return
	}
	touchStamp(stamp)
	opts := ResolveGCOptions()
	opts.NonBlocking = true
	_, _ = New(root).GC(opts)
}

// readUsed returns the artifact's last-used time, falling back to the dir mtime
// for entries with no (or an unreadable) sidecar. The bool is false only when dir
// cannot be stat'd at all.
func readUsed(dir string) (time.Time, bool) {
	if data, err := os.ReadFile(filepath.Join(dir, lastUsedFile)); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && n > 0 {
			return time.Unix(0, n), true
		}
	}
	if fi, err := os.Stat(dir); err == nil {
		return fi.ModTime(), true
	}
	return time.Time{}, false
}

// dirBytes sums the sizes of all regular files under dir, excluding the
// lastused recency sidecar (GC bookkeeping, not artifact payload) so the budget
// accounts for the bytes a re-fetch would actually have to recreate.
func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == lastUsedFile {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// blobDirs lists the entry dirs directly under a CLI blob tree (cli/ or
// cli-source/). A missing tree lists nothing.
func blobDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
}

// removeEmptyShards removes empty sha256/<xx> fan-out dirs after eviction.
func removeEmptyShards(shaRoot string) {
	entries, err := os.ReadDir(shaRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(shaRoot, e.Name())
		if sub, err := os.ReadDir(dir); err == nil && len(sub) == 0 {
			_ = os.Remove(dir)
		}
	}
}

// gcThrottleElapsed reports whether at least interval has passed since the stamp
// was last touched (or it never was).
func gcThrottleElapsed(stamp string, interval time.Duration) bool {
	fi, err := os.Stat(stamp)
	if err != nil {
		return true
	}
	return time.Since(fi.ModTime()) >= interval
}

// touchStamp creates/updates the throttle stamp's mtime to now.
func touchStamp(stamp string) {
	if err := os.MkdirAll(filepath.Dir(stamp), dirPerm); err != nil {
		return
	}
	now := time.Now()
	if err := os.WriteFile(stamp, []byte(now.Format(time.RFC3339)), 0o644); err != nil {
		return
	}
	_ = os.Chtimes(stamp, now, now)
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return def
}
