package cachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const emitKey = "/proj:test~emit"

// artifactConsumer is a test job of test-proj whose manifest port folds the
// artifact of the emit job of proj, beside whatever plain dependencies it
// waits for.
func artifactConsumer(deps ...string) *jobmodel.ScheduledJob {
	job := cacheableJob("test", "/test-proj", "test-proj", "test-proj", append([]string{emitKey}, deps...)...)
	job.ArtifactInputs = []jobmodel.ArtifactInput{{
		Port:      "manifest",
		Output:    "manifest",
		Producers: []jobmodel.ArtifactProducer{{Key: emitKey, Project: "/proj"}},
	}}
	return job
}

func artifactKey(t *testing.T, job *jobmodel.ScheduledJob, hashes map[string]string) string {
	t.Helper()
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	key, err := ComputeJobCacheHash(ws, job, nil, nil, cache, hashes)
	if err != nil {
		t.Fatalf("ComputeJobCacheHash: %v", err)
	}
	return key
}

// The consumer folds the artifact's digest, not its producer's key: a
// producer whose key moved but wrote the same bytes keeps the consumer's key,
// and changed bytes move it. A plain edge to another job still folds that
// job's key.
func TestArtifactInput_TheKeyFoldsTheArtifactNotTheProducerKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	digest := ArtifactDigestKey(emitKey, "manifest")
	consumer := artifactConsumer("/proj:test~lint")
	before := artifactKey(t, consumer, map[string]string{emitKey: "key-1", digest: "artifact-a", "/proj:test~lint": "lint-1"})
	moved := artifactKey(t, consumer, map[string]string{emitKey: "key-2", digest: "artifact-a", "/proj:test~lint": "lint-1"})
	if before != moved {
		t.Errorf("a producer key that moved with the same artifact moved the consumer key: %s -> %s", before, moved)
	}
	changed := artifactKey(t, consumer, map[string]string{emitKey: "key-2", digest: "artifact-b", "/proj:test~lint": "lint-1"})
	if changed == before {
		t.Error("changed artifact bytes kept the consumer key")
	}
	plain := artifactKey(t, consumer, map[string]string{emitKey: "key-1", digest: "artifact-a", "/proj:test~lint": "lint-2"})
	if plain == before {
		t.Error("a plain dependency's key that moved kept the consumer key")
	}

	// Without the artifact input, the same edge folds the producer's key.
	byKey := cacheableJob("test", "/test-proj", "test-proj", "test-proj", emitKey)
	if artifactKey(t, byKey, map[string]string{emitKey: "key-1", digest: "artifact-a"}) ==
		artifactKey(t, byKey, map[string]string{emitKey: "key-2", digest: "artifact-a"}) {
		t.Error("an edge without an artifact input does not fold the producer's key")
	}
}

// An optional artifact input with no producer in the plan folds a fixed absent
// value: the key holds still, and differs from the key of a producer's
// artifact, even an empty one.
func TestArtifactInput_AnAbsentProducerFoldsAFixedValue(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	absent := cacheableJob("test", "/test-proj", "test-proj", "test-proj")
	absent.ArtifactInputs = []jobmodel.ArtifactInput{{Port: "manifest", Output: "manifest", Optional: true}}
	first := artifactKey(t, absent, map[string]string{"/proj:other": "x"})
	second := artifactKey(t, absent, nil)
	if first != second {
		t.Errorf("an absent producer's key moved: %s -> %s", first, second)
	}
	present := artifactKey(t, artifactConsumer(), map[string]string{ArtifactDigestKey(emitKey, "manifest"): ArtifactEmptyDigest})
	if present == first {
		t.Error("an absent producer and an empty artifact share a key")
	}
}

