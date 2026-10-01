package store

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

// isolateStoreBudget points the store, the GC root set and the usage record at
// one temp directory and clears the ambient GC environment, so a budget reads
// only what the test wrote.
func isolateStoreBudget(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	t.Setenv(storeDirEnv, root)
	t.Setenv(storeMaxBytesEnv, "")
	t.Setenv(gcGraceEnv, "")
	t.Setenv(maxIdleBuildsEnv, "")
	return root
}

// putFilled stores an entry whose single output file is size bytes of fill, so
// entries with different fills hold different CAS blobs (putSized's zero-filled
// files would all share one blob per size).
func putFilled(t *testing.T, s *LocalStore, hash string, size int, fill byte) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.bin"), bytes.Repeat([]byte{fill}, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(hash, entryWith(src)); err != nil {
		t.Fatalf("Put %s: %v", hash, err)
	}
}

func writeUsageRecord(t *testing.T, storeRoot string, usage int64) {
	t.Helper()
	if err := os.MkdirAll(storeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeRoot, gcUsageFile), []byte(strconv.FormatInt(usage, 10)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readUsageRecord(t *testing.T, storeRoot string) (int64, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(storeRoot, gcUsageFile))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("usage record %q is not a byte count: %v", data, err)
	}
	return n, true
}

func budgetConfig(maxBytes int64) Config {
	grace := time.Duration(0) // only ProtectSince protects, so the test sees exactly that rule
	return Config{MaxBytes: &maxBytes, GCGrace: &grace}
}

