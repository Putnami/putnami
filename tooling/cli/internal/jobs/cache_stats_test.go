package jobs

import (
	"sync"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
)

func TestCacheStats_SnapshotAggregates(t *testing.T) {
	t.Parallel()
	s := &CacheStats{}
	s.recordSetup((90 * time.Millisecond).Nanoseconds())
	s.recordNegotiate((60 * time.Millisecond).Nanoseconds(), 15, 12)
	// Three jobs restored from remote hits, each saving its original build time.
	s.recordRestore(100_000)
	s.recordRestore(80_000)
	s.recordRestore(0) // a hit with unknown duration contributes count, not time
	s.recordHintWarm()
	s.recordFetch(8_400_000)
	base := time.Now()
	s.recordMaterialize(base, base.Add(40*time.Millisecond)) // a single 40ms window
	// The provider reports its upload totals once, at drain, via the run summary.
	s.recordProviderSummary(&cache.SummaryResult{
		RestoredCount: 3,
		RestoredBytes: 8_400_000,
		UploadedCount: 1,
		UploadedBytes: 2_100_000,
	})
	s.recordUploadWall((30 * time.Millisecond).Nanoseconds())
	s.recordUploadFailure()

	snap := s.Snapshot()
	if snap.SetupMs != 90 || snap.NegotiateMs != 60 || snap.UploadMs != 30 {
		t.Errorf("durations = setup %d / negotiate %d / upload %d", snap.SetupMs, snap.NegotiateMs, snap.UploadMs)
	}
	if snap.KeysRequested != 15 || snap.Hits != 12 || snap.Misses != 3 {
		t.Errorf("negotiate counts = %d keys / %d hits / %d miss", snap.KeysRequested, snap.Hits, snap.Misses)
	}
	if snap.Restored != 3 || snap.TimeSavedMs != 180_000 {
		t.Errorf("restore = %d jobs / %d ms saved, want 3 / 180000", snap.Restored, snap.TimeSavedMs)
	}
	if snap.HintsWarmed != 1 {
		t.Errorf("hints warmed = %d, want 1", snap.HintsWarmed)
	}
	if snap.BytesFetched != 8_400_000 || snap.BytesUploaded != 2_100_000 {
		t.Errorf("bytes = %d fetched / %d uploaded", snap.BytesFetched, snap.BytesUploaded)
	}
	if snap.ProviderSummaryRestoredCount != 3 || snap.ProviderSummaryRestoredBytes != 8_400_000 ||
		snap.ProviderSummaryUploadedCount != 1 || snap.ProviderSummaryUploadedBytes != 2_100_000 {
		t.Errorf("provider summary = restored %d/%d uploaded %d/%d",
			snap.ProviderSummaryRestoredCount, snap.ProviderSummaryRestoredBytes,
			snap.ProviderSummaryUploadedCount, snap.ProviderSummaryUploadedBytes)
	}
	if !snap.ProviderSummaryAvailable {
		t.Error("provider summary should be marked available after recordProviderSummary")
	}
	if snap.RestoreMs != 40 {
		t.Errorf("RestoreMs = %d, want 40", snap.RestoreMs)
	}
	if snap.Uploads != 1 || snap.UploadErrors != 1 {
		t.Errorf("upload = %d uploads / %d errors", snap.Uploads, snap.UploadErrors)
	}
	// Overhead is the cache's own wall cost: setup + negotiate + restore + upload.
	if got := snap.OverheadMs(); got != 220 {
		t.Errorf("OverheadMs = %d, want 220", got)
	}
	if (*CacheStatsSnapshot)(nil).OverheadMs() != 0 {
		t.Error("nil snapshot overhead should be 0")
	}
}

func TestCacheStats_ProviderRestoreSummaryDoesNotGreenHints(t *testing.T) {
	t.Parallel()
	s := &CacheStats{}
	s.recordHintWarm()
	s.recordProviderSummary(&cache.SummaryResult{RestoredCount: 1, RestoredBytes: 42})
	snap := s.Snapshot()
	if snap.Restored != 0 || snap.HintsWarmed != 1 {
		t.Fatalf("provider transfer summary changed accepted results: %+v", snap)
	}
	if snap.ProviderSummaryRestoredCount != 1 || snap.BytesFetched != 42 {
		t.Fatalf("provider transfer accounting lost: %+v", snap)
	}
}

func TestCacheStats_RestoreWallUnionsOverlap(t *testing.T) {
	t.Parallel()
	base := time.Now()
	ms := func(n int64) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }

	s := &CacheStats{}
	// Two heavily overlapping windows [0,100] and [50,150] => union 150ms, plus a
	// disjoint window [200,260] => +60ms. Summing would wrongly report 260ms.
	s.recordMaterialize(ms(50), ms(150))
	s.recordMaterialize(ms(0), ms(100))
	s.recordMaterialize(ms(200), ms(260))
	s.recordMaterialize(ms(80), ms(80)) // zero-width window is ignored

	if got := s.Snapshot().RestoreMs; got != 210 {
		t.Errorf("RestoreMs (union) = %d, want 210 (150 overlap + 60 disjoint)", got)
	}
}

