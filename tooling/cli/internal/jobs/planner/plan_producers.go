// Plan-time enforcement of typed producer requirements.
//
// A task that needs another task's output declares a required `from: "task"`
// input port. Not `optional` means "I cannot run without this", and the
// consuming pipeline step names its producer in `with`. This file is what makes
// that statement true — a required port whose producer is missing from the
// PLAN fails planning, naming the consumer (job key, extension, task, port) and
// the producer that is absent, instead of letting the task discover mid-run
// that a file was never written.
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
package planner

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
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
func validatePlanProducers(planned []*jobmodel.ScheduledJob) error {
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
func missingProducerLines(planned []*jobmodel.ScheduledJob, limit int) []string {
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
func producerGap(job *jobmodel.ScheduledJob, port string, present map[string]bool) (string, bool) {
	binding, bound := job.Step.With[port]
	switch {
	case !bound:
		return "but its pipeline step binds no producer to it; declare the port `optional` if the task " +
			"handles absence, or bind it with `with`", true
	case binding.FromStep == "":
		// A literal value or a context binding is a source; no job produces it.
		return "", false
	case extension.IsExternalRef(binding.FromStep):
		// Cross-project producers are resolved and validated by the DAG check,
		// except an artifact input's, which needs one direct dependency that
		// runs its step.
		for _, input := range job.ArtifactInputs {
			if input.Port == port && input.Upstream && len(input.Producers) == 0 {
				return fmt.Sprintf("whose producer step %q runs in no direct dependency; declare the port `optional` "+
					"if the task handles absence", binding.FromStep), true
			}
		}
		return "", false
	case present[producerStepKey(job.Project.ID, job.Extension.Name, job.CommandName(), binding.FromStep)]:
		return "", false
	}
	return fmt.Sprintf("whose producer step %q is not in the plan (an `if` condition or an activation gate "+
		"excluded it); declare the port `optional` if the task handles absence", binding.FromStep), true
}

// resolveArtifactInputs stamps the artifact inputs of the plan.
//
// A task input port that declares `fold: "artifact"` keys its consumer on the
// digest of the producer output its step binds, not on the producer's cache
// key (CLI ADR 0043). The manifest names the producer by step: one of the same
// command, extension and project, or `^step`, that step of the same command in
// every direct dependency that runs it, the jobs a `^step` dependency resolves
// to (resolveExternalDeps). The binding is an edge
// (extension.WithArtifactEdges adds it to the step's dependsOn when the
// manifest resolves), so every producer resolved here already is one of the
// consumer's functional dependencies.
//
// It stamps ArtifactInputs on each consumer and, on each producer, the output
// ports some consumer binds (ArtifactOutputs), the digests the scheduler
// publishes with the producer's result. It rejects a plan in which a producer
// does not declare the bound output as an artifact (extension.ArtifactOutputFor).
// A required port without a producer is validatePlanProducers' to report.
// Diagnostics follow plan order, then port.
func resolveArtifactInputs(jobs []*jobmodel.ScheduledJob, ws *workspace.Workspace) error {
	idx := buildPlannedIndex(jobs)
	outputs := make(map[string]map[string]bool)
	var problems []string
	for _, job := range jobs {
		if job == nil || job.Step == nil || job.Extension == nil || job.Project == nil || job.JobDef == nil {
			continue
		}
		task, ok := job.Extension.Tasks[job.Step.Task]
		if !ok {
			continue
		}
		bindings := extension.StepArtifactBindings(*job.Step, task)
		if len(bindings) == 0 {
			continue
		}
		inputs := make([]jobmodel.ArtifactInput, 0, len(bindings))
		for _, binding := range bindings {
			input := jobmodel.ArtifactInput{
				Port:     binding.Port,
				Output:   binding.Output,
				Upstream: binding.Upstream,
				Optional: binding.Optional,
			}
			for _, key := range artifactProducerKeys(job, binding, idx, ws) {
				producer := idx.byKey[key]
				producerTask, declared := producer.Extension.Tasks[producer.Step.Task]
				if !declared {
					continue
				}
				if _, _, err := extension.ArtifactOutputFor(producerTask, binding.Output); err != nil {
					reason := err.Error()
					var typed *extension.ArtifactOutputError
					if errors.As(err, &typed) {
						reason = typed.Message + " (" + typed.Code + ")"
					}
					problems = append(problems, fmt.Sprintf("  %s (extension %s, task %q) binds input %q to output %q of %s, whose task %q: %s",
						job.Key(), plannedProviderName(job), job.Step.Task, binding.Port, binding.Output, key, producer.Step.Task, reason))
					continue
				}
				input.Producers = append(input.Producers, jobmodel.ArtifactProducer{Key: key, Project: producer.Project.ID})
				if outputs[key] == nil {
					outputs[key] = make(map[string]bool)
				}
				outputs[key][binding.Output] = true
				if !slices.Contains(job.DependsOn, key) {
					job.DependsOn = jobmodel.DedupeSorted(append(job.DependsOn, key))
				}
			}
			sort.Slice(input.Producers, func(i, j int) bool {
				a, b := input.Producers[i], input.Producers[j]
				if a.Project != b.Project {
					return a.Project < b.Project
				}
				return a.Key < b.Key
			})
			inputs = append(inputs, input)
		}
		job.ArtifactInputs = inputs
	}
	for key, ports := range outputs {
		names := make([]string, 0, len(ports))
		for port := range ports {
			names = append(names, port)
		}
		sort.Strings(names)
		idx.byKey[key].ArtifactOutputs = names
	}
	if len(problems) == 0 {
		return nil
	}
	if total := len(problems); total > maxDAGDiagnosticLines {
		problems = append(problems[:maxDAGDiagnosticLines], fmt.Sprintf("  ... and %d more artifact inputs", total-maxDAGDiagnosticLines))
	}
	return fmt.Errorf("plan has artifact inputs whose producer declares no such artifact:\n%s", strings.Join(problems, "\n"))
}

// artifactProducerKeys returns the planned pipeline-step jobs one artifact
// binding of job names, sorted: steps of job's own extension, since the
// binding names a step of the manifest that declares it.
func artifactProducerKeys(job *jobmodel.ScheduledJob, binding extension.ArtifactBinding, idx *plannedIndex, ws *workspace.Workspace) []string {
	command := job.CommandName()
	var keys []string
	if binding.Upstream {
		if ws == nil || ws.Graph == nil {
			return nil
		}
		for _, dependency := range ws.Graph.UpstreamOf(job.Project.ID) {
			for _, key := range idx.matchingKeys(dependency, command, binding.Step) {
				if producer := idx.byKey[key]; producer != nil && producer.Extension != nil && producer.Extension.Name == job.Extension.Name {
					keys = append(keys, key)
				}
			}
		}
	} else {
		for _, candidate := range idx.byProject[job.Project.ID] {
			if candidate != job && candidate.Extension != nil && candidate.Extension.Name == job.Extension.Name &&
				candidate.CommandName() == command && candidate.StepID() == binding.Step {
				keys = append(keys, candidate.Key())
			}
		}
	}
	return slices.DeleteFunc(jobmodel.DedupeSorted(keys), func(key string) bool {
		producer := idx.byKey[key]
		return producer == nil || producer.Project == nil || producer.Step == nil || producer.Extension == nil
	})
}
