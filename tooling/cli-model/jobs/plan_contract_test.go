package jobs

import (
	"slices"
	"testing"

	"go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
)

// ofKind gives a planned job the command kind discovery resolved for its
// command.
func ofKind(kind string, job *ScheduledJob) *ScheduledJob {
	job.JobDef.Traits.Kind = kind
	return job
}

// TestProducesVerificationReportFollowsTheKindAndTheDeclaration pins the one
// rule both halves of the spec gate share. Only a job of a verification
// command writes the report, whatever the command is called. Within it a v3
// declaration is a closed footprint: only a command-output FILE at the
// reserved filename writes the report, and every near miss does not. Without
// a declaration the job's shape decides: the whole verification command
// counts, a pipeline step that declares nothing does not.
func TestProducesVerificationReportFollowsTheKindAndTheDeclaration(t *testing.T) {
	spectest.Proves(t, "cli/job-model", "declared-verification-producers",
		"a-producer-is-a-declaring-task-of-a-command-of-its-kind")
	t.Parallel()
	verification := extension.CommandKindVerification
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
	undeclaredStep := ofKind(verification, declaredCaptureJob("test", "test", nil))
	undeclaredStep.Extension.Tasks["test"] = extension.TaskDefinition{}

	cases := map[string]struct {
		job  *ScheduledJob
		want bool
	}{
		"the declared report file": {
			job: ofKind(verification, declaredCaptureJob("test", "test", report(reportFile))), want: true},
		"the report beside the other test outputs": {
			job: ofKind(verification, declaredCaptureJob("test", "test", map[string]extension.DeclaredOutput{
				"featureVerification": reportFile,
				"coverage": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput,
					Path: "coverage.out"},
			})), want: true},
		"a declaration without outputs, like a test-env step": {
			job: ofKind(verification, declaredCaptureJob("test", "test-env", nil))},
		"the reserved name under the default project root": {
			job: ofKind(verification, declaredCaptureJob("test", "test",
				near(func(o *extension.DeclaredOutput) { o.Root = "" })))},
		"the reserved name under the workspace root": {
			job: ofKind(verification, declaredCaptureJob("test", "test",
				near(func(o *extension.DeclaredOutput) { o.Root = extension.OutputRootWorkspace })))},
		"an invocation-scoped report": {
			job: ofKind(verification, declaredCaptureJob("test", "test", near(func(o *extension.DeclaredOutput) {
				o.Root, o.Scope = "", extensionproto.OutputScopeInvocation
			})))},
		"a directory at the reserved name": {
			job: ofKind(verification, declaredCaptureJob("test", "test",
				near(func(o *extension.DeclaredOutput) { o.Kind = extension.OutputKindDirectory })))},
		"another command-output file": {
			job: ofKind(verification, declaredCaptureJob("test", "test",
				near(func(o *extension.DeclaredOutput) { o.Path = "coverage.out" })))},
		"the report file declared under a verification command of another name": {
			job: ofKind(verification, declaredCaptureJob("check", "run", report(reportFile))), want: true},
		"the report file declared under a test command without the verification kind": {
			job: declaredCaptureJob("test", "test", report(reportFile))},
		"the report file declared under a criteria command": {
			job: ofKind(extension.CommandKindCriteria, declaredCaptureJob("validate", "specs", report(reportFile)))},
		"a pipeline step whose task declares nothing": {
			job: undeclaredStep},
		"a verification command job with no pipeline step": {
			job:  ofKind(verification, &ScheduledJob{JobDef: &extension.JobDefinition{Name: "check", CommandName: "check"}}),
			want: true},
		"a test command job with no pipeline step and no kind": {
			job: &ScheduledJob{JobDef: &extension.JobDefinition{Name: "test", CommandName: "test"}}},
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

// TestProducesSpecCriteriaProjectionFollowsTheKindAndTheDeclaration pins how
// the spec gate tells a projection producer from every other task: only a
// task of a criteria command that declares a command-output FILE at the
// reserved filename counts, whatever the command is called. A job without a
// declaration never does.
func TestProducesSpecCriteriaProjectionFollowsTheKindAndTheDeclaration(t *testing.T) {
	spectest.Proves(t, "cli/job-model", "declared-verification-producers",
		"a-producer-is-a-declaring-task-of-a-command-of-its-kind")
	t.Parallel()
	criteria := extension.CommandKindCriteria
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
			job: ofKind(criteria, declaredCaptureJob("validate", "specs-validate", declared)), want: true},
		"the reserved name under the default project root": {
			job: ofKind(criteria, declaredCaptureJob("validate", "specs-validate",
				near(func(o *extension.DeclaredOutput) { o.Root = "" })))},
		"a directory at the reserved name": {
			job: ofKind(criteria, declaredCaptureJob("validate", "specs-validate",
				near(func(o *extension.DeclaredOutput) { o.Kind = extension.OutputKindDirectory })))},
		"another command-output file": {
			job: ofKind(criteria, declaredCaptureJob("validate", "specs-validate",
				near(func(o *extension.DeclaredOutput) { o.Path = "report.json" })))},
		"the projection file declared under a criteria command of another name": {
			job: ofKind(criteria, declaredCaptureJob("check", "specs-validate", declared)), want: true},
		"the projection file declared under a validate command without the criteria kind": {
			job: declaredCaptureJob("validate", "specs-validate", declared)},
		"the projection file declared under a verification command": {
			job: ofKind(extension.CommandKindVerification, declaredCaptureJob("test", "specs-validate", declared))},
		"a declaration without outputs": {
			job: ofKind(criteria, declaredCaptureJob("validate", "lint-specs", nil))},
		"a criteria command job with no pipeline step": {
			job: ofKind(criteria, &ScheduledJob{JobDef: &extension.JobDefinition{Name: "validate", CommandName: "validate"}})},
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

// TestCommandsOfKindFollowTheResolvedTraits pins how the spec gate finds the
// commands it plans: the commands of a kind are the commands whose resolved
// traits declare it in any extension, and the projection commands are the
// criteria commands one of whose pipeline steps runs a task that declares the
// projection. No command name decides, and each answer is sorted and free of
// repeats across extensions.
func TestCommandsOfKindFollowTheResolvedTraits(t *testing.T) {
	spectest.Proves(t, "cli/job-model", "declared-verification-producers",
		"a-producer-is-a-declaring-task-of-a-command-of-its-kind")
	t.Parallel()
	file := func(path string) *extension.TaskDeclaration {
		return &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
			"out": {Kind: extension.OutputKindFile, Root: extension.OutputRootCommandOutput, Path: path, OptionalEmpty: true},
		}}
	}
	pipeline := func(name, kind string, tasks ...string) *extension.JobDefinition {
		steps := make([]extension.PipelineStep, len(tasks))
		for i, task := range tasks {
			steps[i] = extension.PipelineStep{ID: task, Task: task}
		}
		return &extension.JobDefinition{Name: name, Kind: "command", PipelineSteps: steps,
			Traits: extension.CommandTraits{Kind: kind}}
	}
	verification, criteria := extension.CommandKindVerification, extension.CommandKindCriteria
	lang := &extension.ExtensionDescription{Name: "@acme/lang",
		Jobs: map[string]*extension.JobDefinition{
			"test":  pipeline("test", verification, "generate", "run-tests"),
			"check": pipeline("check", verification, "run-checks"),
			"build": pipeline("build", extensionproto.CommandKindFiles, "compile"),
			"empty": nil,
		},
		Tasks: map[string]extension.TaskDefinition{
			"generate":   {Declares: &extension.TaskDeclaration{}},
			"run-tests":  {Declares: file(features.VerificationReportFilename)},
			"run-checks": {Declares: file(features.VerificationReportFilename)},
			"compile":    {Declares: file("bin")},
		}}
	sdd := &extension.ExtensionDescription{Name: "@acme/sdd",
		Jobs: map[string]*extension.JobDefinition{
			"criteria": pipeline("criteria", criteria, "project-specs"),
			"validate": pipeline("validate", criteria, "lint-specs"),
			"audit":    pipeline("audit", "", "project-specs"),
		},
		Tasks: map[string]extension.TaskDefinition{
			"project-specs": {Declares: file(features.SpecCriteriaProjectionFilename)},
			"lint-specs":    {Declares: &extension.TaskDeclaration{}},
		}}
	// A test command whose manifest declared traits without a kind is not a
	// verification command, whatever its name.
	legacy := &extension.ExtensionDescription{Name: "@acme/legacy",
		Jobs: map[string]*extension.JobDefinition{
			"test":   {Name: "test", Kind: "command", TraitsDeclared: true, Traits: extension.CommandTraits{Heavy: true}},
			"verify": {Name: "verify", Kind: "command", Traits: extension.CommandTraits{Kind: verification}},
		}}
	extensions := []*extension.ExtensionDescription{sdd, nil, lang, legacy}

	if got := CommandsOfKind(extensions, verification); !slices.Equal(got, []string{"check", "test", "verify"}) {
		t.Errorf("verification commands = %v, want [check test verify]", got)
	}
	if got := CommandsOfKind(extensions, criteria); !slices.Equal(got, []string{"criteria", "validate"}) {
		t.Errorf("criteria commands = %v, want [criteria validate]", got)
	}
	if got := CommandsOfKind([]*extension.ExtensionDescription{legacy}, verification); !slices.Equal(got, []string{"verify"}) {
		t.Errorf("verification commands of the legacy extension = %v, want only verify", got)
	}
	if got := SpecCriteriaProjectionCommands(extensions); !slices.Equal(got, []string{"criteria"}) {
		t.Errorf("projection commands = %v, want [criteria]", got)
	}
	if got := CommandsOfKind(nil, verification); len(got) != 0 {
		t.Errorf("CommandsOfKind(nil) = %v, want none", got)
	}
	if got := SpecCriteriaProjectionCommands(nil); len(got) != 0 {
		t.Errorf("SpecCriteriaProjectionCommands(nil) = %v, want none", got)
	}
}

