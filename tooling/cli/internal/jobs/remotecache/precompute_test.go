package remotecache

import (
	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// cacheableJob builds a minimal cache-enabled job for a project.
func cacheableJob(name, projID, projName, projPath string, deps ...string) *jobmodel.ScheduledJob {
	const task = "cacheable"
	return &jobmodel.ScheduledJob{
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