// TestRunBudget_ProjectsFromRecordedUsageAndSparesWhatTheRunTouched pins the
// store's half of the acceptance criteria: growth that pushes the PROJECTED
// usage over the budget triggers a pass at the next batch boundary, and that
// pass evicts only an entry the run never touched.
func TestRunBudget_ProjectsFromRecordedUsageAndSparesWhatTheRunTouched(t *testing.T) {
	root := isolateStoreBudget(t)
	ws := t.TempDir()

	// Two entries from earlier runs. "held" is one this run will look up.
	seed := NewLocalStore(root)
	stale, held, grown, grown2, dup := hashN(1), hashN(2), hashN(3), hashN(4), hashN(5)
	putFilled(t, seed, stale, 4000, 's')
	putFilled(t, seed, held, 4000, 'h')
	before := time.Now().Add(-time.Minute)
	writeLastUsed(seed.blobDir(stale), before, 1)
	writeLastUsed(seed.blobDir(held), before, 1)
	// The last pass measured far less than is on disk now, so only the
	// projection (not a walk) can explain what happens next.
	writeUsageRecord(t, root, 1000)

	run := NewLocalStore(root)
	b := NewRunBudget(ws, budgetConfig(10000), run)
	run.markUsed(held) // the lookup an in-flight job makes

	putFilled(t, run, grown, 4000, 'g')
	b.AfterBatch()
	b.wg.Wait()
	// Projected 1000+4000 is within budget: no pass, so nothing was measured
	// or evicted even though the disk holds more than the budget.
	if e, _ := run.Get(stale); e == nil {
		t.Fatal("a pass ran while the projection was within budget")
	}
	if got, _ := readUsageRecord(t, root); got != 1000 {
		t.Fatalf("usage record = %d, want the untouched 1000", got)
	}

	// Identical content is deduplicated: it adds no CAS bytes, so it must not
	// move the projection.
	putFilled(t, run, dup, 4000, 'g')
	if got := run.added.Load(); got != 4000 {
		t.Fatalf("added = %d after a deduplicated write, want 4000", got)
	}

	putFilled(t, run, grown2, 6000, 'x')
	if got := run.added.Load(); got != 10000 {
		t.Fatalf("added = %d, want the 10000 CAS bytes this handle wrote", got)
	}
	b.AfterBatch() // projected 1000+10000 > 10000: a pass is due
	b.wg.Wait()

	if e, _ := run.Get(stale); e != nil {
		t.Error("the entry the run never touched survived an over-budget pass")
	}
	for _, h := range []string{held, grown, grown2, dup} {
		if e, _ := run.Get(h); e == nil {
			t.Errorf("entry %s the run touched was evicted", h)
		}
	}
	recorded, ok := readUsageRecord(t, root)
	if !ok || recorded < 14000 {
		t.Fatalf("usage record = %d (present=%v), want the measured usage after the pass (>= 14000)", recorded, ok)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.known || b.usage != recorded || b.since != 10000 {
		t.Errorf("budget state known=%v usage=%d since=%d, want the pass's measurement %d from 10000",
			b.known, b.usage, b.since, recorded)
	}
}

// TestRunBudget_FirstWriteMeasuresWhenNoPassHasRecordedUsage pins the one walk
// the budget takes without a projection: with no recorded measurement, the
// first boundary after the run's first write measures the store, and records
// it for every later run. A boundary before any write walks nothing.
func TestRunBudget_FirstWriteMeasuresWhenNoPassHasRecordedUsage(t *testing.T) {
	root := isolateStoreBudget(t)
	run := NewLocalStore(root)
	b := NewRunBudget(t.TempDir(), budgetConfig(1<<30), run)

	b.AfterBatch()
	b.wg.Wait()
	if _, ok := readUsageRecord(t, root); ok {
		t.Fatal("a boundary before any write measured the store")
	}

	putFilled(t, run, hashN(1), 3000, 'a')
	b.AfterBatch()
	b.wg.Wait()
	recorded, ok := readUsageRecord(t, root)
	if !ok || recorded < 3000 {
		t.Fatalf("usage record = %d (present=%v), want the measured usage (>= 3000)", recorded, ok)
	}
	if e, _ := run.Get(hashN(1)); e == nil {
		t.Error("a within-budget measuring pass evicted an entry")
	}

	// Caching off: no budget, and a nil budget is a no-op at both call sites.
	var off *RunBudget
	off.AfterBatch()
	if report := off.Close(); report != (RunBudgetReport{}) {
		t.Errorf("a nil budget reported %+v", report)
	}
	if NewRunBudget(t.TempDir(), Config{}, nil) != nil {
		t.Error("NewRunBudget without a store handle returned a budget")
	}
}

// TestRunBudget_OverBudgetMeasurementWaitsForGrowth keeps a store the run
// cannot shrink from being walked at every boundary: after a measurement that
// was already over budget, the next pass waits for another tenth of the budget
// of growth. That pass then corrects a stale record.
func TestRunBudget_OverBudgetMeasurementWaitsForGrowth(t *testing.T) {
	root := isolateStoreBudget(t)
	seed := NewLocalStore(root)
	putFilled(t, seed, hashN(1), 3000, 's')
	writeUsageRecord(t, root, 20000) // stale: far more than is on disk

	run := NewLocalStore(root)
	b := NewRunBudget(t.TempDir(), budgetConfig(10000), run)

	putFilled(t, run, hashN(2), 500, 'a') // grown 500, not over a tenth (1000)
	b.AfterBatch()
	b.wg.Wait()
	if got, _ := readUsageRecord(t, root); got != 20000 {
		t.Fatalf("usage record = %d, want 20000: a pass ran before the run grew a tenth of the budget", got)
	}

	putFilled(t, run, hashN(3), 1000, 'b') // grown 1500
	b.AfterBatch()
	b.wg.Wait()
	recorded, _ := readUsageRecord(t, root)
	if recorded >= 10000 || recorded < 4500 {
		t.Fatalf("usage record = %d, want the corrected measurement of about 4.5 KB", recorded)
	}
	for _, h := range []string{hashN(1), hashN(2), hashN(3)} {
		if e, _ := run.Get(h); e == nil {
			t.Errorf("entry %s was evicted by a pass that measured the store within budget", h)
		}
	}
}

// TestRunBudget_ConsecutiveRunsAccumulateTheirGrowth: the post-build pass is
// throttled to once an hour, so a run that ends without a measuring pass adds
// its growth to the record, and the next run starts from that sum instead of
// the same stale measurement.
func TestRunBudget_ConsecutiveRunsAccumulateTheirGrowth(t *testing.T) {
	root := isolateStoreBudget(t)
	writeUsageRecord(t, root, 8000)

	first := NewLocalStore(root)
	b1 := NewRunBudget(t.TempDir(), budgetConfig(10000), first)
	putFilled(t, first, hashN(1), 1500, 'a')
	b1.AfterBatch() // 8000+1500 is within budget: no pass
	if report := b1.Close(); report.Passes != 0 {
		t.Fatalf("first run started %d passes within budget", report.Passes)
	}
	if got, _ := readUsageRecord(t, root); got != 9500 {
		t.Fatalf("usage record = %d after the first run, want 8000+1500", got)
	}
	b1.Close()
	if got, _ := readUsageRecord(t, root); got != 9500 {
		t.Fatalf("usage record = %d after a second Close, want the growth added once", got)
	}

	second := NewLocalStore(root)
	b2 := NewRunBudget(t.TempDir(), budgetConfig(10000), second)
	putFilled(t, second, hashN(2), 1500, 'b')
	b2.AfterBatch() // 9500+1500 crosses the budget only because the first run's growth was kept
	report := b2.Close()
	if report.Passes != 1 {
		t.Fatalf("second run started %d passes, want 1: it did not see the first run's growth", report.Passes)
	}
	if got, _ := readUsageRecord(t, root); got >= 9500 || got < 3000 {
		t.Errorf("usage record = %d, want the pass's measurement (about 3 KB) replacing the sum", got)
	}
}

// TestRunBudget_CloseAddsNothingWithoutARecord: a run that knows nothing about
// the rest of the store must not turn its own bytes into the machine's usage.
func TestRunBudget_CloseAddsNothingWithoutARecord(t *testing.T) {
	root := isolateStoreBudget(t)
	run := NewLocalStore(root)
	b := NewRunBudget(t.TempDir(), budgetConfig(1<<30), run)
	putFilled(t, run, hashN(1), 1500, 'a')
	b.Close()
	if got, ok := readUsageRecord(t, root); ok {
		t.Fatalf("Close created a usage record of %d from this run's bytes alone", got)
	}
}

// TestUpdateGCUsage_ConcurrentWritersLoseNoUpdate: runs sharing the record
// read, add and write it under its lock, so no addition is lost however they
// interleave — two writers in a loop, and two runs closing at once.
func TestUpdateGCUsage_ConcurrentWritersLoseNoUpdate(t *testing.T) {
	root := isolateStoreBudget(t)
	writeUsageRecord(t, root, 1000)
	stamp := gcStampPath()

	a, b := NewLocalStore(root), NewLocalStore(root)
	budgetA := NewRunBudget(t.TempDir(), budgetConfig(1<<30), a)
	budgetB := NewRunBudget(t.TempDir(), budgetConfig(1<<30), b)
	putFilled(t, a, hashN(1), 700, 'a')
	putFilled(t, b, hashN(2), 300, 'b')

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 50 {
				updateGCUsage(stamp, func(prev int64, ok bool) (int64, bool) { return prev + 1, ok })
			}
		})
	}
	wg.Go(func() { budgetA.Close() })
	wg.Go(func() { budgetB.Close() })
	wg.Wait()

	if got, _ := readUsageRecord(t, root); got != 1000+100+700+300 {
		t.Fatalf("usage record = %d, want %d: a concurrent update was lost", got, 1000+100+700+300)
	}
}

