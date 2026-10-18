package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// cacheableJob builds a minimal cache-enabled job for a project.
func cacheableJob(name, projID, projName, projPath string, deps ...string) *ScheduledJob {
	const task = "cacheable"
	return &ScheduledJob{
		Project: &workspace.Project{ID: projID, Name: projName, Path: projPath},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Tasks: map[string]extension.TaskDefinition{
				task: {Declares: &extension.TaskDeclaration{}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name:          name,
			ExtensionName: "@test/ext",
			Cache:         true,
		},
		Step:      &extension.PipelineStep{Task: task},
		DependsOn: deps,
	}
}

func TestInvocationProducerGoEmbedFailureReachesConsumerKeys(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	source := filepath.Join(ws.Root, "proj", "embed.go")
	if err := os.WriteFile(source, []byte("package proj\nimport _ \"embed\"\n//go:embed missing.sql\nvar payload string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	producer := cacheableJob("prepare", "/proj", "proj", "proj")
	producer.JobDef.Cache = false // invocation-scoped producers do not publish keys
	producer.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Files: []string{"go-embed:build"}}}
	consumer := cacheableJob("consume", "/test-proj", "test-proj", "test-proj", producer.Key())
	consumer.InvocationProducer = producer
	ws = workspace.NewWorkspace(ws.Root, nil, []*workspace.Project{producer.Project, consumer.Project})
	cache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	}
	for _, test := range []struct {
		name string
		key  func() (string, error)
	}{
		{"precompute", func() (string, error) {
			keys, err := cachekey.PrecomputeKeys(ws, []*ScheduledJob{producer, consumer}, nil, nil, cache(), cachekey.CacheBypass{})
			return keys[consumer.Key()], err
		}},
		{"execution", func() (string, error) { return cachekey.ComputeJobCacheHash(ws, consumer, nil, nil, cache(), nil) }},
		{"selection", func() (string, error) {
			return cachekey.ComputeJobCacheHashWith(ws, consumer, nil, nil, cache(), nil, true)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, err := test.key()
			if !errors.Is(err, store.ErrGoEmbedInput) || key != "" || !strings.Contains(err.Error(), "missing.sql") {
				t.Fatalf("invalid invocation producer yielded consumer key %q, %v", key, err)
			}
		})
	}
	// The producer contributes to the identity through InvocationProducer. It
	// need not be a scheduled dependency to exercise task opening below.
	consumer.DependsOn = nil
	for _, tc := range []struct {
		name      string
		withCache bool
		bypass    bool
		cacheable bool
	}{
		{"declared", true, false, true},
		{"bypass", true, true, true},
		{"nil cache", false, false, true},
		{"uncacheable consumer", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumer.JobDef.Cache = tc.cacheable
			var manager *store.CacheManager
			if tc.withCache {
				manager = cache()
			}
			result := runSharedScheduler(context.Background(), ws, []*ScheduledJob{consumer}, SchedulerConfig{NoCache: tc.bypass}, manager)
			row := result.Results[consumer.Key()]
			if result.Success || row == nil || row.CacheKey != "" || row.Status != string(TaskStatusFailed) || row.Error == nil || !strings.Contains(row.Error.Message, "missing.sql") {
				t.Fatalf("consumer reached execution despite invalid producer: row=%+v", row)
			}
		})
	}
}

func TestClosureGoEmbedFailureTerminatesConsumerBeforeExecution(t *testing.T) {
	root := t.TempDir()
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "embed.go"), []byte("package lib\nimport _ \"embed\"\n//go:embed missing.sql\nvar payload string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := cacheableJob("consume", app.ID, app.Name, app.Path)
	job.Project = app
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{ClosureFiles: []string{"go-embed:build"}}}
	cache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	}
	if _, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache(), nil); !errors.Is(err, store.ErrGoEmbedInput) {
		t.Fatalf("closure member failure lost typed semantic category: %v", err)
	}
	for _, tc := range []struct {
		name      string
		withCache bool
		bypass    bool
		cacheable bool
	}{
		{"declared", true, false, true},
		{"bypass", true, true, true},
		{"nil cache", false, false, true},
		{"uncacheable", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job.JobDef.Cache = tc.cacheable
			var manager *store.CacheManager
			if tc.withCache {
				manager = cache()
			}
			result := runSharedScheduler(context.Background(), ws, []*ScheduledJob{job}, SchedulerConfig{NoCache: tc.bypass}, manager)
			row := result.Results[job.Key()]
			if result.Success || row == nil || row.CacheKey != "" || row.Status != string(TaskStatusFailed) || row.Error == nil || !strings.Contains(row.Error.Message, "missing.sql") {
				t.Fatalf("closure failure reached execution: row=%+v", row)
			}
		})
	}
}
