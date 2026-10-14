package cachekey

import (
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestSerializeWriteResources_CacheKeyUnaffected proves write serialization is
// orthogonal to caching: a serialize predecessor does not enter the cache key,
// so cache restore/store behavior is identical with or without it.
func TestSerializeWriteResources_CacheKeyUnaffected(t *testing.T) {
	t.Parallel()
	wsp := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(wsp.Root, ".putnami", "store")))

	base := &jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: "build~generate", ExtensionName: "@test/ext", Cache: true},
	}
	withEdge := &jobmodel.ScheduledJob{
		Project:        base.Project,
		Extension:      base.Extension,
		JobDef:         base.JobDef,
		SerializeAfter: []string{"/proj:other~writer"},
	}

	keyBase, err := ComputeJobCacheHash(wsp, base, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("hash base: %v", err)
	}
	keyEdge, err := ComputeJobCacheHash(wsp, withEdge, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("hash with serialize edge: %v", err)
	}
	if keyBase != keyEdge {
		t.Errorf("serialize edge changed cache key: %s != %s", keyBase, keyEdge)
	}
}
