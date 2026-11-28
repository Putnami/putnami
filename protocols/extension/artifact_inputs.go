package extension

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// UpstreamStepPrefix opens an InputBinding.FromStep that names a step of the
// direct dependencies: "^tsc" binds the step tsc of the same command in every
// direct dependency project that runs it.
const UpstreamStepPrefix = "^"

// BoundStep splits a binding's fromStep into the step id it names and whether
// it names that step in the direct dependencies.
func BoundStep(fromStep string) (string, bool) {
	if step, ok := strings.CutPrefix(fromStep, UpstreamStepPrefix); ok {
		return step, true
	}
	return fromStep, false
}

// ArtifactBinding is the step binding of one input port that folds an
// artifact (TaskInputPort.FoldsArtifact).
type ArtifactBinding struct {
	// Port is the consumer task's input port.
	Port string
	// Step is the producer step id, without UpstreamStepPrefix.
	Step string
	// Upstream reports that the binding names Step in every direct dependency
	// that runs it.
	Upstream bool
	// Output is the producer task's output port.
	Output string
	// Optional is the port's optional flag: without a producer in the plan,
	// the port folds a fixed absent value instead of failing the plan.
	Optional bool
}

// StepArtifactBindings returns, sorted by port, the step's bindings of the
// task's input ports that fold an artifact. A port the step binds by a value
// or a context, or does not bind, has no producer step and is not listed.
func StepArtifactBindings(step PipelineStep, task TaskDefinition) []ArtifactBinding {
	var bindings []ArtifactBinding
	for _, port := range sortedKeys(task.Inputs) {
		declared := task.Inputs[port]
		binding, bound := step.With[port]
		if !declared.FoldsArtifact() || !bound || binding.FromStep == "" {
			continue
		}
		id, upstream := BoundStep(binding.FromStep)
		bindings = append(bindings, ArtifactBinding{
			Port:     port,
			Step:     id,
			Upstream: upstream,
			Output:   binding.Output,
			Optional: declared.Optional,
		})
	}
	return bindings
}

// WithArtifactEdges returns steps with each step's dependsOn completed by the
// edges its artifact bindings imply: the bound step of the same command when
// the command runs it, and the `^step` reference of an upstream binding. A
// binding is an edge, so a step need not repeat it in dependsOn. steps is not
// modified; a step whose bindings imply nothing new is returned as it is.
func WithArtifactEdges(steps []PipelineStep, tasks map[string]TaskDefinition) []PipelineStep {
	ids := make(map[string]bool, len(steps))
	for _, step := range steps {
		ids[step.ID] = true
	}
	var out []PipelineStep
	for i, step := range steps {
		task, ok := tasks[step.Task]
		var added []string
		if ok {
			for _, binding := range StepArtifactBindings(step, task) {
				edge := binding.Step
				if binding.Upstream {
					edge = UpstreamStepPrefix + binding.Step
				} else if !ids[binding.Step] || binding.Step == step.ID {
					continue
				}
				if !slices.Contains(step.DependsOn, edge) && !slices.Contains(added, edge) {
					added = append(added, edge)
				}
			}
		}
		if len(added) == 0 {
			if out != nil {
				out = append(out, step)
			}
			continue
		}
		if out == nil {
			out = append(make([]PipelineStep, 0, len(steps)), steps[:i]...)
		}
		step.DependsOn = append(slices.Clone(step.DependsOn), added...)
		out = append(out, step)
	}
	if out == nil {
		return steps
	}
	return out
}

// ArtifactOutputError is why an output port names no artifact the engine can
// digest. Code is the stable diagnostic code.
type ArtifactOutputError struct {
	Code    string
	Message string
}

func (e *ArtifactOutputError) Error() string { return e.Message }

