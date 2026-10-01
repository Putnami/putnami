package jobs

import (
	"path/filepath"
	"reflect"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// resourceJob builds a ScheduledJob that declares write/read resources.
func resourceJob(projID, name string, deps []string, writes, reads []extension.ResourceRef) *ScheduledJob {
	pathName := projID
	if len(pathName) > 0 && pathName[0] == '/' {
		pathName = pathName[1:]
	}
	return &ScheduledJob{
		Project:   &workspace.Project{ID: projID, Name: pathName, Path: pathName},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef: &extension.JobDefinition{
			Name:          name,
			ExtensionName: "@test/ext",
			Writes:        writes,
			Reads:         reads,
		},
		DependsOn: deps,
	}
}

// proj builds project-scoped resource refs from ids.
func proj(ids ...string) []extension.ResourceRef {
	refs := make([]extension.ResourceRef, len(ids))
	for i, id := range ids {
		refs[i] = extension.ResourceRef{ID: id}
	}
	return refs
}

// ws builds workspace-scoped resource refs from ids.
func wsRes(ids ...string) []extension.ResourceRef {
	refs := make([]extension.ResourceRef, len(ids))
	for i, id := range ids {
		refs[i] = extension.ResourceRef{ID: id, Scope: extension.ResourceScopeWorkspace}
	}
	return refs
}

func TestSerializeWriteResources_ConflictingWritersSerialized(t *testing.T) {
	t.Parallel()
	build := resourceJob("/p", "build~generate", nil, proj("gen"), nil)
	pkg := resourceJob("/p", "package~generate", nil, proj("gen"), nil)

	serializeWriteResources([]*ScheduledJob{build, pkg})

	// Lower topological position (build < package by key) runs first.
	if len(build.SerializeAfter) != 0 {
		t.Errorf("build~generate should have no serialize edges, got %v", build.SerializeAfter)
	}
	want := []string{"/p:build~generate"}
	if !reflect.DeepEqual(pkg.SerializeAfter, want) {
		t.Errorf("package~generate serializeAfter = %v, want %v", pkg.SerializeAfter, want)
	}
	// Write serialization must never become a functional dependency.
	if len(pkg.DependsOn) != 0 {
		t.Errorf("write serialization leaked into DependsOn: %v", pkg.DependsOn)
	}
}

func TestSerializeWriteResources_IndependentWritersNotSerialized(t *testing.T) {
	t.Parallel()
	// Different resources in the same project do not conflict.
	gen := resourceJob("/p", "build~generate", nil, proj("gen"), nil)
	dist := resourceJob("/p", "build~bundle", nil, proj("dist"), nil)
	// Same resource id but different projects: project scope keeps them apart.
	other := resourceJob("/q", "build~generate", nil, proj("gen"), nil)

	serializeWriteResources([]*ScheduledJob{gen, dist, other})

	for _, job := range []*ScheduledJob{gen, dist, other} {
		if len(job.SerializeAfter) != 0 {
			t.Errorf("%s should not be serialized, got %v", job.Key(), job.SerializeAfter)
		}
	}
}

func TestSerializeWriteResources_ReadersParallelWriterExclusive(t *testing.T) {
	t.Parallel()
	// build~generate writes the tree; its readers consume it (functional deps).
	writer := resourceJob("/p", "build~generate", nil, proj("gen"), nil)
	r1 := resourceJob("/p", "build~compile", []string{writer.Key()}, nil, proj("gen"))
	r2 := resourceJob("/p", "build~types", []string{writer.Key()}, nil, proj("gen"))
	// A second, independent writer in another command rewrites the same tree.
	writer2 := resourceJob("/p", "test~generate", nil, proj("gen"), nil)

	serializeWriteResources([]*ScheduledJob{writer, r1, r2, writer2})

	// Readers derive from their writer functionally, so no serialize edge is
	// added — and they share none between themselves, so they stay parallel.
	for _, r := range []*ScheduledJob{r1, r2} {
		if len(r.SerializeAfter) != 0 {
			t.Errorf("%s should derive from its writer functionally, got serializeAfter %v", r.Key(), r.SerializeAfter)
		}
	}
	// The independent writer waits for the entire build block — the writer and
	// both of its readers — before overwriting the shared tree.
	for _, want := range []string{writer.Key(), r1.Key(), r2.Key()} {
		if !containsStr(writer2.SerializeAfter, want) {
			t.Errorf("test~generate serializeAfter = %v, want to contain %s", writer2.SerializeAfter, want)
		}
	}
	if len(writer.SerializeAfter) != 0 {
		t.Errorf("first writer should have no serialize edges, got %v", writer.SerializeAfter)
	}
}

func TestSerializeWriteResources_ReadOnlyTasksNotSerialized(t *testing.T) {
	t.Parallel()
	// No writer of "gen" exists, so readers are free to run concurrently — this
	// is how a no-fix lint check proves it does not write project sources.
	check1 := resourceJob("/p", "lint~check", nil, nil, proj("gen"))
	check2 := resourceJob("/p", "lint~staticcheck", nil, nil, proj("gen"))
	// A job that declares nothing is never a candidate for serialization.
	bare := resourceJob("/p", "lint~format-check", nil, nil, nil)
	// A writer of an unrelated resource does not pull the readers in.
	unrelated := resourceJob("/p", "build~bundle", nil, proj("dist"), nil)

	serializeWriteResources([]*ScheduledJob{check1, check2, bare, unrelated})

	for _, job := range []*ScheduledJob{check1, check2, bare, unrelated} {
		if len(job.SerializeAfter) != 0 {
			t.Errorf("%s should not be serialized, got %v", job.Key(), job.SerializeAfter)
		}
	}
}

func TestSerializeWriteResources_SourceFixWriterSerializesReaders(t *testing.T) {
	t.Parallel()
	lintFix := resourceJob("/p", "lint~format", nil, proj("sources"), nil)
	build := resourceJob("/p", "build~compile", nil, nil, proj("sources"))
	test := resourceJob("/p", "test~test", nil, nil, proj("sources"))

	serializeWriteResources([]*ScheduledJob{lintFix, build, test})

	for _, want := range []string{build.Key(), test.Key()} {
		if !containsStr(lintFix.SerializeAfter, want) {
			t.Errorf("lint fix should serialize after source reader %s, got %v", want, lintFix.SerializeAfter)
		}
	}
	if len(build.SerializeAfter) != 0 || len(test.SerializeAfter) != 0 {
		t.Errorf("source readers should stay parallel with each other: build=%v test=%v", build.SerializeAfter, test.SerializeAfter)
	}
}

func TestSerializeWriteResources_WorkspaceScopeConflictsAcrossProjects(t *testing.T) {
	t.Parallel()
	a := resourceJob("/a", "publish~push", nil, wsRes("out"), nil)
	b := resourceJob("/b", "publish~push", nil, wsRes("out"), nil)

	serializeWriteResources([]*ScheduledJob{a, b})

	// /a sorts before /b, so /b serializes after /a.
	if len(a.SerializeAfter) != 0 {
		t.Errorf("/a:publish~push should run first, got %v", a.SerializeAfter)
	}
	if !reflect.DeepEqual(b.SerializeAfter, []string{a.Key()}) {
		t.Errorf("/b:publish~push serializeAfter = %v, want [%s]", b.SerializeAfter, a.Key())
	}

	// The same ids at project scope must NOT conflict across projects.
	c := resourceJob("/a", "publish~push", nil, proj("out"), nil)
	d := resourceJob("/b", "publish~push", nil, proj("out"), nil)
	serializeWriteResources([]*ScheduledJob{c, d})
	if len(c.SerializeAfter) != 0 || len(d.SerializeAfter) != 0 {
		t.Errorf("project-scoped resources must not conflict across projects: c=%v d=%v", c.SerializeAfter, d.SerializeAfter)
	}
}

func TestSerializeWriteResources_FollowsFunctionalOrderNoCycle(t *testing.T) {
	t.Parallel()
	// first must run before second functionally; both write the same resource.
	first := resourceJob("/p", "build~generate", nil, proj("gen"), nil)
	second := resourceJob("/p", "build~regen", []string{first.Key()}, proj("gen"), nil)

	serializeWriteResources([]*ScheduledJob{second, first})

	// The serialize edge follows the functional order and is pruned as redundant.
	if len(second.SerializeAfter) != 0 {
		t.Errorf("redundant serialize edge should be pruned, got %v", second.SerializeAfter)
	}
	if len(first.SerializeAfter) != 0 {
		t.Errorf("first must not serialize after its own dependent, got %v", first.SerializeAfter)
	}
	if err := validatePlanDAG([]*ScheduledJob{first, second}); err != nil {
		t.Fatalf("plan with serialize edges must stay schedulable: %v", err)
	}
}

func TestSerializeWriteResources_ReadWriteSameJobIsWriter(t *testing.T) {
	t.Parallel()
	// describe both reads and writes the gen tree → treated as an exclusive writer.
	describe := resourceJob("/p", "build~describe", nil, proj("gen"), proj("gen"))
	generate := resourceJob("/p", "build~generate", nil, proj("gen"), nil)

	serializeWriteResources([]*ScheduledJob{describe, generate})

	// build~describe sorts before build~generate; the second writer waits.
	if !reflect.DeepEqual(generate.SerializeAfter, []string{describe.Key()}) {
		t.Errorf("generate serializeAfter = %v, want [%s]", generate.SerializeAfter, describe.Key())
	}
	if len(describe.SerializeAfter) != 0 {
		t.Errorf("first writer should have no edges, got %v", describe.SerializeAfter)
	}
}

// TestSerializeWriteResources_CacheKeyUnaffected proves write serialization is
// orthogonal to caching: a serialize predecessor does not enter the cache key,
// so cache restore/store behavior is identical with or without it.
func TestSerializeWriteResources_CacheKeyUnaffected(t *testing.T) {
	t.Parallel()
	wsp := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(wsp.Root, ".putnami", "store")))

	base := &ScheduledJob{
		Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: "build~generate", ExtensionName: "@test/ext", Cache: true},
	}
	withEdge := &ScheduledJob{
		Project:        base.Project,
		Extension:      base.Extension,
		JobDef:         base.JobDef,
		SerializeAfter: []string{"/proj:other~writer"},
	}

	keyBase, err := computeJobCacheHash(wsp, base, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	keyEdge, err := computeJobCacheHash(wsp, withEdge, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("hash with serialize edge: %v", err)
	}
	if keyBase != keyEdge {
		t.Errorf("serialize edge changed cache key: %s != %s", keyBase, keyEdge)
	}
}

