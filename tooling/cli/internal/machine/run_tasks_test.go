package machine

import (
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestRunOf_ReadsTheSchedulersProjection pins that a renderer's run view reads
// the tasks the scheduler projected once (SessionOutcome.Tasks), and projects
// the results itself only when the outcome carries none.
func TestRunOf_ReadsTheSchedulersProjection(t *testing.T) {
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	job := &scheduler.ScheduledJob{Project: project, JobDef: &extension.JobDefinition{Name: "build", CommandName: "build"}}
	results := map[string]*scheduler.JobResult{job.Key(): {Status: "success"}}

	projected := scheduler.RunTasks([]*scheduler.ScheduledJob{job}, results)
	projected[0].Task.Error = &scheduler.JobError{Message: "from the scheduler's projection"}
	run := RunOf([]*scheduler.ScheduledJob{job}, results, scheduler.SessionOutcome{Tasks: projected})
	if len(run.Tasks) != 1 || run.Tasks[0].Result.Error == nil {
		t.Fatalf("run tasks = %+v, want the scheduler's projection", run.Tasks)
	}

	run = RunOf([]*scheduler.ScheduledJob{job}, results, scheduler.SessionOutcome{})
	if len(run.Tasks) != 1 || run.Tasks[0].Result.Error != nil || run.Tasks[0].Result.Status != scheduler.TaskStatusSuccess {
		t.Fatalf("run tasks = %+v, want the results projected", run.Tasks)
	}
}
