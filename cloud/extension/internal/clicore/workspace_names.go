package clicore

import (
	"path/filepath"
	"sort"
	"sync"
)

// WorkspaceProject is one project of the workspace membership the native CLI
// resolved: the name the workspace gives it and its absolute directory.
type WorkspaceProject struct {
	Name string
	Dir  string
}

// workspaceNames holds the membership of the current invocation's job context.
// The workspace names a project from several sources (its putnami.json
// `name`, its provider's source name, a scope name pattern, its directory), so
// a project named only by its provider has no putnami.json `name` at all.
// Every project lookup in this package reads these names first, and reads a
// putnami.json `name` only when the invocation carried no membership.
var workspaceNames struct {
	sync.RWMutex
	byDir    map[string]string
	projects []WorkspaceProject
}

// UseWorkspaceProjects records the resolved membership of the current
// invocation. An empty list clears it, so the lookups fall back to the
// putnami.json `name` of each project.
func UseWorkspaceProjects(projects []WorkspaceProject) {
	byDir := map[string]string{}
	kept := make([]WorkspaceProject, 0, len(projects))
	for _, project := range projects {
		if project.Name == "" || project.Dir == "" {
			continue
		}
		dir := canonicalDir(project.Dir)
		byDir[dir] = project.Name
		if abs, err := filepath.Abs(project.Dir); err == nil {
			byDir[filepath.Clean(abs)] = project.Name
		}
		kept = append(kept, WorkspaceProject{Name: project.Name, Dir: dir})
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Dir < kept[j].Dir })
	workspaceNames.Lock()
	defer workspaceNames.Unlock()
	if len(kept) == 0 {
		workspaceNames.byDir, workspaceNames.projects = nil, nil
		return
	}
	workspaceNames.byDir, workspaceNames.projects = byDir, kept
}

// workspaceNameAt returns the resolved name of the project in dir, when the
// invocation carried a membership that holds it.
func workspaceNameAt(dir string) (string, bool) {
	workspaceNames.RLock()
	defer workspaceNames.RUnlock()
	if workspaceNames.byDir == nil {
		return "", false
	}
	if name, found := workspaceNames.byDir[filepath.Clean(dir)]; found {
		return name, true
	}
	name, found := workspaceNames.byDir[canonicalDir(dir)]
	return name, found
}

// resolvedWorkspaceProjects returns the recorded membership under root, and
// whether it holds any project there. A membership with no project under root
// describes another tree, so the lookups walk root instead.
func resolvedWorkspaceProjects(root string) ([]WorkspaceProject, bool) {
	workspaceNames.RLock()
	defer workspaceNames.RUnlock()
	if workspaceNames.byDir == nil {
		return nil, false
	}
	root = canonicalDir(root)
	projects := make([]WorkspaceProject, 0, len(workspaceNames.projects))
	for _, project := range workspaceNames.projects {
		if IsUnder(project.Dir, root) {
			projects = append(projects, project)
		}
	}
	return projects, len(projects) > 0
}

// canonicalDir is dir made absolute, with its symlinks resolved when it exists.
func canonicalDir(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return filepath.Clean(dir)
}
