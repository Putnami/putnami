package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildSyntheticStore publishes entries through the real Put path, each with
// filesPerEntry distinct small output files (so every file is its own CAS
// blob) under a hash spread across the fan-out directories like a real key,
// and returns the store's scan with one entry in ten as GC victims.
func buildSyntheticStore(b *testing.B, root string, entries, filesPerEntry int) (*storeScan, []gcEntry) {
	b.Helper()
	s := NewLocalStore(root)
	src := b.TempDir()
	for e := range entries {
		dir := filepath.Join(src, fmt.Sprintf("e%d", e))
		for f := range filesPerEntry {
			path := filepath.Join(dir, "out", fmt.Sprintf("f%d.txt", f))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(path, fmt.Appendf(nil, "entry %d file %d %0512d", e, f, e*filesPerEntry+f), 0o644); err != nil {
				b.Fatal(err)
			}
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "entry %d", e)))
		if err := s.Put(hash, entryWith(dir)); err != nil {
			b.Fatalf("Put %s: %v", hash, err)
		}
		_ = os.RemoveAll(dir)
	}
	scan, state := scanStore(root, false)
	if state != scanDone {
		b.Fatalf("scanStore state = %v", state)
	}
	victims := make([]gcEntry, 0, entries/10)
	for i, e := range scan.entries {
		if i%10 == 0 {
			victims = append(victims, e)
		}
	}
	return scan, victims
}

// BenchmarkEvictStore_ExclusiveLockHold measures how long one collection pass
// holds a store's EXCLUSIVE lock — the time every lookup, publish and restore
// on that store waits, in every process sharing it — on synthetic stores of
// realistic shape, evicting a tenth of their entries. hold-ms is the total
// across evictStore's locked sections, max-section-ms the longest one, which is
// what a single waiter can be kept waiting. Run it with:
//
//	./putnamiw test --projects @putnami/cli --run '^$' \
//	  --bench BenchmarkEvictStore_ExclusiveLockHold --benchtime 1x --coverage=false --test-verbose
func BenchmarkEvictStore_ExclusiveLockHold(b *testing.B) {
	for _, shape := range []struct{ entries, filesPerEntry int }{
		{entries: 1000, filesPerEntry: 10},
		{entries: 3000, filesPerEntry: 10},
		{entries: 10000, filesPerEntry: 5},
	} {
		name := fmt.Sprintf("entries=%d/blobs=%d/evict=%d", shape.entries, shape.entries*shape.filesPerEntry, shape.entries/10)
		b.Run(name, func(b *testing.B) {
			var held, maxHeld time.Duration
			for range b.N {
				b.StopTimer()
				root := b.TempDir()
				scan, victims := buildSyntheticStore(b, root, shape.entries, shape.filesPerEntry)
				b.StartTimer()
				out := evictStore(root, victims, scan, time.Now(), func(time.Time) bool { return false }, true, time.Time{})
				b.StopTimer()
				if out.busy || out.evicted != len(victims) || out.swept != len(victims)*shape.filesPerEntry {
					b.Fatalf("evictStore busy=%v evicted=%d swept=%d, want %d and %d",
						out.busy, out.evicted, out.swept, len(victims), len(victims)*shape.filesPerEntry)
				}
				held += out.held
				maxHeld = max(maxHeld, out.maxHeld)
			}
			b.ReportMetric(float64(held.Milliseconds())/float64(b.N), "hold-ms/op")
			b.ReportMetric(float64(maxHeld.Milliseconds()), "max-section-ms")
		})
	}
}
