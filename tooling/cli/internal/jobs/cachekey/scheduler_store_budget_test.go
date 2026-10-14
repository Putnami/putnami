package cachekey

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/store"
)

// TestCacheStats_RecordsTheStoreBudgetForTheSessionRecord pins the session
// record's cache member for the in-run budget: absent until a pass ran, then
// every outcome under its wire name, and enough on its own to publish the
// cache summary.
func TestCacheStats_RecordsTheStoreBudgetForTheSessionRecord(t *testing.T) {
	var none *CacheStats
	none.RecordStoreBudget(store.RunBudgetReport{Passes: 1}) // caching off: no stats, no panic

	stats := &CacheStats{}
	stats.RecordStoreBudget(store.RunBudgetReport{})
	if snap := stats.Snapshot(); snap.StoreBudget != nil || snap.HasActivity() {
		t.Fatalf("a budget that never ran a pass reached the snapshot: %+v", snap.StoreBudget)
	}

	stats.RecordStoreBudget(store.RunBudgetReport{
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
