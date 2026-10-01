package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// putBudgetEntry publishes an entry whose one output file is size bytes of
// fill, so every fill is its own CAS blob.
func putBudgetEntry(s *store.LocalStore, hash string, size int, fill byte) error {
	dir, err := os.MkdirTemp("", "store-budget-entry-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "out.bin"), bytes.Repeat([]byte{fill}, size), 0o644); err != nil {
		return err
	}
	return s.Put(hash, &store.Entry{Result: &store.EntryResult{Status: "success"}, FilesDir: dir})
}

// TestRunPlan_StoreBudgetCollectsAtABatchBoundaryAndSparesInFlightEntries
// proves the acceptance criterion: a run whose store grows past the budget mid-run triggers
// a collection pass at the next batch boundary, while other work is still in
// flight, and that pass evicts only an entry no job of the run has touched.
//
// Two jobs run side by side. "hold" looks up an entry (the stamp a restoring
// task writes) and stays in flight until a pass has evicted something. "grow"
// publishes 4 KB, which takes the store past its 10 KB budget; its completion
// is the batch boundary. The grace window is 0, so the run's own protection
// boundary is the only rule that can spare the held entry. The session's cache
// member then reports the pass.
func TestRunPlan_StoreBudgetCollectsAtABatchBoundaryAndSparesInFlightEntries(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	t.Setenv("PUTNAMI_STORE_DIR", storeRoot)
	t.Setenv("PUTNAMI_STORE_MAX_BYTES", "")
	t.Setenv("PUTNAMI_STORE_GC_GRACE", "")
	t.Setenv("PUTNAMI_STORE_MAX_IDLE_BUILDS", "")

	stale := strings.Repeat("1", 64)
	held := strings.Repeat("2", 64)
	grown := strings.Repeat("3", 64)

	// Entries earlier runs left behind, last used before this run starts.
	seed := store.NewLocalStore(storeRoot)
	if err := putBudgetEntry(seed, stale, 4000, 's'); err != nil {
		t.Fatal(err)
	}
	if err := putBudgetEntry(seed, held, 4000, 'h'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // keep the seeds' stamps strictly before the run's boundary

	cache := store.NewCacheManager(store.NewLocalStore(storeRoot))
	maxBytes, grace := int64(10000), time.Duration(0)
	budget := store.NewRunBudget(t.TempDir(), store.Config{MaxBytes: &maxBytes, GCGrace: &grace}, cache.Store())

	holdJob := makeScheduledJob("app", "hold", nil)
	growJob := makeScheduledJob("lib", "grow", nil)
	failed := func(format string, args ...any) *JobResult {
		return &JobResult{Status: "failed", Error: &JobError{Message: fmt.Sprintf(format, args...)}}
	}
	holding := make(chan struct{})
	internal := map[string]InternalJobRunner{
		holdJob.Key(): func(ctx context.Context, _ map[string]*JobResult) *JobResult {
			cache.MarkUsed(held)
			close(holding)
			deadline := time.Now().Add(20 * time.Second)
			for {
				if e, _ := cache.Store().Get(stale); e == nil {
					break
				}
				if time.Now().After(deadline) {
					return failed("no collection pass ran while this job was in flight")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if e, _ := cache.Store().Get(held); e == nil {
				return failed("the pass evicted the entry this in-flight job looked up")
			}
			return &JobResult{Status: "success"}
		},
		growJob.Key(): func(ctx context.Context, _ map[string]*JobResult) *JobResult {
			select {
			case <-holding:
			case <-time.After(20 * time.Second):
				return failed("the holding job never started")
			}
			if err := putBudgetEntry(cache.Store(), grown, 4000, 'g'); err != nil {
				return failed("publish: %v", err)
			}
			return &JobResult{Status: "success"}
		},
	}

	ws := &workspace.Workspace{Name: "store-budget", Root: t.TempDir(), Graph: workspace.BuildGraph(nil)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result := RunPlan(ctx, RunRequest{
		Workspace:    ws,
		Plan:         []*ScheduledJob{holdJob, growJob},
		InternalJobs: internal,
		Config:       SchedulerConfig{MaxParallel: 2},
		Renderer:     &mockRenderer{},
		Cache:        cache,
		StoreBudget:  budget,
	})
	for key, res := range result.Results {
		if res.Status != "success" {
			t.Errorf("%s: %s %+v", key, res.Status, res.Error)
		}
	}
	if !result.Success {
		t.Fatal("run failed")
	}

	if e, _ := cache.Store().Get(stale); e != nil {
		t.Error("the entry no job touched survived the over-budget pass")
	}
	for name, hash := range map[string]string{"held": held, "grown": grown} {
		if e, _ := cache.Store().Get(hash); e == nil {
			t.Errorf("the %s entry, used during the run, was evicted", name)
		}
	}

	// The session record's cache member (cacheSummary(result.Cache)) says so.
	if result.Cache == nil || result.Cache.StoreBudget == nil {
		t.Fatalf("the session's cache snapshot carries no store budget: %+v", result.Cache)
	}
	if got := result.Cache.StoreBudget; got.Passes < 1 || got.EvictedEntries != 1 || got.FreedBytes < 4000 || got.StoresSkippedBusy != 0 {
		t.Errorf("store budget in the session record = %+v, want the pass that evicted the untouched 4 KB entry", got)
	}
}

// TestCacheStats_RecordsTheStoreBudgetForTheSessionRecord pins the session
// record's cache member for the in-run budget: absent until a pass ran, then
// every outcome under its wire name, and enough on its own to publish the
// cache summary.
func TestCacheStats_RecordsTheStoreBudgetForTheSessionRecord(t *testing.T) {
	var none *CacheStats
	none.recordStoreBudget(store.RunBudgetReport{Passes: 1}) // caching off: no stats, no panic

	stats := &CacheStats{}
	stats.recordStoreBudget(store.RunBudgetReport{})
	if snap := stats.Snapshot(); snap.StoreBudget != nil || snap.HasActivity() {
		t.Fatalf("a budget that never ran a pass reached the snapshot: %+v", snap.StoreBudget)
	}

	stats.recordStoreBudget(store.RunBudgetReport{
		Passes: 2, EvictedEntries: 3, FreedBytes: 4096, StoresSkippedBusy: 1,
		LockHeld: 250 * time.Millisecond, MaxLockHeld: 200 * time.Millisecond,
	})
	snap := stats.Snapshot()
	if !snap.HasActivity() {
		t.Error("a run whose budget ran a pass has no cache summary to publish")
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	want := `"storeBudget":{"passes":2,"evictedEntries":3,"freedBytes":4096,"storesSkippedBusy":1,"lockHoldMs":250,"maxLockHoldMs":200}`
	if !strings.Contains(string(data), want) {
		t.Errorf("session cache member = %s, want it to contain %s", data, want)
	}
}
