package extension

import (
	"fmt"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// StepSeparator separates a command from one of its steps: in a plan name
// (build~compile) and in a command dependency on one step (build~describe).
const StepSeparator = "~"

// SessionBarrierPrefix marks a command dependency as a session barrier: the
// prerequisite command runs across the run's project selection.
const SessionBarrierPrefix = "!"

// CommandDependency is one entry of a command's dependsOn, read by
// ParseCommandDependency.
type CommandDependency struct {
	// Command is the prerequisite command. The planner plans all of it.
	Command string
	// Step is the one step of Command the dependent waits for. Empty means
	// the dependent waits for every leaf step of Command.
	Step string
	// Session is true for a session barrier (`!command`): the prerequisite
	// runs across the run's selection instead of the dependent's project.
	Session bool
}

// ParseCommandDependency reads one entry of a command's dependsOn: `command`,
// `command~step` or `!command`. It splits at the first StepSeparator and
// never fails; strict validation (validateCommandDependency) refuses the
// entries that are not one of the three forms. Every reader of a command's
// dependsOn uses it, so each one names the same command for an entry.
func ParseCommandDependency(dependency string) CommandDependency {
	parsed := CommandDependency{Command: dependency}
	if rest, ok := strings.CutPrefix(dependency, SessionBarrierPrefix); ok {
		parsed.Command, parsed.Session = rest, true
	}
	if command, step, ok := strings.Cut(parsed.Command, StepSeparator); ok {
		parsed.Command, parsed.Step = command, step
	}
	return parsed
}

// DeclaresStepDependency reports whether a command of the manifest depends on
// one step of another command: a dependsOn entry that holds StepSeparator. It
// is the vocabulary of protocolcli.StepDependencyContract. An entry that holds
// the separator but is not well formed counts too, so a manifest stamped below
// the contract never loads it; strict validation and, from the contract, the
// loader refuse it.
func DeclaresStepDependency(m *Manifest) bool {
	if m == nil {
		return false
	}
	for _, command := range m.Commands {
		for _, dependency := range command.DependsOn {
			if strings.Contains(dependency, StepSeparator) {
				return true
			}
		}
	}
	return false
}

// validateManifestCommandDependencies runs validateCommandDependency over the
// dependsOn of every command.
func validateManifestCommandDependencies(m *Manifest) []diag.Diagnostic {
	var diags []diag.Diagnostic
	for _, name := range sortedKeys(m.Commands) {
		for i, dependency := range m.Commands[name].DependsOn {
			diags = append(diags, validateCommandDependency(fmt.Sprintf("commands.%s.dependsOn[%d]", name, i), dependency)...)
		}
	}
	return diags
}

// validateCommandDependency refuses a command dependency that is not
// `command`, `command~step` or `!command`: a pipeline-step reference (a `^`,
// `/` or `*` prefix), an empty command or step, a second separator, and a
// session barrier on one step.
func validateCommandDependency(field, dependency string) []diag.Diagnostic {
	parsed := ParseCommandDependency(dependency)
	refuse := func(format string, args ...any) []diag.Diagnostic {
		return []diag.Diagnostic{diag.Errorf("invalid-command-dependency", field, format, args...)}
	}
	switch {
	case isExternalRef(parsed.Command):
		return refuse("dependency %q uses pipeline-step syntax; command-level dependsOn accepts command, command~step or !command", dependency)
	case !strings.Contains(dependency, StepSeparator):
		return nil
	case parsed.Session:
		return refuse("dependency %q names a step of a session barrier; a session barrier names a whole command", dependency)
	case parsed.Command == "" || parsed.Step == "":
		return refuse("dependency %q names an empty command or step; the form is command~step", dependency)
	case strings.Contains(parsed.Step, StepSeparator):
		return refuse("dependency %q holds a second %q; the form is command~step", dependency, StepSeparator)
	case isExternalRef(parsed.Step):
		return refuse("dependency %q uses pipeline-step syntax for its step; the form is command~step", dependency)
	}
	return nil
}