// A ^step binding folds the sorted list of each dependency's project and
// digest: the order the plan lists the producers in does not move the key,
// and moving a digest from one project to another does.
func TestArtifactInput_AnUpstreamListIsOrderIndependent(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	upstream := func(producers ...jobmodel.ArtifactProducer) *jobmodel.ScheduledJob {
		job := cacheableJob("test", "/app", "app", "proj", producers[0].Key, producers[1].Key)
		job.ArtifactInputs = []jobmodel.ArtifactInput{{Port: "outDir", Output: "outDir", Upstream: true, Producers: producers}}
		return job
	}
	a := jobmodel.ArtifactProducer{Key: "/a:test~tsc", Project: "/a"}
	b := jobmodel.ArtifactProducer{Key: "/b:test~tsc", Project: "/b"}
	hashes := map[string]string{
		ArtifactDigestKey(a.Key, "outDir"): "artifact-a", ArtifactDigestKey(b.Key, "outDir"): "artifact-b",
		a.Key: "key-a", b.Key: "key-b",
	}
	forward := artifactKey(t, upstream(a, b), hashes)
	if reversed := artifactKey(t, upstream(b, a), hashes); reversed != forward {
		t.Errorf("the producers' order moved the key: %s -> %s", forward, reversed)
	}
	swapped := map[string]string{
		ArtifactDigestKey(a.Key, "outDir"): "artifact-b", ArtifactDigestKey(b.Key, "outDir"): "artifact-a",
		a.Key: "key-a", b.Key: "key-b",
	}
	if artifactKey(t, upstream(a, b), swapped) == forward {
		t.Error("swapping two dependencies' artifacts kept the key")
	}
}

// Before its producer published the artifact's digest, a consumer folds the
// producer's key: PrecomputeKeys computes that key, which a producer edit
// moves, and the key the consumer computes once the digest is published is
// another one, so PlanKeyMovement reports the consumer moving, and every job
// whose key folds the consumer's.
func TestArtifactInput_BeforePublicationTheKeyFoldsTheProducerKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-consumer-of-an-artifact-is-keyed-when-it-starts")
	ws := makeExecutorTestWorkspace(t)
	producer := cacheableJob("emit", "/proj", "proj", "proj")
	producer.JobDef.Name = "test~emit"
	if producer.Key() != emitKey {
		t.Fatalf("producer key = %s", producer.Key())
	}
	consumer := artifactConsumer()
	report := cacheableJob("report", "/test-proj", "test-proj", "test-proj", consumer.Key())
	planned := []*jobmodel.ScheduledJob{producer, consumer, report}

	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	fallback := artifactKey(t, consumer, map[string]string{emitKey: keys[emitKey]})
	if keys[consumer.Key()] != fallback {
		t.Errorf("precomputed consumer key %s, want the key folding the producer's key %s", keys[consumer.Key()], fallback)
	}
	if moved := artifactKey(t, consumer, map[string]string{emitKey: "another-key"}); moved == fallback {
		t.Error("before publication, a producer key that moved kept the consumer key")
	}
	published := artifactKey(t, consumer, map[string]string{emitKey: keys[emitKey], ArtifactDigestKey(emitKey, "manifest"): "artifact-a"})
	if published == fallback {
		t.Error("the published artifact's key is the precomputed one")
	}

	movement := PlanKeyMovement(KeyInputs{Workspace: ws}, planned)
	if !movement.Moving[consumer.Key()] || !movement.Moving[report.Key()] || movement.Moving[producer.Key()] {
		t.Errorf("Moving = %v, want the consumer and its dependent, not the producer", movement.Moving)
	}
}