// ArtifactOutputFor returns the declared output an artifact binding of the
// task's output port digests, with its id.
//
// The output port corresponds to the one durable declared output of the task
// (declares.outputs) whose id is the port name or whose pathFrom names the
// port. A port that states a kind states that output's kind, and a port that
// states a path states that output's literal path. A port the task does not
// declare, no corresponding output, two of them, an invocation-scoped one, or
// a kind or a path that disagrees is an ArtifactOutputError.
func ArtifactOutputFor(task TaskDefinition, port string) (string, DeclaredOutput, error) {
	outputPort, declared := task.Outputs[port]
	if !declared {
		return "", DeclaredOutput{}, &ArtifactOutputError{Code: "unknown-reference",
			Message: fmt.Sprintf("output %q is not an output port of the task", port)}
	}
	var ids []string
	if task.Declares != nil {
		for id, output := range task.Declares.Outputs {
			if id == port || (output.PathFrom != "" && output.PathFrom == port) {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	switch len(ids) {
	case 0:
		return "", DeclaredOutput{}, &ArtifactOutputError{Code: "undeclared-artifact",
			Message: fmt.Sprintf("output port %q names no declared output: an artifact is a declares.outputs entry whose id is the port or whose pathFrom names it", port)}
	case 1:
	default:
		return "", DeclaredOutput{}, &ArtifactOutputError{Code: "ambiguous-artifact",
			Message: fmt.Sprintf("output port %q names the declared outputs %s; an artifact names exactly one", port, strings.Join(ids, ", "))}
	}
	id := ids[0]
	output := task.Declares.Outputs[id]
	if output.IsInvocationScoped() {
		return "", DeclaredOutput{}, &ArtifactOutputError{Code: "invalid-artifact-output",
			Message: fmt.Sprintf("output port %q names the invocation-scoped output %q, which no key may read", port, id)}
	}
	if outputPort.Kind != "" && outputPort.Kind != output.Kind {
		return "", DeclaredOutput{}, &ArtifactOutputError{Code: "invalid-artifact-output",
			Message: fmt.Sprintf("output port %q is a %s, and the declared output %q a %s", port, outputPort.Kind, id, output.Kind)}
	}
	if outputPort.Path != "" {
		portPath, portErr := NormalizeOutputPath(outputPort.Path)
		outputPath, outputErr := NormalizeOutputPath(output.Path)
		if output.Path == "" || portErr != nil || outputErr != nil || portPath != outputPath {
			return "", DeclaredOutput{}, &ArtifactOutputError{Code: "invalid-artifact-output",
				Message: fmt.Sprintf("output port %q states path %q, and the declared output %q does not declare that literal path", port, outputPort.Path, id)}
		}
	}
	return id, output, nil
}

// DeclaresArtifactInputs reports whether the manifest uses the vocabulary of
// protocolcli.ArtifactInputsContract: a task input port that declares fold, or
// a task output port that declares artifactExcludes. A step binding whose
// fromStep names a step of the direct dependencies (^step) binds an artifact
// only through a port that folds one, so it adds nothing of its own: a
// manifest that declares no fold keeps such a binding inert, as contracts
// below 32 read it.
func DeclaresArtifactInputs(m *Manifest) bool {
	if m == nil {
		return false
	}
	for _, task := range m.Tasks {
		for _, port := range task.Inputs {
			if port.Fold != "" {
				return true
			}
		}
		for _, port := range task.Outputs {
			if len(port.ArtifactExcludes) > 0 {
				return true
			}
		}
	}
	return false
}

// ArtifactExcludes returns the canonical subpaths the artifact of the task's
// output port leaves out of its digest (TaskOutputPort.ArtifactExcludes),
// sorted, once each. An entry that does not normalize is dropped: its bytes
// stay in the digest, which validation reports.
func ArtifactExcludes(task TaskDefinition, port string) []string {
	declared := task.Outputs[port].ArtifactExcludes
	if len(declared) == 0 {
		return nil
	}
	excludes := make([]string, 0, len(declared))
	for _, entry := range declared {
		if normalized, err := NormalizeOutputPath(entry); err == nil {
			excludes = append(excludes, normalized)
		}
	}
	sort.Strings(excludes)
	return slices.Compact(excludes)
}

// ValidateArtifactInputs checks the artifact-input vocabulary: a fold from the
// closed vocabulary on a task port only, a `^step` binding only for a port that
// folds an artifact, each binding of such a port naming a producer step and an
// output port that corresponds to a declared output (ArtifactOutputFor), and
// artifactExcludes only on a port that names a directory artifact. A `^step`
// binding whose step this manifest's command does not run is resolved against
// the plan, so it is not checked here. Inert for a manifest that declares none
// of it (DeclaresArtifactInputs), whose `^step` bindings stay inert as before
// contract 32.
func ValidateArtifactInputs(m *Manifest) []diag.Diagnostic {
	if !DeclaresArtifactInputs(m) {
		return nil
	}
	var diags []diag.Diagnostic
	for _, name := range sortedKeys(m.Tasks) {
		task := m.Tasks[name]
		for _, port := range sortedKeys(task.Outputs) {
			diags = append(diags, validateArtifactExcludes(fmt.Sprintf("tasks.%s.outputs.%s.artifactExcludes", name, port), task, port)...)
		}
		for _, port := range sortedKeys(task.Inputs) {
			declared := task.Inputs[port]
			field := fmt.Sprintf("tasks.%s.inputs.%s.fold", name, port)
			switch {
			case declared.Fold == "":
			case !slices.Contains(ValidTaskInputFolds, declared.Fold):
				diags = append(diags, diag.Errorf("invalid-enum", field,
					"invalid fold %q; must be one of: %s", declared.Fold, strings.Join(ValidTaskInputFolds, ", ")))
			case declared.From != TaskInputFromTask:
				diags = append(diags, diag.Errorf("invalid-value", field,
					"fold %q is valid only on a %q input port, not on one from %q", declared.Fold, TaskInputFromTask, declared.From))
			}
		}
	}
	for _, name := range sortedKeys(m.Commands) {
		command := m.Commands[name]
		steps := make(map[string]PipelineStep, len(command.Run))
		for _, step := range command.Run {
			steps[step.ID] = step
		}
		for i, step := range command.Run {
			task := m.Tasks[step.Task]
			for _, port := range sortedKeys(step.With) {
				binding := step.With[port]
				field := fmt.Sprintf("commands.%s.run[%d].with.%s", name, i, port)
				declared := task.Inputs[port]
				id, upstream := BoundStep(binding.FromStep)
				if !declared.FoldsArtifact() {
					if upstream {
						diags = append(diags, diag.Errorf("invalid-upstream-binding", field+".fromStep",
							"binding %q names %q, a step of the direct dependencies, which binds only an input port that declares fold %q",
							port, binding.FromStep, TaskInputFoldArtifact))
					}
					continue
				}
				diags = append(diags, validateArtifactBinding(m, field, port, binding, step, steps, id, upstream)...)
			}
		}
	}
	return diags
}

// validateArtifactExcludes checks the artifactExcludes of one output port: the
// port names a directory artifact, and each entry is a canonical relative path
// declared once.
func validateArtifactExcludes(field string, task TaskDefinition, port string) []diag.Diagnostic {
	declared := task.Outputs[port].ArtifactExcludes
	if len(declared) == 0 {
		return nil
	}
	var diags []diag.Diagnostic
	if _, output, err := ArtifactOutputFor(task, port); err != nil {
		code := "invalid-artifact-output"
		typed := &ArtifactOutputError{}
		if errors.As(err, &typed) {
			code = typed.Code
		}
		diags = append(diags, diag.Errorf(code, field, "artifactExcludes of output port %q: %s", port, err))
	} else if output.Kind != OutputKindDirectory {
		diags = append(diags, diag.Errorf("invalid-artifact-exclude", field,
			"only a port that names a %q artifact leaves a subpath out of its digest; output port %q names a %s",
			OutputKindDirectory, port, output.Kind))
	}
	seen := make(map[string]string, len(declared))
	for i, entry := range declared {
		entryField := fmt.Sprintf("%s[%d]", field, i)
		normalized, err := NormalizeOutputPath(entry)
		if err != nil || normalized != entry {
			diags = append(diags, diag.Errorf("invalid-output-path", entryField,
				"artifact exclude %q is not a canonical relative path inside the output", entry))
			continue
		}
		if first, duplicate := seen[normalized]; duplicate {
			diags = append(diags, diag.Errorf("duplicate-artifact-exclude", entryField,
				"artifact exclude %q is declared more than once (first as %q)", entry, first))
			continue
		}
		seen[normalized] = entry
	}
	return diags
}

// validateArtifactBinding checks one binding of an input port that folds an
// artifact.
func validateArtifactBinding(
	m *Manifest,
	field, port string,
	binding InputBinding,
	step PipelineStep,
	steps map[string]PipelineStep,
	id string,
	upstream bool,
) []diag.Diagnostic {
	if binding.FromStep == "" {
		return []diag.Diagnostic{diag.Errorf("invalid-artifact-binding", field,
			"input port %q folds an artifact, so its binding names a producer step (fromStep) and its output port (output)", port)}
	}
	if id == "" {
		return []diag.Diagnostic{diag.Errorf("invalid-artifact-binding", field+".fromStep",
			"binding %q names no step", port)}
	}
	if binding.Output == "" {
		return []diag.Diagnostic{diag.Errorf("required-field", field+".output",
			"binding %q of an artifact input names the producer's output port", port)}
	}
	if !upstream && id == step.ID {
		return []diag.Diagnostic{diag.Errorf("invalid-artifact-binding", field+".fromStep",
			"binding %q names its own step %q; an artifact comes from another step", port, id)}
	}
	producer, found := steps[id]
	if !found {
		if upstream {
			return nil
		}
		return []diag.Diagnostic{diag.Errorf("unknown-reference", field+".fromStep",
			"binding %q names step %q, which this command does not run", port, id)}
	}
	producerTask, known := m.Tasks[producer.Task]
	if !known {
		return nil
	}
	if _, _, err := ArtifactOutputFor(producerTask, binding.Output); err != nil {
		code := "invalid-artifact-output"
		typed := &ArtifactOutputError{}
		if errors.As(err, &typed) {
			code = typed.Code
		}
		return []diag.Diagnostic{diag.Errorf(code, field+".output",
			"binding %q: task %q of step %q: %s", port, producer.Task, id, err)}
	}
	return nil
}
