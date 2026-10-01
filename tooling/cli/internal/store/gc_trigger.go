package store

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/tooling/cli/internal/flock"
)

const (
	// gcStampFile records when opportunistic GC last ran, throttling it.
	gcStampFile = ".gc-stamp"
	// opportunisticGCInterval bounds how often a build triggers a budget walk.
	opportunisticGCInterval = time.Hour
	// gcUsageFile, beside gcStampFile, holds the store usage in decimal bytes:
	// what the last complete collection pass measured, net of what it freed,
	// plus the growth runs have added since (RunBudget.Close). It is what lets
	// a run's in-run budget start from a number instead of a walk.
	gcUsageFile = ".gc-usage"
)

// MaybeOpportunisticGC runs a throttled, best-effort GC pass over the global
// store after a build in workspaceRoot. It is throttled to at most once per
// machine per opportunisticGCInterval via a stamp file, so the (potentially
// expensive) budget walk happens rarely, and only evicts when over budget or
// for idle reclaim. GC runs in NON-BLOCKING mode so a finishing build never
// stalls behind a busy sibling store. All errors are swallowed: GC is advisory
// and must never fail or slow a build beyond the throttled interval. cfg carries
// the config-file GC settings (env still overrides).
func MaybeOpportunisticGC(workspaceRoot string, cfg Config) {
	stamp := gcStampPath()
	if stamp == "" {
		return
	}
	if !gcThrottleElapsed(stamp, opportunisticGCInterval) {
		return
	}
	// Touch the stamp before running so concurrent builds don't all trigger at
	// once (a small race where two builds both run is harmless — GC takes each
	// store's exclusive lock and serializes).
	touchStamp(stamp)

	roots := StoreRootsForGCIncluding(workspaceRoot)
	if len(roots) == 0 {
		return
	}
	res, err := RunGC(roots, GCOptions{
		MaxBytes:           ResolveMaxBytes(cfg),
		Grace:              ResolveGCGrace(cfg),
		MaxIdleGenerations: ResolveMaxIdleBuilds(cfg),
		NonBlocking:        true,
	})
	recordGCUsage(stamp, res, err)
}

// RunBudget holds the store's byte budget while a run is still in flight.
// MaybeOpportunisticGC runs only once a run has ended, and at most
// hourly, so on its own it cannot stop one long run from growing the store past
// the budget, or past a small disk, before that run ends.
//
// The check the scheduler makes at every batch boundary is cheap by
// construction: projected usage is the usage last recorded in gcUsageFile (read
// once when the budget is created) plus the CAS bytes this run's store handle
// has written since (LocalStore.added, an atomic counter). Nothing walks the
// store unless that projection exceeds the budget, or nothing has been
// recorded yet; then the first boundary after this run's first write takes
// one. A measuring pass overwrites the record; a run that ends with bytes no
// measurement saw adds them to it (Close), so consecutive runs within the
// post-build pass's hourly throttle still see each other's growth.
//
// A pass runs in the background: the coordinator never waits for it, and it
// never fails the run. It leaves idle reclaim to the post-build pass and spares
// every entry used since the budget was created (GCOptions.ProtectSince) on top
// of the grace window. Every entry this run looks up, restores, prefetches or
// publishes carries a lastUsed at or after that instant, so no job of this run
// ever loses an entry it has touched: the most a pass can cost the run is a
// cache miss on an entry it had not reached yet. The same rule is why a pass
// can evict no entry on a fresh disk, where every entry is this run's own; it
// can only sweep CAS blobs no entry references.
//
// A pass DOES make the run wait: while it removes entries and sweeps blobs it
// holds the store's exclusive lock, and every lookup, publish and restore on
// that store, in this run and in any other session sharing the store, waits
// for it. evictStore holds it in sections of about 200 ms (gcHoldBudget), so a
// waiter waits about that long at a time; the session record reports the
// total and the longest section (RunBudgetReport).
type RunBudget struct {
	workspaceRoot string
	local         *LocalStore
	opts          GCOptions
	stamp         string // gcStampPath(); the usage record sits beside it

	mu      sync.Mutex
	known   bool  // usage is a measurement: the record, or a pass of this run
	usage   int64 // bytes that measurement saw, net of what its pass freed
	since   int64 // local.added when that measurement read this store
	running bool
	// busyRetries counts the passes in a row that could not finish: a store
	// they needed was busy. Such a pass is not a measurement, so the
	// projection stays over budget and the next boundary retries it — at
	// once up to maxBusyRetries times, then only after the run has grown by
	// another tenth of the budget since lastPass.
	busyRetries int
	lastPass    int64 // local.added when the last pass read this store
	report      RunBudgetReport
	wg          sync.WaitGroup
}

