package remotecache

import (
	"os"
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
// upload-eligibility logic (prepareUpload), which needs only the stats sink — no
// provider session or network.
func newUploadTestCache() *RemoteCache {
	r := &RemoteCache{}
	r.SetCacheStats(&cachekey.CacheStats{})
	return r
}

func TestPrepareUpload_SharesACheapArtifactWithItsBytes(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := cacheableJob("build", "/proj", "proj", "proj")
	hash := "ba" + strings.Repeat("d", cache.KeyLength-2)

	// A 5 ms build with real output bytes is shared like any other entry: no
	// duration or size rule leaves a non-side-effecting result out of the
	// remote cache.
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "out.js"), []byte("compiled bytes"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "build", Project: "proj", DurationMs: 5}
	if err := cm.Save(hash, &store.EntryResult{Status: "success"}, meta, outDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := newUploadTestCache()
	in, ok := r.prepareUpload(hash, job, cm)
	if !ok {
		t.Fatal("a cheap bytes-carrying artifact must be eligible for upload")
	}
	if in.Key != hash {
		t.Fatalf("upload key = %q, want %q", in.Key, hash)
	}
	if in.Manifest == nil || len(in.Manifest.Files) != 1 || in.Manifest.Files[0].Path != "out.js" {
		t.Fatalf("upload manifest = %+v, want the one built file", in.Manifest)
	}
	if ManifestBytes(in.Manifest) == 0 {
		t.Fatal("upload manifest carries no bytes; the case under test is a bytes-carrying entry")
	}
	if in.Result == nil || in.Result.DurationMs != 5 {
		t.Fatalf("upload result = %+v, want the recorded 5 ms duration", in.Result)
	}
}

func TestPrepareUpload_FilesLessCheapResultUploads(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := cacheableJob("config-merge", "/proj", "proj", "proj")
	hash := "cd" + strings.Repeat("e", cache.KeyLength-2)

	// A cheap files-less result uploads with a zero-file manifest: the exchange
	// registers the key→result mapping and moves no blobs.
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "config-merge", Project: "proj", DurationMs: 5}
	if err := cm.Save(hash, &store.EntryResult{Status: "skipped"}, meta, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := newUploadTestCache()
	in, ok := r.prepareUpload(hash, job, cm)
	if !ok {
		t.Fatalf("a cheap files-less result must be eligible for upload")
	}
	if in.Manifest == nil || len(in.Manifest.Files) != 0 {
		t.Fatalf("files-less upload must carry a zero-file manifest, got %+v", in.Manifest)
	}
}

func TestPrepareUpload_StatusOnlyResultGetsEmptyManifest(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := cacheableJob("lint", "/proj", "proj", "proj")
	hash := "fa" + strings.Repeat("c", cache.KeyLength-2)

	// No output dir → status-only entry (no manifest).
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "lint", Project: "proj", DurationMs: 1500}
	if err := cm.Save(hash, &store.EntryResult{Status: "success"}, meta, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := newUploadTestCache()
	in, ok := r.prepareUpload(hash, job, cm)
	if !ok {
		t.Fatalf("a status-only result must be eligible for upload")
	}
	if in.Manifest == nil || len(in.Manifest.Files) != 0 {
		t.Fatalf("status-only upload must carry a zero-file manifest, got %+v", in.Manifest)
	}
}

func TestPrepareUpload_SkipsAJobWithExternalEffectsByTrait(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "publication-follows-declared-effects",
		"a-side-effecting-job-is-never-shared-by-trait")
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	hash := "ab" + strings.Repeat("0", cache.KeyLength-2)
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "ship", Project: "proj", DurationMs: 1500}
	if err := cm.Save(hash, &store.EntryResult{Status: "success"}, meta, ""); err != nil {
		t.Fatalf("Save: %v", err)
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
		if _, ok := r.prepareUpload(hash, job, cm); ok {
			t.Fatalf("a job with a %s must never be eligible for remote upload", label)
		}
	}
	if _, ok := r.prepareUpload(hash, named, cm); !ok {
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

// The prepared input keeps the cached events whatever the provider answered,
// and the upload drops them unless the provider advertised action-events.
func TestProviderResult_CarriesEventsOnlyWhenAdvertised(t *testing.T) {
	spectest.Proves(t, "cache/remote-cache-contract", "the-write-path-is-store-then-commit", "the-cli-uploads-cached-events-only-when-advertised")
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := cacheableJob("lint", "/proj", "proj", "proj")
	hash := "ce" + strings.Repeat("1", cache.KeyLength-2)
	events := uploadTestEvents()
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "lint", Project: "proj", DurationMs: 1500}
	if err := cm.Save(hash, &store.EntryResult{Status: "success", Events: events}, meta, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	in, ok := newUploadTestCache().prepareUpload(hash, job, cm)
	if !ok {
		t.Fatalf("expected an eligible upload")
	}
	if len(in.Result.Events) != len(events) {
		t.Fatalf("the prepared input must keep the cached events, got %d want %d", len(in.Result.Events), len(events))
	}

	// The provider has not advertised event support: events are dropped, and
	// the prepared input keeps them.
	withoutEvents := newUploadTestCache()
	if got := withoutEvents.providerResult(in.Result); len(got.Events) != 0 || got.Status != "success" || got.DurationMs != 1500 {
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

func TestComputeRequiredInputs_ExcludesLocalHitDependencies(t *testing.T) {
	t.Parallel()
	dep := cacheableJob("build", "/dep", "dep", "dep")
	consumer := cacheableJob("test", "/proj", "proj", "proj", dep.Key())
	planned := []*jobmodel.ScheduledJob{dep, consumer}

	r := &RemoteCache{
		Keys: map[string]string{
			dep.Key():      "dephash",
			consumer.Key(): "consumerhash",
		},
		LocalHits: map[string]bool{},
	}

	// The consumer executes locally, so its dependency is a required input: a
	// files-less hit for it must be rebuilt, not silently accepted.
	if req := r.computeRequiredInputs(planned, r.Keys); !req["dephash"] {
		t.Fatalf("a dependency of a locally-executing job must be a required input, got %v", req)
	}

	// When the consumer is itself a local hit it never executes, so its inputs are
	// not consumed and not required.
	r.LocalHits["consumerhash"] = true
	if req := r.computeRequiredInputs(planned, r.Keys); req["dephash"] {
		t.Fatalf("a local-hit job's dependency must not be a required input, got %v", req)
	}
}

// A job whose key may move has no recorded key, so it counts as one that may
// execute even when its precomputed key is a local hit, and its dependency stays
// a required input even when that dependency's key may move too: the guard
// names dependencies through the whole negotiated key set.
func TestComputeRequiredInputs_KeepsMovableKeysRequired(t *testing.T) {
	t.Parallel()
	dep := cacheableJob("build", "/dep", "dep", "dep")
	consumer := cacheableJob("test", "/proj", "proj", "proj", dep.Key())
	planned := []*jobmodel.ScheduledJob{dep, consumer}
	negotiated := map[string]string{dep.Key(): "dephash", consumer.Key(): "consumerhash"}

	r := &RemoteCache{Keys: map[string]string{}, LocalHits: map[string]bool{"consumerhash": true}}
	if req := r.computeRequiredInputs(planned, negotiated); !req["dephash"] {
		t.Fatalf("the dependency of a job whose key may move must stay a required input, got %v", req)
	}
}
