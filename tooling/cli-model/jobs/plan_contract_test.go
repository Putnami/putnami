package jobs

import (
	"testing"

	"go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
)

// TestProducesVerificationReportFollowsTheDeclaration pins the one rule both
// halves of the spec gate share. A v3 declaration is a closed
// footprint: only a command-output FILE at the reserved filename, on the test
// command, writes the report, and every near miss does not. Without a
// declaration the job's shape decides: the whole test command counts, a
// pipeline step that declares nothing does not.
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
		"the report file declared outside the test command": {
			job: declaredCaptureJob("build", "compile", report(reportFile))},
		"a pipeline step whose task declares nothing": {
			job: undeclaredStep},
		"a test command job with no pipeline step": {
			job: &ScheduledJob{JobDef: &extension.JobDefinition{Name: "test", CommandName: "test"}}, want: true},
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
