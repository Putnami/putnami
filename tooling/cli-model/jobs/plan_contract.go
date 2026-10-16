package jobs

import (
	"sort"

	"go.putnami.dev/cli/model/extension"
	features "go.putnami.dev/protocol/features"
)

// ProducesVerificationReport reports whether a planned job writes its project's
// reserved feature verification report. It is the one rule for every question
// about the report: the spec gate asks it which projects' verification ran in
// a session and which results carry observations, the cache lookup asks it
// which of a candidate's jobs can hold a recoverable report, and a warm hit
// asks it which artifact record to restore.
//
// Only a job of a command whose resolved traits declare the verification kind
// writes it. The kind alone is not that fact: a narrowed run plans each
// dependency's `test~generate` and `test~config-merge` as prerequisites, and a
// test environment adds `test~test-env` and its teardown. All of them belong
// to the verification command; none runs the suite or writes the report.
//
// A v3 declaration is the task's closed filesystem footprint, so when one exists
// it decides: the job writes the report only when it declares a command-output
// FILE at the reserved filename, which is the output a task-owned cache entry
// records and a warm hit restores.
//
// With NO declaration the job's shape decides, and the two shapes are not the
// same fact:
//
//   - a job with no pipeline step is the WHOLE verification command. It runs
//     the suite and may write the report, so it counts. Only a declared capture
//     publishes a task-owned entry, so the store holds none for it, and the
//     cache lookup reads the project unreachable — the honest answer for
//     evidence no entry can hold.
//   - a pipeline STEP whose task declares nothing says nothing about the report,
//     and nothing claims it runs the suite, so it does not count. A verification
//     pipeline built only of such steps therefore has no report producer: its
//     project reads as consulted and silent, which blocks rather than warns.
func ProducesVerificationReport(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil || job.JobDef.Traits.Kind != extension.CommandKindVerification {
		return false
	}
	declaration := TaskDeclarationOf(job)
	if declaration == nil {
		return job.Step == nil
	}
	return declaresCommandOutputFile(declaration, features.VerificationReportFilename)
}

// ProducesSpecCriteriaProjection reports whether a planned job is a project's
// criteria-projection producer: a task of a command whose resolved traits
// declare the criteria kind, whose v3 declaration names a command-output FILE
// at the reserved projection filename. The declaration marks that output
// optional and empty when the project's specs state no requirement, so a
// producer that succeeded without emitting it answers "this project states no
// requirement". A job without a declaration is never a producer.
func ProducesSpecCriteriaProjection(job *ScheduledJob) bool {
	if job == nil || job.JobDef == nil || job.JobDef.Traits.Kind != extension.CommandKindCriteria {
		return false
	}
	declaration := TaskDeclarationOf(job)
	return declaration != nil && declaresCommandOutputFile(declaration, features.SpecCriteriaProjectionFilename)
}

// CommandsOfKind returns, sorted, the commands of extensions whose resolved
// traits declare kind in at least one extension: the command's own traits when
// its manifest declares them, otherwise its verb's in the merged vocabulary.
func CommandsOfKind(extensions []*extension.ExtensionDescription, kind string) []string {
	found := make(map[string]bool)
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		for command, definition := range ext.Jobs {
			if definition != nil && definition.Traits.Kind == kind {
				found[command] = true
			}
		}
	}
	return sortedCommands(found)
}

// SpecCriteriaProjectionCommands returns, sorted, the criteria commands of
// extensions one of whose pipeline steps runs a task that declares the
// projection (ProducesSpecCriteriaProjection). The spec gate plans them to
// learn which projects a producer applies to. It reads manifest definitions
// only, so step conditions and activation stay the planner's: a listed command
// can still plan no producer for a given project.
func SpecCriteriaProjectionCommands(extensions []*extension.ExtensionDescription) []string {
	found := make(map[string]bool)
	for _, ext := range extensions {
		if ext == nil {
			continue
		}
		for command, definition := range ext.Jobs {
			if definition == nil || found[command] {
				continue
			}
			probe := &ScheduledJob{Extension: ext, JobDef: definition}
			for i := range definition.PipelineSteps {
				probe.Step = &definition.PipelineSteps[i]
				if ProducesSpecCriteriaProjection(probe) {
					found[command] = true
					break
				}
			}
		}
	}
	return sortedCommands(found)
}

func sortedCommands(found map[string]bool) []string {
	commands := make([]string, 0, len(found))
	for command := range found {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	return commands
}

// declaresCommandOutputFile reports whether declaration names a command-output
// file at path, the shape a task-owned cache entry records and a warm hit
// restores.
func declaresCommandOutputFile(declaration *extension.TaskDeclaration, path string) bool {
	for _, output := range declaration.Outputs {
		if output.Kind == extension.OutputKindFile &&
			output.EffectiveRoot() == extension.OutputRootCommandOutput &&
			output.Path == path {
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
