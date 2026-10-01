package store

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

// Garbage collection for the machine-global store.
//
// The budget is global across every per-repo store under ~/.putnami/store. GC
// evicts whole cache entries oldest-first (by lastUsed) until projected usage is
// under a low watermark, then sweeps the CAS for blobs no surviving entry
// references. Reclamation is accounted by DEDUPLICATED bytes: a blob shared by
// several entries frees space only when its LAST referencing entry is evicted,
// so the victim selection refcounts blob digests across entries rather than
// summing per-entry logical sizes (which would over-count shared blobs and
// under-evict).
//
// Correctness rests on two things:
//   - The store-wide EXCLUSIVE lock around every removal. CAS publishers hold
//     the SHARED lock for their whole ingest, so inside an exclusive section the
//     surviving task-entry manifests fully describe every live CAS blob. The
//     lock is held in sections of about 200 ms (exclusiveSections), not for the
//     whole pass, because every lookup, publish and restore on the store waits
//     while it is held; each section that sweeps first re-reads the manifests of
//     entries published since, and a victim leaves the index by one atomic
//     rename, its tree deleted after the lock is released. OCI publishers use
//     atomic directory renames and assembly materializes a hit into its private
//     work directory before using it, so concurrent eviction is a safe miss.
//   - A grace period. Readers (a build whose .putnami/out symlink points into an
//     entry) do not hold a lock for their whole lifetime, so GC never evicts an
//     entry used within the grace window. A pass that runs DURING a build (the
//     in-run budget, gc_trigger.go) also spares every entry used since that
//     build started (GCOptions.ProtectSince), whatever the grace, so no job of
//     that build loses an entry it has already looked up, restored, prefetched
//     or published, however long the build runs.
//
// The budget covers the build store and nothing else. The machine caches an
// extension owns live outside every store root (resolve.go): the Go cache at
// ~/.putnami/cache/go with its build, prog and mod trees (resolved by the Go
// extension itself), and every extension machine root under
// ~/.putnami/cache/extensions (protocols/extension MachineCacheRoot). GC here
// neither counts nor evicts them. Each is bounded by its owning extension's
// reserved `cache-gc` command, which core starts after a run and never during
// one; the Go extension's budget is PUTNAMI_GO_CACHE_MAX_BYTES, 10 GiB across
// build, prog and mod. TestStoreBudget_LeavesExtensionMachineCachesAlone pins
// this side of the boundary.

// GCOptions configures a collection pass.
type GCOptions struct {
	// MaxBytes is the global budget. GC runs only when total usage exceeds it;
	// <= 0 uses the configured default.
	MaxBytes int64
	// Grace spares entries used within this window from eviction; < 0 means 0.
	Grace time.Duration
	// Now is injectable for tests; the zero value means time.Now().
	Now time.Time
	// NonBlocking, when set, makes GC skip any store whose lock is currently
	// contended (beyond LockWait) instead of waiting, so a pass never stalls
	// behind a busy sibling store. It says nothing about the stores a pass
	// does lock: their users wait for its exclusive sections.
	NonBlocking bool
	// MaxIdleGenerations evicts entries not hit in this many builds (store
	// generations), independent of the byte budget. 0 disables idle reclaim.
	MaxIdleGenerations int64
	// ProtectSince, when set, spares every entry used at or after it, in
	// addition to the grace window. A pass that runs while a build is still in
	// flight sets it to that build's start, so it can only evict entries the
	// build has not touched — however long the build has run and whatever the
	// configured grace.
	ProtectSince time.Time
	// LockWait, with NonBlocking, is how long the pass keeps retrying the
	// exclusive lock of a store it holds victims in before skipping that store,
	// counted once across the whole pass. Every retry is a non-blocking try, so
	// the wait never queues ahead of a publisher: no writer is ever starved,
	// only the pass can be. Stores with nothing to evict get a single try.
	// Zero keeps the single try everywhere.
	LockWait time.Duration
}

// gcLockPoll is the interval between two exclusive-lock tries under LockWait.
// A publisher's shared hold lasts one lookup, restore or ingest, so gaps are
// frequent and short; polling this often finds one without busy-looping.
const gcLockPoll = 5 * time.Millisecond

