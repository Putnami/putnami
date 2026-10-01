package jobs

// CacheStatsSnapshot is the immutable, machine-readable view of a run's cache
// activity, attached to SchedulerResult and consumed by the summary renderer.
// Durations are milliseconds for parity with the scheduler tuning report.
type CacheStatsSnapshot struct {
	LocalHits             int64 `json:"localHits,omitempty"`
	LocalMisses           int64 `json:"localMisses,omitempty"`
	LocalServedMs         int64 `json:"localServedMs,omitempty"`
	LocalKeysMs           int64 `json:"localKeysMs,omitempty"`
	LocalBindingsMs       int64 `json:"localBindingsMs,omitempty"`
	LocalRestoreVerifyMs  int64 `json:"localRestoreVerifyMs,omitempty"`
	LocalSpawnedProcesses int64 `json:"localSpawnedProcesses,omitempty"`

	SetupMs        int64 `json:"setupMs"`
	NegotiateMs    int64 `json:"negotiateMs"`
	KeysRequested  int64 `json:"keysRequested"`
	Hits           int64 `json:"hits"`
	Misses         int64 `json:"misses"`
	Restored       int64 `json:"restored"`
	HintsWarmed    int64 `json:"hintsWarmed"`
	TimeSavedMs    int64 `json:"timeSavedMs"`
	BytesFetched   int64 `json:"bytesFetched"`
	RestoreMs      int64 `json:"restoreMs"`
	Uploads        int64 `json:"uploads"`
	BlobsUploaded  int64 `json:"blobsUploaded"`
	BytesUploaded  int64 `json:"bytesUploaded"`
	BytesDeduped   int64 `json:"bytesDeduped"`
	UploadMs       int64 `json:"uploadMs"`
	UploadErrors   int64 `json:"uploadErrors"`
	UploadsSkipped int64 `json:"uploadsSkipped"`

	// ProviderSummary* are the terminal cache-provider Summary totals. They are
	// kept separately from the live counters above so callers can distinguish
	// exact provider-reported transfer accounting from locally observed events.
	ProviderSummaryRestoredCount int64 `json:"providerSummaryRestoredCount"`
	ProviderSummaryRestoredBytes int64 `json:"providerSummaryRestoredBytes"`
	ProviderSummaryUploadedCount int64 `json:"providerSummaryUploadedCount"`
	ProviderSummaryUploadedBytes int64 `json:"providerSummaryUploadedBytes"`
	ProviderSummaryAvailable     bool  `json:"providerSummaryAvailable"`

	// StoreBudget is what the run's in-run store budget did: the collection
	// passes it started at batch boundaries and their outcome. Absent when no
	// pass ran.
	StoreBudget *StoreBudgetStats `json:"storeBudget,omitempty"`
}

// StoreBudgetStats is the outcome of the collection passes a run's in-run
// store budget started at batch boundaries, when the store's projected usage
// crossed its byte budget.
type StoreBudgetStats struct {
	// Passes is the number of collection passes started.
	Passes int64 `json:"passes"`
	// EvictedEntries is the number of cache entries they evicted.
	EvictedEntries int64 `json:"evictedEntries"`
	// FreedBytes is the disk space they reclaimed.
	FreedBytes int64 `json:"freedBytes"`
	// StoresSkippedBusy counts stores a pass skipped because another holder
	// had them locked: during the scan, so their bytes went unmeasured, or
	// with entries to evict, so the budget was not enforced there. A pass
	// that skipped one is retried.
	StoresSkippedBusy int64 `json:"storesSkippedBusy"`
	// LockHoldMs is the total time the passes held stores' exclusive locks.
	// Every lookup, publish and restore on a store, in every session sharing
	// it, waits while its lock is held.
	LockHoldMs int64 `json:"lockHoldMs"`
	// MaxLockHoldMs is the longest single hold: the longest any of those
	// waited at once.
	MaxLockHoldMs int64 `json:"maxLockHoldMs"`
}

// HasActivity reports whether this snapshot contains cache work worth
// publishing. Setup alone is intentionally insufficient: a configured provider
// that received no key should not create an all-zero cache summary.
func (s *CacheStatsSnapshot) HasActivity() bool {
	return s != nil && (s.LocalHits > 0 || s.LocalMisses > 0 || s.LocalServedMs > 0 ||
		s.KeysRequested > 0 || s.Uploads > 0 || s.Restored > 0 || s.HintsWarmed > 0 ||
		s.StoreBudget != nil)
}

// OverheadMs is the total wall time the cache machinery itself cost: the pre-run
// setup, the negotiate round trip, the restore (materializing hits into the local
// store), and the post-build uploads. The summary contrasts it with TimeSavedMs to
// answer "did the cache help?". Restore aggregates across concurrently restored
// jobs, so OverheadMs is an upper bound on the wall time the cache added — but it
// no longer hides the dominant cost of a slow restore.
func (s *CacheStatsSnapshot) OverheadMs() int64 {
	if s == nil {
		return 0
	}
	return s.SetupMs + s.NegotiateMs + s.RestoreMs + s.UploadMs
}
