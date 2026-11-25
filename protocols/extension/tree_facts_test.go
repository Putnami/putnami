package extension

import (
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

// TestTreeFactInputsBindTheirTask pins the two rules of ADR 0003 of
// protocols/job a manifest can break: the workspace's cut is read only by a
// task whose key holds it, and history only by an uncached task.
func TestTreeFactInputsBindTheirTask(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "a-tree-fact-input-binds-its-task", "a-workspace-cut-needs-a-workspace-git-input-and-history-an-uncached-task")
	disabled := false
	runtime := TaskInputPort{From: TaskInputFromRuntime}
	for _, tc := range []struct {
		name  string
		task  TaskDefinition
		field string
	}{
		{"workspace cut keyed", TaskDefinition{Inputs: map[string]TaskInputPort{
			"repository":                        {From: TaskInputFromWorkspace, Files: []string{"git:**"}},
			RuntimeInputWorkspaceCandidateFiles: runtime,
		}}, ""},
		{"workspace cut through the cache key", TaskDefinition{Cache: &TaskCachePolicy{Key: &TaskCacheKey{
			WorkspaceFiles: []string{"git:**"}, Runtime: []string{RuntimeInputWorkspaceCandidateFiles},
		}}}, ""},
		{"workspace cut unkeyed", TaskDefinition{Inputs: map[string]TaskInputPort{
			"repository":                        {From: TaskInputFromProject, Files: []string{"git:**"}},
			RuntimeInputWorkspaceCandidateFiles: runtime,
		}}, "tasks.t.inputs." + RuntimeInputWorkspaceCandidateFiles},
		{"workspace cut keyed by a narrower pattern", TaskDefinition{Inputs: map[string]TaskInputPort{
			"repository":                        {From: TaskInputFromWorkspace, Files: []string{"git:**/*.md"}},
			RuntimeInputWorkspaceCandidateFiles: runtime,
		}}, "tasks.t.inputs." + RuntimeInputWorkspaceCandidateFiles},
		{"history uncached", TaskDefinition{Cache: &TaskCachePolicy{Enabled: &disabled},
			Inputs: map[string]TaskInputPort{RuntimeInputChangeHistory: runtime}}, ""},
		{"history cached by default", TaskDefinition{
			Inputs: map[string]TaskInputPort{RuntimeInputChangeHistory: runtime}}, "tasks.t.inputs." + RuntimeInputChangeHistory},
		{"candidate cut needs no other input", TaskDefinition{
			Inputs: map[string]TaskInputPort{RuntimeInputCandidateFiles: runtime}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags := ValidateTreeFactInputs(&Manifest{Tasks: map[string]TaskDefinition{"t": tc.task}})
			if tc.field == "" {
				if diag.HasErrors(diags) {
					t.Fatalf("refused: %v", diags)
				}
				return
			}
			if len(diags) != 1 || diags[0].Field != tc.field || !strings.Contains(diags[0].Message, "task \"t\"") {
				t.Fatalf("diagnostics = %v, want one on %s", diags, tc.field)
			}
		})
	}
}
