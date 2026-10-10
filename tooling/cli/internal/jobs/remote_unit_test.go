package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/tooling/cli/internal/store"
)

// newUploadTestCache builds a minimal RemoteCache for exercising the pure
// upload-eligibility logic (prepareUpload), which needs only the stats sink — no
// provider session or network.
func newUploadTestCache() *RemoteCache {
	return &RemoteCache{stats: &CacheStats{}}
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
	if manifestBytes(in.Manifest) == 0 {
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

func TestPrepareUpload_SkipsSideEffectingTask(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := cacheableJob("publish", "/proj", "proj", "proj")
	hash := "ab" + strings.Repeat("0", cache.KeyLength-2)

	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "publish", Project: "proj", DurationMs: 1500}
	if err := cm.Save(hash, &store.EntryResult{Status: "success"}, meta, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := newUploadTestCache()
	if _, ok := r.prepareUpload(hash, job, cm); ok {
		t.Fatalf("a side-effecting task must never be eligible for remote upload")
	}
}

func TestPrepareUpload_IncludesActionEventsOnlyWhenAdvertised(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := cacheableJob("lint", "/proj", "proj", "proj")
	hash := "ce" + strings.Repeat("1", cache.KeyLength-2)
	events := []cache.ActionEvent{
		{Version: 1, Type: EventTypeMetric, Data: map[string]any{"name": "lint-warnings", "value": float64(2), "unit": "count"}},
		{Version: 1, Type: EventTypeSummary, Data: map[string]any{"message": "2 warnings"}},
	}
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "lint", Project: "proj", DurationMs: 1500}
	if err := cm.Save(hash, &store.EntryResult{Status: "success", Events: events}, meta, ""); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The provider has not advertised event support: events are dropped.
	withoutEvents := newUploadTestCache()
	in, ok := withoutEvents.prepareUpload(hash, job, cm)
	if !ok {
		t.Fatalf("expected an eligible upload")
	}
	if len(in.Result.Events) != 0 {
		t.Fatalf("events must be omitted when the provider does not advertise support, got %d", len(in.Result.Events))
	}

	// With support advertised, events ride along.
	withEvents := newUploadTestCache()
	withEvents.actionEvents = true
	in, ok = withEvents.prepareUpload(hash, job, cm)
	if !ok {
		t.Fatalf("expected an eligible upload")
	}
	if len(in.Result.Events) != len(events) {
		t.Fatalf("events must be included when advertised, got %d want %d", len(in.Result.Events), len(events))
	}
}

func TestComputeRequiredInputs_ExcludesLocalHitDependencies(t *testing.T) {
	t.Parallel()
	dep := cacheableJob("build", "/dep", "dep", "dep")
	consumer := cacheableJob("test", "/proj", "proj", "proj", dep.Key())
	planned := []*ScheduledJob{dep, consumer}

	r := &RemoteCache{
		keys: map[string]string{
			dep.Key():      "dephash",
			consumer.Key(): "consumerhash",
		},
		localHits: map[string]bool{},
	}

	// The consumer executes locally, so its dependency is a required input: a
	// files-less hit for it must be rebuilt, not silently accepted.
	if req := r.computeRequiredInputs(planned); !req["dephash"] {
		t.Fatalf("a dependency of a locally-executing job must be a required input, got %v", req)
	}

	// When the consumer is itself a local hit it never executes, so its inputs are
	// not consumed and not required.
	r.localHits["consumerhash"] = true
	if req := r.computeRequiredInputs(planned); req["dephash"] {
		t.Fatalf("a local-hit job's dependency must not be a required input, got %v", req)
	}
}
