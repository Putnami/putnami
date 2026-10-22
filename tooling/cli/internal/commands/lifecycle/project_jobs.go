package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchain"
	"go.putnami.dev/tooling/cli/internal/jobs/toolchainpins"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The project jobs of create and deps. The core knows no language: the
// extension of a project runs its language steps in these workspace lifecycle
// jobs, selected by project ID.
const (
	// depsEditJob edits the manifest of a project's language for deps add,
	// remove and prune. Its params are action (add, remove or prune) and edits,
	// one {project, path, modules} entry per project.
	depsEditJob = "deps-edit"
)

// projectSetupPrepareStage is the params.stage of the project-setup run that
// only writes the files the workspace lock pins a toolchain from, such as a new
// Go project's go.work, and runs no toolchain.
const projectSetupPrepareStage = "prepare"

// errProjectSetup marks a project-setup job that ran and failed. The job's
// diagnostic says why.
var errProjectSetup = errors.New("the project-setup job failed")

// setUpProject runs the project-setup job of the extension of the project
// create rendered at projectPath, with params path and, for the create of an
// init that chose a channel, that channel. A project whose extensions declare
// no project-setup needs none, and the run does nothing.
//
// Before the run, the toolchains the job requires resolve as they do for the
// job (toolchain.UnresolvedRuntimeToolchainLocks). When one does not, the job
// first runs its prepare stage, which writes the files the extensions read
// their pins from; the toolchain pins phase then records what the extensions
// answer, and the workspace installers run when it added or changed the pin
// of a toolchain the extension's runtime locks. The job then runs once.
// installErr is the failure of that phase or of those installers: the setup
// may still succeed, and the caller reports it.
func setUpProject(ctx context.Context, wsRoot, projectPath string, env LifecycleEnv) (installErr, err error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	project := projectAtPath(ws, projectPath)
	if project == nil {
		return nil, fmt.Errorf("the workspace lists no project at %s", projectPath)
	}
	run := func(stage string) error {
		params := map[string]any{"path": projectPath}
		maps.Copy(params, env.installParams())
		if stage != "" {
			params["stage"] = stage
		}
		result, err := runWorkspaceJob(ctx, env, WorkspaceJobRequest{
			WorkspaceRoot: wsRoot,
			Config:        wsproto.Load(wsRoot),
			Job:           toolchain.ProjectSetupCommand,
			Params:        params,
			Projects:      project.ID,
			Display:       env.Display,
			OnAction:      env.OnAction,
		})
		if err != nil {
			return err
		}
		if result.Outcome == WorkspaceJobFailed {
			return errProjectSetup
		}
		return nil
	}
	ext, err := projectJobExtension(ctx, wsRoot, ws, project, toolchain.ProjectSetupCommand)
	if err != nil {
		return nil, err
	}
	if ext == nil {
		return nil, run("")
	}
	unresolved, err := toolchain.UnresolvedRuntimeToolchainLocks(ctx, wsRoot, ext, toolchain.ProjectSetupCommand, os.Environ())
	if err != nil {
		return nil, fmt.Errorf("resolve the toolchains of %s: %w", ext.Name, err)
	}
	if len(unresolved) == 0 {
		return nil, run("")
	}
	if err := run(projectSetupPrepareStage); err != nil {
		return nil, err
	}
	pins, err := resolveToolchainPins(ctx, wsRoot, env)
	switch pinned := pinnedLocks(pins, extension.RuntimeToolchainLocks(ext)); {
	case err != nil:
		installErr = err
	case len(pinned) > 0:
		iox.Fprintf(env.out(), "  The workspace lock now pins %s; running the workspace installers before the setup…\n", strings.Join(pinned, ", "))
		installErr = DepsInstall(shared.WithImplicitInstall(ctx), wsRoot, wsproto.Load(wsRoot), "", "", env)
	}
	return installErr, run("")
}

// pinnedLocks returns the sorted locks whose pin the toolchain pins phase
// added or changed.
func pinnedLocks(pins toolchainpins.Result, locks []string) []string {
	var pinned []string
	for _, lock := range locks {
		if slices.Contains(pins.Added, lock) || slices.Contains(pins.Changed, lock) {
			pinned = append(pinned, lock)
		}
	}
	sort.Strings(pinned)
	return pinned
}

// projectAtPath is the project of ws whose directory is the workspace-relative
// path, or nil.
func projectAtPath(ws *workspace.Workspace, path string) *workspace.Project {
	for _, project := range ws.Projects {
		if filepath.ToSlash(project.Path) == path {
			return project
		}
	}
	return nil
}

// projectJobProviders maps the ID of each project of ws to the extension
// whose job runs for it, as the planner matches it: an extension the project
// lists in its dependencies or extensions, by name or by path ID. Of several,
// the first by name wins. A project absent from the map has none.
func projectJobProviders(ctx context.Context, wsRoot string, ws *workspace.Workspace, job string) (map[string]string, error) {
	providers, err := discoverJobProviders(ctx, wsRoot, ws, job)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(providers))
	for id, ext := range providers {
		names[id] = ext.Name
	}
	return names, nil
}

