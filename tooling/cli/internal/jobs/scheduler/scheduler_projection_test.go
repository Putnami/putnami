package scheduler

import (
	"context"
	"reflect"
	"testing"

	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestScheduler_ProjectsEveryTaskOnceAfterTheFinalizers pins the end of a run:
// the tasks are projected once, after the finalizers amended the rows, and the
// renderer's Finish, the reduction and the result all read that projection.
func TestScheduler_ProjectsEveryTaskOnceAfterTheFinalizers(t *testing.T) {
	ws := &workspace.Workspace{Name: "test-ws", Root: t.TempDir(), Graph: workspace.BuildGraph(nil)}
	planned := []*ScheduledJob{makeScheduledJob("a", "build", nil), makeScheduledJob("b", "build", nil)}
	renderer := &mockRenderer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := RunPlan(ctx, RunRequest{
		Workspace: ws,
		Plan:      planned,
		Config: SchedulerConfig{
			MaxParallel: 1,
			NoCache:     true,
			FinalizeResults: func(results map[string]*JobResult) {
				results["/b:build"] = &JobResult{Status: "failed", Error: &JobError{Message: "amended by a finalizer"}}
			},
		},
		Renderer: renderer,
	})

	if want := RunTasks(planned, result.Results); !reflect.DeepEqual(result.Tasks, want) || len(want) != 2 {
		t.Fatalf("result tasks = %+v, want the projection of the final rows %+v", result.Tasks, want)
	}
	if amended := result.Tasks[1].Task; amended.Error == nil || amended.Error.Message != "amended by a finalizer" {
		t.Errorf("the projection predates the finalizer: %+v", amended)
	}
	renderer.mu.Lock()
	finished := renderer.outcome.Tasks
	renderer.mu.Unlock()
	if len(finished) != len(result.Tasks) || &finished[0] != &result.Tasks[0] {
		t.Error("the renderer's Finish did not read the run's one projection")
	}
	if result.Outcome.Tasks != nil {
		t.Error("the run's outcome carries the projection the renderer alone reads")
	}
	if result.Session.Status.Failed != 1 || result.Success {
		t.Errorf("the reduction did not read the amended projection: %+v", result.Session.Status)
	}
}
