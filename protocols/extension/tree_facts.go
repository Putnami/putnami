package extension

import (
	"slices"

	diag "go.putnami.dev/protocol/diagnostic"
)

// ValidateTreeFactInputs checks the tree-fact runtime inputs that bind a
// task's other declarations (go.putnami.dev/protocol/job, doc/adr/0003): a
// task that declares workspaceCandidateFiles keys the workspace's whole cut
// through a workspace `git:**` input, and a task that declares changeHistory
// is never cached, since a commit is state no cache key names.
func ValidateTreeFactInputs(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	var diags []diag.Diagnostic
	for _, name := range sortedKeys(m.Tasks) {
		task := m.Tasks[name]
		field := "tasks." + name
		if declaresRuntimeInput(task, RuntimeInputWorkspaceCandidateFiles) && !keysWorkspaceCut(task) {
			diags = append(diags, diag.Errorf("invalid-value", field+".inputs."+RuntimeInputWorkspaceCandidateFiles,
				"task %q declares %s without a workspace input that holds %q: its key would not hold the cut it reads",
				name, RuntimeInputWorkspaceCandidateFiles, workspaceCutPattern))
		}
		if declaresRuntimeInput(task, RuntimeInputChangeHistory) && task.Cache.IsEnabled() {
			diags = append(diags, diag.Errorf("invalid-value", field+".inputs."+RuntimeInputChangeHistory,
				"task %q declares %s while its cache is enabled: a commit is state no cache key names; set cache.enabled to false",
				name, RuntimeInputChangeHistory))
		}
	}
	return diags
}

// declaresRuntimeInput reports whether a task declares the runtime input
// name, through an input port or its cache key's runtime names.
func declaresRuntimeInput(task TaskDefinition, name string) bool {
	if input, ok := task.Inputs[name]; ok && input.From == TaskInputFromRuntime {
		return true
	}
	return task.Cache != nil && task.Cache.Key != nil && slices.Contains(task.Cache.Key.Runtime, name)
}

// keysWorkspaceCut reports whether a task keys the workspace's whole
// candidate cut: a workspace input, or a cache key's workspace files, that
// holds "git:**".
func keysWorkspaceCut(task TaskDefinition) bool {
	for _, input := range task.Inputs {
		if input.From == TaskInputFromWorkspace && slices.Contains(input.Files, workspaceCutPattern) {
			return true
		}
	}
	return task.Cache != nil && task.Cache.Key != nil && slices.Contains(task.Cache.Key.WorkspaceFiles, workspaceCutPattern)
}

// workspaceCutPattern is the `git:` pattern that keys a workspace's whole
// candidate cut.
const workspaceCutPattern = "git:**"
