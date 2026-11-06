package remotecache

import (
	"path/filepath"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
)

// newUploadTestCache builds a minimal RemoteCache for exercising the pure
// upload-eligibility logic (PrepareTaskUpload), which needs only the stats sink
// — no provider session or network.
func newUploadTestCache() *RemoteCache {
	r := &RemoteCache{}
	r.SetCacheStats(&cachekey.CacheStats{})
	return r
}

func TestPrepareTaskUpload_SkipsAJobWithExternalEffectsByTrait(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publication-follows-declared-effects",
		"a-side-effecting-job-is-never-shared-by-trait")
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	hash := "ab" + strings.Repeat("0", cache.KeyLength-2)
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "ship", Project: "proj", DurationMs: 1500}
	if _, err := cm.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
		Key: hash, Result: &store.EntryResult{Status: "success"}, Metadata: meta,
	}); err != nil {
		t.Fatalf("publish task entry: %v", err)
	}

	registry := cacheableJob("ship", "/proj", "proj", "proj")
	registry.JobDef.Traits.SideEffects = extensionproto.SideEffectsRegistry
	declared := cacheableJob("ship", "/proj", "proj", "proj")
	declared.Extension.Tasks["cacheable"] = extension.TaskDefinition{
		Declares: &extension.TaskDeclaration{Effects: []string{extensionproto.EffectCloud}},
	}
	// The command name decides nothing: a publish job with no declared effect
	// is an ordinary cacheable job.
	named := cacheableJob("publish", "/proj", "proj", "proj")

	r := newUploadTestCache()
	for label, job := range map[string]*jobmodel.ScheduledJob{"registry trait": registry, "declared cloud effect": declared} {
		if _, ok := r.PrepareTaskUpload(hash, job, cm); ok {
			t.Fatalf("a job with a %s must never be eligible for remote upload", label)
		}
	}
	if _, ok := r.PrepareTaskUpload(hash, named, cm); !ok {
		t.Fatalf("a publish job without declared effects must be eligible for remote upload")
	}
}

// uploadTestEvents are the cached events of a lint entry: a metric and a
// summary, both kinds an upload can carry.
func uploadTestEvents() []cache.ActionEvent {
	return []cache.ActionEvent{
		{Version: 1, Type: jobmodel.EventTypeMetric, Data: map[string]any{"name": "lint-warnings", "value": float64(2), "unit": "count"}},
		{Version: 1, Type: jobmodel.EventTypeSummary, Data: map[string]any{"message": "2 warnings"}},
	}
}

// The prepared task upload keeps the cached events whatever the provider
// answered, and the upload drops them unless the provider advertised
// action-events.
func TestProviderResult_CarriesEventsOnlyWhenAdvertised(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-write-path-is-store-then-commit", "the-cli-uploads-cached-events-only-when-advertised")
	t.Parallel()
	events := uploadTestEvents()
	producer := publishFetchTestEntry(t, &store.EntryResult{Status: "success", Events: events})
	in, ok := newUploadTestCache().PrepareTaskUpload(fetchTestKey, fetchTestJob(), producer)
	if !ok {
		t.Fatalf("expected an eligible upload")
	}
	if len(in.Result.Events) != len(events) {
		t.Fatalf("the prepared input must keep the cached events, got %d want %d", len(in.Result.Events), len(events))
	}

	// The provider has not advertised event support: events are dropped, and
	// the prepared input keeps them.
	withoutEvents := newUploadTestCache()
	if got := withoutEvents.providerResult(in.Result); len(got.Events) != 0 || got.Status != "success" || got.DurationMs != 4000 {
		t.Fatalf("without action-events the upload must carry the result without its events, got %+v", got)
	}
	if len(in.Result.Events) != len(events) {
		t.Fatalf("dropping the events must not modify the prepared input, got %d events", len(in.Result.Events))
	}

	// With support advertised, events ride along.
	withEvents := newUploadTestCache()
	withEvents.actionEvents = true
	if got := withEvents.providerResult(in.Result); len(got.Events) != len(events) {
		t.Fatalf("events must be included when advertised, got %d want %d", len(got.Events), len(events))
	}
}