// RunBudgetReport is what a run's in-run budget did, for the session record.
type RunBudgetReport struct {
	Passes         int
	EvictedEntries int
	FreedBytes     int64
	// StoresSkippedBusy counts stores a pass skipped because another holder
	// had them locked: during the scan (their bytes went unmeasured) or with
	// victims to evict (the budget was not enforced there).
	StoresSkippedBusy int
	// LockHeld is the total time the passes held stores' exclusive locks, the
	// time lookups, publishes and restores on them waited; MaxLockHeld is the
	// longest single section.
	LockHeld    time.Duration
	MaxLockHeld time.Duration
}

const (
	// runBudgetLockWait is how long an in-run pass retries the exclusive lock
	// of a store it has victims in (GCOptions.LockWait). The run's own workers
	// hold the shared lock for one lookup, restore or ingest at a time, so a
	// busy store still has gaps; this finds one without ever queuing ahead of
	// a writer.
	runBudgetLockWait = 2 * time.Second
	// maxBusyRetries is how many passes in a row may retry at once after a
	// busy one. A busy store is usually busy for a moment, so the next
	// boundary is the right time to try again; a store that stays busy would
	// otherwise be walked at every boundary.
	maxBusyRetries = 3
)

// NewRunBudget arms the in-run budget for one run writing through local. Create
// it before the run's first cache lookup: that instant is the run's protection
// boundary. A nil local (caching off) yields a nil budget, whose methods are
// no-ops, and so does a handle on a RELOCATED store (a cache verification's
// temporary store): its bytes are not the machine's to count, and collecting it
// mid-verification would take the entries the verification's hit run is about
// to read.
func NewRunBudget(workspaceRoot string, cfg Config, local *LocalStore) *RunBudget {
	if local == nil || local.Root() != ResolveStoreRoot(workspaceRoot) {
		return nil
	}
	b := &RunBudget{
		workspaceRoot: workspaceRoot,
		local:         local,
		opts: GCOptions{
			MaxBytes:     ResolveMaxBytes(cfg),
			Grace:        ResolveGCGrace(cfg),
			NonBlocking:  true,
			LockWait:     runBudgetLockWait,
			ProtectSince: time.Now(),
		},
		stamp: gcStampPath(),
		since: local.added.Load(),
	}
	b.lastPass = b.since
	updateGCUsage(b.stamp, func(prev int64, ok bool) (int64, bool) {
		b.known, b.usage = ok, prev
		return 0, false
	})
	return b
}

// AfterBatch is the check the scheduler makes each time a dispatch group
// completes. It costs an atomic load and a comparison unless a pass is due, and
// the coordinator never waits for one: a due pass starts in the background, and
// a boundary that arrives while one is running changes nothing.
func (b *RunBudget) AfterBatch() {
	if b == nil {
		return
	}
	added := b.local.added.Load()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return
	}
	grown := added - b.since
	over := (!b.known && grown > 0) || (b.known && b.usage+grown > b.opts.MaxBytes)
	var due bool
	switch {
	case !over:
		due = false
	case b.busyRetries > maxBusyRetries:
		due = added-b.lastPass > b.opts.MaxBytes/10
	case b.busyRetries > 0, !b.known, b.usage <= b.opts.MaxBytes:
		// A retry, the one walk that measures an unmeasured store, or this
		// run's own growth crossing the budget.
		due = true
	default:
		// The last measurement was already over budget: what it could not
		// evict was protected. Walking again at every boundary would reclaim
		// nothing more, so wait for the run to grow by another tenth.
		due = grown > b.opts.MaxBytes/10
	}
	if !due {
		return
	}
	b.running = true
	b.report.Passes++
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		// This run's own store is scanned FIRST, and its counter read right
		// before: the scan then sees every byte counted before `snapshot`, and
		// the only bytes both the scan and added-snapshot can count are those
		// published while that one store is being walked — a conservative
		// over-count bounded by the run's growth during that walk.
		own := b.local.Root()
		roots := append([]string{own}, slices.DeleteFunc(StoreRootsForGCIncluding(b.workspaceRoot), func(root string) bool { return root == own })...)
		snapshot := b.local.added.Load()
		res, err := RunGC(roots, b.opts)
		usage, measured := recordGCUsage(b.stamp, res, err)
		b.mu.Lock()
		defer b.mu.Unlock()
		b.running = false
		b.lastPass = snapshot
		if res != nil {
			b.report.EvictedEntries += res.EvictedEntries
			b.report.FreedBytes += res.FreedBytes
			b.report.StoresSkippedBusy += res.ScanSkippedBusy + res.EvictSkippedBusy
			b.report.LockHeld += res.LockHeld
			b.report.MaxLockHeld = max(b.report.MaxLockHeld, res.MaxLockHeld)
		}
		if !measured {
			b.busyRetries++
			return
		}
		b.known, b.usage, b.since, b.busyRetries = true, usage, snapshot, 0
	}()
}

