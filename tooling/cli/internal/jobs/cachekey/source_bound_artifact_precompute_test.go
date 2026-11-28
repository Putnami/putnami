package cachekey

import (
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// A source-bound producer's entry lives at its key bound to its project's
// source binding. A key computed before it ran reads that entry where the
// producer's own lookup reads it, at the binding entryKey resolves, and folds
// the digest it recorded: the consumer's precomputed key is the key its
// execution computes after the producer's hit. An entry at another binding,
// no binding, or no resolver leaves the consumer folding the producer's key.
func TestArtifactInput_APrecomputedKeyReadsASourceBoundProducerAtItsBinding(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-consumer-of-an-artifact-is-keyed-when-it-starts")
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	producer := emitProducer()
	producer.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{SourceBound: true}
	if !TaskIsSourceBound(producer) {
		t.Fatal("the producer is not source-bound")
	}
	consumer := artifactConsumer()
	planned := []*jobmodel.ScheduledJob{producer, consumer}
	at := func(binding string, ok bool) EntryKeyResolver {
		return func(job *jobmodel.ScheduledJob, key string) (string, bool) {
			if !TaskIsSourceBound(job) {
				return key, true
			}
			if !ok {
				return "", false
			}
			return SourceBoundEntryKey(key, binding), true
		}
	}
	precompute := func(entryKey EntryKeyResolver) map[string]string {
		t.Helper()
		keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{}, AtEntryKeys(entryKey))
		if err != nil {
			t.Fatalf("PrecomputeKeys: %v", err)
		}
		return keys
	}

	const binding = "source-v1:sha256:current"
	cold := precompute(at(binding, true))
	spec := store.DeclaredEntryOutput{ID: "manifest", Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "out/manifest.json"}
	entry := ingestArtifactEntry(t, cache, SourceBoundEntryKey(cold[emitKey], binding), []store.DeclaredEntryOutput{spec},
		map[string]string{"project/out/manifest.json": `{"files":1}`})
	digest, ok := ArtifactDigestFromEntry(entry, "manifest")
	if !ok {
		t.Fatal("the producer entry records no manifest digest")
	}
	executed, err := ComputeJobCacheHash(ws, consumer, nil, nil, cache, map[string]string{
		emitKey: cold[emitKey], ArtifactDigestKey(emitKey, "manifest"): digest,
	})
	if err != nil {
		t.Fatal(err)
	}

	if warm := precompute(at(binding, true)); warm[consumer.Key()] != executed {
		t.Fatalf("precomputed consumer key %s, want the key its execution computes after the producer's hit %s", warm[consumer.Key()], executed)
	}
	fallback, err := ComputeJobCacheHash(ws, consumer, nil, nil, cache, map[string]string{emitKey: cold[emitKey]})
	if err != nil {
		t.Fatal(err)
	}
	for name, entryKey := range map[string]EntryKeyResolver{
		"another binding": at("source-v1:sha256:other", true),
		"no binding":      at("", false),
		"no resolver":     nil,
	} {
		if got := precompute(entryKey); got[consumer.Key()] != fallback {
			t.Errorf("%s: precomputed consumer key %s, want the key folding the producer's key %s", name, got[consumer.Key()], fallback)
		}
	}
	keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
	if err != nil || keys[consumer.Key()] != fallback {
		t.Fatalf("PrecomputeKeys consumer key %s (%v), want the key folding the producer's key %s", keys[consumer.Key()], err, fallback)
	}
}
