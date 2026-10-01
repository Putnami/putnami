package jobs

import (
	"context"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// inputDigestFixture plans three cacheable nodes over two projects:
// `/a:build~compile` keyed on a/input.txt, `/b:build~compile` keyed on
// b/input.txt, and `/b:build~consume`, keyed on b/input.txt too but depending
// on `/a:build~compile`, so it consumes a's input through the upstream key.
func inputDigestFixture(t *testing.T) (*workspace.Workspace, []*ScheduledJob) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a", "input.txt"), "a\n")
	writeTestFile(t, filepath.Join(root, "b", "input.txt"), "b\n")
	script := filepath.Join(root, "ok.sh")
	writeExecutable(t, script,
		"#!/bin/sh\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"meta\",\"data\":{\"protocol\":2}}'\n"+
			"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"OK\"}}'\n"+
			"exit 0\n")

	a := &workspace.Project{ID: "/a", Name: "a", Path: "a"}
	b := &workspace.Project{ID: "/b", Name: "b", Path: "b"}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{a, b})
	ws.Name = "input-digest-ws"

	ext := &extension.ExtensionDescription{
		Name:    "@test/digest",
		Version: "1.0.0",
		Path:    filepath.Join(root, "ext"),
		Tasks: map[string]extension.TaskDefinition{
			"compile-task": {Declares: &extension.TaskDeclaration{}},
			"consume-task": {Declares: &extension.TaskDeclaration{}},
		},
	}
	newJob := func(project *workspace.Project, name, task string) *ScheduledJob {
		commandName, step := jobCommandAndStep(name)
		return &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &extension.PipelineStep{ID: step, Task: task},
			JobDef: &extension.JobDefinition{
				ExtensionName:   "@test/digest",
				Name:            name,
				CommandName:     commandName,
				StepID:          step,
				Command:         script,
				Cwd:             "{projectRoot}",
				Cache:           true,
				FilePatterns:    []string{"input.txt"},
				TaskCachePolicy: &extension.TaskCachePolicy{NoOutput: true},
			},
		}
	}
	planned := []*ScheduledJob{
		newJob(a, "build~compile", "compile-task"),
		newJob(b, "build~compile", "compile-task"),
		newJob(b, "build~consume", "consume-task"),
	}
	planned[2].DependsOn = []string{planned[0].Key()}
	return ws, planned
}

// TestInputDigest_MovesExactlyWithTheInputsItWasKeyedOn is the acceptance
// criterion on a fixture workspace: two runs on the same tree give every task
// the same digest, whatever the second run reused, and touching one declared
// input moves the digests of exactly the tasks that consume it — its own task
// and the dependent that folds that task's key — and no other.
func TestInputDigest_MovesExactlyWithTheInputsItWasKeyedOn(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/diagnostics-results", "task-input-digest", "same-tree-same-digest-touched-input-moves-only-its-consumers")
	requireShell(t)

	ws, planned := inputDigestFixture(t)
	storeRoot := filepath.Join(t.TempDir(), "store")
	// One store, a fresh CacheManager per run: a second `putnami` invocation.
	run := func() map[string]TaskResult {
		cache := store.NewCacheManager(store.NewLocalStore(storeRoot))
		result := runSharedScheduler(context.Background(), ws, planned, SchedulerConfig{MaxParallel: 1}, cache)
		tasks := make(map[string]TaskResult, len(planned))
		for i, job := range planned {
			task := TaskResultOf(job, resultOf(t, result, planned, i))
			if task.Status != TaskStatusSuccess {
				t.Fatalf("%s status = %q, want success", job.Key(), task.Status)
			}
			if len(task.InputDigest) != len("sha256:")+64 || task.InputDigest[:len("sha256:")] != "sha256:" {
				t.Fatalf("%s inputDigest = %q, want the sha256: spelling of its key", job.Key(), task.InputDigest)
			}
			tasks[job.Key()] = task
		}
		return tasks
	}
	compileA, compileB, consumeB := planned[0].Key(), planned[1].Key(), planned[2].Key()

	cold := run()
	warm := run()
	for key, task := range warm {
		if task.Reuse != ReuseLocalCache {
			t.Errorf("%s reuse on the second run = %q, want local-cache", key, task.Reuse)
		}
		if task.InputDigest != cold[key].InputDigest {
			t.Errorf("%s digest moved between two runs on the same tree: %s -> %s",
				key, cold[key].InputDigest, task.InputDigest)
		}
	}
	if cold[compileB].InputDigest == cold[consumeB].InputDigest {
		t.Fatal("two different tasks keyed on the same file share a digest: the digest does not name the task")
	}

	writeTestFile(t, filepath.Join(ws.Root, "a", "input.txt"), "a, touched\n")
	touched := run()
	for _, key := range []string{compileA, consumeB} {
		if touched[key].InputDigest == warm[key].InputDigest {
			t.Errorf("%s consumes a/input.txt, but its digest did not move", key)
		}
		if touched[key].Reuse != ReuseNone {
			t.Errorf("%s reuse after its input moved = %q, want it executed", key, touched[key].Reuse)
		}
	}
	if touched[compileB].InputDigest != warm[compileB].InputDigest {
		t.Errorf("%s does not consume a/input.txt, but its digest moved: %s -> %s",
			compileB, warm[compileB].InputDigest, touched[compileB].InputDigest)
	}
	if touched[compileB].Reuse != ReuseLocalCache {
		t.Errorf("%s reuse = %q, want local-cache: its inputs did not move", compileB, touched[compileB].Reuse)
	}
}