// The view a key reads its dependencies through copies the published
// artifact digests its artifact inputs read, and nothing else of the map.
func TestArtifactInput_TheViewCopiesThePublishedDigests(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-consumer-of-an-artifact-is-keyed-when-it-starts")
	consumer := artifactConsumer()
	published := map[string]string{
		emitKey:                                 "key-1",
		ArtifactDigestKey(emitKey, "manifest"):  "artifact-a",
		ArtifactDigestKey(emitKey, "unrelated"): "other",
	}
	view := KeylessResolver{
		ByKey:     JobsByKey([]*jobmodel.ScheduledJob{consumer}),
		Published: func(key string) (string, bool) { value, ok := published[key]; return value, ok },
		Identity:  func(string) (string, bool) { return "", false },
	}.View(consumer)
	if view[ArtifactDigestKey(emitKey, "manifest")] != "artifact-a" || view[emitKey] != "key-1" {
		t.Fatalf("view = %v, want the producer's key and its manifest digest", view)
	}
	if _, ok := view[ArtifactDigestKey(emitKey, "unrelated")]; ok {
		t.Fatalf("view = %v copies a digest no artifact input reads", view)
	}
}

// A directory's digest is its sorted relative paths, modes and content
// digests: the listing order does not move it, a mode or a byte does, and no
// file is the empty digest.
func TestArtifactDigest_ADirectoryIsItsSortedTree(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	a := ArtifactFile{Path: "a.js", Mode: 0o644, Digest: "sha256:aa"}
	b := ArtifactFile{Path: "lib/b.js", Mode: 0o644, Digest: "sha256:bb"}
	tree := ArtifactDigest(extension.OutputKindDirectory, []ArtifactFile{a, b})
	if got := ArtifactDigest(extension.OutputKindDirectory, []ArtifactFile{b, a}); got != tree {
		t.Errorf("the listing order moved the digest: %s -> %s", tree, got)
	}
	executable := b
	executable.Mode = 0o755
	if ArtifactDigest(extension.OutputKindDirectory, []ArtifactFile{a, executable}) == tree {
		t.Error("a mode change kept the digest")
	}
	edited := b
	edited.Digest = "sha256:cc"
	if ArtifactDigest(extension.OutputKindDirectory, []ArtifactFile{a, edited}) == tree {
		t.Error("a content change kept the digest")
	}
	if got := ArtifactDigest(extension.OutputKindDirectory, nil); got != ArtifactEmptyDigest {
		t.Errorf("an empty directory = %s, want %s", got, ArtifactEmptyDigest)
	}
	if ArtifactDigest(extension.OutputKindFile, []ArtifactFile{a}) == ArtifactDigest(extension.OutputKindFile, []ArtifactFile{edited}) {
		t.Error("two files of different bytes share a digest")
	}
}

// emitProducer is the emit job of proj whose manifest file a planned consumer
// binds as an artifact.
func emitProducer() *jobmodel.ScheduledJob {
	return &jobmodel.ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext", Tasks: map[string]extension.TaskDefinition{
			"emit": {
				Kind: "command", Command: "emit",
				Outputs: map[string]extension.TaskOutputPort{"manifest": {}},
				Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"manifest": {Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "out/manifest.json"},
				}},
			},
		}},
		JobDef:          &extension.JobDefinition{Name: "test~emit", ExtensionName: "@test/ext", Cache: true},
		Step:            &extension.PipelineStep{ID: "emit", Task: "emit"},
		ArtifactOutputs: []string{"manifest"},
	}
}