// GCResult summarizes a collection pass.
type GCResult struct {
	StoresScanned  int
	ScannedBytes   int64
	EvictedEntries int
	IdleEvicted    int // subset of EvictedEntries removed by idle reclaim
	SweptBlobs     int
	FreedBytes     int64
	// ScanSkippedBusy counts stores the scan skipped because another holder
	// had them exclusively (NonBlocking only). Their bytes are missing from
	// ScannedBytes, so the pass measured less than the disk holds.
	ScanSkippedBusy int
	// EvictSkippedBusy counts stores that held victims but whose exclusive
	// lock stayed contended past LockWait, or was lost between two sections,
	// so the budget was not (fully) enforced there.
	EvictSkippedBusy int
	// LockHeld is the total time the pass held stores' exclusive locks (evict,
	// sweep and tidy), MaxLockHeld the longest single section (about
	// gcHoldBudget). While a store's exclusive lock is held, every lookup,
	// publish and restore on that store waits, in every process sharing it.
	LockHeld    time.Duration
	MaxLockHeld time.Duration
}

// scanState is how scanStore's attempt at one root ended.
type scanState int

const (
	scanNotStore scanState = iota // not a usable store: nothing to count
	scanBusy                      // contended in non-blocking mode: its bytes are unknown
	scanDone
)

// storeEviction is evictStore's outcome for one store.
type storeEviction struct {
	evicted, idleEvicted, swept int
	freed                       int64
	// busy is set when the store held victims and the pass could not get its
	// exclusive lock, first or back after a section: the budget was not (fully)
	// enforced there, and what it had not reached is left for the next pass. A
	// sweep-only store is never busy: its orphans are housekeeping, still on
	// disk and in ScannedBytes when skipped.
	busy bool
	// held is how long the exclusive lock was held in total, maxHeld its
	// longest single section.
	held, maxHeld time.Duration
}

// blobRef is one CAS blob referenced by an entry's manifest.
type blobRef struct {
	digest string
	size   int64
}

// gcEntry is one cache entry considered for eviction.
type gcEntry struct {
	storeRoot string
	blobDir   string
	ident     os.FileInfo // the entry directory as the scan saw it; nil for OCI entries
	lastUsed  time.Time
	gen       int64 // generation last hit; -1 when unknown (excludes from idle reclaim)
	blobs     []blobRef
	rawSize   int64 // non-CAS bytes owned exclusively by this entry (OCI layers)
	idle      bool
}

// blobKey identifies a CAS blob within a specific store (blobs never cross
// stores, so the store root is part of the key).
type blobKey struct {
	store  string
	digest string
}

