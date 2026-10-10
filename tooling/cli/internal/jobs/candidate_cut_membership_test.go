package jobs

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A task keyed on the Git candidate cut reads the workspace membership from its
// context, so its key moves with the membership even when no candidate file
// moves: a project that user config or an ignored scope manifest adds is a
// different set of projects to judge. A task keyed on ordinary workspace files
// does not read the membership that way, and its key stays put.
func TestCandidateCutKeyMovesWithTheMembership(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeFileAt(t, filepath.Join(root, "app", "main.go"), "package main\n")
	writeFileAt(t, filepath.Join(root, "shared.lock"), "lock\n")
	writeFileAt(t, filepath.Join(root, ".gitignore"), "tool/\n")
	writeFileAt(t, filepath.Join(root, "tool", "main.go"), "package main\n")
	treeKeyedGit(t, root, "add", "-A")
	treeKeyedGit(t, root, "commit", "-q", "-m", "init")

	ext := &extension.ExtensionDescription{Name: "@test/ext", Tasks: map[string]extension.TaskDefinition{"check": {}}}
	key := func(ws *workspace.Workspace, workspaceFiles string) string {
		t.Helper()
		def := extension.JobDefinition{
			Name: "validate~check", ExtensionName: ext.Name, Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{WorkspaceFiles: []string{workspaceFiles}}},
		}
		job := &ScheduledJob{Project: ws.ProjectByID("/app"), Extension: ext,
			Step: &extension.PipelineStep{ID: "check", Task: "check"}, JobDef: &def}
		hash, err := computeJobCacheHash(ws, job, nil, nil, store.NewCacheManager(store.NewLocalStore(t.TempDir())), nil)
		if err != nil {
			t.Fatalf("key of %s over %s: %v", job.Key(), workspaceFiles, err)
		}
		return hash
	}
	app := func() *workspace.Project { return &workspace.Project{ID: "/app", Name: "app", Path: "app"} }
	before := testWorkspace(root, app())
	after := testWorkspace(root, app(), &workspace.Project{ID: "/tool", Name: "tool", Path: "tool"})

	if key(before, "git:**") == key(after, "git:**") {
		t.Error("a task keyed on the candidate cut kept its key when the membership gained a project")
	}
	if key(before, "shared.lock") != key(after, "shared.lock") {
		t.Error("a task keyed on ordinary workspace files moved its key with the membership")
	}
}
