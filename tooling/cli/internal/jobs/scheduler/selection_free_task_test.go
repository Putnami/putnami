package scheduler

import (
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// selectionRun is one run's selection as the planner hands it to a
// workspace-once job: the projects attachSelectedProjects stamps and the
// resolved selection AttachRunSelection stamps.
type selectionRun struct {
	name      string
	projects  []*workspace.Project
	selection *protocoljob.Selection
}

func selectionRuns() []selectionRun {
	billing := &workspace.Project{ID: "/billing", Name: "billing", Path: "billing"}
	ledger := &workspace.Project{ID: "/ledger", Name: "ledger", Path: "ledger"}
	orders := &workspace.Project{ID: "/orders", Name: "orders", Path: "orders"}
	return []selectionRun{
		{"--all", []*workspace.Project{billing, ledger, orders},
			&protocoljob.Selection{Mode: protocoljob.SelectionModeAll, ProjectIDs: []string{"/billing", "/ledger", "/orders"}}},
		{"--impacted", []*workspace.Project{billing, ledger},
			&protocoljob.Selection{Mode: protocoljob.SelectionModeImpacted, Scoped: true, Baseline: "origin/main", BaselineSource: "trunk", ProjectIDs: []string{"/billing", "/ledger"}}},
		{"--projects", []*workspace.Project{ledger},
			&protocoljob.Selection{Mode: protocoljob.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/ledger"}}},
	}
}

// workspaceOnceJob is the features step of validate-workspace planned for run,
// its task declaring selection.
func workspaceOnceJob(t *testing.T, run selectionRun, selection string) *ScheduledJob {
	t.Helper()
	job := declareCacheTestTask(&ScheduledJob{
		Project:   &workspace.Project{ID: "putnami", Name: "putnami", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@test/sdd", Path: t.TempDir()},
		JobDef: &extension.JobDefinition{
			Name: "validate-workspace~features", CommandName: "validate-workspace", ExtensionName: "@test/sdd", Cache: true,
		},
		SelectedProjects: run.projects,
		Selection:        run.selection,
	}, "features-validate")
	task := job.Extension.Tasks["features-validate"]
	task.Selection = selection
	job.Extension.Tasks["features-validate"] = task
	return job
}

// TestATaskThatReadsNoSelectionKeysOnceForEverySelection pins both halves of
// the declaration: a workspace-once task that declares selection none keys the
// same under --all, --impacted and --projects, and its job context carries no
// selection, so it cannot read what its key omits.
func TestATaskThatReadsNoSelectionKeysOnceForEverySelection(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-run-fails-on-its-selected-projects-findings",
		"a-task-that-reads-no-selection-keys-once-for-every-selection")
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	keys := map[string]string{}
	for _, run := range selectionRuns() {
		job := workspaceOnceJob(t, run, extensionproto.TaskSelectionNone)
		key, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("%s: compute the cache key: %v", run.name, err)
		}
		keys[run.name] = key

		ctx := BuildJobContext(ws, job, nil, nil, nil)
		if ctx.Selection != nil || len(ctx.SelectedProjects) != 0 {
			t.Errorf("%s: context selection = %+v, selectedProjects = %+v; want none", run.name, ctx.Selection, ctx.SelectedProjects)
		}
		for _, entry := range BuildEnvVars(ctx) {
			if strings.HasPrefix(entry, "PUTNAMI_SELECTED_") {
				t.Errorf("%s: the job environment carries %s", run.name, entry)
			}
		}
		// The plan still knows the job runs once for the workspace.
		if identity := job.TypedIdentity(); identity.Scope != protocolcli.TaskScopeWorkspace {
			t.Errorf("%s: identity scope = %q, want workspace", run.name, identity.Scope)
		}
	}
	if keys["--all"] != keys["--impacted"] || keys["--all"] != keys["--projects"] {
		t.Fatalf("keys under three selections = %v, want one", keys)
	}
}

// TestAnUndeclaredWorkspaceOnceTaskStillKeysOnTheSelection pins the default:
// a workspace-once task that does not declare it may read the selection, so
// its context carries it and its key holds the selected projects.
func TestAnUndeclaredWorkspaceOnceTaskStillKeysOnTheSelection(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-run-fails-on-its-selected-projects-findings",
		"a-task-that-reads-no-selection-keys-once-for-every-selection")
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	seen := map[string]string{}
	for _, run := range selectionRuns() {
		job := workspaceOnceJob(t, run, "")
		key, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("%s: compute the cache key: %v", run.name, err)
		}
		if other, ok := seen[key]; ok {
			t.Errorf("%s keys like %s; the selected projects must reach the key", run.name, other)
		}
		seen[key] = run.name

		ctx := BuildJobContext(ws, job, nil, nil, nil)
		if ctx.Selection != run.selection || len(ctx.SelectedProjects) != len(run.projects) {
			t.Errorf("%s: context selection = %+v, %d selected project(s); want the run's", run.name, ctx.Selection, len(ctx.SelectedProjects))
		}
	}
}
