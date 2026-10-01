package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestIngestTaskEntry_ConcurrentSameKey models every worktree on a machine
// capturing the same task at once against the one machine-global store: separate
// LocalStore handles, separate staging roots, one address. Exactly one intact
// entry must survive, with no torn descriptor and no leaked staging directory.
func TestIngestTaskEntry_ConcurrentSameKey(t *testing.T) {
	root := t.TempDir()
	key := "concurrent-ingest-key"

	const writers = 12
	var wg sync.WaitGroup
	errs := make([]error, writers)
	entries := make([]*TaskEntry, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := NewLocalStore(root) // distinct handle per "process"
			staging := t.TempDir()
			out := dirOutput("dist", "dist")
			stage(t, staging, out, "main.js", "built")
			stage(t, staging, out, "sub/chunk.js", "chunk")
			entries[i], errs[i] = s.IngestTaskEntry(staging, taskSpec(key, out))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: IngestTaskEntry: %v", i, err)
			continue
		}
		// Every writer observes the SAME published entry — first-writer-wins,
		// and the loser reads back the winner's copy rather than its own stage.
		if entries[i] == nil || entries[i].Address != entries[0].Address {
			t.Errorf("writer %d observed a different entry: %+v", i, entries[i])
		}
	}

	s := NewLocalStore(root)
	entry, err := s.LookupTaskEntry(key)
	if err != nil || entry == nil {
		t.Fatalf("entry missing after concurrent ingest: %v", err)
	}
	if len(entry.Outputs) != 1 || !entry.Outputs[0].Present() || entry.Outputs[0].Files != 2 {
		t.Errorf("torn declared-output manifest: %+v", entry.Outputs)
	}
	assertBlobFile(t, entry, "dist/main.js", "built")
	assertBlobFile(t, entry, "dist/sub/chunk.js", "chunk")

	tmpEntries, _ := os.ReadDir(filepath.Join(root, "tmp"))
	for _, e := range tmpEntries {
		t.Errorf("leftover staging dir: %s", e.Name())
	}
}

// TestMaterializeTaskOutput_ConcurrentSameDestination pins the swap's safety
// property under contention: population happens under a unique staging name, so
// a directory that is live at the destination is ALWAYS complete. Every writer
// must also succeed — losing the two-rename swap is retried, not reported.
//
// The observer discards any sample that straddled a swap, and that exclusion is
// load-bearing rather than convenient. A reader which has already opened the
// destination directory keeps reading THAT inode; once a writer renames it aside
// and reclaims it, the stale handle watches a detached tree being emptied and
// reports 0, 1 or 2 files. Reclaiming the replaced tree at all makes that
// unavoidable, and the legacy path is strictly worse (replaceDirPreserving
// RemoveAll's the destination in place before renaming). What IS guaranteed, and
// what the surviving samples check, is that the inode living at the destination
// is never mutated after it becomes visible: a regression that populated the
// destination in place would keep the inode stable and be caught here.
func TestMaterializeTaskOutput_ConcurrentSameDestination(t *testing.T) {
	s := NewLocalStore(t.TempDir())
	want := map[string]string{"main.js": "built", "sub/chunk.js": "chunk", "sub/deep/x.js": "x"}
	entry := ingestTree(t, s, "concurrent-materialize-key", want)

	dest := filepath.Join(t.TempDir(), "out")
	stop := make(chan struct{})
	var partial atomic.Int64
	var observed atomic.Int64

	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			before, err := os.Lstat(dest)
			if err != nil || !before.IsDir() {
				continue // absent: the legitimate two-syscall swap window
			}
			n, ok := countTreeFiles(dest)
			if !ok {
				continue
			}
			// Same inode before and after ⇒ this sample observed one live
			// destination tree throughout, not a tree being reclaimed.
			if after, err := os.Lstat(dest); err != nil || !os.SameFile(before, after) {
				continue
			}
			observed.Add(1)
			if n != len(want) {
				partial.Add(1)
			}
		}
	}()

	// The writers keep the churn alive until the observer has actually sampled
	// it, up to a bound. Fixing the round count at 4 made the non-vacuity guard
	// below a function of scheduling luck: on a CPU-quota-limited runner (and
	// under -race) eight writers can finish the whole swap sequence before the
	// observer goroutine gets a slice, leaving observed == 0 and failing a test
	// about materialize on the runner's core count.
	const (
		writers   = 8
		minRounds = 4
		maxRounds = 64
	)
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for round := range maxRounds {
				if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
					errs[i] = err
					return
				}
				if round+1 >= minRounds && observed.Load() > 0 {
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// The destination is quiescent now, so the observer's next iteration is
	// guaranteed to land a clean sample. Waiting for it keeps the guard below an
	// assertion about materialize rather than about scheduling: a runner that
	// starved the observer for the entire churn still evaluates completeness,
	// against the settled tree, instead of failing outright.
	for range 1000 {
		if observed.Load() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	if observed.Load() == 0 {
		t.Fatal("the observer never saw the destination: the completeness check proves nothing")
	}
	if partial.Load() > 0 {
		t.Errorf("observed %d partially populated destinations (of %d samples)", partial.Load(), observed.Load())
	}
	assertTree(t, dest, want)

	parent := filepath.Dir(dest)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if e.Name() != "out" {
			t.Errorf("leftover staging/replaced path: %s", e.Name())
		}
	}
}