// TestSerializeWriteResources_PrecomputeIgnoresSerializeEdges verifies the
// plan-time cache key pass treats serialized-but-independent writers as
// independent — the successor's key does not fold in the predecessor's hash.
func TestSerializeWriteResources_PrecomputeIgnoresSerializeEdges(t *testing.T) {
	t.Parallel()
	wsp := makeExecutorTestWorkspace(t)

	a := cacheableJob("build~generate", "/proj", "proj", "proj")
	a.JobDef.Writes = proj("gen")
	b := cacheableJob("package~generate", "/proj", "proj", "proj")
	b.JobDef.Writes = proj("gen")

	plan := []*ScheduledJob{a, b}
	serializeWriteResources(plan)
	if len(b.SerializeAfter) == 0 {
		t.Fatal("expected package~generate to serialize after build~generate")
	}
	if len(b.DependsOn) != 0 {
		t.Fatalf("serialize edge must not appear in DependsOn: %v", b.DependsOn)
	}

	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(wsp.Root, ".putnami", "store")))
	keys, err := PrecomputeKeys(wsp, plan, nil, nil, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}

	// b has no functional deps, so its key equals the standalone execution key.
	standalone := store.NewCacheManager(store.NewLocalStore(filepath.Join(wsp.Root, ".putnami", "store")))
	wantB, err := computeJobCacheHash(wsp, b, nil, nil, standalone, nil)
	if err != nil {
		t.Fatalf("compute key b: %v", err)
	}
	if keys[b.Key()] != wantB {
		t.Errorf("serialized successor key = %s, want standalone %s", keys[b.Key()], wantB)
	}
}
