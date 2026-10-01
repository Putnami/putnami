package extension

import (
	"fmt"
	"strings"
)

// ContractError describes a single contract validation failure.
type ContractError struct {
	Extension string
	Command   string
	Step      string
	Task      string
	Message   string
}

func (e *ContractError) Error() string {
	loc := e.Extension
	if e.Command != "" {
		loc += "." + e.Command
	}
	if e.Step != "" {
		loc += "." + e.Step
	}
	if e.Task != "" {
		loc += " (task: " + e.Task + ")"
	}
	return fmt.Sprintf("%s: %s", loc, e.Message)
}

// ValidateContracts validates task input/output port wiring across all
// extension commands. It checks at plan time that:
// 1. Input port names in step "with" bindings match declared task input ports
// 2. Output port names in "fromStep" bindings match declared task output ports
// 3. fromStep references point to valid step IDs within the pipeline
// 4. Command-level output bindings reference valid steps and paths
func ValidateContracts(extensions []*ExtensionDescription) []ContractError {
	var errors []ContractError

	for _, ext := range extensions {
		for cmdName := range ext.Commands {
			cmdDef := findCommandDef(ext, cmdName)
			if cmdDef == nil {
				continue
			}

			steps := cmdDef.PipelineSteps
			if len(steps) == 0 {
				continue
			}

			// Build maps of step IDs and their tasks
			stepIDs := make(map[string]bool, len(steps))
			stepTasks := make(map[string]string, len(steps)) // stepID → taskName
			for _, step := range steps {
				stepIDs[step.ID] = true
				stepTasks[step.ID] = step.Task
			}

			for _, step := range steps {
				task, hasTask := ext.Tasks[step.Task]
				if !hasTask {
					errors = append(errors, ContractError{
						Extension: ext.Name,
						Command:   cmdName,
						Step:      step.ID,
						Message:   fmt.Sprintf("references unknown task %q", step.Task),
					})
					continue
				}

				// Validate step "with" bindings against task input ports
				errors = append(errors, validateStepInputs(ext.Name, cmdName, step, task, stepIDs, stepTasks, ext.Tasks)...)

				// Validate step dependency references
				errors = append(errors, validateStepDeps(ext.Name, cmdName, step, stepIDs)...)
			}

			// Validate command-level output bindings
			if cmdDef.PipelineOutputs != nil {
				errors = append(errors, validateCommandOutputs(ext.Name, cmdName, cmdDef.PipelineOutputs, stepIDs, stepTasks, ext.Tasks)...)
			}
		}
	}

	return errors
}

// validateStepInputs checks that "with" bindings match declared task input ports.
func validateStepInputs(
	extName, cmdName string,
	step PipelineStep,
	task TaskDefinition,
	stepIDs map[string]bool,
	stepTasks map[string]string,
	allTasks map[string]TaskDefinition,
) []ContractError {
	var errors []ContractError

	for bindingName, binding := range step.With {
		// Check that the binding name matches a declared task input port (if inputs are declared)
		if len(task.Inputs) > 0 {
			if _, declared := task.Inputs[bindingName]; !declared {
				declaredNames := taskInputNames(task)
				errors = append(errors, ContractError{
					Extension: extName,
					Command:   cmdName,
					Step:      step.ID,
					Task:      step.Task,
					Message: fmt.Sprintf(
						"binding %q does not match any declared input port; declared inputs: [%s]",
						bindingName, strings.Join(declaredNames, ", ")),
				})
			}
		}

		// For fromStep bindings, validate the step reference and output port
		if binding.FromStep != "" {
			if !stepIDs[binding.FromStep] && !IsExternalRef(binding.FromStep) {
				errors = append(errors, ContractError{
					Extension: extName,
					Command:   cmdName,
					Step:      step.ID,
					Task:      step.Task,
					Message:   fmt.Sprintf("binding %q references unknown step %q", bindingName, binding.FromStep),
				})
			} else if binding.Output != "" && stepIDs[binding.FromStep] {
				// Check that the referenced output port exists on the source task
				sourceTaskName := stepTasks[binding.FromStep]
				if sourceTask, ok := allTasks[sourceTaskName]; ok && len(sourceTask.Outputs) > 0 {
					if _, declared := sourceTask.Outputs[binding.Output]; !declared {
						declaredNames := taskOutputNames(sourceTask)
						errors = append(errors, ContractError{
							Extension: extName,
							Command:   cmdName,
							Step:      step.ID,
							Task:      step.Task,
							Message: fmt.Sprintf(
								"binding %q references output %q from step %q (task %q), but task declares outputs: [%s]",
								bindingName, binding.Output, binding.FromStep, sourceTaskName,
								strings.Join(declaredNames, ", ")),
						})
					}
				}
			}
		}
	}

	return errors
}

// validateStepDeps checks that dependsOn references point to valid steps.
func validateStepDeps(extName, cmdName string, step PipelineStep, stepIDs map[string]bool) []ContractError {
	var errors []ContractError
	for _, dep := range step.DependsOn {
		if IsExternalRef(dep) {
			continue // External refs are validated at plan time by the planner
		}
		if !stepIDs[dep] {
			errors = append(errors, ContractError{
				Extension: extName,
				Command:   cmdName,
				Step:      step.ID,
				Message:   fmt.Sprintf("dependsOn references unknown step %q", dep),
			})
		}
	}
	return errors
}

// validateCommandOutputs checks that command-level output bindings reference valid steps.
func validateCommandOutputs(
	extName, cmdName string,
	outputs map[string]OutputBinding,
	stepIDs map[string]bool,
	_ map[string]string,
	_ map[string]TaskDefinition,
) []ContractError {
	var errors []ContractError
	for outputName, binding := range outputs {
		if !stepIDs[binding.FromStep] {
			errors = append(errors, ContractError{
				Extension: extName,
				Command:   cmdName,
				Message: fmt.Sprintf(
					"command output %q references unknown step %q",
					outputName, binding.FromStep),
			})
		}
	}
	return errors
}

// --- Helpers ---

func findCommandDef(ext *ExtensionDescription, cmdName string) *JobDefinition {
	// Look for a job that matches the command name (may be the root job or a pipeline)
	for name, job := range ext.Jobs {
		if name == cmdName {
			return job
		}
	}
	return nil
}

func taskInputNames(task TaskDefinition) []string {
	names := make([]string, 0, len(task.Inputs))
	for name := range task.Inputs {
		names = append(names, name)
	}
	return names
}

func taskOutputNames(task TaskDefinition) []string {
	names := make([]string, 0, len(task.Outputs))
	for name := range task.Outputs {
		names = append(names, name)
	}
	return names
}
