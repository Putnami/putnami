package jobs

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// selectionKeyExtension is a two-step build pipeline whose second step depends
// on the first, both in-project and across the dependency edge — the shape
// every real language extension has, and the only shape in which a task's key
// can see another project at all.
func selectionKeyExtension() *extension.ExtensionDescription {
	declared := &extension.TaskDeclaration{}
	return &extension.ExtensionDescription{
		Name:    "@test/lang",
		Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{
			"build": {
				ExtensionName:   "@test/lang",
				Name:            "build",
				Kind:            "command",
				Command:         "true",
				Cache:           true,
				ActivationFiles: []string{"**/*.src"},
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
					{ID: "compile", Task: "build-compile", DependsOn: []string{"^compile", "generate"}},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"build-generate": {Kind: "command", Command: "true", Declares: declared},
			"build-compile":  {Kind: "command", Command: "true", Declares: declared},
		},
	}
}

// TestCacheKeyIsTheSameUnderEverySelectionThatPlansTheTask pins the
// acceptance criterion: a task's identity must not depend on which OTHER
// projects the invocation happened to select.
//
// It is not obvious that it does not. A task's key folds the cache keys of its
// dependencies, and `^`-references resolve against the PLANNED jobs, so a
// dependency present in one selection's plan and absent from the other would
// publish an upstream hash in one run and not the other — and the same task
// would key differently under `--projects <p>` and `--impacted` with identical
// inputs. What closes that hole is the planner's fixpoint (emitMissingExternalDeps):
// it walks the workspace dependency GRAPH, not the selection, and adds every
// upstream job a `^` reference needs before dependencies are resolved. The
// selection therefore decides which tasks RUN, never how one is keyed.
//
// That is why the original measurement needed no key divergence to explain it,
// and why the fix is on the invalidation path instead.
func TestCacheKeyIsTheSameUnderEverySelectionThatPlansTheTask(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "failure-replay-cache",
		"a-task-keys-the-same-under-every-selection")

	root := t.TempDir()
	for _, name := range []string{"lib", "app", "site"} {
		writeProjectFile(t, root, name, "main.src", "unit "+name+"\n")
	}
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib", Extensions: []string{"@test/lang"}}
	app := &workspace.Project{
		ID: "/app", Name: "app", Path: "app",
		Dependencies: []string{"lib"}, Extensions: []string{"@test/lang"},
	}
	site := &workspace.Project{
		ID: "/site", Name: "site", Path: "site",
		Dependencies: []string{"app"}, Extensions: []string{"@test/lang"},
	}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app, site})
	ws.Name = "selection-key-ws"
	exts := []*extension.ExtensionDescription{selectionKeyExtension()}
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))

	keysFor := func(name string, selected ...*workspace.Project) map[string]string {
		t.Helper()
		planned, err := Plan(ws, []string{"build"}, selected, exts, nil, nil, nil)
		if err != nil {
			t.Fatalf("%s: plan: %v", name, err)
		}
		keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
		if err != nil {
			t.Fatalf("%s: precompute keys: %v", name, err)
		}
		// The dependency's job is planned whatever the selection: without it
		// the comparison below would prove nothing, because the upstream hash
		// the key folds would simply be absent everywhere.
		if keys["/lib:build~generate"] == "" {
			t.Fatalf("%s: the dependency's upstream job was not planned, so no upstream hash was folded", name)
		}
		return keys
	}

	// `--projects /app` narrows to the named project and drops its dependents.
	narrow := keysFor("--projects /app", app)
	for _, selection := range []struct {
		name     string
		selected []*workspace.Project
	}{
		// The dependency named explicitly alongside the task's own project.
		{"--projects /lib,/app", []*workspace.Project{lib, app}},
		// A dependent joins, which is what --impacted adds over --projects.
		{"--projects /app,/site", []*workspace.Project{app, site}},
		// The whole workspace, as --all or a wide impacted set resolves.
		{"--all", []*workspace.Project{lib, app, site}},
	} {
		wide := keysFor(selection.name, selection.selected...)
		for _, key := range []string{"/app:build~generate", "/app:build~compile"} {
			if narrow[key] != wide[key] {
				t.Errorf("%s keyed %s as %q, want the narrow selection's %q: "+
					"a task's identity must not depend on which other projects the run selected",
					selection.name, key, wide[key], narrow[key])
			}
		}
	}
}