// TestMaterializeTaskOutput_RacingGC pins the reason materialize extends the
// existing lease/touch pattern instead of forking one: LookupTaskEntry stamps
// lastUsed under the store's SHARED lock and materialize holds that lock for the
// whole copy, so a GC pass running concurrently (and needing the EXCLUSIVE lock)
// cannot reclaim the blob out from under a restore inside the grace window.
func TestMaterializeTaskOutput_RacingGC(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	want := map[string]string{"main.js": "built", "sub/chunk.js": "chunk"}
	ingestTree(t, s, "gc-race-key", want)

	dest := filepath.Join(t.TempDir(), "out")
	stop := make(chan struct{})
	gcDone := make(chan struct{})
	var gcPasses atomic.Int64

	go func() {
		defer close(gcDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Budget of 1 byte: every pass wants to evict everything it is
			// allowed to. Grace spares entries hit in the last minute, which is
			// exactly the protection a just-looked-up entry relies on.
			if _, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: time.Minute}); err == nil {
				gcPasses.Add(1)
			}
		}
	}()
	var stopGCOnce sync.Once
	stopGC := func() {
		stopGCOnce.Do(func() {
			close(stop)
			<-gcDone
		})
	}
	defer stopGC()

	for round := range 25 {
		reader := NewLocalStore(root)
		entry, err := reader.LookupTaskEntry("gc-race-key")
		if err != nil {
			t.Fatalf("round %d: LookupTaskEntry: %v", round, err)
		}
		if entry == nil {
			t.Fatalf("round %d: a hit within the grace window was evicted", round)
		}
		if _, err := reader.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
			t.Fatalf("round %d: materialize raced GC: %v", round, err)
		}
		assertTree(t, dest, want)
	}
	stopGC()

	if gcPasses.Load() == 0 {
		t.Error("no GC pass completed: the race was never exercised")
	}
}

// TestTaskEntry_GCEvictsUnprotectedEntries is the positive control for the test
// above: with the grace window closed, the very same GC configuration DOES
// reclaim a task-owned entry. Without it, "materialize survived GC" could mean
// GC never evicts these entries at all.
func TestTaskEntry_GCEvictsUnprotectedEntries(t *testing.T) {
	root := t.TempDir()
	s := NewLocalStore(root)
	ingestTree(t, s, "evictable-key", map[string]string{"main.js": "built"})

	if got, _ := s.LookupTaskEntry("evictable-key"); got == nil {
		t.Fatal("entry missing before GC")
	}
	res, err := RunGC([]string{root}, GCOptions{MaxBytes: 1, Grace: 0, Now: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("RunGC: %v", err)
	}
	if res.EvictedEntries == 0 {
		t.Fatal("GC evicted nothing: the racing test's grace window proves nothing")
	}
	if got, err := s.LookupTaskEntry("evictable-key"); err != nil || got != nil {
		t.Errorf("evicted entry still served: got=%v err=%v", got, err)
	}
	// The CAS bytes go with it, so a task-owned entry is ordinary to the sweep.
	if res.SweptBlobs == 0 {
		t.Error("no CAS blob swept: task-owned manifests are not being accounted")
	}
}

// countTreeFiles reports the number of regular files under dir. ok is false when
// the path does not currently exist or is not a directory — the legitimate swap
// window — so the caller can distinguish "absent" from "present but partial".
func countTreeFiles(dir string) (int, bool) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return 0, false
	}
	count := 0
	err = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, false // raced a swap mid-walk; not an observation
	}
	return count, true
}

// BenchmarkTaskOutputMaterializeVsSymlink measures what retiring the CAS-symlink
// all-hit fast path costs (the decision recorded in task_entry.go). Three ways
// to put one cached output tree where a consumer expects it:
//
//	symlink         the retired fast path — one symlink() into the blob
//	materialize     the new atomic primitive (stage + rename)
//	legacy-restore  what the scheduler ALREADY does whenever the fast path does
//	                not apply (CacheManager.RestoreDir → replaceDirPreserving)
//
// The third is the honest comparison: the fast path only ever applied to an
// all-hit command whose single snapshot covered the whole directory, and every
// other restore in the CLI today pays the copy. See the recorded numbers in
// task_entry.go.
func BenchmarkTaskOutputMaterializeVsSymlink(b *testing.B) {
	const files = 256
	const size = 16 * 1024

	root := b.TempDir()
	s := NewLocalStore(root)
	staging := b.TempDir()
	out := dirOutput("dist", "dist")
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	for i := range files {
		p := filepath.Join(TaskStagingPath(staging, out), fmt.Sprintf("chunk-%03d.js", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, payload, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	entry, err := s.IngestTaskEntry(staging, taskSpec("bench-key", out))
	if err != nil {
		b.Fatal(err)
	}
	target := filepath.Join(entry.FilesDir, "dist")

	b.Run("symlink", func(b *testing.B) {
		dir := b.TempDir()
		for i := 0; b.Loop(); i++ {
			link := filepath.Join(dir, fmt.Sprintf("out-%d", i))
			if err := os.Symlink(target, link); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("materialize", func(b *testing.B) {
		dest := filepath.Join(b.TempDir(), "out")
		for b.Loop() {
			if _, err := s.MaterializeTaskOutput(entry, "dist", dest, ""); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("legacy-restore", func(b *testing.B) {
		dest := filepath.Join(b.TempDir(), "out")
		for b.Loop() {
			if err := replaceDirPreserving(target, dest, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
