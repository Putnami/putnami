package cachekey

import (
	"os"
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/store"
)

// A cacheable job that waits for a job whose cache is off, such as a moved
// test waiting for a build its tool runs, folds the key that dependency would
// have: an edit to the dependency's sources moves the dependent's key, the
// keyless job itself still has no key, and a plan whose keyless jobs no
// cacheable job folds computes no identity for them.
func TestPrecomputeKeys_AKeylessDependencyMovesTheKeyOfItsDependent(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-reads-a-dependency-that-publishes-none", "a-keyless-dependency-moves-the-key-of-its-dependent")
	ws := makeExecutorTestWorkspace(t)
	graph := cacheableJob("graph", "/proj", "proj", "proj")
	graph.JobDef.Cache = false
	build := cacheableJob("build", "/proj", "proj", "proj", "/proj:graph")
	build.JobDef.Cache = false
	test := cacheableJob("test", "/test-proj", "test-proj", "test-proj", "/proj:build")
	planned := []*jobmodel.ScheduledJob{graph, build, test}

	if got := KeylessIdentities(planned); !got["/proj:build"] || !got["/proj:graph"] || got["/test-proj:test"] {
		t.Fatalf("KeylessIdentities = %v, want the keyless build and its keyless graph step", got)
	}
	if got := KeylessIdentities([]*jobmodel.ScheduledJob{graph, build}); len(got) != 0 {
		t.Fatalf("KeylessIdentities of a plan no cacheable job folds = %v, want none", got)
	}

	precompute := func() map[string]string {
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
		if err != nil {
			t.Fatalf("PrecomputeKeys: %v", err)
		}
		return keys
	}
	before := precompute()
	if _, ok := before["/proj:build"]; ok {
		t.Fatalf("the keyless build has a key: %v", before)
	}
	if err := os.WriteFile(filepath.Join(ws.Root, "proj", "src.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	after := precompute()
	if before["/test-proj:test"] == "" || before["/test-proj:test"] == after["/test-proj:test"] {
		t.Errorf("the dependent's key %q did not move when its keyless dependency's source changed (%q)", before["/test-proj:test"], after["/test-proj:test"])
	}

	// Execution folds the same identity: the build's would-be key, itself
	// folding its graph step's.
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	graphIdentity, err := ComputeJobCacheHash(ws, graph, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("graph identity: %v", err)
	}
	buildIdentity, err := ComputeJobCacheHash(ws, build, nil, nil, cache, map[string]string{"/proj:graph": graphIdentity})
	if err != nil {
		t.Fatalf("build identity: %v", err)
	}
	want, err := ComputeJobCacheHash(ws, test, nil, nil, cache, map[string]string{"/proj:build": buildIdentity})
	if err != nil {
		t.Fatalf("test key: %v", err)
	}
	if after["/test-proj:test"] != want {
		t.Errorf("precomputed key %s, want the execution key %s", after["/test-proj:test"], want)
	}
}