// RunGC enforces the global byte budget across the given per-repo store roots.
// It is safe to run concurrently with builds in other processes (it takes each
// store's exclusive lock only while mutating it) and is a no-op within budget.
func RunGC(roots []string, opts GCOptions) (*GCResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultStoreMaxBytes
	}
	grace := opts.Grace
	if grace < 0 {
		grace = 0
	}

	maxIdle := opts.MaxIdleGenerations
	if maxIdle < 0 {
		maxIdle = 0
	}

	res := &GCResult{}

	// Phase A — enumerate every entry (with its manifest blob refs) and total
	// disk usage. Each store is read under its shared lock so an in-flight
	// publisher is never observed mid-ingest.
	var all []gcEntry
	var scannedRoots []string
	var leaseRoots []string
	var total int64
	storeGen := map[string]int64{}
	scans := map[string]*storeScan{}
	for _, root := range roots {
		scan, state := scanStore(root, opts.NonBlocking)
		if state == scanBusy {
			res.ScanSkippedBusy++
		}
		if state != scanDone {
			continue
		}
		res.StoresScanned++
		scannedRoots = append(scannedRoots, root)
		scans[root] = scan
		total += scan.total
		storeGen[root] = scan.generation
		all = append(all, scan.entries...)
		if scan.hasLeases {
			leaseRoots = append(leaseRoots, root)
		}
	}
	res.ScannedBytes = total
	// Nothing to do only when we are within budget AND idle reclaim is off (it
	// runs regardless of budget to reap abandoned entries). Lease roots still
	// need a mutation pass so released/crashed sidecars stay transient.
	if total <= maxBytes && maxIdle == 0 && len(leaseRoots) == 0 {
		return res, nil
	}

	// Refcount every blob across all entries so we can credit an eviction with
	// the blob's bytes only when its LAST referrer is evicted (accurate dedup
	// accounting, unlike summing per-entry logical sizes).
	refcount := map[blobKey]int{}
	blobSize := map[blobKey]int64{}
	for _, e := range all {
		for _, b := range e.blobs {
			k := blobKey{e.storeRoot, b.digest}
			refcount[k]++
			blobSize[k] = b.size
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].lastUsed.Before(all[j].lastUsed) })

	victimsByStore := map[string][]gcEntry{}
	picked := map[string]bool{}
	var projectedFreed int64
	// spared is the one protection rule, applied here and again under each
	// store's exclusive lock (evictStore): used within the grace window, or at
	// or after ProtectSince.
	spared := func(lastUsed time.Time) bool {
		if grace > 0 && now.Sub(lastUsed) < grace {
			return true
		}
		return !opts.ProtectSince.IsZero() && !lastUsed.Before(opts.ProtectSince)
	}
	protected := func(e gcEntry) bool { return spared(e.lastUsed) }
	markVictim := func(e gcEntry, idle bool) {
		e.idle = idle
		victimsByStore[e.storeRoot] = append(victimsByStore[e.storeRoot], e)
		picked[e.blobDir] = true
		projectedFreed += e.rawSize
		for _, b := range e.blobs {
			k := blobKey{e.storeRoot, b.digest}
			if refcount[k] > 0 {
				refcount[k]--
				if refcount[k] == 0 {
					projectedFreed += blobSize[k]
				}
			}
		}
	}

	// Idle pass — reclaim entries not hit in maxIdle builds, regardless of budget
	// (still honoring the grace window). Entries with an unknown generation
	// (gen < 0: legacy / never stamped) are excluded.
	if maxIdle > 0 {
		for _, e := range all {
			// "not hit in the last maxIdle builds" → idle (currentGen - lastHitGen)
			// of at least maxIdle.
			if e.gen >= 0 && storeGen[e.storeRoot]-e.gen >= maxIdle && !protected(e) {
				markVictim(e, true)
			}
		}
	}

	// Budget pass — only if still over budget after idle reclaim, evict
	// oldest-first down to the low watermark (80% of budget — hysteresis so GC
	// doesn't re-trigger every build).
	if total-projectedFreed > maxBytes {
		low := maxBytes - maxBytes/5
		for _, e := range all {
			if total-projectedFreed <= low {
				break
			}
			if picked[e.blobDir] || protected(e) {
				continue
			}
			markVictim(e, false)
		}
	}

	rootsToMutate := map[string]bool{}
	for root := range victimsByStore {
		rootsToMutate[root] = true
	}
	if total > maxBytes {
		for _, root := range scannedRoots {
			rootsToMutate[root] = true
		}
	}
	for _, root := range leaseRoots {
		rootsToMutate[root] = true
	}
	evictStores(rootsToMutate, victimsByStore, scans, now, spared, opts, res)
	return res, nil
}

// evictStores is RunGC's Phase B: evict + sweep each store to mutate, in a
// stable order, under its exclusive lock (evictStore), adding what each did to
// res. LockWait is one deadline for the whole pass.
func evictStores(rootsToMutate map[string]bool, victimsByStore map[string][]gcEntry, scans map[string]*storeScan,
	now time.Time, spared func(time.Time) bool, opts GCOptions, res *GCResult) {
	mutateRoots := make([]string, 0, len(rootsToMutate))
	for root := range rootsToMutate {
		mutateRoots = append(mutateRoots, root)
	}
	sort.Strings(mutateRoots)
	lockDeadline := time.Now().Add(opts.LockWait)
	for _, root := range mutateRoots {
		out := evictStore(root, victimsByStore[root], scans[root], now, spared, opts.NonBlocking, lockDeadline)
		if out.busy {
			res.EvictSkippedBusy++
		}
		res.EvictedEntries += out.evicted
		res.IdleEvicted += out.idleEvicted
		res.SweptBlobs += out.swept
		res.FreedBytes += out.freed
		res.LockHeld += out.held
		res.MaxLockHeld = max(res.MaxLockHeld, out.maxHeld)
	}
}