// TestCommandWorkloadFollowsTheResolvedTraits pins how a command's lifecycle
// and source rewriting are read: from the resolved traits of its jobs in any
// loaded extension, never from its name. serve wins over once across
// extensions, a step name answers for its command, and a serve or format
// command without the trait has neither.
func TestCommandWorkloadFollowsTheResolvedTraits(t *testing.T) {
	t.Parallel()
	command := func(name string, traits extension.CommandTraits) *extension.JobDefinition {
		return &extension.JobDefinition{Name: name, Kind: "command", Traits: traits}
	}
	serve, once := extension.WorkloadServe, extension.WorkloadOnce
	lang := &extension.ExtensionDescription{Name: "@acme/lang",
		Jobs: map[string]*extension.JobDefinition{
			"daemon":  command("daemon", extension.CommandTraits{Workload: serve}),
			"migrate": command("migrate", extension.CommandTraits{Workload: once}),
			"tidy":    command("tidy", extension.CommandTraits{RewritesSources: true}),
			"both":    command("both", extension.CommandTraits{Workload: once}),
			"empty":   nil,
		}}
	legacy := &extension.ExtensionDescription{Name: "@acme/legacy",
		Jobs: map[string]*extension.JobDefinition{
			"serve":  command("serve", extension.CommandTraits{RequiresRunnable: true}),
			"format": command("format", extension.CommandTraits{}),
			"both":   command("both", extension.CommandTraits{Workload: serve}),
		}}
	extensions := []*extension.ExtensionDescription{nil, lang, legacy}

	for command, want := range map[string]string{
		"daemon": serve, "daemon~run": serve, "migrate": once, "both": serve,
		"serve": "", "format": "", "tidy": "", "missing": "",
	} {
		if got := CommandWorkload(extensions, command); got != want {
			t.Errorf("CommandWorkload(%s) = %q, want %q", command, got, want)
		}
	}
	for command, want := range map[string]bool{"tidy": true, "tidy~apply": true, "format": false, "daemon": false} {
		if got := CommandRewritesSources(extensions, command); got != want {
			t.Errorf("CommandRewritesSources(%s) = %v, want %v", command, got, want)
		}
	}
	if got := CommandsOfWorkload(extensions, serve); !slices.Equal(got, []string{"both", "daemon"}) {
		t.Errorf("serve-workload commands = %v, want [both daemon]", got)
	}
	if got := CommandsOfWorkload(extensions, once); !slices.Equal(got, []string{"migrate"}) {
		t.Errorf("once-workload commands = %v, want [migrate]", got)
	}
	if got := CommandsOfWorkload(extensions, ""); len(got) != 0 {
		t.Errorf("CommandsOfWorkload(\"\") = %v, want none", got)
	}
	if got := CommandWorkload(nil, "serve"); got != "" {
		t.Errorf("CommandWorkload(nil, serve) = %q, want none", got)
	}
}