func TestCacheStats_LocalServingUnionsParallelPhases(t *testing.T) {
	t.Parallel()
	base := time.Now()
	ms := func(n int64) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }

	s := &CacheStats{}
	s.recordLocalBindings(ms(0), ms(100), 4)
	s.recordLocalKeys(ms(50), ms(150))
	s.recordLocalRestoreVerify(ms(140), ms(200))
	s.recordLocalHit()
	s.recordLocalHit()
	s.recordLocalMiss()

	snap := s.Snapshot()
	if snap.LocalHits != 2 || snap.LocalMisses != 1 {
		t.Fatalf("local lookup counts = %d hit / %d miss", snap.LocalHits, snap.LocalMisses)
	}
	if snap.LocalServedMs != 200 {
		t.Fatalf("local served wall = %dms, want the 200ms union", snap.LocalServedMs)
	}
	// Bindings have priority over keys, which have priority over restore-verify:
	// [0,100] is bindings, [100,150] keys, and [150,200] restore-verify.
	if snap.LocalKeysMs != 50 || snap.LocalBindingsMs != 100 || snap.LocalRestoreVerifyMs != 50 {
		t.Fatalf("local phases = keys %d / bindings %d / restore %d",
			snap.LocalKeysMs, snap.LocalBindingsMs, snap.LocalRestoreVerifyMs)
	}
	if got := snap.LocalKeysMs + snap.LocalBindingsMs + snap.LocalRestoreVerifyMs; got != snap.LocalServedMs {
		t.Fatalf("local phase sum = %dms, want served wall %dms", got, snap.LocalServedMs)
	}
	if snap.LocalSpawnedProcesses != 4 {
		t.Fatalf("local spawned processes = %d, want 4", snap.LocalSpawnedProcesses)
	}
	if !snap.HasActivity() {
		t.Fatal("local-only work must publish a cache summary")
	}
}

func TestCacheStats_LocalServingPartitionsNestedAndParallelOverlap(t *testing.T) {
	t.Parallel()
	base := time.Now()
	ms := func(n int64) time.Time { return base.Add(time.Duration(n) * time.Millisecond) }

	s := &CacheStats{}
	// Restore is the broad outer operation. A nested binding takes [20,80], and
	// parallel key workers cover [40,90] and [50,70]. Stable priority assigns
	// [20,80] to bindings, [80,90] to keys, and the residual [0,20]+[90,100]
	// to restore-verify. The duplicate key interval does not multiply wall time.
	s.recordLocalRestoreVerify(ms(0), ms(100))
	s.recordLocalBindings(ms(20), ms(80), 2)
	s.recordLocalKeys(ms(40), ms(90))
	s.recordLocalKeys(ms(50), ms(70))

	snap := s.Snapshot()
	if snap.LocalServedMs != 100 || snap.LocalBindingsMs != 60 ||
		snap.LocalKeysMs != 10 || snap.LocalRestoreVerifyMs != 30 {
		t.Fatalf("partition = served %d / keys %d / bindings %d / restore %d",
			snap.LocalServedMs, snap.LocalKeysMs, snap.LocalBindingsMs, snap.LocalRestoreVerifyMs)
	}
}

func TestCacheStats_LocalServingMillisecondRoundingNeverExceedsServed(t *testing.T) {
	t.Parallel()
	base := time.Now()
	s := &CacheStats{}
	s.recordLocalKeys(base, base.Add(1400*time.Microsecond))
	s.recordLocalBindings(base.Add(1400*time.Microsecond), base.Add(2800*time.Microsecond), 0)
	s.recordLocalRestoreVerify(base.Add(2800*time.Microsecond), base.Add(4200*time.Microsecond))

	snap := s.Snapshot()
	phaseSum := snap.LocalKeysMs + snap.LocalBindingsMs + snap.LocalRestoreVerifyMs
	if snap.LocalServedMs != 4 || phaseSum != 3 {
		t.Fatalf("rounded attribution = served %d / phase sum %d, want 4 / 3", snap.LocalServedMs, phaseSum)
	}
	if phaseSum > snap.LocalServedMs || snap.LocalServedMs-phaseSum > 2 {
		t.Fatalf("rounded phase sum %d is not a valid decomposition of served %d", phaseSum, snap.LocalServedMs)
	}
}

func TestCacheStats_ConcurrentRecording(t *testing.T) {
	t.Parallel()
	s := &CacheStats{}
	const workers = 50
	var wg sync.WaitGroup
	base := time.Now()
	for i := range workers {
		wg.Go(func() {
			s.recordRestore(10)
			s.recordFetch(100)
			start := base.Add(time.Duration(i) * time.Microsecond)
			s.recordMaterialize(start, start.Add(500*time.Nanosecond)) // disjoint 500ns windows
			s.recordProviderHit()
		})
	}
	wg.Wait()

	snap := s.Snapshot()
	if snap.Restored != workers || snap.TimeSavedMs != workers*10 {
		t.Errorf("restore under concurrency = %d / %d ms", snap.Restored, snap.TimeSavedMs)
	}
	// recordProviderHit promotes a miss to a hit; with no negotiated misses seeded
	// here it just counts hits, exercising its atomic CAS loop under concurrency.
	if snap.BytesFetched != workers*100 || snap.Hits != workers {
		t.Errorf("fetch/hit under concurrency = %d fetched / %d hits", snap.BytesFetched, snap.Hits)
	}
}