// projectJobExtension is the extension whose job runs for project
// (projectJobProviders), or nil when none does.
func projectJobExtension(ctx context.Context, wsRoot string, ws *workspace.Workspace, project *workspace.Project, job string) (*extension.ExtensionDescription, error) {
	providers, err := discoverJobProviders(ctx, wsRoot, ws, job)
	if err != nil {
		return nil, err
	}
	return providers[project.ID], nil
}

// discoverJobProviders maps the ID of each project of ws to the description
// of the extension whose job runs for it (projectJobProviders). Discovery
// builds fresh descriptions, so a resolution recorded on one reaches no run.
func discoverJobProviders(ctx context.Context, wsRoot string, ws *workspace.Workspace, job string) (map[string]*extension.ExtensionDescription, error) {
	cfg := wsproto.Load(wsRoot)
	// A fresh worktree has not materialized the lock-pinned extensions yet,
	// and discovery would miss them (runWorkspaceJob).
	_ = EnsureArtifacts(ctx, wsRoot, cfg)
	paths := make([]string, len(ws.Projects))
	for i, project := range ws.Projects {
		paths[i] = project.Path
	}
	discovered, err := extension.DiscoverExtensions(wsRoot, cfg, paths)
	if err != nil {
		return nil, fmt.Errorf("discover extensions: %w", err)
	}
	byName := make(map[string]*extension.ExtensionDescription, len(discovered))
	for _, ext := range discovered {
		if ext != nil {
			byName[ext.Name] = ext
		}
	}
	definitions := extension.BuildJobMap(discovered)[job]
	providers := map[string]*extension.ExtensionDescription{}
	for _, project := range ws.Projects {
		for _, definition := range definitions {
			ext := byName[definition.ExtensionName]
			if ext == nil || !listsExtension(project, definition) {
				continue
			}
			if current, ok := providers[project.ID]; !ok || ext.Name < current.Name {
				providers[project.ID] = ext
			}
		}
	}
	return providers, nil
}

// listsExtension reports whether project lists the extension of definition in
// its dependencies or its extensions, by name or by path ID.
func listsExtension(project *workspace.Project, definition *extension.JobDefinition) bool {
	id := ""
	if definition.ExtensionPath != "" {
		id = workspace.ProjectIDFromPath(definition.ExtensionPath)
	}
	for _, listed := range append(append([]string(nil), project.Dependencies...), project.Extensions...) {
		if listed == definition.ExtensionName || (id != "" && listed == id) {
			return true
		}
	}
	return false
}

// depsEdit is the edit a deps-edit job makes to the manifest of one project.
type depsEdit struct {
	project *workspace.Project
	// modules are dependency specs (add), dependency names (remove) or the
	// workspace-relative directories of the projects whose dependency is
	// dropped (prune).
	modules []string
}

// depsEditEntry is one element of the deps-edit job's params.edits: the
// project's name, its workspace-relative directory and the modules of its
// edit (depsEdit). The fields are in the order the job's params encode them.
type depsEditEntry struct {
	Modules []string `json:"modules"`
	Path    string   `json:"path"`
	Project string   `json:"project"`
}

// runDepsEdit runs the deps-edit job of editor, the extension of every project
// edits names, with action and one {project, path, modules} entry per edit.
// A run in which no deps-edit job matched a project edited nothing, and fails.
func runDepsEdit(ctx context.Context, wsRoot, editor, action string, edits []depsEdit, env LifecycleEnv) error {
	ids := make([]string, 0, len(edits))
	entries := make([]depsEditEntry, 0, len(edits))
	for _, edit := range edits {
		ids = append(ids, edit.project.ID)
		entries = append(entries, depsEditEntry{
			Modules: edit.modules,
			Path:    filepath.ToSlash(edit.project.Path),
			Project: edit.project.Name,
		})
	}
	result, err := runWorkspaceJob(ctx, env, WorkspaceJobRequest{
		WorkspaceRoot: wsRoot,
		Config:        wsproto.Load(wsRoot),
		Job:           depsEditJob,
		Params:        map[string]any{"action": action, "edits": entries},
		Projects:      strings.Join(ids, ","),
		Display:       env.Display,
		OnAction:      env.OnAction,
	})
	if err != nil {
		return err
	}
	switch result.Outcome {
	case WorkspaceJobFailed:
		return fmt.Errorf("deps %s: the %s job of %s failed", action, depsEditJob, editor)
	case WorkspaceJobMissing, WorkspaceJobNoMatches:
		return fmt.Errorf("deps %s: no %s job of %s ran for %s", action, depsEditJob, editor, strings.Join(ids, ", "))
	}
	return nil
}

// projectExtensionsText names the extensions project lists, for a refusal.
func projectExtensionsText(project *workspace.Project) string {
	if len(project.Extensions) == 0 {
		return "it lists no extension"
	}
	listed := append([]string(nil), project.Extensions...)
	sort.Strings(listed)
	return "its extensions (" + strings.Join(listed, ", ") + ") declare none"
}