// Close joins the pass AfterBatch may have left running, adds the bytes this
// run wrote that no measurement has seen to the usage record, and returns what
// the budget did. The scheduler calls it once, after its workers are gone, so
// the process never exits in the middle of an eviction.
//
// The growth is added only to a record that exists: a run that knows nothing
// about the rest of the store must not turn its own bytes into the machine's
// usage. The post-build pass, when its throttle lets it run, overwrites the sum
// with a measurement. A byte another process's pass already measured is added
// again here — the same conservative bias, corrected by the next measurement.
func (b *RunBudget) Close() RunBudgetReport {
	if b == nil {
		return RunBudgetReport{}
	}
	b.wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if unaccounted := b.local.added.Load() - b.since; unaccounted > 0 {
		updateGCUsage(b.stamp, func(prev int64, ok bool) (int64, bool) {
			return prev + unaccounted, ok
		})
		b.since += unaccounted
	}
	return b.report
}

// recordGCUsage persists the usage a collection pass measured, net of what it
// freed, and returns it. A pass that scanned no store, or skipped one that was
// busy (during the scan, so its bytes are missing, or with victims in it, so
// it did not finish) measured nothing and records nothing.
func recordGCUsage(stamp string, res *GCResult, err error) (int64, bool) {
	if err != nil || res == nil || res.StoresScanned == 0 || res.ScanSkippedBusy > 0 || res.EvictSkippedBusy > 0 {
		return 0, false
	}
	usage := max(res.ScannedBytes-res.FreedBytes, 0)
	updateGCUsage(stamp, func(int64, bool) (int64, bool) { return usage, true })
	return usage, true
}

// updateGCUsage reads and rewrites the usage record beside stamp under its own
// lock (gcUsageFile+".lock"): next receives the current value (ok is false when
// there is none) and returns the value to write and whether to write at all.
// Measurements and a run's growth both go through here, so no write lands
// between another's read and its write. The lock guards one small read and
// write, so a blocking acquire waits microseconds; the write is a temp file
// renamed into place. Best-effort: a lost update costs the next run one
// measuring pass or one late one, never a wrong eviction.
func updateGCUsage(stamp string, next func(prev int64, ok bool) (int64, bool)) {
	if stamp == "" {
		return
	}
	dir := filepath.Dir(stamp)
	if os.MkdirAll(dir, 0o755) != nil {
		return
	}
	lock, err := flock.Acquire(filepath.Join(dir, gcUsageFile+".lock"), true, false)
	if err != nil {
		return
	}
	defer func() { _ = lock.Release() }()

	path := filepath.Join(dir, gcUsageFile)
	var prev int64
	ok := false
	if data, err := os.ReadFile(path); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && n >= 0 {
			prev, ok = n, true
		}
	}
	value, write := next(prev, ok)
	if !write {
		return
	}
	tmp, err := os.CreateTemp(dir, gcUsageFile+"-")
	if err != nil {
		return
	}
	_, writeErr := tmp.WriteString(strconv.FormatInt(value, 10))
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// gcStampPath returns where the throttle stamp lives: under ~/.putnami/store
// normally, or inside the overridden store dir when PUTNAMI_STORE_DIR is set.
func gcStampPath() string {
	if parent, ok := GlobalStoreParent(); ok {
		if dir := os.Getenv(storeDirEnv); dir == "" {
			return filepath.Join(parent, gcStampFile)
		}
	}
	roots := StoreRootsForGC()
	if len(roots) == 0 {
		return ""
	}
	return filepath.Join(roots[0], gcStampFile)
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
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		return
	}
	now := time.Now()
	if err := os.WriteFile(stamp, []byte(now.Format(time.RFC3339)), 0o644); err != nil {
		return
	}
	_ = os.Chtimes(stamp, now, now)
}
