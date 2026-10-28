package toolchain

import (
	"strings"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// CommandDependencyClosure returns the commands a run of commands can reach:
// each command, the commands it depends on, and the target command and gates
// of each session prerequisite, transitively across extensions. A dependency
// on one step ("build~describe") reaches the whole command it names.
func CommandDependencyClosure(
	extensions []*extension.ExtensionDescription,
	commands []string,
) map[string]bool {
	active := make(map[string]bool, len(commands))
	for _, command := range commands {
		active[command] = true
	}
	changed := true
	for changed {
		changed = false
		addDependency := func(rawDependency string) {
			dependency := extension.ParseCommandDependency(strings.TrimSpace(rawDependency)).Command
			if dependency != "" && !active[dependency] {
				active[dependency] = true
				changed = true
			}
		}
		for _, ext := range extensions {
			if ext == nil {
				continue
			}
			for commandName, def := range ext.Jobs {
				if def == nil {
					continue
				}
				if !active[commandName] {
					continue
				}
				for _, rawDependency := range def.CommandDependsOn {
					addDependency(rawDependency)
				}
				// Session prerequisites are expanded by the planner after this
				// synchronization stage. Include both their target command and their
				// gates here so every extension runtime the final DAG can reference is
				// ready before planning begins.
				for _, prerequisite := range def.SessionPrerequisites {
					addDependency(prerequisite.Command)
					for _, gate := range prerequisite.DependsOn {
						addDependency(gate)
					}
				}
			}
		}
	}
	return active
}
