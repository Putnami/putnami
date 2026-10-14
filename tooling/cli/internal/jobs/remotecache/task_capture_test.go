package remotecache

import (
	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

const captureTestProject = "proj"

// declaredJob builds a plan node whose manifest task carries a v3 declaration.
// The declaration hangs off the extension's task table (never off JobDefinition),
// exactly as extension.Resolve leaves it, so the tests exercise the same lookup
// the scheduler does.
func declaredJob(name, task string, declaration *extension.TaskDeclaration, writes ...extension.ResourceRef) *jobmodel.ScheduledJob {
	return &jobmodel.ScheduledJob{
		Project: &workspace.Project{ID: "/" + captureTestProject, Name: captureTestProject, Path: captureTestProject},
		Extension: &extension.ExtensionDescription{
			Name:  "@test/ext",
			Tasks: map[string]extension.TaskDefinition{task: {Declares: declaration}},
		},
		JobDef: &extension.JobDefinition{Name: name, Cache: true, Writes: writes},
		Step:   &extension.PipelineStep{Task: task},
	}
}

func dirDeclaration(root, path string, optional bool) extension.DeclaredOutput {
	return extension.DeclaredOutput{Kind: extension.OutputKindDirectory, Root: root, Path: path, OptionalEmpty: optional}
}

func fileDeclaration(root, path string, optional bool) extension.DeclaredOutput {
	return extension.DeclaredOutput{Kind: extension.OutputKindFile, Root: root, Path: path, OptionalEmpty: optional}
}
