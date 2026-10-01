package jobs

import (
	"go.putnami.dev/cli/model/extension"
	features "go.putnami.dev/protocol/features"
)

// ProducesVerificationReport reports whether a planned job writes its project's
// reserved feature verification report. It is the one rule for two questions:
// the spec gate asks it which projects' tests ran in a session, and the
// cache lookup asks it which of a candidate's jobs can hold a recoverable report.
//
// The command name alone is not that fact. A narrowed run plans each
// dependency's `test~generate` and `test~config-merge` as prerequisites, and a
// test environment adds `test~test-env` and its teardown. All of them carry
// command `test`; none runs the suite or writes the report.
//
// A v3 declaration is the task's closed filesystem footprint, so when one exists
// it decides: the job writes the report only when it declares a command-output
// FILE at the reserved filename, which is the output a task-owned cache entry
// records and a warm hit restores.
//
// With NO declaration the job's shape decides, and the two shapes are not the
// same fact:
//
//   - a job with no pipeline step is the WHOLE test command. It runs the suite
//     and may write the report, so it counts. Only a declared capture publishes
//     a task-owned entry, so the store holds none for it, and the cache lookup
//     reads the project unreachable — the honest answer for evidence no entry
//     can hold.
//   - a pipeline STEP whose task declares nothing says nothing about the report,
//     and nothing claims it runs the suite, so it does not count. A `test`
//     pipeline built only of such steps therefore has no report producer: its
//     project reads as consulted and silent, which blocks rather than warns.
func ProducesVerificationReport(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil || job.CommandName() != "test" {
		return false
	}
	declaration := TaskDeclarationOf(job)
	if declaration == nil {
		return job.Step == nil
	}
	for _, output := range declaration.Outputs {
		if output.Kind == extension.OutputKindFile &&
			output.EffectiveRoot() == extension.OutputRootCommandOutput &&
			output.Path == features.VerificationReportFilename {
			return true
		}
	}
	return false
}

// TaskDeclarationOf returns the v3 declaration of the manifest task the plan
// node executes, or nil when the node is a v2 task (or has no manifest task at
// all, as a non-pipeline command job does).
func TaskDeclarationOf(job *ScheduledJob) *extension.TaskDeclaration {
	if job == nil || job.Step == nil || job.Step.Task == "" || job.Extension == nil {
		return nil
	}
	task, ok := job.Extension.Tasks[job.Step.Task]
	if !ok {
		return nil
	}
	return task.Declares
}
