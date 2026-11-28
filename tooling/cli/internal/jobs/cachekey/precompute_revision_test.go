package cachekey

import (
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// A revision key reads no store: with the producer's entry local, the
// consumer's revision key is still the one that folds the producer's key, the
// key any machine computes from the revision, while PrecomputeKeys folds the
// digest the entry recorded. The other keys are the same in both.
func TestPrecomputeKeysRevisionOnly_ReadsNoLocalEntry(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-consumer-of-an-artifact-is-keyed-when-it-starts")
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	producer := emitProducer()
	consumer := artifactConsumer()
	planned := []*jobmodel.ScheduledJob{producer, consumer}

	before, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{}, RevisionOnly())
	if err != nil {
		t.Fatalf("PrecomputeKeys(RevisionOnly): %v", err)
	}
	spec := store.DeclaredEntryOutput{ID: "manifest", Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "out/manifest.json"}
	ingestArtifactEntry(t, cache, before[emitKey], []store.DeclaredEntryOutput{spec},
		map[string]string{"project/out/manifest.json": `{"files":1}`})

	revision, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{}, RevisionOnly())
	if err != nil {
		t.Fatalf("PrecomputeKeys(RevisionOnly): %v", err)
	}
	local, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	if revision[consumer.Key()] != before[consumer.Key()] {
		t.Errorf("a local producer entry moved the consumer's revision key: %s -> %s", before[consumer.Key()], revision[consumer.Key()])
	}
	if local[consumer.Key()] == revision[consumer.Key()] {
		t.Error("PrecomputeKeys ignored the producer's local entry")
	}
	if revision[emitKey] != local[emitKey] || len(revision) != len(local) {
		t.Errorf("revision keys %v and local keys %v differ beyond the consumer", revision, local)
	}
}
