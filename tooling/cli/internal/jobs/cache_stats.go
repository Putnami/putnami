package jobs

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/store"
)

// CacheStats accumulates local and remote build-cache activity across a single
// run so the CLI can account for the work performed between task executions as
// well as remote-cache economics.
//
// Its counters are updated from many goroutines at once (the negotiate happens
// on the coordinator, restores and uploads on worker goroutines, downloads on
// background prefetch goroutines), so every counter is an atomic and every method
// is safe for concurrent use. Interval sets are mutex-guarded: concurrent work
// is attributed from its wall-time union rather than summed worker durations.
type CacheStats struct {
	// The local read leg runs partly before workers exist (version/capability
	// bindings) and partly on worker goroutines (keys and restore/verification).
	// Counters are atomic; interval sets are mutex guarded so Snapshot can report
	// elapsed wall as a union instead of multiplying overlapping worker time.
	localHits             atomic.Int64
	localMisses           atomic.Int64
	localSpawnedProcesses atomic.Int64
	localMu               sync.Mutex
	localIntervals        []localCacheInterval

	// Setup is the wall time spent resolving the cache config and token before
	// the run timer starts — the cost the user observed happening "before
	// negotiate" and outside the session clock. Recorded so it is no longer
	// invisible.
	setupNanos atomic.Int64

	// Negotiate is the single batch read round trip.
	negotiateNanos atomic.Int64
	keysRequested  atomic.Int64
	hits           atomic.Int64
	misses         atomic.Int64

	// Restore is the read-side payoff: jobs served from a remote hit instead of
	// rebuilt, and the build time that avoided.
	restored     atomic.Int64
	hintsWarmed  atomic.Int64
	timeSavedMs  atomic.Int64
	bytesFetched atomic.Int64 // bytes materialized from remote hits
	// restoreMu guards restoreIntervals, the [start, end] window each hit spent
	// being materialized into the local store (download + verify + install).
	// Restores run concurrently on worker goroutines, so the wall cost they add to
	// the run is the UNION of these windows, not their sum — summing would
	// overstate a parallel restore as badly as the old code understated it (by not
	// measuring it at all). Snapshot folds the union into RestoreMs.
	restoreMu        sync.Mutex
	restoreIntervals []restoreInterval

	// Upload is the write-side cost: blobs pushed to CAS after a local build,
	// and the dedup the server reported (blobs it already had).
	uploads       atomic.Int64
	blobsUploaded atomic.Int64
	bytesUploaded atomic.Int64
	bytesDeduped  atomic.Int64
	// uploadWallNanos is the time the build waited at the end for background
	// uploads to drain — the latency uploads actually ADDED to the makespan.
	// Uploads that fully overlapped the build contribute nothing here. It is the
	// honest "spent on upload" figure now that uploads run off the worker path.
	uploadWallNanos atomic.Int64
	uploadFailures  atomic.Int64
	// uploadsSkipped counts freshly built misses NOT stored remotely because
	// they fell below the break-even guard (too cheap to be worth the transfer).
	uploadsSkipped atomic.Int64

	// providerSummary* preserves the exact totals returned by the cache
	// provider's terminal Summary operation. The aggregate counters above are
	// useful during a run (and may be updated while individual transfers
	// complete); these fields make the provider's authoritative end-of-run byte
	// accounting available to machine-readable consumers such as benchmarks.
	// storeBudget is what the run's in-run store budget did, set once at the
	// end of the run (recordStoreBudget) and immutable after.
	storeBudgetMu sync.Mutex
	storeBudget   *StoreBudgetStats

	providerSummaryRestoredCount atomic.Int64
	providerSummaryRestoredBytes atomic.Int64
	providerSummaryUploadedCount atomic.Int64
	providerSummaryUploadedBytes atomic.Int64
	providerSummaryAvailable     atomic.Bool
}

type restoreInterval struct {
	start time.Time
	end   time.Time
}

type localCachePhase uint8