// storeScan is one store as the scan saw it, under its shared lock.
type storeScan struct {
	entries    []gcEntry
	total      int64
	generation int64
	hasLeases  bool
	// casFiles holds, with its size, every file under cas/ a sweep may delete
	// once no entry references it: a file in a two-character fan-out directory
	// (a blob, or a copy's blob- temp an interrupted ingest left) and a fetch-
	// temp at the top of cas/. Nothing else under cas/ is ever deleted.
	casFiles map[string]int64
	// fanouts is each blobs/ fan-out directory as it was just before the scan
	// listed it, so a sweep re-lists only the ones that changed since.
	fanouts map[string]fanoutStamp
}

// fanoutStamp is a blobs/ fan-out directory's mtime and when it was read. An
// entry only ever appears in or leaves a fan-out directory by a rename (it is
// published by renaming its staged directory into place and never rewritten),
// so an unchanged mtime means the same entries — once the mtime is older than
// gcRacyWindow at the time it was read. A younger one may hide a change made in
// the same timestamp tick on a coarse filesystem, so it is re-listed anyway.
type fanoutStamp struct {
	mtime, read time.Time
}

// gcRacyWindow is how old a fan-out directory's mtime must be, when read, for
// an unchanged mtime to prove it unchanged (see fanoutStamp).
const gcRacyWindow = 2 * time.Second

// scanStore reads one store's entries and total disk usage under its shared
// lock. Total = CAS bytes (the unique, deduplicated content) + task-entry
// metadata + standalone OCI layer entries; files/ hardlinks are not summed
// separately since they share inodes with the CAS. state is scanNotStore when
// the path is not a usable store and scanBusy when (in non-blocking mode) the
// lock is contended; only scanDone returns a scan.
func scanStore(root string, nonBlocking bool) (*storeScan, scanState) {
	blobsRoot := filepath.Join(root, "blobs")
	casRoot := filepath.Join(root, "cas")
	ociRoot := filepath.Join(root, OCILayerCacheDirName)
	leasesRoot := filepath.Join(root, leaseDirName)
	if !isDir(blobsRoot) && !isDir(casRoot) && !isDir(ociRoot) && !isDir(leasesRoot) {
		return nil, scanNotStore
	}

	s := NewLocalStore(root)
	var release func()
	if nonBlocking {
		var locked bool
		release, locked = s.tryLockShared()
		if !locked {
			return nil, scanBusy // skip this store this pass
		}
	} else {
		release = s.lockShared()
	}
	defer release()

	scan := &storeScan{generation: currentGeneration(root), hasLeases: hasLeaseSidecars(root), casFiles: map[string]int64{}, fanouts: map[string]fanoutStamp{}}
	// leases/ is intentionally absent from total: heartbeat sidecars are
	// coordination state, not cached content, and must not drive eviction.
	_ = filepath.WalkDir(casRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		scan.total += info.Size()
		rel, _ := filepath.Rel(casRoot, path)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if (len(parts) == 2 && len(parts[0]) == 2) || (len(parts) == 1 && strings.HasPrefix(parts[0], "fetch-")) {
			scan.casFiles[path] = info.Size()
		}
		return nil
	})

	prefixes, _ := os.ReadDir(blobsRoot)
	for _, p := range prefixes {
		if !p.IsDir() {
			continue
		}
		fanout := filepath.Join(blobsRoot, p.Name())
		if info, err := p.Info(); err == nil {
			// Read BEFORE the listing, so a change during it shows as a new mtime.
			scan.fanouts[fanout] = fanoutStamp{mtime: info.ModTime(), read: time.Now()}
		}
		hashDirs, _ := os.ReadDir(fanout)
		for _, h := range hashDirs {
			if !h.IsDir() {
				continue
			}
			blobDir := filepath.Join(blobsRoot, p.Name(), h.Name())
			scan.total += blobMetaBytes(blobDir)
			lastUsed, gen := readLastUsed(blobDir)
			ident, _ := h.Info()
			scan.entries = append(scan.entries, gcEntry{
				storeRoot: root,
				blobDir:   blobDir,
				ident:     ident,
				lastUsed:  lastUsed,
				gen:       gen,
				blobs:     readManifestBlobs(blobDir),
			})
		}
	}

	// OCI layers are immutable standalone blobs rather than CAS references from
	// task entries. Each complete entry owns all of its bytes, so it participates
	// directly in the same global oldest-first budget. Dot-prefixed staging dirs
	// are never observed as entries; publishers atomically rename them only after
	// both the blob and record are complete.
	versions, _ := os.ReadDir(ociRoot)
	for _, version := range versions {
		if !version.IsDir() || !strings.HasPrefix(version.Name(), "v") {
			continue
		}
		versionDir := filepath.Join(ociRoot, version.Name())
		prefixes, _ := os.ReadDir(versionDir)
		for _, prefix := range prefixes {
			if !prefix.IsDir() || strings.HasPrefix(prefix.Name(), ".") {
				continue
			}
			prefixDir := filepath.Join(versionDir, prefix.Name())
			children, _ := os.ReadDir(prefixDir)
			for _, child := range children {
				if child.IsDir() && !strings.HasPrefix(child.Name(), ".") {
					entryDir := filepath.Join(prefixDir, child.Name())
					size := dirBytes(entryDir)
					lastUsed, _ := readLastUsed(entryDir)
					scan.total += size
					scan.entries = append(scan.entries, gcEntry{
						storeRoot: root,
						blobDir:   entryDir,
						lastUsed:  lastUsed,
						gen:       -1,
						rawSize:   size,
					})
				}
			}
		}
	}
	return scan, scanDone
}

