package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// putSized stores an entry whose single output file has the given byte size,
// producing a distinct CAS blob of that size, and returns the hash.
func putSized(t *testing.T, s *LocalStore, hash string, size int) {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.bin"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(hash, entryWith(src)); err != nil {
		t.Fatalf("Put %s: %v", hash, err)
	}
}

func TestRunGC_WithinBudget_NoOp(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	putSized(t, s, hashN(1), 1000)

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1 << 30, Grace: 0})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.EvictedEntries != 0 || res.SweptBlobs != 0 {
		t.Errorf("within budget should be a no-op, got %+v", res)
	}
	if e, _ := s.Get(hashN(1)); e == nil {
		t.Error("entry evicted while within budget")
	}
}

func TestRunGC_EvictsOldestAndSweepsOrphans(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()

	putSized(t, s, hashN(1), 2000) // old → should be evicted
	putSized(t, s, hashN(2), 1000) // recent → should survive
	writeLastUsed(s.blobDir(hashN(1)), now.Add(-2*time.Hour), 1)
	writeLastUsed(s.blobDir(hashN(2)), now, 1)

	// Budget forces eviction of the single oldest entry.
	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 2500, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.EvictedEntries != 1 {
		t.Errorf("EvictedEntries = %d, want 1", res.EvictedEntries)
	}
	if res.SweptBlobs != 1 {
		t.Errorf("SweptBlobs = %d, want 1 (the orphaned 2000-byte blob)", res.SweptBlobs)
	}
	if res.FreedBytes < 2000 {
		t.Errorf("FreedBytes = %d, want >= 2000", res.FreedBytes)
	}
	if e, _ := s.Get(hashN(1)); e != nil {
		t.Error("oldest entry should have been evicted")
	}
	if e, _ := s.Get(hashN(2)); e == nil {
		t.Error("recent entry should have survived")
	}
}

func TestRunGC_GraceProtectsRecent(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()

	putSized(t, s, hashN(1), 5000)
	putSized(t, s, hashN(2), 5000)
	// Both used just now — within any reasonable grace window.
	writeLastUsed(s.blobDir(hashN(1)), now, 1)
	writeLastUsed(s.blobDir(hashN(2)), now, 1)

	// Far over budget, but grace must protect both live entries.
	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.EvictedEntries != 0 {
		t.Errorf("grace should protect recent entries, evicted %d", res.EvictedEntries)
	}
	if e, _ := s.Get(hashN(1)); e == nil {
		t.Error("entry 1 evicted despite grace")
	}
	if e, _ := s.Get(hashN(2)); e == nil {
		t.Error("entry 2 evicted despite grace")
	}
}