const (
	localCacheKeys localCachePhase = iota + 1
	localCacheBindings
	localCacheRestoreVerify
)

type localCacheInterval struct {
	restoreInterval
	phase localCachePhase
}

func (s *CacheStats) recordLocalHit() {
	if s != nil {
		s.localHits.Add(1)
	}
}

func (s *CacheStats) recordLocalMiss() {
	if s != nil {
		s.localMisses.Add(1)
	}
}

func (s *CacheStats) recordLocalPhase(phase localCachePhase, start, end time.Time) {
	if s == nil || !end.After(start) {
		return
	}
	s.localMu.Lock()
	s.localIntervals = append(s.localIntervals, localCacheInterval{
		restoreInterval: restoreInterval{start: start, end: end},
		phase:           phase,
	})
	s.localMu.Unlock()
}

func (s *CacheStats) recordLocalKeys(start, end time.Time) {
	s.recordLocalPhase(localCacheKeys, start, end)
}

func (s *CacheStats) recordLocalBindings(start, end time.Time, spawnedProcesses int) {
	s.recordLocalPhase(localCacheBindings, start, end)
	if s != nil && spawnedProcesses > 0 {
		s.localSpawnedProcesses.Add(int64(spawnedProcesses))
	}
}

func (s *CacheStats) recordLocalRestoreVerify(start, end time.Time) {
	s.recordLocalPhase(localCacheRestoreVerify, start, end)
}

type localCacheAttribution struct {
	served        int64
	keys          int64
	bindings      int64
	restoreVerify int64
}

type localCacheEvent struct {
	at    time.Time
	phase localCachePhase
	delta int
}

// localWallAttributionNanos partitions the union of local-cache intervals into
// additive phases. A timeline segment can have several active phases because
// workers run in parallel and restore may call version re-stamping. Each such
// segment is assigned once, by stable priority: bindings, keys, then
// restore-verify. Giving bindings priority preserves the attribution of nested
// source-binding subprocess work; the broader restore window receives the
// remaining wall. Thus phase totals never multiply parallel time and their
// exact nanosecond sum equals served.
func (s *CacheStats) localWallAttributionNanos() localCacheAttribution {
	if s == nil {
		return localCacheAttribution{}
	}
	s.localMu.Lock()
	intervals := make([]localCacheInterval, len(s.localIntervals))
	copy(intervals, s.localIntervals)
	s.localMu.Unlock()
	if len(intervals) == 0 {
		return localCacheAttribution{}
	}

	events := make([]localCacheEvent, 0, len(intervals)*2)
	for _, interval := range intervals {
		events = append(events,
			localCacheEvent{at: interval.start, phase: interval.phase, delta: 1},
			localCacheEvent{at: interval.end, phase: interval.phase, delta: -1},
		)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })

	var out localCacheAttribution
	var active [localCacheRestoreVerify + 1]int
	previous := events[0].at
	for i := 0; i < len(events); {
		at := events[i].at
		if at.After(previous) {
			nanos := at.Sub(previous).Nanoseconds()
			switch {
			case active[localCacheBindings] > 0:
				out.bindings += nanos
			case active[localCacheKeys] > 0:
				out.keys += nanos
			case active[localCacheRestoreVerify] > 0:
				out.restoreVerify += nanos
			default:
				nanos = 0
			}
			out.served += nanos
		}
		for i < len(events) && events[i].at.Equal(at) {
			active[events[i].phase] += events[i].delta
			i++
		}
		previous = at
	}
	return out
}

func (s *CacheStats) recordSetup(nanos int64) {
	s.setupNanos.Store(nanos)
}

// recordNegotiate captures the outcome of the one batch read round trip: how
// long it took, how many keys were asked, and how the server split them into
// hits and misses.
func (s *CacheStats) recordNegotiate(nanos int64, keys, hits int) {
	s.negotiateNanos.Store(nanos)
	s.keysRequested.Store(int64(keys))
	s.hits.Store(int64(hits))
	s.misses.Store(int64(keys - hits))
}