// The exclusive lock is held in SECTIONS, never for a whole store. Measured by
// BenchmarkEvictStore_ExclusiveLockHold on a loaded laptop before this bound
// existed, one pass held it for 0.9 s (1,000 entries / 10,000 blobs), 2.8 s
// (3,000 / 30,000) and 5.9 s (10,000 / 50,000) while evicting a tenth of the
// entries, and every lookup, publish and restore on the store, in every process
// sharing it, waited that long. With sections the longest single hold measured
// 0.2 s, 0.2 s and 0.4 s (the last is the one full re-list after a thousand
// victims touched every fan-out directory), for a total of 0.2 s, 0.8 s and
// 3.2 s spread across the pass.
// gcTrashPrefix names the tmp/ directory a pass renames its victims into: the
// rename is what removes an entry, atomically; deleting the tree is done after
// the lock is released.
const gcTrashPrefix = "gc-trash-"

// The section bounds are variables only so tests can force a section per unit
// of work and a failed re-acquisition; nothing in production assigns them.
var (
	// gcHoldBudget is how long one section may hold the exclusive lock before
	// it lets the publishers and readers it excludes through.
	gcHoldBudget = 200 * time.Millisecond
	// gcSectionMinWork is the least work a section does before it may yield,
	// so a store whose relist alone outlasts the budget still makes progress.
	// Small, because slow metadata operations would otherwise stretch every
	// section well past the budget (measured: 256 unlinks took 0.5 s under
	// load).
	gcSectionMinWork = 32
	// gcReacquireWait bounds, in non-blocking mode, how long a pass that has
	// just released the lock retries it to finish its work. The pass released
	// it for the publishers and readers queued behind it, and their holds are
	// short.
	gcReacquireWait = time.Second
	// gcSectionGap, when set (tests only), runs between two sections: after the
	// lock is released and before it is taken again.
	gcSectionGap func()
)

// exclusiveSections holds a store's exclusive lock in sections of about
// gcHoldBudget. Under it no publisher is mid-ingest and no reader is
// mid-restore; between two sections both run.
type exclusiveSections struct {
	s           *LocalStore
	nonBlocking bool
	// deadline bounds, in non-blocking mode, the retries of the FIRST
	// acquisition (GCOptions.LockWait; a zero deadline is a single try).
	deadline time.Time
	release  func()
	// section counts acquisitions, so a caller can tell it has been through
	// a gap in which anything may have been published.
	section int
	start   time.Time
	worked  int
	held    time.Duration
	maxHeld time.Duration
}

