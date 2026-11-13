package nx

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.putnami.dev/tooling/stacks"
)

// TargetsOption is the member of the workspace options of the extension that
// maps a Nx target name, such as test:lib, to the task that runs it: build,
// lint or test.
const TargetsOption = "targets"

// CodeTargetUnmatched is an entry of options["@putnami/nx"].targets that
// names a target no Nx project has.
const CodeTargetUnmatched = "nx.target_unmatched"

// WorkspaceTargets reads options["@putnami/nx"].targets of the workspace at
// root: each Nx target name with the task that runs it; nil when the
// workspace records none. An entry whose value is no task, or whose name is a
// task's own, is an error.
func WorkspaceTargets(root string) (map[string]string, error) {
	doc, err := readWorkspace(root)
	if err != nil || doc == nil {
		return nil, err
	}
	layer, _ := doc.Options[ExtensionName].(map[string]any)
	return targetEntries(layer)
}

// targetEntries reads the targets member of the extension's option layer.
func targetEntries(layer map[string]any) (map[string]string, error) {
	value, ok := layer[TargetsOption]
	if !ok || value == nil {
		return nil, nil
	}
	member := fmt.Sprintf("options[%q].%s", ExtensionName, TargetsOption)
	entries, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is %s, not an object that maps a Nx target name to build, lint or test", member, jsonText(value))
	}
	out := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		task, _ := entries[name].(string)
		switch {
		case name == "":
			return nil, fmt.Errorf("%s maps an empty target name", member)
		case slices.Contains(lifecycleTasks, name):
			return nil, fmt.Errorf("%s maps %q, the target the %s task runs without an entry", member, name, name)
		case !slices.Contains(lifecycleTasks, task):
			return nil, fmt.Errorf("%s maps %q to %s, which is no task: choose build, lint or test", member, name, jsonText(entries[name]))
		}
		out[name] = task
	}
	return out, nil
}

// jsonText is a decoded JSON value as JSON, to quote in an error.
func jsonText(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

// runStep is the step that runs a target of the project: the profile's step
// of a task, with the target's name in place of the task's.
func runStep(target string) []string {
	return []string{"npx", "--no", "nx", "run", runStepPrefix + target, "--outputStyle=static"}
}

// runStepPrefix starts the argument of runStep that names the target, and
// runStepArg is its position.
const (
	runStepPrefix = "{component}:"
	runStepArg    = 4
)

// nxSteps are the steps overrides the sync owns: nx run on each target the
// workspace maps to a task.
var nxSteps = stacks.StepsOverride{Extension: ExtensionName, Tasks: lifecycleTasks, Synced: syncShaped}

// syncShaped reports whether steps are absent or the ones the sync writes:
// each the runStep of a target.
func syncShaped(steps any) bool {
	if steps == nil {
		return true
	}
	list, _ := steps.([]any)
	if len(list) == 0 {
		return false
	}
	for _, item := range list {
		step, _ := item.([]any)
		args := stringList(step)
		if len(args) != len(step) || len(args) <= runStepArg {
			return false
		}
		target, ok := strings.CutPrefix(args[runStepArg], runStepPrefix)
		if !ok || target == "" || !slices.Equal(args, runStep(target)) {
			return false
		}
	}
	return true
}

// mappedSteps are the steps of each task that runs targets the workspace
// maps to it: one runStep per target, in name order.
func mappedSteps(named map[string][]string) map[string][][]string {
	steps := map[string][][]string{}
	for task, targets := range named {
		for _, target := range targets {
			steps[task] = append(steps[task], runStep(target))
		}
	}
	return steps
}

// shadowedReason is the reason of a mapped target on a project whose task
// runs the target named after it.
func shadowedReason(project, target, task string) string {
	return fmt.Sprintf("it maps to %s, which runs the project's %s target: run nx run %s:%s", task, task, project, target)
}

// unmatchedTargets lists, sorted, the entries of the mapping that name a
// target none of the projects has.
func unmatchedTargets(mapping map[string]string, has map[string]bool) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(mapping)) {
		if !has[name] {
			out = append(out, name)
		}
	}
	return out
}
