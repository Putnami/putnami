package stacks

import "slices"

// CommandOrderTasks are the commands whose dependsOn an adoption sync owns in
// a project's putnami.json (CLI ADR 0097): the build, test and lint a tool
// task maps to.
var CommandOrderTasks = []string{TaskBuild, TaskTest, TaskLint}

// SetCommandOrder writes into a putnami.json document, for each command of
// CommandOrderTasks, commands.<command>.dependsOn as order gives it: the
// prerequisites the tool runs before that command, `command` for the
// project's own and `^command` for the projects it depends on. A command order
// does not name loses its dependsOn, with every object that empties. Every
// other command, and every other member of a command, stays.
func SetCommandOrder(doc map[string]any, order map[string][]string) {
	for _, command := range CommandOrderTasks {
		deps := order[command]
		if len(deps) > 0 {
			list := make([]any, 0, len(deps))
			for _, dep := range deps {
				list = append(list, dep)
			}
			ensureObject(ensureObject(doc, "commands"), command)["dependsOn"] = list
			continue
		}
		commands, _ := doc["commands"].(map[string]any)
		entry, _ := commands[command].(map[string]any)
		if entry == nil {
			continue
		}
		delete(entry, "dependsOn")
		pruneEmpty(commands, command)
		pruneEmpty(doc, "commands")
	}
}

// AddCommandOrder adds deps, each once and in that order, to the order of
// command.
func AddCommandOrder(order map[string][]string, command string, deps ...string) {
	for _, dep := range deps {
		if !slices.Contains(order[command], dep) {
			order[command] = append(order[command], dep)
		}
	}
}