// acquire takes the lock: blocking, or in non-blocking mode by retrying a
// contended try until the deadline (at least once). It reports whether the
// lock is held.
func (x *exclusiveSections) acquire() bool {
	if !x.nonBlocking {
		x.release = x.s.lockExclusive()
	} else {
		deadline := x.deadline
		if x.section > 0 {
			deadline = time.Now().Add(gcReacquireWait)
		}
		release, locked := x.s.tryLockExclusive()
		for !locked && time.Now().Before(deadline) {
			time.Sleep(gcLockPoll)
			release, locked = x.s.tryLockExclusive()
		}
		if !locked {
			return false
		}
		x.release = release
	}
	x.section++
	x.start, x.worked = time.Now(), 0
	return true
}

// yield accounts one unit of work about to be done under the lock. Once the
// section has spent gcHoldBudget and done gcSectionMinWork units, it releases
// the lock and takes it again. It returns false when the lock could not be
// taken again: the caller no longer holds it and must stop.
func (x *exclusiveSections) yield() bool {
	if x.worked >= gcSectionMinWork && time.Since(x.start) >= gcHoldBudget {
		x.close()
		if gcSectionGap != nil {
			gcSectionGap()
		}
		if !x.acquire() {
			return false
		}
	}
	x.worked++
	return true
}

// close releases the lock if it is held and accounts the section's hold.
func (x *exclusiveSections) close() {
	if x.release == nil {
		return
	}
	held := time.Since(x.start)
	x.held += held
	x.maxHeld = max(x.maxHeld, held)
	x.release()
	x.release = nil
}

// liveBlobs is the set of CAS blob paths some entry of the store references.
// It starts from the manifests the scan read and only grows, so it is always
// a superset of what the entries it has seen reference.
type liveBlobs struct {
	s     *LocalStore
	paths map[string]bool
	// fanouts is each fan-out directory as last listed (fanoutStamp).
	fanouts map[string]fanoutStamp
	// known is each entry directory as last read, to spot one published or
	// replaced since in a fan-out directory that changed: a new name, another
	// inode, or another mtime (a hit rewrites the lastused sidecar, so it
	// counts, conservatively).
	known map[string]os.FileInfo
}

// refresh reads the manifest of every entry published or replaced since the
// store was last listed. It costs one stat per fan-out directory; only one
// that changed is listed again (one lstat per entry), and only an entry that
// changed has its manifest read. Run inside the section that deletes, it makes
// the set complete for that section: no publisher is mid-ingest, and every
// entry published before the section began is on disk to be listed.
func (l *liveBlobs) refresh() {
	blobsRoot := filepath.Join(l.s.root, "blobs")
	prefixes, _ := os.ReadDir(blobsRoot)
	for _, p := range prefixes {
		info, err := p.Info()
		if err != nil || !info.IsDir() {
			continue
		}
		fanout := filepath.Join(blobsRoot, p.Name())
		if seen, ok := l.fanouts[fanout]; ok && seen.mtime.Equal(info.ModTime()) && seen.read.Sub(seen.mtime) >= gcRacyWindow {
			continue
		}
		l.fanouts[fanout] = fanoutStamp{mtime: info.ModTime(), read: time.Now()}
		hashDirs, _ := os.ReadDir(fanout)
		for _, h := range hashDirs {
			info, err := h.Info()
			if err != nil || !info.IsDir() {
				continue
			}
			blobDir := filepath.Join(blobsRoot, p.Name(), h.Name())
			if old := l.known[blobDir]; old != nil && os.SameFile(old, info) && old.ModTime().Equal(info.ModTime()) {
				continue
			}
			l.known[blobDir] = info
			for _, b := range readManifestBlobs(blobDir) {
				l.paths[l.s.casBlobPath(b.digest)] = true
			}
		}
	}
}