func TestRunGC_SharedBlobSurvivesWhileReferenced(t *testing.T) {
	// Two entries share identical output bytes → one CAS blob, nlink==2. Evicting
	// one entry must NOT sweep the blob (the surviving entry still references it).
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()

	src1 := t.TempDir()
	src2 := t.TempDir()
	for _, src := range []string{src1, src2} {
		if err := os.WriteFile(filepath.Join(src, "f.bin"), make([]byte, 4000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(hashN(1), entryWith(src1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(hashN(2), entryWith(src2)); err != nil {
		t.Fatal(err)
	}
	writeLastUsed(s.blobDir(hashN(1)), now.Add(-2*time.Hour), 1) // evict this one
	writeLastUsed(s.blobDir(hashN(2)), now, 1)                   // keep this one

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 4500, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.SweptBlobs != 0 {
		t.Errorf("shared blob must not be swept while referenced, swept %d", res.SweptBlobs)
	}
	// Surviving entry's bytes must still be readable through its files/ hardlink.
	if e, _ := s.Get(hashN(2)); e == nil {
		t.Error("referenced entry evicted")
	} else if _, err := os.ReadFile(filepath.Join(e.FilesDir, "f.bin")); err != nil {
		t.Errorf("surviving entry's blob unreadable: %v", err)
	}
}

func TestRunGC_SharedBlobFreedOnlyWhenAllReferrersEvicted(t *testing.T) {
	// E1 and E2 have identical output bytes → ONE shared 6000-byte CAS blob;
	// E3 has a distinct 1000-byte blob. Real disk = 7000. Freeing the shared
	// blob requires evicting BOTH E1 and E2. This guards against the logical-size
	// projection bug that would stop after evicting only E1 (thinking 6000 freed)
	// while the shared blob — and thus the disk — stayed put.
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()

	putSized(t, s, hashN(1), 6000) // zeros → identical content/digest as E2
	putSized(t, s, hashN(2), 6000)
	putSized(t, s, hashN(3), 1000)
	writeLastUsed(s.blobDir(hashN(1)), now.Add(-3*time.Hour), 1)
	writeLastUsed(s.blobDir(hashN(2)), now.Add(-2*time.Hour), 1)
	writeLastUsed(s.blobDir(hashN(3)), now, 1)

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 5000, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.EvictedEntries != 2 {
		t.Errorf("EvictedEntries = %d, want 2 (both sharers of the blob)", res.EvictedEntries)
	}
	if res.SweptBlobs != 1 {
		t.Errorf("SweptBlobs = %d, want 1 (the shared blob, freed only after both referrers gone)", res.SweptBlobs)
	}
	if res.FreedBytes < 6000 {
		t.Errorf("FreedBytes = %d, want >= 6000", res.FreedBytes)
	}
	if e, _ := s.Get(hashN(3)); e == nil {
		t.Error("recent entry E3 should survive")
	}
}

func TestRunGC_NonBlockingSkipsBusyStore(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	putSized(t, s, hashN(1), 5000)
	writeLastUsed(s.blobDir(hashN(1)), now.Add(-2*time.Hour), 1)

	// Hold the store's exclusive lock from a separate handle (models a sibling
	// build/GC). A non-blocking GC must skip rather than wait.
	hold := NewLocalStore(root)
	release := hold.lockExclusive()
	defer release()

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: 0, Now: now, NonBlocking: true})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.StoresScanned != 0 || res.EvictedEntries != 0 {
		t.Errorf("non-blocking GC should skip a locked store, got %+v", res)
	}
	if e, _ := s.Get(hashN(1)); e == nil {
		t.Error("entry should be untouched when GC skips a busy store")
	}
}

func TestRunGC_IdleReclaim(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	putSized(t, s, hashN(1), 1000)
	putSized(t, s, hashN(2), 1000)
	writeGenerationFile(root, 200)
	writeLastUsed(s.blobDir(hashN(1)), now.Add(-2*time.Hour), 50) // idle 150 > 100 → evict
	writeLastUsed(s.blobDir(hashN(2)), now, 150)                  // idle 50, recent → keep

	// Far UNDER budget, but idle reclaim should still drop the stale entry.
	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, Now: now, MaxIdleGenerations: 100})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.IdleEvicted != 1 || res.EvictedEntries != 1 {
		t.Errorf("want 1 idle eviction, got EvictedEntries=%d IdleEvicted=%d", res.EvictedEntries, res.IdleEvicted)
	}
	if e, _ := s.Get(hashN(1)); e != nil {
		t.Error("stale entry should be idle-evicted")
	}
	if e, _ := s.Get(hashN(2)); e == nil {
		t.Error("recently-hit entry should survive idle reclaim")
	}
}

func TestRunGC_IdleReclaim_GraceOverrides(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	putSized(t, s, hashN(1), 1000)
	writeGenerationFile(root, 200)
	// Idle by generation (150 > 100) BUT hit within the grace window: grace wins,
	// because a live reader may still hold it.
	writeLastUsed(s.blobDir(hashN(1)), now, 50)

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, Now: now, MaxIdleGenerations: 100})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.IdleEvicted != 0 {
		t.Errorf("grace must protect even idle entries, idle-evicted %d", res.IdleEvicted)
	}
	if e, _ := s.Get(hashN(1)); e == nil {
		t.Error("grace-protected entry evicted")
	}
}

func TestRunGC_IdleReclaim_DisabledByDefaultOption(t *testing.T) {
	// MaxIdleGenerations==0 (zero value) disables idle reclaim entirely.
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	putSized(t, s, hashN(1), 1000)
	writeGenerationFile(root, 9999)
	writeLastUsed(s.blobDir(hashN(1)), now.Add(-72*time.Hour), 1) // ancient

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1 << 30, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.EvictedEntries != 0 {
		t.Errorf("idle reclaim off → nothing evicted under budget, got %d", res.EvictedEntries)
	}
}

