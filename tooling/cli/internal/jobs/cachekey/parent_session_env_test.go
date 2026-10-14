package cachekey

import (
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// TestParentSessionEnvIsNeverACacheKeyInput is the guard the nesting member
// rests on, and the single most expensive thing this feature could get wrong.
//
// A session id is UNIQUE PER RUN. If any value derived from it reached a task's
// cache key, every cacheable task in every workspace would miss on every run,
// forever, and the miss would look like ordinary cache behavior rather than a
// bug. The variable therefore travels on the scheduler's execution-only channel
// (jobProcessEnv → extraEnv → cmd.Env) and nowhere else.
//
// The key builder reads the environment only through declared inputs — a task
// contract's cache.key.env, and a project's options.<layer>.envInputs — so an
// undeclared variable cannot reach it. This pins that for THIS variable by
// name, because "no project declares it today" is a fact about the current
// tree, and a regression here is silent.
func TestParentSessionEnvIsNeverACacheKeyInput(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	newJob := func() *jobmodel.ScheduledJob {
		job := &jobmodel.ScheduledJob{
			Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
			Extension: &extension.ExtensionDescription{Name: "@test/ext", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name: "build", CommandName: "build", ExtensionName: "@test/ext", Cache: true,
			},
		}
		return declareCacheTestTask(job, "")
	}

	hash := func() string {
		t.Helper()
		got, err := ComputeJobCacheHash(ws, newJob(), nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return got
	}

	// Set explicitly, including the empty case: this test runs inside a task of
	// a real CLI run, which exports the variable, so an implicit "unset" case
	// would compare two set values and prove nothing.
	t.Setenv(protocolcli.ParentSessionEnv, "")
	unset := hash()
	t.Setenv(protocolcli.ParentSessionEnv, "20260913-023611-4a5b87")
	if got := hash(); got != unset {
		t.Errorf("%s moved the task cache key: every task in the workspace would miss on every nested run",
			protocolcli.ParentSessionEnv)
	}
	t.Setenv(protocolcli.ParentSessionEnv, "20260913-104455-ffffff")
	if got := hash(); got != unset {
		t.Errorf("a second %s value moved the task cache key: a per-run value reached the key",
			protocolcli.ParentSessionEnv)
	}
}