// evictStore removes the victim entries and sweeps the CAS blobs no surviving
// entry references, holding the store's exclusive lock only in sections of
// about gcHoldBudget (exclusiveSections), so the lookups, publishes and
// restores it excludes wait at most about that long at a time.
//
// The work that needs no lock is done outside it. A victim is removed by one
// atomic rename into a trash directory, after its protection is re-checked
// under the lock (it may have been used since enumeration), and the trash is
// deleted once the lock is released: a reader sees the whole entry or none of
// it. The live set starts from the manifests the scan already read, and every
// section that deletes first refreshes it with the entries published or
// replaced since (liveBlobs.refresh), so a blob a publisher linked before the
// section began is never swept, and no publisher can be mid-ingest during it.
//
// In non-blocking mode the first acquisition retries a contended lock until
// lockDeadline — only when there are victims; a sweep-only store gets a single
// try — and gives up when it never gets it; a pass that loses the lock between
// two sections stops there, leaving what it did not reach for the next pass.
// Either way out.busy says so when there were victims. out.held is the total
// time the lock was held, out.maxHeld its longest section.
func evictStore(root string, victims []gcEntry, scan *storeScan, now time.Time, spared func(time.Time) bool, nonBlocking bool, lockDeadline time.Time) (out storeEviction) {
	s := NewLocalStore(root)
	if len(victims) == 0 {
		lockDeadline = time.Time{}
	}
	hasVictims := len(victims) > 0
	tmpRoot := filepath.Join(root, "tmp")
	// A pass that died between renaming its victims aside and deleting them
	// left its trash here. Nothing indexes a trash tree, so it goes first.
	stale, _ := filepath.Glob(filepath.Join(tmpRoot, gcTrashPrefix+"*"))
	for _, dir := range stale {
		_ = os.RemoveAll(dir)
	}

	lock := &exclusiveSections{s: s, nonBlocking: nonBlocking, deadline: lockDeadline}
	if !lock.acquire() {
		return storeEviction{busy: hasVictims} // a later pass / explicit gc handles it
	}
	var trash string
	defer func() {
		lock.close()
		out.held, out.maxHeld = lock.held, lock.maxHeld
		if trash != "" {
			_ = os.RemoveAll(trash) // outside the lock: nothing indexes it any more
		}
	}()

	live := &liveBlobs{s: s, paths: map[string]bool{}, fanouts: map[string]fanoutStamp{}, known: map[string]os.FileInfo{}}
	isVictim := make(map[string]bool, len(victims))
	for _, v := range victims {
		isVictim[v.blobDir] = true
	}
	var casFiles map[string]int64
	if scan != nil {
		casFiles = scan.casFiles
		for fanout, stamp := range scan.fanouts {
			live.fanouts[fanout] = stamp
		}
		for _, e := range scan.entries {
			live.known[e.blobDir] = e.ident // nil (OCI, or unreadable) reads as unknown
			if !isVictim[e.blobDir] {
				for _, b := range e.blobs {
					live.paths[s.casBlobPath(b.digest)] = true
				}
			}
		}
	}

	for _, v := range victims {
		if !lock.yield() {
			out.busy = hasVictims
			return out
		}
		removed := false
		if last, _ := readLastUsed(v.blobDir); last.IsZero() || !spared(last) {
			if trash == "" {
				if os.MkdirAll(tmpRoot, 0o755) == nil {
					trash, _ = os.MkdirTemp(tmpRoot, gcTrashPrefix)
				}
			}
			if trash != "" {
				err := os.Rename(v.blobDir, filepath.Join(trash, strconv.Itoa(out.evicted)))
				removed = err == nil
				if os.IsNotExist(err) {
					continue // already gone (e.g. a concurrent clean) — don't count it
				}
			}
		}
		if !removed {
			// Spared (used since enumeration) or not removable: still indexed.
			for _, b := range v.blobs {
				live.paths[s.casBlobPath(b.digest)] = true
			}
			continue
		}
		out.evicted++
		out.freed += v.rawSize
		if v.idle {
			out.idleEvicted++
		}
	}

	// Sweep, in a stable order, the CAS files the scan listed and no entry it
	// read references. The live set only grows, so what is live now stays live.
	candidates := make([]string, 0, len(casFiles))
	for path := range casFiles {
		if !live.paths[path] {
			candidates = append(candidates, path)
		}
	}
	sort.Strings(candidates)
	refreshed := 0
	for _, path := range candidates {
		if !lock.yield() {
			out.busy = hasVictims
			return out
		}
		if refreshed != lock.section {
			live.refresh()
			refreshed = lock.section
		}
		if live.paths[path] {
			continue
		}
		if os.Remove(path) == nil {
			out.swept++
			out.freed += casFiles[path]
		}
	}

	// Tidy now-empty 2-char prefix dirs left by eviction/sweep, and the
	// released or expired leases. Skipped when the lock cannot be taken back.
	if !lock.yield() {
		return out
	}
	removeEmptyPrefixDirs(filepath.Join(root, "blobs"))
	removeEmptyPrefixDirs(filepath.Join(root, "cas"))
	ociRoot := filepath.Join(root, OCILayerCacheDirName)
	versions, _ := os.ReadDir(ociRoot)
	for _, version := range versions {
		if !version.IsDir() {
			continue
		}
		versionDir := filepath.Join(ociRoot, version.Name())
		removeEmptyPrefixDirs(versionDir)
		if entries, err := os.ReadDir(versionDir); err == nil && len(entries) == 0 {
			_ = os.Remove(versionDir)
		}
	}
	reapExpiredLeases(root, now)
	return out
}