// TestRunBudget_BusyScanIsNotAMeasurementAndIsRetriedBoundedly: a pass that
// could not read the store (another holder has it exclusively) measured
// nothing. It keeps the prior state, so the next boundary retries at once —
// up to maxBusyRetries times; after that only growth lets another try
// through, so a store that stays busy is not walked at every boundary.
func TestRunBudget_BusyScanIsNotAMeasurementAndIsRetriedBoundedly(t *testing.T) {
	root := isolateStoreBudget(t)
	seed := NewLocalStore(root)
	putFilled(t, seed, hashN(1), 6000, 's')
	writeLastUsed(seed.blobDir(hashN(1)), time.Now().Add(-time.Minute), 1)
	writeUsageRecord(t, root, 9000)

	run := NewLocalStore(root)
	b := NewRunBudget(t.TempDir(), budgetConfig(10000), run)
	putFilled(t, run, hashN(2), 4000, 'g') // projected 13000; on disk over 10 KB with the 6 KB seed

	release := NewLocalStore(root).lockExclusive()
	for want := 1; want <= 1+maxBusyRetries; want++ {
		b.AfterBatch()
		b.wg.Wait()
		if b.report.Passes != want {
			t.Fatalf("passes = %d, want %d: a busy pass was not retried at the next boundary", b.report.Passes, want)
		}
	}
	b.AfterBatch()
	b.wg.Wait()
	if b.report.Passes != 1+maxBusyRetries {
		t.Fatalf("passes = %d: a store that stays busy was walked again without growth", b.report.Passes)
	}
	if b.report.StoresSkippedBusy != 1+maxBusyRetries {
		t.Errorf("StoresSkippedBusy = %d, want %d", b.report.StoresSkippedBusy, 1+maxBusyRetries)
	}
	if got, _ := readUsageRecord(t, root); got != 9000 {
		t.Fatalf("usage record = %d, want the prior 9000: a busy pass was recorded as a measurement", got)
	}
	if !b.known || b.usage != 9000 || b.since != 0 {
		t.Fatalf("budget state known=%v usage=%d since=%d, want the prior measurement kept", b.known, b.usage, b.since)
	}
	release()

	putFilled(t, run, hashN(3), 1100, 'h') // another tenth of the budget since the last attempt
	b.AfterBatch()
	b.wg.Wait()
	if e, _ := run.Get(hashN(1)); e != nil {
		t.Error("the retried pass, with the store free, did not evict the untouched entry")
	}
	if b.busyRetries != 0 || !b.known || b.usage == 9000 {
		t.Errorf("after a complete pass busyRetries=%d known=%v usage=%d, want a fresh measurement", b.busyRetries, b.known, b.usage)
	}
}