func TestRunGC_SweepsRootFetchTempsWhenOverBudget(t *testing.T) {
	root := t.TempDir()
	casRoot := filepath.Join(root, "cas")
	if err := os.MkdirAll(casRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fetchTemp := filepath.Join(casRoot, "fetch-orphan")
	if err := os.WriteFile(fetchTemp, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	nonTemp := filepath.Join(casRoot, "manual-root-file")
	if err := os.WriteFile(nonTemp, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: 0})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.SweptBlobs != 1 {
		t.Errorf("SweptBlobs = %d, want 1 root fetch temp", res.SweptBlobs)
	}
	if res.FreedBytes < 1024 {
		t.Errorf("FreedBytes = %d, want at least 1024", res.FreedBytes)
	}
	if _, err := os.Stat(fetchTemp); !os.IsNotExist(err) {
		t.Errorf("root fetch temp should be swept, stat err=%v", err)
	}
	if _, err := os.Stat(nonTemp); err != nil {
		t.Errorf("non-fetch root file should be left alone: %v", err)
	}
}

func TestRunGC_OCILayersShareStoreBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	oldEntry := filepath.Join(root, OCILayerCacheDirName, "v1", "aa", hashN(1))
	recentEntry := filepath.Join(root, OCILayerCacheDirName, "v1", "bb", hashN(2))
	for path, size := range map[string]int{oldEntry: 2000, recentEntry: 1000} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "layer.tar.gz"), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(oldEntry, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recentEntry, now, now); err != nil {
		t.Fatal(err)
	}

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 2500, Grace: time.Hour, Now: now})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.ScannedBytes != 3000 {
		t.Errorf("ScannedBytes = %d, want 3000", res.ScannedBytes)
	}
	if res.EvictedEntries != 1 || res.FreedBytes != 2000 {
		t.Errorf("OCI eviction = %+v, want one entry and 2000 bytes", res)
	}
	if _, err := os.Stat(oldEntry); !os.IsNotExist(err) {
		t.Errorf("old OCI entry should be evicted, stat err=%v", err)
	}
	if _, err := os.Stat(recentEntry); err != nil {
		t.Errorf("recent OCI entry should survive: %v", err)
	}
}

func TestEvictStore_DoesNotCountGraceSkippedIdleVictim(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	now := time.Now()
	putSized(t, s, hashN(1), 1000)
	blobDir := s.blobDir(hashN(1))
	writeLastUsed(blobDir, now, 1)

	scan, _ := scanStore(root, false)
	out := evictStore(root, []gcEntry{{
		storeRoot: root,
		blobDir:   blobDir,
		blobs:     readManifestBlobs(blobDir),
		idle:      true,
	}}, scan, now, func(last time.Time) bool { return now.Sub(last) < time.Hour }, false, time.Time{})
	if out.evicted != 0 || out.idleEvicted != 0 {
		t.Errorf("grace-skipped idle victim counted as evicted: evicted=%d idle=%d", out.evicted, out.idleEvicted)
	}
	if e, _ := s.Get(hashN(1)); e == nil {
		t.Error("grace-skipped entry should survive")
	}
}

// TestRunGC_ProtectSinceSparesEntriesUsedDuringTheRun pins the in-run pass's
// rule with no grace at all: an entry used at or after ProtectSince survives an
// over-budget pass that evicts an older one. The second half is the race the
// rule exists for: a job's lookup stamp landing between enumeration and
// eviction. evictStore re-reads the stamp under the exclusive lock whatever the
// grace (it used to skip that re-check when grace was 0).
func TestRunGC_ProtectSinceSparesEntriesUsedDuringTheRun(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	since := time.Now()
	putFilled(t, s, hashN(1), 2000, 'a')
	putFilled(t, s, hashN(2), 2000, 'b')
	writeLastUsed(s.blobDir(hashN(1)), since.Add(-time.Second), 1)
	writeLastUsed(s.blobDir(hashN(2)), since, 1)

	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: 0, ProtectSince: since})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.EvictedEntries != 1 {
		t.Errorf("EvictedEntries = %d, want 1 (only the entry used before ProtectSince)", res.EvictedEntries)
	}
	if e, _ := s.Get(hashN(1)); e != nil {
		t.Error("entry last used before ProtectSince survived an over-budget pass")
	}
	if e, _ := s.Get(hashN(2)); e == nil {
		t.Error("entry used at ProtectSince was evicted")
	}

	putFilled(t, s, hashN(3), 2000, 'c')
	blobDir := s.blobDir(hashN(3))
	writeLastUsed(blobDir, since.Add(time.Second), 1) // stamped after enumeration saw it stale
	scan, _ := scanStore(root, false)
	out := evictStore(root, []gcEntry{{
		storeRoot: root,
		blobDir:   blobDir,
		lastUsed:  since.Add(-time.Hour),
		blobs:     readManifestBlobs(blobDir),
	}}, scan, time.Now(), func(last time.Time) bool { return !last.Before(since) }, false, time.Time{})
	if out.evicted != 0 {
		t.Error("evictStore removed a victim whose on-disk stamp is protected")
	}
	if e, _ := s.Get(hashN(3)); e == nil {
		t.Error("entry used between enumeration and eviction was evicted")
	}
}