// recordProviderHit reclassifies one negotiated key from miss to hit. The
// provider protocol returns no batch hit/miss split, so Negotiate seeds every
// requested key as a miss (hits=0) and each successful Restore promotes its key
// here, keeping hits+misses == keysRequested. misses floors at zero so a stray
// restore for a key Negotiate did not count can never drive it negative.
func (s *CacheStats) recordProviderHit() {
	s.hits.Add(1)
	for {
		cur := s.misses.Load()
		if cur <= 0 {
			return
		}
		if s.misses.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// recordRestore counts one job served from a remote hit and the build time it
// avoided (the cached entry's original duration).
func (s *CacheStats) recordRestore(timeSavedMs int64) {
	s.restored.Add(1)
	if timeSavedMs > 0 {
		s.timeSavedMs.Add(timeSavedMs)
	}
}

// recordHintWarm counts a provider hit whose blobs were admitted to CAS but
// whose result was deliberately rejected by the run's authoritative policy.
// It is distinct from Restored: a warmed hint never marks a job green.
func (s *CacheStats) recordHintWarm() {
	s.hintsWarmed.Add(1)
}

// recordFetch counts bytes materialized from a remote hit. It runs once per
// distinct hash (inside the materialize-once guard), so a hit consumed by both
// a prefetch and a Restore is counted a single time.
func (s *CacheStats) recordFetch(bytes int64) {
	if bytes > 0 {
		s.bytesFetched.Add(bytes)
	}
}

// recordMaterialize records the window [start, end] one remote hit spent being
// materialized into the local store (download + verify + install). Like
// recordFetch it runs once per distinct hash, so a hit shared by a prefetch and a
// Restore is timed a single time. restoreWallNanos later unions overlapping
// windows so concurrent restores are not double-counted. time.Time preserves
// Go's monotonic clock readings from time.Now, keeping elapsed restore math
// immune to wall-clock adjustments.
func (s *CacheStats) recordMaterialize(start, end time.Time) {
	if !end.After(start) {
		return
	}
	s.restoreMu.Lock()
	s.restoreIntervals = append(s.restoreIntervals, restoreInterval{start: start, end: end})
	s.restoreMu.Unlock()
}

// restoreWallNanos returns the wall time restores added to the run: the measure
// of the union of every materialize window. Concurrent restores that overlap
// count once, so the figure never exceeds the run's makespan.
func (s *CacheStats) restoreWallNanos() int64 {
	s.restoreMu.Lock()
	iv := make([]restoreInterval, len(s.restoreIntervals))
	copy(iv, s.restoreIntervals)
	s.restoreMu.Unlock()
	return intervalWallNanos(iv)
}

func intervalWallNanos(iv []restoreInterval) int64 {
	if len(iv) == 0 {
		return 0
	}
	sort.Slice(iv, func(i, j int) bool { return iv[i].start.Before(iv[j].start) })
	var total time.Duration
	curStart, curEnd := iv[0].start, iv[0].end
	for _, p := range iv[1:] {
		if p.start.After(curEnd) { // disjoint: close the current run, open a new one
			total += curEnd.Sub(curStart)
			curStart, curEnd = p.start, p.end
			continue
		}
		if p.end.After(curEnd) { // overlapping/adjacent: extend the current run
			curEnd = p.end
		}
	}
	return (total + curEnd.Sub(curStart)).Nanoseconds()
}

// recordUploadWall records the wall time the build spent draining background
// uploads at the end of the run — the latency uploads added to the makespan.
func (s *CacheStats) recordUploadWall(nanos int64) {
	s.uploadWallNanos.Store(nanos)
}

func (s *CacheStats) recordUploadFailure() {
	s.uploadFailures.Add(1)
}

// recordUploadSkipped counts one built miss that was not stored remotely
// because it fell below the break-even guard.
func (s *CacheStats) recordUploadSkipped() {
	s.uploadsSkipped.Add(1)
}

func (s *CacheStats) recordProviderSummary(sum *cache.SummaryResult) {
	if sum == nil {
		return
	}
	s.providerSummaryRestoredCount.Store(int64(sum.RestoredCount))
	s.providerSummaryRestoredBytes.Store(sum.RestoredBytes)
	s.providerSummaryUploadedCount.Store(int64(sum.UploadedCount))
	s.providerSummaryUploadedBytes.Store(sum.UploadedBytes)
	s.providerSummaryAvailable.Store(true)
	if sum.RestoredBytes > 0 && s.bytesFetched.Load() < sum.RestoredBytes {
		s.bytesFetched.Store(sum.RestoredBytes)
	}
	if sum.UploadedCount > 0 {
		s.uploads.Store(int64(sum.UploadedCount))
	}
	if sum.UploadedBytes > 0 {
		s.bytesUploaded.Store(sum.UploadedBytes)
	}
}

// recordStoreBudget keeps what the run's in-run store budget did for the
// session record. A budget that never started a pass records nothing, so the
// member stays absent.
func (s *CacheStats) recordStoreBudget(report store.RunBudgetReport) {
	if s == nil || report.Passes == 0 {
		return
	}
	stats := &StoreBudgetStats{
		Passes:            int64(report.Passes),
		EvictedEntries:    int64(report.EvictedEntries),
		FreedBytes:        report.FreedBytes,
		StoresSkippedBusy: int64(report.StoresSkippedBusy),
		LockHoldMs:        report.LockHeld.Milliseconds(),
		MaxLockHoldMs:     report.MaxLockHeld.Milliseconds(),
	}
	s.storeBudgetMu.Lock()
	s.storeBudget = stats
	s.storeBudgetMu.Unlock()
}

// Snapshot returns the current totals as a plain value.
func (s *CacheStats) Snapshot() *CacheStatsSnapshot {
	local := s.localWallAttributionNanos()
	s.storeBudgetMu.Lock()
	storeBudget := s.storeBudget
	s.storeBudgetMu.Unlock()
	return &CacheStatsSnapshot{
		LocalHits:             s.localHits.Load(),
		LocalMisses:           s.localMisses.Load(),
		LocalServedMs:         local.served / 1e6,
		LocalKeysMs:           local.keys / 1e6,
		LocalBindingsMs:       local.bindings / 1e6,
		LocalRestoreVerifyMs:  local.restoreVerify / 1e6,
		LocalSpawnedProcesses: s.localSpawnedProcesses.Load(),

		SetupMs:        s.setupNanos.Load() / 1e6,
		NegotiateMs:    s.negotiateNanos.Load() / 1e6,
		KeysRequested:  s.keysRequested.Load(),
		Hits:           s.hits.Load(),
		Misses:         s.misses.Load(),
		Restored:       s.restored.Load(),
		HintsWarmed:    s.hintsWarmed.Load(),
		TimeSavedMs:    s.timeSavedMs.Load(),
		BytesFetched:   s.bytesFetched.Load(),
		RestoreMs:      s.restoreWallNanos() / 1e6,
		Uploads:        s.uploads.Load(),
		BlobsUploaded:  s.blobsUploaded.Load(),
		BytesUploaded:  s.bytesUploaded.Load(),
		BytesDeduped:   s.bytesDeduped.Load(),
		UploadMs:       s.uploadWallNanos.Load() / 1e6,
		UploadErrors:   s.uploadFailures.Load(),
		UploadsSkipped: s.uploadsSkipped.Load(),

		ProviderSummaryRestoredCount: s.providerSummaryRestoredCount.Load(),
		ProviderSummaryRestoredBytes: s.providerSummaryRestoredBytes.Load(),
		ProviderSummaryUploadedCount: s.providerSummaryUploadedCount.Load(),
		ProviderSummaryUploadedBytes: s.providerSummaryUploadedBytes.Load(),
		ProviderSummaryAvailable:     s.providerSummaryAvailable.Load(),

		StoreBudget: storeBudget,
	}
}
