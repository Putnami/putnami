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
	// A `git:` pattern reaches the key from the workspace, the project or the
	// closure; the membership must follow it from each.
	key := func(ws *workspace.Workspace, from, pattern string) string {
		t.Helper()
		taskKey := &extension.TaskCacheKey{}
		switch from {
		case "workspace":
			taskKey.WorkspaceFiles = []string{pattern}
		case "project":
			taskKey.Files = []string{pattern}
		case "closure":
			taskKey.ClosureFiles = []string{pattern}
		}
		def := extension.JobDefinition{
			Name: "validate~check", ExtensionName: ext.Name, Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: taskKey},
		}
		job := &ScheduledJob{Project: ws.ProjectByID("/app"), Extension: ext,
			Step: &extension.PipelineStep{ID: "check", Task: "check"}, JobDef: &def}
		hash, err := computeJobCacheHash(ws, job, nil, nil, store.NewCacheManager(store.NewLocalStore(t.TempDir())), nil)
		if err != nil {
			t.Fatalf("key of %s over %s %s: %v", job.Key(), from, pattern, err)
		}
		return hash
	}
	app := func() *workspace.Project { return &workspace.Project{ID: "/app", Name: "app", Path: "app"} }
	before := testWorkspace(root, app())
	after := testWorkspace(root, app(), &workspace.Project{ID: "/tool", Name: "tool", Path: "tool"})

	for _, from := range []string{"workspace", "project", "closure"} {
		if key(before, from, "git:**") == key(after, from, "git:**") {
			t.Errorf("a task keyed on the %s candidate cut kept its key when the membership gained a project", from)
		}
	}
	if key(before, "workspace", "shared.lock") != key(after, "workspace", "shared.lock") {
		t.Error("a task keyed on ordinary workspace files moved its key with the membership")
	}
	if key(before, "workspace", "git:**") != key(testWorkspace(root, app()), "workspace", "git:**") {
		t.Error("a task keyed on the candidate cut moved its key over one membership")
	}
}
