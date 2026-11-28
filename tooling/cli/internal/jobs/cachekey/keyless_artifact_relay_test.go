package cachekey

import (
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// relayPlan is a keyless setup job of proj that binds the manifest of the
// emit job of proj as an artifact input, and a cacheable run of proj that
// waits for it and consumes its invocation-scoped resource: the shape of the
// Go test-env step between describe and test-exec.
type relayPlan struct {
	emit, env, run *jobmodel.ScheduledJob
}

func newRelayPlan(outputs ...string) relayPlan {
	emit := emitProducer()
	env := cacheableJob("test~env", "/proj", "proj", "proj", emitKey)
	env.JobDef.Cache = false
	for _, output := range outputs {
		env.ArtifactInputs = append(env.ArtifactInputs, jobmodel.ArtifactInput{
			Port: output, Output: output, Producers: []jobmodel.ArtifactProducer{{Key: emitKey, Project: "/proj"}},
		})
	}
	run := cacheableJob("test~run", "/proj", "proj", "proj", env.Key())
	run.InvocationProducer = env
	return relayPlan{emit: emit, env: env, run: run}
}

func (p relayPlan) jobs() []*jobmodel.ScheduledJob {
	return []*jobmodel.ScheduledJob{p.emit, p.env, p.run}
}

// resolver answers the dependency keys of the plan from published.
func (p relayPlan) resolver(published map[string]string) KeylessResolver {
	return KeylessResolver{
		ByKey:     JobsByKey(p.jobs()),
		Published: func(key string) (string, bool) { value, ok := published[key]; return value, ok },
		Identity:  func(string) (string, bool) { return "", false },
	}
}

// key is the key job computes when it starts with published in the run's
// hash map.
func (p relayPlan) key(t *testing.T, job *jobmodel.ScheduledJob, published map[string]string) string {
	t.Helper()
	return artifactKey(t, job, p.resolver(published).View(job))
}

// through is what the run's key reads of the keyless setup job: the digest
// of what reaches it, or "" when nothing does.
func (p relayPlan) through(published map[string]string) string {
	return p.resolver(published).View(p.run)[p.env.Key()]
}

// producerOnly is a cacheable job of proj that consumes the setup job's
// resource without waiting for it, so its key reads the setup job through
// its action alone.
func (p relayPlan) producerOnly() *jobmodel.ScheduledJob {
	job := cacheableJob("test~solo", "/proj", "proj", "proj")
	job.InvocationProducer = p.env
	return job
}

// A keyless job that binds its dependency's output as an artifact relays the
// output's digest on that edge, not the dependency's key: a dependency whose
// key moved but wrote the same bytes keeps the key of the job behind the
// keyless one, through the digest that reaches it and through the action of
// the invocation producer it folds, and changed bytes move it.
func TestKeylessRelay_AnArtifactBindingRelaysTheDigestNotTheProducerKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	plan := newRelayPlan("manifest")
	digest := ArtifactDigestKey(emitKey, "manifest")
	first := map[string]string{emitKey: "key-1", digest: "artifact-a"}
	moved := map[string]string{emitKey: "key-2", digest: "artifact-a"}
	changed := map[string]string{emitKey: "key-2", digest: "artifact-b"}

	if plan.through(first) == "" || plan.through(first) != plan.through(moved) {
		t.Errorf("what reaches the keyless job moved with its dependency's key: %q -> %q", plan.through(first), plan.through(moved))
	}
	if plan.through(changed) == plan.through(first) {
		t.Error("changed artifact bytes kept what reaches the keyless job")
	}
	solo := plan.producerOnly()
	if plan.key(t, solo, first) != plan.key(t, solo, moved) {
		t.Error("the invocation producer's action moved with its dependency's key")
	}
	if plan.key(t, solo, changed) == plan.key(t, solo, first) {
		t.Error("changed artifact bytes kept the invocation producer's action")
	}
	if plan.key(t, plan.run, first) != plan.key(t, plan.run, moved) {
		t.Error("the key behind the keyless job moved with its dependency's key")
	}
	if plan.key(t, plan.run, changed) == plan.key(t, plan.run, first) {
		t.Error("changed artifact bytes kept the key behind the keyless job")
	}

	view := plan.resolver(first).View(plan.run)
	if view[digest] != "artifact-a" {
		t.Errorf("view = %v, want the digest the invocation producer's artifact input reads", view)
	}
}