// TestRunBudget_EvictBusyPassIsRetriedOnceTheStoreIsFree: the scan read the
// store, but its exclusive lock stayed busy (a publisher holds it shared), so
// the budget was not enforced. That is not a measurement either: the next
// boundary retries without waiting for growth.
func TestRunBudget_EvictBusyPassIsRetriedOnceTheStoreIsFree(t *testing.T) {
	root := isolateStoreBudget(t)
	seed := NewLocalStore(root)
	putFilled(t, seed, hashN(1), 6000, 's')
	writeLastUsed(seed.blobDir(hashN(1)), time.Now().Add(-time.Minute), 1)
	writeUsageRecord(t, root, 9000)

	run := NewLocalStore(root)
	b := NewRunBudget(t.TempDir(), budgetConfig(10000), run)
	b.opts.LockWait = 0 // one try, so the test does not wait out LockWait
	// On disk: 11 KB and more, over the 10 KB budget.
	putFilled(t, run, hashN(2), 5000, 'g')

	release := NewLocalStore(root).lockShared()
	b.AfterBatch()
	b.wg.Wait()
	if e, _ := run.Get(hashN(1)); e == nil {
		t.Fatal("evicted while another holder had the store")
	}
	if b.report.StoresSkippedBusy != 1 || b.busyRetries != 1 {
		t.Fatalf("StoresSkippedBusy=%d busyRetries=%d, want the skipped store counted", b.report.StoresSkippedBusy, b.busyRetries)
	}
	if got, _ := readUsageRecord(t, root); got != 9000 {
		t.Fatalf("usage record = %d, want 9000: an unfinished pass was recorded", got)
	}
	release()

	retry := time.Now()
	b.AfterBatch() // no growth: retried because the last pass did not finish
	b.wg.Wait()
	retried := time.Since(retry)
	if b.report.Passes != 2 {
		t.Fatalf("passes = %d, want the retry", b.report.Passes)
	}
	if e, _ := run.Get(hashN(1)); e != nil {
		t.Error("the retry did not evict the untouched entry")
	}
	if b.report.EvictedEntries != 1 || b.report.FreedBytes < 4000 {
		t.Errorf("report %+v, want the eviction and its bytes", b.report)
	}
	// The busy pass never got the lock, so the whole hold is the retry's: no
	// longer than the retry took, and no shorter than its longest section.
	if b.report.MaxLockHeld > b.report.LockHeld || b.report.LockHeld > retried {
		t.Errorf("report %+v, want the retry's lock hold, at most the %v the retry took", b.report, retried)
	}
	// The retry evicted under the lock, so it held it. Windows advances the
	// monotonic clock every 0.5 to 15.6 ms, and a shorter hold reads as 0s
	// there; elsewhere the clock resolves it.
	if b.report.LockHeld <= 0 && runtime.GOOS != "windows" {
		t.Errorf("report %+v, want the eviction's lock hold", b.report)
	}
}