// ingestArtifactEntry stores at key an entry whose outputs hold files, by
// declared output id, at the root and path each spec names.
func ingestArtifactEntry(t *testing.T, cache *store.CacheManager, key string, specs []store.DeclaredEntryOutput, files map[string]string) *store.TaskEntry {
	t.Helper()
	staging := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := cache.IngestTaskEntry(staging, store.TaskEntrySpec{
		Key:     key,
		Result:  &store.EntryResult{Status: "success"},
		Outputs: specs,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", key, err)
	}
	return entry
}

// A key computed before the producers ran folds the digest a producer's local
// entry recorded, the one its hit publishes, so a recovery that looks the
// consumer's entry up at its precomputed key finds the entry the consumer's
// execution stored. Without a local producer entry the key folds the
// producer's key, and the lookup misses. The consumer stays moving: the
// producer may still execute.
func TestArtifactInput_APrecomputedKeyFoldsTheDigestALocalEntryRecorded(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-consumer-of-an-artifact-is-keyed-when-it-starts")
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	producer := emitProducer()
	consumer := artifactConsumer()
	planned := []*jobmodel.ScheduledJob{producer, consumer}
	precompute := func() map[string]string {
		t.Helper()
		keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
		if err != nil {
			t.Fatalf("PrecomputeKeys: %v", err)
		}
		return keys
	}

	cold := precompute()
	if len(cold) != 2 {
		t.Fatalf("keys = %v, want the two job keys alone", cold)
	}
	spec := store.DeclaredEntryOutput{ID: "manifest", Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "out/manifest.json"}
	producerEntry := ingestArtifactEntry(t, cache, cold[emitKey], []store.DeclaredEntryOutput{spec},
		map[string]string{"project/out/manifest.json": `{"files":1}`})
	digest, ok := ArtifactDigestFromEntry(producerEntry, "manifest")
	if !ok {
		t.Fatal("the producer entry records no manifest digest")
	}
	executed, err := ComputeJobCacheHash(ws, consumer, nil, nil, cache, map[string]string{
		emitKey: cold[emitKey], ArtifactDigestKey(emitKey, "manifest"): digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cold[consumer.Key()] == executed {
		t.Fatal("without a local producer entry the precomputed key is the executed one")
	}
	ingestArtifactEntry(t, cache, executed, nil, nil)

	warm := precompute()
	if len(warm) != 2 {
		t.Fatalf("keys = %v, want the two job keys alone", warm)
	}
	if warm[consumer.Key()] != executed {
		t.Fatalf("precomputed consumer key %s, want the key its execution stored %s", warm[consumer.Key()], executed)
	}
	if present, err := CacheEntryPresent(cache, consumer, warm[consumer.Key()]); err != nil || !present {
		t.Fatalf("a recovery at the precomputed key misses the consumer's entry: %v %v", present, err)
	}
	if movement := PlanKeyMovement(KeyInputs{Workspace: ws}, planned); !movement.Moving[consumer.Key()] {
		t.Error("the consumer is no longer reported moving")
	}
}

// The build stamp is part of no artifact digest: two entries of a project
// .gen directory that differ only in the stamp record one digest, and a file
// output at the stamp's path holds nothing.
func TestArtifactDigest_TheBuildStampIsNoPartOfIt(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	gen := store.DeclaredEntryOutput{ID: "gen", Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen"}
	digestOf := func(key, stamp string) string {
		t.Helper()
		entry := ingestArtifactEntry(t, cache, key, []store.DeclaredEntryOutput{gen}, map[string]string{
			"project/.gen/client.ts":    "export const a = 1\n",
			"project/.gen/version.json": stamp,
		})
		digest, ok := ArtifactDigestFromEntry(entry, "gen")
		if !ok {
			t.Fatalf("entry %s records no gen digest", key)
		}
		return digest
	}
	first, second := digestOf("run-1", `{"buildTime":"1"}`), digestOf("run-2", `{"buildTime":"2"}`)
	if first != second {
		t.Fatalf("two stamps moved the digest: %s -> %s", first, second)
	}
	want := ArtifactDigest(extension.OutputKindDirectory, []ArtifactFile{{Path: "client.ts", Mode: 0o644, Digest: "sha256:" + sha256Hex("export const a = 1\n")}})
	if first != want {
		t.Fatalf("digest %s, want the digest of client.ts alone %s", first, want)
	}
	if !IsArtifactVersionStamp(extension.OutputRootProject, extension.OutputKindFile, ".gen/version.json", "") ||
		IsArtifactVersionStamp(extension.OutputRootWorkspace, extension.OutputKindFile, ".gen/version.json", "") ||
		IsArtifactVersionStamp(extension.OutputRootProject, extension.OutputKindDirectory, "out", "version.json") {
		t.Fatal("IsArtifactVersionStamp names a file other than the project's build stamp")
	}
}

func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
