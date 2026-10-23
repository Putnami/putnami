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

// A failed input selector of the producer whose invocation-scoped resource a
// consumer reads fails the consumer's key, and the consumer, before execution
// in every cache mode.
func TestInvocationProducerSelectorFailureReachesConsumerKeys(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	producer := cacheableJob("prepare", "/proj", "proj", "proj")
	producer.JobDef.Cache = false // invocation-scoped producers do not publish keys
	policy := &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Files: []string{"sel:build"}}}
	producer.JobDef.TaskCachePolicy = policy
	producer.Extension.Tasks["cacheable"] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{}, Cache: policy}
	resolver := listedInputs(store.InputExpansion{Files: []string{"missing.sql"}})
	consumer := cacheableJob("consume", "/test-proj", "test-proj", "test-proj", producer.Key())
	consumer.InvocationProducer = producer
	ws = workspace.NewWorkspace(ws.Root, nil, []*workspace.Project{producer.Project, consumer.Project})
	cache := func() *store.CacheManager {
		manager := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		manager.SetInputResolver(resolver)
		return manager
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
			if !errors.Is(err, store.ErrInputSelector) || key != "" || !strings.Contains(err.Error(), "missing.sql") {
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
			// Without a manager the run has no resolver, which fails the
			// selector as surely as the missing file does.
			want := "missing.sql"
			if !tc.withCache {
				want = "no resolver expands selector"
			}
			result := runSharedScheduler(context.Background(), ws, []*ScheduledJob{consumer}, SchedulerConfig{NoCache: tc.bypass}, manager)
			row := result.Results[consumer.Key()]
			if result.Success || row == nil || row.CacheKey != "" || row.Status != string(TaskStatusFailed) || row.Error == nil || !strings.Contains(row.Error.Message, want) {
				t.Fatalf("consumer reached execution despite invalid producer: row=%+v", row)
			}
		})
	}
}

// A closure pattern that names an input selector, which a manifest cannot
// declare, fails the key and the task before execution in every cache mode.
func TestClosureSelectorFailureTerminatesConsumerBeforeExecution(t *testing.T) {
	root := t.TempDir()
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	for _, dir := range []string{"lib", "app"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	job := cacheableJob("consume", app.ID, app.Name, app.Path)
	job.Project = app
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{ClosureFiles: []string{"sel:build"}}}
	cache := func() *store.CacheManager {
		manager := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
		manager.SetInputResolver(listedInputs(store.InputExpansion{}))
		return manager
	}
	if _, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache(), nil); !errors.Is(err, store.ErrInputSelector) {
		t.Fatalf("a closure selector lost its typed category: %v", err)
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
			if result.Success || row == nil || row.CacheKey != "" || row.Status != string(TaskStatusFailed) || row.Error == nil || !strings.Contains(row.Error.Message, "sel:build") {
				t.Fatalf("closure failure reached execution: row=%+v", row)
			}
		})
	}
}