// TestRecordGCUsage_BusyScanRecordsNothing: a store skipped as busy during the
// scan is missing from ScannedBytes, so the pass is not a measurement of the
// machine's usage, for the post-build pass as for the in-run one.
func TestRecordGCUsage_BusyScanRecordsNothing(t *testing.T) {
	root := isolateStoreBudget(t)
	other := filepath.Join(t.TempDir(), "other")
	putFilled(t, NewLocalStore(root), hashN(1), 1000, 'a')
	putFilled(t, NewLocalStore(other), hashN(2), 1000, 'b')

	release := NewLocalStore(other).lockExclusive()
	res, err := RunGC([]string{root, other}, GCOptions{MaxBytes: 1 << 30, NonBlocking: true})
	release()
	if err != nil || res.StoresScanned != 1 || res.ScanSkippedBusy != 1 {
		t.Fatalf("RunGC = %+v, %v; want one store scanned and one skipped busy", res, err)
	}
	if _, ok := recordGCUsage(gcStampPath(), res, err); ok {
		t.Error("a pass that skipped a busy store reported a measurement")
	}
	if got, ok := readUsageRecord(t, root); ok {
		t.Errorf("usage record = %d, written from a scan that missed a store", got)
	}

	res, err = RunGC([]string{root, other}, GCOptions{MaxBytes: 1 << 30, NonBlocking: true})
	if usage, ok := recordGCUsage(gcStampPath(), res, err); !ok || usage < 2000 {
		t.Errorf("a complete pass recorded %d (ok=%v), want both stores", usage, ok)
	}
}

// TestNewRunBudget_RelocatedStoreArmsNoBudget: a cache verification writes to
// a temporary store of its own. Its bytes are not the machine's, so they never
// reach the machine-wide record, and no pass collects it mid-verification.
func TestNewRunBudget_RelocatedStoreArmsNoBudget(t *testing.T) {
	root := isolateStoreBudget(t)
	writeUsageRecord(t, root, 1000)
	relocated := NewLocalStore(filepath.Join(t.TempDir(), "verify-store"))
	b := NewRunBudget(t.TempDir(), budgetConfig(1), relocated)
	if b != nil {
		t.Fatal("a budget was armed on a relocated store")
	}
	putFilled(t, relocated, hashN(1), 5000, 'v')
	b.AfterBatch()
	b.Close()
	if got, _ := readUsageRecord(t, root); got != 1000 {
		t.Fatalf("usage record = %d, want the machine's 1000 untouched", got)
	}
	if e, _ := relocated.Get(hashN(1)); e == nil {
		t.Error("the relocated store was collected")
	}
}

// TestStoreBudget_LeavesExtensionMachineCachesAlone pins one property of the
// store's side of the budget: its root set holds no language cache, so a
// pass neither counts nor evicts the Go cache (build, prog, mod) or an
// extension's machine root, even when it evicts everything it does manage.
// The Go extension resolves its own root; with no override it is
// ~/.putnami/cache/go, and go/extension's
// TestGoCacheRootLivesOutsideTheBuildStore pins that side.
func TestStoreBudget_LeavesExtensionMachineCachesAlone(t *testing.T) {
	home := hometest.Temp(t)
	t.Setenv(storeDirEnv, "")
	t.Setenv(extensionproto.MachineCacheDirEnv, "")
	ws := t.TempDir()

	storeRoot := ResolveStoreRoot(ws)
	roots := StoreRootsForGCIncluding(ws)
	if !slices.Contains(roots, storeRoot) {
		t.Fatalf("GC roots %v do not include the workspace store %s", roots, storeRoot)
	}
	goRoot := filepath.Join(home, ".putnami", "cache", "go")
	machineRoot := extensionproto.MachineCacheRoot("@putnami/go", ws)
	cacheFiles := []string{
		filepath.Join(goRoot, "build", "ab", strings.Repeat("ab", 32)+"-a"),
		filepath.Join(goRoot, "prog", "cd", strings.Repeat("cd", 32)+"-d"),
		filepath.Join(goRoot, "mod", "example.com", "m@v1.0.0", "m.txt"),
		filepath.Join(machineRoot, "blob"),
	}
	for _, path := range cacheFiles {
		for _, root := range roots {
			if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("%s lies inside store root %s: the store budget would own it", path, root)
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, 64<<10), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := NewLocalStore(storeRoot)
	putFilled(t, s, hashN(1), 1000, 'e')
	res, err := RunGC(roots, GCOptions{MaxBytes: 1, Grace: 0})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.ScannedBytes >= 64<<10 {
		t.Errorf("ScannedBytes = %d: the store budget counted a language cache", res.ScannedBytes)
	}
	if e, _ := s.Get(hashN(1)); e != nil {
		t.Error("the over-budget pass evicted nothing it manages, so it proved nothing")
	}
	for _, path := range cacheFiles {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("store GC removed %s: %v", path, err)
		}
	}
}
