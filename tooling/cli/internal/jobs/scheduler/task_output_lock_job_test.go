package scheduler

import (
	"go.putnami.dev/cli/model/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// outputLockJob is a plan node for project path projectPath running name
// (command~step), whose task declares outputs.
func outputLockJob(projectPath, name string, outputs map[string]extension.DeclaredOutput) *ScheduledJob {
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/" + projectPath, Name: projectPath, Path: projectPath},
		Extension: &extension.ExtensionDescription{Name: "@test/ext"},
		JobDef:    &extension.JobDefinition{Name: name, ExtensionName: "@test/ext"},
	}
	if outputs != nil {
		job.Extension.Tasks = map[string]extension.TaskDefinition{
			"task": {Declares: &extension.TaskDeclaration{Outputs: outputs}},
		}
		job.Step = &extension.PipelineStep{Task: "task"}
	}
	return job
}
