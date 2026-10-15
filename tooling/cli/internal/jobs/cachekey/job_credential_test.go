package cachekey

import (
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A hosted run declares itself offline in its own environment
// (runcredential.Capture), and the key of a task that declares the offline
// signal as an input reads it there: the task keys apart from the same task
// on a run that may download. A task that does not declare it keys the same.
func TestOfflineSignalKeysADeclaringTaskApart(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	newJob := func(env ...string) *jobmodel.ScheduledJob {
		job := &jobmodel.ScheduledJob{
			Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
			Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name: "build~tidy", CommandName: "build", ExtensionName: "@test/ext", Cache: true,
				TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Env: env}},
			},
		}
		return declareCacheTestTask(job, "tidy")
	}
	declaring, other := newJob(extensionproto.OfflineDependenciesEnv), newJob()
	key := func(job *jobmodel.ScheduledJob) string {
		t.Helper()
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		got, err := ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return got
	}

	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	localDeclaring, localOther := key(declaring), key(other)
	// What runcredential.Capture leaves in the environment of a hosted run.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if key(declaring) == localDeclaring {
		t.Error("the offline signal left the key of a task that declares it unmoved")
	}
	if key(other) != localOther {
		t.Error("the offline signal moved the key of a task that does not declare it")
	}
}
