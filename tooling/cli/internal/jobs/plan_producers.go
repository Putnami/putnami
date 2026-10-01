// Plan-time enforcement of typed producer requirements.
//
// A task that needs another task's output used to be expressed two ways, both
// of them core's guesses about somebody else's workload:
//
//   - `traits.preflight: config-schema`, which made core stat a project's infra
//     markers, parse its Go imports looking for `config.Config[…]`, and fail the
//     job when it could not find a schema artifact core does not own; and
//   - a fallback on the COMMAND NAME for the cloud commands that had not
//     declared the trait.
//
// Both are deleted. The replacement is the declaration that was already in the
// contract and never enforced: a `from: "task"` input port. Not `optional`
// means "I cannot run without this", and the consuming pipeline step names its
// producer in `with`. This file is what makes that statement true — a required
// port whose producer is missing from the PLAN fails planning, naming the
// consumer (job key, extension, task, port) and the producer that is absent,
// instead of letting the task discover mid-run that a file was never written.
//
// Why the plan and not the manifest: a manifest is static, a plan is not. `if`
// conditions and activation gates splice steps out per project and per
// parameter set, so the same manifest can leave a producer in one plan and drop
// it from another. Only the plan knows which of the two happened.
//
// A binding that is not a step reference — a literal `value`, a context `from`
// — satisfies the port without a producer job: the requirement is "this input
// has a source", not "a subprocess must run". Cross-project refs (`^`, `/`,
// `*`) are resolved by resolveExternalDeps and validated by the DAG check, so
// they are accepted here rather than re-resolved with a second, divergent rule.
package jobs

import (
	"fmt"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// producerStepKey is the identity a `with` binding names once resolved against
// the plan: the project, the extension that owns the pipeline, the command, and
// the step id. All four are needed — one extension's `generate` step is not
// another's, and the same step id under a different command is a different job.
func producerStepKey(projectID, extensionName, command, stepID string) string {
	return projectID + "\x00" + extensionName + "\x00" + command + "\x00" + stepID
}

// validatePlanProducers rejects a plan in which a required typed task input has
// no producer. Ordering is plan order, then sorted port name, so two runs over
// one workspace emit byte-identical diagnostics.
func validatePlanProducers(planned []*ScheduledJob) error {
	if len(planned) == 0 {
		return nil
	}
	if lines := missingProducerLines(planned, maxDAGDiagnosticLines); len(lines) > 0 {
		return fmt.Errorf("plan has required task inputs with no producer:\n%s", strings.Join(lines, "\n"))
	}
	return nil
}

// missingProducerLines reports every required task input port the plan cannot
// satisfy, naming both identities. Nodes that are not expanded pipeline steps
// contribute nothing, so a plain command job is never implicated.
func missingProducerLines(planned []*ScheduledJob, limit int) []string {
	present := make(map[string]bool, len(planned))
	for _, job := range planned {
		if job == nil || job.Project == nil || job.Extension == nil || job.StepID() == "" {
			continue
		}
		present[producerStepKey(job.Project.ID, job.Extension.Name, job.CommandName(), job.StepID())] = true
	}

	var lines []string
	total := 0
	for _, job := range planned {
		if job == nil || job.Step == nil || job.Step.Task == "" || job.Extension == nil || job.Project == nil {
			continue
		}
		task, ok := job.Extension.Tasks[job.Step.Task]
		if !ok {
			continue
		}
		// Canonical port order, never map-iteration order: the diagnostics of
		// two runs over one workspace must be byte-identical.
		var ports []string
		for name, port := range task.Inputs {
			if port.RequiresProducer() {
				ports = append(ports, name)
			}
		}
		sort.Strings(ports)

		for _, port := range ports {
			reason, missing := producerGap(job, port, present)
			if !missing {
				continue
			}
			total++
			if len(lines) < limit {
				lines = append(lines, fmt.Sprintf("  %s (extension %s, task %q) requires input %q, %s",
					job.Key(), plannedProviderName(job), job.Step.Task, port, reason))
			}
		}
	}
	if total > len(lines) {
		lines = append(lines, fmt.Sprintf("  ... and %d more unsatisfied inputs", total-len(lines)))
	}
	return lines
}

// producerGap decides whether one required port is unsatisfied and, if so, what
// to tell the manifest author. The two gaps are distinct faults with distinct
// fixes: an unbound port is a manifest that never named a producer, a pruned
// producer is a manifest that named one this plan did not keep.
func producerGap(job *ScheduledJob, port string, present map[string]bool) (string, bool) {
	binding, bound := job.Step.With[port]
	switch {
	case !bound:
		return "but its pipeline step binds no producer to it; declare the port `optional` if the task " +
			"handles absence, or bind it with `with`", true
	case binding.FromStep == "":
		// A literal value or a context binding is a source; no job produces it.
		return "", false
	case extension.IsExternalRef(binding.FromStep):
		// Cross-project producers are resolved and validated by the DAG check.
		return "", false
	case present[producerStepKey(job.Project.ID, job.Extension.Name, job.CommandName(), binding.FromStep)]:
		return "", false
	}
	return fmt.Sprintf("whose producer step %q is not in the plan (an `if` condition or an activation gate "+
		"excluded it); declare the port `optional` if the task handles absence", binding.FromStep), true
}
