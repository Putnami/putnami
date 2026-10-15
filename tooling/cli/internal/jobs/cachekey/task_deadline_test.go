package cachekey

import (
	"path/filepath"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func deadlineTestJob(manifestTimeoutMs int, tuning map[string]wsproto.ProjectTaskTuning) *jobmodel.ScheduledJob {
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
	if tuning != nil {
		project.Config = &wsproto.ProjectConfig{Name: "proj", Tasks: tuning}
	}
	return &jobmodel.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/go"},
		JobDef: &extension.JobDefinition{
			Name:          "lint~golangci-lint",
			CommandName:   "lint",
			ExtensionName: "@putnami/go",
			TimeoutMs:     manifestTimeoutMs,
		},
	}
}

// TestTaskTimeoutTuningLeavesCacheKeyUnchanged proves the execution-only
// promise at the actual cache-key boundary. The task declares putnami.json as
// an input exactly as extension manifests do; changing a scheduler tuning must
// not split its cache history, while source and non-tuning config edits still
// must move the key.
func TestTaskTimeoutTuningLeavesCacheKeyUnchanged(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	configPath := filepath.Join(ws.Root, "proj", wsproto.ConfigFilename)
	writeFileAt(t, filepath.Join(ws.Root, "proj", "src", "main.go"), "package main\n")

	writeConfig := func(body string) {
		t.Helper()
		writeFileAt(t, configPath, body)
	}
	hashFor := func() string {
		t.Helper()
		job := deadlineTestJob(600_000, nil)
		job.JobDef.Cache = true
		job.JobDef.FilePatterns = []string{"src/**/*", wsproto.ConfigFilename}
		declareCacheTestTask(job, "golangci-lint")
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		hash, err := ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute cache key: %v", err)
		}
		return hash
	}

	const baseConfig = `{"name":"proj","tags":["go"]}`
	writeConfig(baseConfig)
	baseline := hashFor()

	writeFileAt(t, filepath.Join(ws.Root, "proj", "src", "main.go"), "package main // changed\n")
	if moved := hashFor(); moved == baseline {
		t.Fatal("a source edit did not move the cache key; the fixture is not live")
	}
	writeFileAt(t, filepath.Join(ws.Root, "proj", "src", "main.go"), "package main\n")

	writeConfig(`{"name":"proj","tags":["go","changed"]}`)
	if moved := hashFor(); moved == baseline {
		t.Fatal("a non-tuning project config edit did not move the cache key")
	}

	for _, config := range []string{
		`{"name":"proj","tags":["go"],"tasks":{"lint":{"timeoutMs":900000}}}`,
		`{"name":"proj","tags":["go"],"tasks":{"lint~golangci-lint":{"timeoutMs":120000}}}`,
		`{"name":"proj","tags":["go"],"tasks":{"lint":{"cpuWeight":4},"build":{"timeoutMs":1}}}`,
	} {
		writeConfig(config)
		if got := hashFor(); got != baseline {
			t.Errorf("cache key = %s, want untuned key %s", got, baseline)
		}
	}
}
