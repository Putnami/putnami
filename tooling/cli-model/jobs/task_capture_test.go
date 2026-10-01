package jobs

import (
	"testing"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/workspace"
)

func declaredCaptureJob(command, step string, outputs map[string]extension.DeclaredOutput) *ScheduledJob {
	return &ScheduledJob{
		Project: &workspace.Project{Name: "example", Path: "example"},
		Extension: &extension.ExtensionDescription{Tasks: map[string]extension.TaskDefinition{
			step: {Declares: &extension.TaskDeclaration{Outputs: outputs}},
		}},
		JobDef: &extension.JobDefinition{Name: command + "~" + step},
		Step:   &extension.PipelineStep{Task: step},
	}
}

// Eligibility is a property of the DECLARATION, not of the command the step runs
// in. The same task declaring the same output is eligible under `package` and
// under `build` alike: an earlier fix removed the package exception by removing the
// write that justified it, so a rule keyed on a command name would now reject
// work whose ownership is fully declared.
func TestUsesDeclaredCaptureIsCommandAgnostic(t *testing.T) {
	ownedDirectory := map[string]extension.DeclaredOutput{
		"owned": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "owned"},
	}
	multipleOutputs := map[string]extension.DeclaredOutput{
		"owned":    {Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "owned"},
		"manifest": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, Path: "manifest.json"},
	}
	projectRooted := map[string]extension.DeclaredOutput{
		"generated": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen"},
	}

	cases := []struct {
		name    string
		command string
		step    string
		outputs map[string]extension.DeclaredOutput
		want    bool
	}{
		{name: "owned directory under package", command: "package", step: "owned", outputs: ownedDirectory, want: true},
		{name: "the same declaration under build", command: "build", step: "owned", outputs: ownedDirectory, want: true},
		{name: "two owned outputs under package", command: "package", step: "owned", outputs: multipleOutputs, want: true},
		{name: "project-rooted output under package", command: "package", step: "generate", outputs: projectRooted, want: true},
		{name: "declaration whose id differs from its path", command: "package", step: "owned", outputs: map[string]extension.DeclaredOutput{
			"candidate": {Kind: extension.OutputKindDirectory, Root: extension.OutputRootCommandOutput, Path: "owned"},
		}, want: true},
		{name: "empty declaration is a status-only contract", command: "package", step: "verify", outputs: nil, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := declaredCaptureJob(tc.command, tc.step, tc.outputs)
			if got := UsesDeclaredCapture(job); got != tc.want {
				t.Fatalf("UsesDeclaredCapture() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A task with no v3 declaration stays out: nothing states what it owns, so a
// captured entry could not be proven to reproduce it.
func TestUsesDeclaredCaptureRefusesAnUndeclaredTask(t *testing.T) {
	job := declaredCaptureJob("package", "legacy", nil)
	job.Extension.Tasks["legacy"] = extension.TaskDefinition{}
	if UsesDeclaredCapture(job) {
		t.Fatal("a task without a v3 declaration was cache eligible")
	}

	// And the eligibility does not come from a cache policy: a task that
	// declares nothing stays ineligible however its cache block is spelled. The
	// `package` command used to grant capture on a policy assertion instead
	// (`restoreMode: all-or-nothing`, now a deprecated no-op); nothing in a
	// policy can stand in for stating what the task owns.
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Deterministic: true, VersionAware: true}
	if UsesDeclaredCapture(job) {
		t.Fatal("a cache policy made an undeclared task eligible")
	}
}