// readManifestBlobs returns the CAS blob refs (digest + size) from an entry's
// manifest, or nil for entries without output files.
func readManifestBlobs(blobDir string) []blobRef {
	data, err := os.ReadFile(filepath.Join(blobDir, manifestFilename))
	if err != nil {
		return nil
	}
	var m cache.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	refs := make([]blobRef, 0, len(m.Files))
	for _, f := range m.Files {
		refs = append(refs, blobRef{digest: f.Digest, size: f.Size})
	}
	return refs
}

// blobMetaBytes sums an entry's small metadata files (everything but the files/
// hardlink tree, which is accounted via the CAS).
func blobMetaBytes(blobDir string) int64 {
	var total int64
	for _, name := range []string{
		"meta.json", "result.json", manifestFilename, lastUsedFile,
		// A negative (failure) entry is exactly this one file plus its lastUsed
		// sidecar, so listing it here is what makes the record count against the
		// store's byte budget like every other blob (task_failure.go).
		taskFailureRecordFilename,
	} {
		if fi, err := os.Stat(filepath.Join(blobDir, name)); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// dirBytes sums the sizes of all regular files under dir.
func dirBytes(dir string) int64 {
	var total int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// removeEmptyPrefixDirs removes empty {xx}/ fan-out directories under dir.
func removeEmptyPrefixDirs(dir string) {
	prefixes, _ := os.ReadDir(dir)
	for _, p := range prefixes {
		if !p.IsDir() {
			continue
		}
		sub := filepath.Join(dir, p.Name())
		if entries, err := os.ReadDir(sub); err == nil && len(entries) == 0 {
			os.Remove(sub)
		}
	}
}

// CleanStore wipes all cached content (blobs/, cas/, tmp/, oci/) from a single
// per-repo store under its exclusive lock, so it waits for in-flight CAS
// publishes to drain and blocks new CAS publishers for the wipe. OCI publishers
// tolerate a concurrent removal because they retain their build-owned blob and
// publish cache entries atomically. The store directory and its lock file are
// preserved so cross-process lock identity stays intact.
// Active compute leases are also preserved; released/expired leases are reaped.
// Returns the file count and bytes removed.
func CleanStore(root string) (files int, bytes int64, err error) {
	if !isDir(root) {
		return 0, 0, nil
	}
	s := NewLocalStore(root)
	release := s.lockExclusive()
	defer release()

	for _, sub := range []string{"blobs", "cas", "tmp", OCILayerCacheDirName} {
		dir := filepath.Join(root, sub)
		f, b := countDir(dir)
		files += f
		bytes += b
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return files, bytes, fmt.Errorf("remove %s: %w", sub, rmErr)
		}
	}
	reapExpiredLeases(root, time.Now())
	return files, bytes, nil
}

// countDir returns the number of regular files and total bytes under dir.
func countDir(dir string) (files int, bytes int64) {
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes
}