// forceSections makes every unit of evictStore's work its own exclusive
// section, runs gap between two sections, and restores the production bounds
// when the test ends. Tests using it must not run in parallel.
func forceSections(t *testing.T, gap func()) {
	t.Helper()
	hold, work, reacquire, prevGap := gcHoldBudget, gcSectionMinWork, gcReacquireWait, gcSectionGap
	gcHoldBudget, gcSectionMinWork, gcSectionGap = 0, 1, gap
	t.Cleanup(func() {
		gcHoldBudget, gcSectionMinWork, gcReacquireWait, gcSectionGap = hold, work, reacquire, prevGap
	})
}

// TestEvictStore_YieldsBetweenSectionsAndKeepsABlobPublishedInTheGap pins the
// sectioned exclusive lock. Between two sections the lock is
// free — a reader or publisher gets through — and a publisher that
// deduplicates onto a blob the scan saw referenced only by a victim keeps it:
// the section that sweeps re-reads the entries published since. The trash a
// crashed pass left, and this pass's own, are gone afterwards.
func TestEvictStore_YieldsBetweenSectionsAndKeepsABlobPublishedInTheGap(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	for i, fill := range []byte{'a', 'b', 'c'} {
		putFilled(t, s, hashN(i+1), 1000, fill) // the victims, in fan-out 00
	}
	const kept = "ff00000000000000000000000000000000000000000000000000000000000001"
	const published = "ff00000000000000000000000000000000000000000000000000000000000002"
	putFilled(t, s, kept, 1000, 'k') // not a victim, in fan-out ff
	crashed := filepath.Join(root, "tmp", gcTrashPrefix+"crashed", "0")
	if err := os.MkdirAll(crashed, 0o755); err != nil {
		t.Fatal(err)
	}
	// Age both fan-out directories, so the scan's stamps are settled rather
	// than racy: only ff's mtime changing can make a refresh list it again,
	// since no victim lives there.
	old := time.Now().Add(-time.Hour)
	for _, fanout := range []string{"00", "ff"} {
		if err := os.Chtimes(filepath.Join(root, "blobs", fanout), old, old); err != nil {
			t.Fatal(err)
		}
	}
	scan, _ := scanStore(root, false)
	if stamp := scan.fanouts[filepath.Join(root, "blobs", "ff")]; stamp.read.Sub(stamp.mtime) < gcRacyWindow {
		t.Fatalf("fan-out stamp %+v is racy; the test needs a settled one", stamp)
	}
	var victims []gcEntry
	for _, e := range scan.entries {
		if filepath.Base(filepath.Dir(e.blobDir)) == "00" {
			victims = append(victims, e)
		}
	}

	gaps := 0
	forceSections(t, func() {
		gaps++
		if gaps != 1 {
			return
		}
		release, free := NewLocalStore(root).tryLockShared()
		if !free {
			t.Error("the exclusive lock was not released between two sections")
			return
		}
		release()
		// Same bytes as the first victim: the ingest links its CAS blob.
		putFilled(t, NewLocalStore(root), published, 1000, 'a')
	})
	out := evictStore(root, victims, scan, time.Now(), func(time.Time) bool { return false }, true, time.Time{})

	if gaps < 2 || out.busy || out.evicted != 3 || out.swept != 2 {
		t.Fatalf("gaps=%d busy=%v evicted=%d swept=%d, want several sections, 3 evicted and the 2 unshared blobs swept",
			gaps, out.busy, out.evicted, out.swept)
	}
	if out.held <= 0 || out.maxHeld > out.held {
		t.Errorf("held=%v maxHeld=%v, want the sections accounted", out.held, out.maxHeld)
	}
	for _, hash := range []string{kept, published} {
		refs := readManifestBlobs(s.blobDir(hash))
		if len(refs) != 1 {
			t.Fatalf("entry %s has manifest %+v", hash, refs)
		}
		if _, err := os.Stat(s.casBlobPath(refs[0].digest)); err != nil {
			t.Errorf("the blob entry %s references was swept: %v", hash, err)
		}
	}
	if trash, _ := filepath.Glob(filepath.Join(root, "tmp", gcTrashPrefix+"*")); len(trash) != 0 {
		t.Errorf("trash left behind: %v", trash)
	}
}

