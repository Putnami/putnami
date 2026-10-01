package engine

import (
	"slices"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func servePipelineJob(project *workspace.Project, name string, cache bool, dependsOn ...string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/fixture"},
		JobDef:    &extension.JobDefinition{Name: name, Cache: cache},
		DependsOn: dependsOn,
	}
}

func jobKeys(planned []*jobs.ScheduledJob) []string {
	keys := make([]string, 0, len(planned))
	for _, job := range planned {
		keys = append(keys, job.Key())
	}
	return keys
}

// A composition must start each member's workload itself, so the engine hands
// back exactly the terminal, uncacheable serve step of every SELECTED project
// and keeps every finite step — including the generate step of a dependency
// nobody asked to serve — in the scheduled run.
func TestWithholdServeSteps_TakesOnlyTheTerminalServeStepOfSelectedProjects(t *testing.T) {
	provider := &workspace.Project{ID: "/provider", Name: "provider", Path: "provider"}
	consumer := &workspace.Project{ID: "/consumer", Name: "consumer", Path: "consumer"}
	library := &workspace.Project{ID: "/library", Name: "library", Path: "library"}

	planned := []*jobs.ScheduledJob{
		servePipelineJob(library, "serve~generate", true),
		servePipelineJob(provider, "serve~generate", true, "/library:serve~generate"),
		servePipelineJob(provider, "serve~describe", true, "/provider:serve~generate"),
		servePipelineJob(provider, "serve~serve", false, "/provider:serve~describe"),
		servePipelineJob(consumer, "serve~generate", true),
		servePipelineJob(consumer, "serve~serve", false, "/consumer:serve~generate"),
		// An uncacheable serve-command step something still depends on is not
		// terminal, and a library's own serve step was not selected.
		servePipelineJob(consumer, "serve~prepare", false),
		servePipelineJob(consumer, "serve~after", true, "/consumer:serve~prepare"),
		servePipelineJob(library, "serve~serve", false),
	}

	kept, withheld := withholdServeSteps(planned, []*workspace.Project{provider, consumer})

	if got, want := jobKeys(withheld), []string{"/provider:serve~serve", "/consumer:serve~serve"}; !slices.Equal(got, want) {
		t.Errorf("withheld = %v, want %v", got, want)
	}
	want := []string{
		"/library:serve~generate", "/provider:serve~generate", "/provider:serve~describe",
		"/consumer:serve~generate", "/consumer:serve~prepare", "/consumer:serve~after", "/library:serve~serve",
	}
	if got := jobKeys(kept); !slices.Equal(got, want) {
		t.Errorf("kept = %v, want %v", got, want)
	}
	for _, job := range kept {
		for _, predecessor := range job.SchedulingPredecessors() {
			if slices.Contains(jobKeys(withheld), predecessor) {
				t.Errorf("kept job %s depends on withheld step %s", job.Key(), predecessor)
			}
		}
	}
}

func TestWithholdServeSteps_NonServeCommandsAreNeverWithheld(t *testing.T) {
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	planned := []*jobs.ScheduledJob{
		servePipelineJob(app, "run~exec", false),
		servePipelineJob(app, "test", false),
		nil,
	}
	kept, withheld := withholdServeSteps(planned, []*workspace.Project{app, nil})
	if len(withheld) != 0 {
		t.Errorf("withheld = %v, want none", jobKeys(withheld))
	}
	if len(kept) != len(planned) {
		t.Errorf("kept %d of %d jobs", len(kept), len(planned))
	}
}
