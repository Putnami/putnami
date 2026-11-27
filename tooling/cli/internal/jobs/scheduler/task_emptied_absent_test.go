package scheduler

import (
	"context"
	"os"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/store"
)

// A restore of a record that holds an emptied output absent writes nothing
// when the destination was already absent before the run began to read the
// tree, so it invalidates nothing: no memoized digest and no read of the
// candidate cut. One that removes a file an earlier run left wrote that path,
// and so does one that finds the destination removed since the run's reads
// began, which another session may have done after a read this run memoized
// (ADR 0119).
func TestARestoreThatEmptiesAnAbsentOutputInvalidatesNothing(t *testing.T) {
	f, index := emptiedFileFixture(t)
	f.setPages(t, "")
	f.executed(t, "execution that writes no page", f.run(t, SchedulerConfig{}))
	assertNoFile(t, "execution that writes no page", index)

	invalidations := 0
	saved := betweenInvalidations
	betweenInvalidations = func() { invalidations++ }
	t.Cleanup(func() { betweenInvalidations = saved })
	// restore runs a session whose reads of the tree began at since.
	restore := func(leg string, since time.Time) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cache := store.NewCacheManager(store.NewLocalStore(f.storeRoot))
		cache.SetTreeReadsSince(since)
		f.hit(t, leg, runBatchScheduler(ctx, f.ws, f.planned, SchedulerConfig{MaxParallel: 1}, cache))
		assertNoFile(t, leg, index)
	}
	settle := func() time.Time {
		time.Sleep(50 * time.Millisecond)
		since := time.Now()
		time.Sleep(50 * time.Millisecond)
		return since
	}

	restore("restore over a page absent before the run", settle())
	if invalidations != 0 {
		t.Fatalf("the restore over a page absent before the run invalidated %d time(s), want none", invalidations)
	}

	writeFileAt(t, index, "<h1>stale</h1>\n")
	restore("restore over a stale page", settle())
	if invalidations == 0 {
		t.Fatal("the restore that removed the stale page invalidated nothing")
	}

	// A removal the restore makes counts whatever the file system's clock
	// stamps: here every change time lies before the run's reads.
	invalidations = 0
	writeFileAt(t, index, "<h1>stale</h1>\n")
	restore("restore over a stale page with a lagging clock", time.Now().Add(time.Hour))
	if invalidations == 0 {
		t.Fatal("the restore that removed the stale page under a lagging clock invalidated nothing")
	}

	invalidations = 0
	writeFileAt(t, index, "<h1>stale</h1>\n")
	since := settle()
	if err := os.Remove(index); err != nil {
		t.Fatal(err)
	}
	restore("restore over a page removed during the run", since)
	if invalidations == 0 {
		t.Fatal("the restore over a page removed after the run's reads began invalidated nothing")
	}
}
