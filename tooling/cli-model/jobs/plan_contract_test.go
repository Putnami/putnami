package jobs

import (
	"slices"
	"testing"

	"go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

// TestProducesVerificationReportFollowsTheDeclaration pins the one rule both
// halves of the spec gate share. A v3 declaration is a closed
// footprint: only a command-output FILE at the reserved filename writes the
// report, whatever the command, and every near miss does not. Without a
// declaration the job's shape decides: the whole test command counts, a
// pipeline step that declares nothing does not, and neither does a job of
// another command with no pipeline step.
func TestProducesVerificationReportFollowsTheDeclaration(t *testing.T) {
	t.Parallel()
	report := func(output extension.DeclaredOutput) map[string]extension.DeclaredOutput {
		return map[string]extension.DeclaredOutput{"featureVerification": output}
	}
	reportFile := extension.DeclaredOutput{Kind: extension.OutputKindFile,
		Root: extension.OutputRootCommandOutput, Path: features.VerificationReportFilename, OptionalEmpty: true}
	near := func(edit func(*extension.DeclaredOutput)) map[string]extension.DeclaredOutput {
		output := reportFile
		edit(&output)
		return report(output)
	}
	undeclaredStep := declaredCaptureJob("test", "test", nil)
	undeclaredStep.Extension.Tasks["test"] = extension.TaskDefinition{}

	cases := map[string]struct {
		job  *ScheduledJob
		want bool
	}{
		"the declared report file": {
			job: declaredCaptureJob("test", "test", report(reportFile)), want: true},
		"the report beside the other test outputs": {
			job: declaredCaptureJob("test", "test", map[string]extension.DeclaredOutput{
				"featureVerification": reportFile,
				"coverage": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput,
					Path: "coverage.out"},
			}), want: true},
		"a declaration without outputs, like a test-env step": {
			job: declaredCaptureJob("test", "test-env", nil)},
		"the reserved name under the default project root": {
			job: declaredCaptureJob("test", "test", near(func(o *extension.DeclaredOutput) { o.Root = "" }))},
		"the reserved name under the workspace root": {
			job: declaredCaptureJob("test", "test",
				near(func(o *extension.DeclaredOutput) { o.Root = extension.OutputRootWorkspace }))},
		"an invocation-scoped report": {
			job: declaredCaptureJob("test", "test", near(func(o *extension.DeclaredOutput) {
				o.Root, o.Scope = "", extensionproto.OutputScopeInvocation
			}))},
		"a directory at the reserved name": {
			job: declaredCaptureJob("test", "test",
				near(func(o *extension.DeclaredOutput) { o.Kind = extension.OutputKindDirectory }))},
		"another command-output file": {
			job: declaredCaptureJob("test", "test", near(func(o *extension.DeclaredOutput) { o.Path = "coverage.out" }))},
		"the report file declared under another command": {
			job: declaredCaptureJob("check", "run", report(reportFile)), want: true},
		"a pipeline step whose task declares nothing": {
			job: undeclaredStep},
		"a test command job with no pipeline step": {
			job: &ScheduledJob{JobDef: &extension.JobDefinition{Name: "test", CommandName: "test"}}, want: true},
		"another command's job with no pipeline step": {
			job: &ScheduledJob{JobDef: &extension.JobDefinition{Name: "check", CommandName: "check"}}},
		"a job without a definition": {
			job: &ScheduledJob{}},
		"no job": {},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ProducesVerificationReport(testCase.job); got != testCase.want {
				t.Errorf("ProducesVerificationReport = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestProducesSpecCriteriaProjectionFollowsTheDeclaration pins how the spec
// gate tells a projection producer from every other task: only a declared
// command-output FILE at the reserved filename counts, whatever the command. A
// job without a declaration never does.
func TestProducesSpecCriteriaProjectionFollowsTheDeclaration(t *testing.T) {
	t.Parallel()
	projectionFile := extension.DeclaredOutput{Kind: extension.OutputKindFile,
		Root: extension.OutputRootCommandOutput, Path: features.SpecCriteriaProjectionFilename, OptionalEmpty: true}
	near := func(edit func(*extension.DeclaredOutput)) map[string]extension.DeclaredOutput {
		output := projectionFile
		edit(&output)
		return map[string]extension.DeclaredOutput{"criteria": output}
	}
	declared := map[string]extension.DeclaredOutput{"criteria": projectionFile}

	cases := map[string]struct {
		job  *ScheduledJob
		want bool
	}{
		"the declared projection file": {
			job: declaredCaptureJob("validate", "specs-validate", declared), want: true},
		"the reserved name under the default project root": {
			job: declaredCaptureJob("validate", "specs-validate", near(func(o *extension.DeclaredOutput) { o.Root = "" }))},
		"a directory at the reserved name": {
			job: declaredCaptureJob("validate", "specs-validate",
				near(func(o *extension.DeclaredOutput) { o.Kind = extension.OutputKindDirectory }))},
		"another command-output file": {
			job: declaredCaptureJob("validate", "specs-validate", near(func(o *extension.DeclaredOutput) { o.Path = "report.json" }))},
		"the projection file declared under another command": {
			job: declaredCaptureJob("criteria", "specs-validate", declared), want: true},
		"a declaration without outputs": {
			job: declaredCaptureJob("validate", "lint-specs", nil)},
		"a validate command job with no pipeline step": {
			job: &ScheduledJob{JobDef: &extension.JobDefinition{Name: "validate", CommandName: "validate"}}},
		"a job without a definition": {
			job: &ScheduledJob{}},
		"no job": {},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := ProducesSpecCriteriaProjection(testCase.job); got != testCase.want {
				t.Errorf("ProducesSpecCriteriaProjection = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestProducerCommandsAreTheDeclaringCommands pins how the spec gate finds the
// report and projection producers before it plans: a command counts when one of
// its pipeline steps runs a task that declares the reserved file, or, for the
// report alone, when it is a `test` command with no pipeline step. No other
// command name decides, and the answer is sorted and free of repeats across
// extensions.
func TestProducerCommandsAreTheDeclaringCommands(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-verification-producers",
		"producer-commands-are-the-commands-whose-tasks-declare-the-files")
	t.Parallel()
	file := func(path string) *extension.TaskDeclaration {
		return &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
			"out": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, Path: path, OptionalEmpty: true},
		}}
	}
	pipeline := func(name string, tasks ...string) *extension.JobDefinition {
		steps := make([]extension.PipelineStep, len(tasks))
		for i, task := range tasks {
			steps[i] = extension.PipelineStep{ID: task, Task: task}
		}
		return &extension.JobDefinition{Name: name, Kind: "command", PipelineSteps: steps}
	}
	lang := &extension.ExtensionDescription{Name: "@acme/lang",
		Jobs: map[string]*extension.JobDefinition{
			"test":  pipeline("test", "generate", "run-tests"),
			"check": pipeline("check", "run-checks"),
			"build": pipeline("build", "compile"),
			"lint":  pipeline("lint", "undeclared"),
			"empty": nil,
		},
		Tasks: map[string]extension.TaskDefinition{
			"generate":   {Declares: &extension.TaskDeclaration{}},
			"run-tests":  {Declares: file(features.VerificationReportFilename)},
			"run-checks": {Declares: file(features.VerificationReportFilename)},
			"compile":    {Declares: file("bin")},
			"undeclared": {},
		}}
	sdd := &extension.ExtensionDescription{Name: "@acme/sdd",
		Jobs: map[string]*extension.JobDefinition{
			"criteria": pipeline("criteria", "project-specs"),
			"validate": pipeline("validate", "lint-specs"),
		},
		Tasks: map[string]extension.TaskDefinition{
			"project-specs": {Declares: file(features.SpecCriteriaProjectionFilename)},
			"lint-specs":    {Declares: &extension.TaskDeclaration{}},
		}}
	legacy := &extension.ExtensionDescription{Name: "@acme/legacy",
		Jobs: map[string]*extension.JobDefinition{
			"test":   {Name: "test", Kind: "command"},
			"verify": {Name: "verify", Kind: "command"},
		}}
	extensions := []*extension.ExtensionDescription{sdd, nil, lang, legacy}

	if got := VerificationReportCommands(extensions); !slices.Equal(got, []string{"check", "test"}) {
		t.Errorf("VerificationReportCommands = %v, want [check test]", got)
	}
	if got := SpecCriteriaProjectionCommands(extensions); !slices.Equal(got, []string{"criteria"}) {
		t.Errorf("SpecCriteriaProjectionCommands = %v, want [criteria]", got)
	}
	if got := VerificationReportCommands([]*extension.ExtensionDescription{legacy}); !slices.Equal(got, []string{"test"}) {
		t.Errorf("VerificationReportCommands without declarations = %v, want only the whole test command", got)
	}
	if got := VerificationReportCommands(nil); len(got) != 0 {
		t.Errorf("VerificationReportCommands(nil) = %v, want none", got)
	}
}