// The relay fails closed. An edge that binds no artifact relays its
// dependency's key, and so does an edge whose digest is not published, or
// only one of whose two bound digests is: the key behind the keyless job then
// moves with its dependency's key and never folds nothing for it.
func TestKeylessRelay_AnEdgeWithoutAPublishedDigestRelaysTheProducerKey(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-reads-a-dependency-that-publishes-none", "a-keyless-dependency-moves-the-key-of-its-dependent")
	manifest := ArtifactDigestKey(emitKey, "manifest")
	for _, tc := range []struct {
		name    string
		plan    relayPlan
		digests map[string]string
	}{
		{"no binding", newRelayPlan(), map[string]string{manifest: "artifact-a"}},
		{"unpublished digest", newRelayPlan("manifest"), nil},
		{"one of two digests published", newRelayPlan("manifest", "report"), map[string]string{manifest: "artifact-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			published := func(key string) map[string]string {
				values := map[string]string{emitKey: key}
				for entry, digest := range tc.digests {
					values[entry] = digest
				}
				return values
			}
			first, moved := published("key-1"), published("key-2")
			if plan := tc.plan; plan.through(first) == "" || plan.through(first) == plan.through(moved) {
				t.Errorf("what reaches the keyless job did not move with its dependency's key: %q -> %q", plan.through(first), plan.through(moved))
			}
			if solo := tc.plan.producerOnly(); tc.plan.key(t, solo, first) == tc.plan.key(t, solo, moved) {
				t.Error("the invocation producer's action did not move with its dependency's key")
			}
			if tc.plan.key(t, tc.plan.run, first) == tc.plan.key(t, tc.plan.run, moved) {
				t.Error("the key behind the keyless job did not move with its dependency's key")
			}
		})
	}
}

// The keys computed before anything runs read the relay as execution does.
// PrecomputeKeys folds the digest the dependency's local entry recorded, so
// the precomputed key behind the keyless job is the one it computes when it
// starts; without a local entry, and under RevisionOnly, which reads
// no store, it folds the dependency's key on that edge. The job stays moving.
func TestKeylessRelay_PrecomputedKeysReadTheRelayAsExecutionDoes(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "a-consumer-of-an-artifact-is-keyed-when-it-starts")
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	plan := newRelayPlan("manifest")
	planned := plan.jobs()
	precompute := func(revision bool) map[string]string {
		t.Helper()
		var keys map[string]string
		var err error
		if revision {
			keys, err = PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{}, RevisionOnly())
		} else {
			keys, err = PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
		}
		if err != nil {
			t.Fatalf("precompute: %v", err)
		}
		if _, ok := keys[plan.env.Key()]; ok || len(keys) != 2 {
			t.Fatalf("keys = %v, want the keys of the two cacheable jobs alone", keys)
		}
		return keys
	}

	cold := precompute(false)
	byKey := plan.key(t, plan.run, map[string]string{emitKey: cold[emitKey]})
	if cold[plan.run.Key()] != byKey {
		t.Fatalf("precomputed key %s without a local entry, want the key folding the dependency's key %s", cold[plan.run.Key()], byKey)
	}
	spec := store.DeclaredEntryOutput{ID: "manifest", Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: "out/manifest.json"}
	entry := ingestArtifactEntry(t, cache, cold[emitKey], []store.DeclaredEntryOutput{spec},
		map[string]string{"project/out/manifest.json": `{"files":1}`})
	digest, ok := ArtifactDigestFromEntry(entry, "manifest")
	if !ok {
		t.Fatal("the dependency's entry records no manifest digest")
	}
	executed := plan.key(t, plan.run, map[string]string{emitKey: cold[emitKey], ArtifactDigestKey(emitKey, "manifest"): digest})
	if executed == byKey {
		t.Fatal("the key folding the published digest is the one folding the dependency's key")
	}
	if warm := precompute(false); warm[plan.run.Key()] != executed {
		t.Errorf("precomputed key %s with a local entry, want the key it computes when it starts %s", warm[plan.run.Key()], executed)
	}
	if revision := precompute(true); revision[plan.run.Key()] != byKey {
		t.Errorf("revision key %s, want the key folding the dependency's key %s", revision[plan.run.Key()], byKey)
	}
	if movement := PlanKeyMovement(KeyInputs{Workspace: ws}, planned); !movement.Moving[plan.run.Key()] {
		t.Error("the job behind the keyless one is not reported moving")
	}
}