// TestEvictStore_StopsBusyWhenTheLockIsTakenBetweenSections: a pass that
// cannot take the lock back after a section stops there, reports busy, and
// leaves every victim it had not reached indexed.
func TestEvictStore_StopsBusyWhenTheLockIsTakenBetweenSections(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	for i, fill := range []byte{'a', 'b', 'c'} {
		putFilled(t, s, hashN(i+1), 1000, fill)
	}
	scan, _ := scanStore(root, false)

	var holder func()
	forceSections(t, func() {
		if holder == nil {
			holder, _ = NewLocalStore(root).tryLockShared()
		}
	})
	gcReacquireWait = 0
	out := evictStore(root, scan.entries, scan, time.Now(), func(time.Time) bool { return false }, true, time.Time{})
	if holder != nil {
		holder()
	}

	if !out.busy || out.evicted != 1 || out.swept != 0 {
		t.Fatalf("busy=%v evicted=%d swept=%d, want a busy stop after the first victim", out.busy, out.evicted, out.swept)
	}
	remaining := 0
	for i := 1; i <= 3; i++ {
		if e, _ := s.Get(hashN(i)); e != nil {
			remaining++
		}
	}
	if remaining != 2 {
		t.Errorf("%d victims still indexed, want the 2 the pass had not reached", remaining)
	}
}

func TestGeneration_BumpAndRead(t *testing.T) {
	root := t.TempDir()
	if g := currentGeneration(root); g != 0 {
		t.Errorf("fresh store generation = %d, want 0", g)
	}
	s := NewLocalStore(root)
	s.ensureGeneration()
	if s.gen != 1 {
		t.Errorf("first generation = %d, want 1", s.gen)
	}
	s.ensureGeneration() // must bump at most once per handle
	if s.gen != 1 {
		t.Errorf("generation advanced on second ensure = %d, want 1", s.gen)
	}
	if g := currentGeneration(root); g != 1 {
		t.Errorf("persisted generation = %d, want 1", g)
	}
	// A fresh handle models a new build and bumps again.
	s2 := NewLocalStore(root)
	s2.ensureGeneration()
	if s2.gen != 2 {
		t.Errorf("second handle generation = %d, want 2", s2.gen)
	}
}

func TestGetAndTouch_StampsUnderLock(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	putSized(t, s, hashN(1), 100)

	before := time.Now()
	entry, err := s.getAndTouch(hashN(1))
	if err != nil || entry == nil {
		t.Fatalf("getAndTouch: entry=%v err=%v", entry, err)
	}
	if got, _ := readLastUsed(s.blobDir(hashN(1))); got.Before(before.Add(-time.Second)) {
		t.Errorf("lastUsed = %v, want ~now", got)
	}
	// Miss must not error or stamp.
	if e, err := s.getAndTouch(hashN(9)); e != nil || err != nil {
		t.Errorf("getAndTouch miss = (%v, %v), want (nil, nil)", e, err)
	}
}

func TestMarkUsed_StampsLastUsed(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	putSized(t, s, hashN(1), 100)

	before := time.Now()
	s.markUsed(hashN(1))

	got, _ := readLastUsed(s.blobDir(hashN(1)))
	if got.Before(before.Add(-time.Second)) {
		t.Errorf("lastUsed = %v, want ~now (>= %v)", got, before)
	}
	if _, err := os.Stat(filepath.Join(s.blobDir(hashN(1)), lastUsedFile)); err != nil {
		t.Errorf("lastused sidecar not written: %v", err)
	}
}

func TestCleanStore_RemovesContentKeepsLock(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	putSized(t, s, hashN(1), 1000)
	ociBlob := filepath.Join(root, OCILayerCacheDirName, "v1", "aa", hashN(2), "layer.tar.gz")
	if err := os.MkdirAll(filepath.Dir(ociBlob), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ociBlob, make([]byte, 500), 0o600); err != nil {
		t.Fatal(err)
	}
	// Force the lock file to exist.
	s.lockShared()()

	files, bytes, err := CleanStore(root)
	if err != nil {
		t.Fatalf("CleanStore: %v", err)
	}
	if files == 0 || bytes == 0 {
		t.Errorf("CleanStore reported nothing removed: files=%d bytes=%d", files, bytes)
	}
	if _, err := os.Stat(filepath.Join(root, "blobs")); !os.IsNotExist(err) {
		t.Error("blobs/ should be gone")
	}
	if _, err := os.Stat(filepath.Join(root, "cas")); !os.IsNotExist(err) {
		t.Error("cas/ should be gone")
	}
	if _, err := os.Stat(filepath.Join(root, OCILayerCacheDirName)); !os.IsNotExist(err) {
		t.Error("oci/ should be gone")
	}
	if _, err := os.Stat(filepath.Join(root, storeLockFile)); err != nil {
		t.Error("lock file should be preserved across clean")
	}
}

func hashN(n int) string {
	const base = "0000000000000000000000000000000000000000000000000000000000000000"
	s := []byte(base)
	s[len(s)-1] = byte('0' + n)
	return string(s)
}
