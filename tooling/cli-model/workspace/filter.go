package workspace

import (
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// FilterOptions configures project selection.
type FilterOptions struct {
	Projects   string // raw: "*", "[impacted]", ".", target expression, or comma-separated names
	FilterTag  string // comma-separated tags to include
	ExcludeTag string // comma-separated tags to exclude
	Exclude    string // comma-separated project names to exclude
	// ExcludeTags from workspace config (default exclusions)
	DefaultExcludeTags []string
	// DirectTarget indicates that projects were explicitly targeted by name.
	// When true, DefaultExcludeTags are skipped (explicit intent overrides defaults).
	DirectTarget bool
	// Cwd is the working directory for resolving relative paths.
	// If empty, defaults to os.Getwd().
	Cwd string
	// ScopeIndex for alias/group resolution. May be nil.
	ScopeIndex *wsproto.ScopeIndex
}

// FilterProjects selects projects based on filter options.
func FilterProjects(ws *Workspace, opts FilterOptions) []*Project {
	candidates := selectCandidates(ws, opts)
	return applyFilters(candidates, opts)
}

func selectCandidates(ws *Workspace, opts FilterOptions) []*Project {
	switch {
	case opts.Projects == "*":
		return ws.Projects
	case opts.Projects == "." || opts.Projects == "":
		// No explicit target: scope to current directory.
		// At workspace root this returns all projects (same as --all).
		return selectCurrentProject(ws)
	default:
		// Route all expressions through ResolveTarget, which handles
		// path patterns, aliases, groups, and falls back to name matching.
		return ResolveTarget(ws, opts.Projects, opts.Cwd, opts.ScopeIndex)
	}
}

func selectCurrentProject(ws *Workspace) []*Project {
	cwd, _ := filepath.Abs(".")
	// Canonicalize through symlinks so cwd is comparable to ws.Root, which
	// FindRoot resolves with CanonicalRoot. Without this, a workspace
	// reached through a symlinked path — e.g. Conductor's per-worktree alias
	// directories — never equals ws.Root, the cwd==root shortcut below fails,
	// and the bare default selects zero projects ("No projects matched").
	cwd = CanonicalRoot(cwd)
	// First try: exact project match (cwd is inside a project)
	if matched := resolveCurrentProject(ws, cwd); matched != nil {
		return matched
	}
	// At workspace root, return all projects
	if cwd == ws.Root {
		return ws.Projects
	}
	// Fallback: cwd is a parent directory — select all projects under it (like ./...)
	prefix := cwd + string(filepath.Separator)
	var result []*Project
	for _, p := range ws.Projects {
		absPath := filepath.Join(ws.Root, p.Path)
		if strings.HasPrefix(absPath, prefix) {
			result = append(result, p)
		}
	}
	return result
}

func selectByNames(ws *Workspace, names []string) []*Project {
	nameSet := make(map[string]bool)
	for _, n := range names {
		cleanName := strings.TrimSpace(n)
		cleanName = strings.TrimSuffix(cleanName, "/")
		nameSet[cleanName] = true
	}
	result := make([]*Project, 0)
	for _, p := range ws.Projects {
		if nameSet[p.Name] || nameSet[p.Path] || nameSet[filepath.Base(p.Path)] {
			result = append(result, p)
		}
	}
	return result
}

func applyFilters(projects []*Project, opts FilterOptions) []*Project {
	result := make([]*Project, 0, len(projects))

	var includeTags map[string]bool
	if opts.FilterTag != "" {
		includeTags = parseCSV(opts.FilterTag)
	}

	// Explicit --exclude-tag always applies
	var explicitExcludeTags map[string]bool
	if opts.ExcludeTag != "" {
		explicitExcludeTags = parseCSV(opts.ExcludeTag)
	}

	// Default exclude tags (from workspace config) only apply when:
	// 1. --tag is NOT specified (explicit inclusion overrides default exclusions), AND
	// 2. Projects are NOT directly targeted by name (explicit intent overrides defaults).
	var defaultExcludeTags map[string]bool
	if includeTags == nil && !opts.DirectTarget && len(opts.DefaultExcludeTags) > 0 {
		defaultExcludeTags = make(map[string]bool, len(opts.DefaultExcludeTags))
		for _, t := range opts.DefaultExcludeTags {
			defaultExcludeTags[t] = true
		}
	}

	excludeNames := make(map[string]bool)
	if opts.Exclude != "" {
		excludeNames = parseCSV(opts.Exclude)
	}

	for _, p := range projects {
		if excludeNames[p.Name] || excludeNames[p.ID] {
			continue
		}
		if includeTags != nil && !hasAnyTag(p.Tags, includeTags) {
			continue
		}
		if explicitExcludeTags != nil && hasAnyTag(p.Tags, explicitExcludeTags) {
			continue
		}
		if defaultExcludeTags != nil && hasAnyTag(p.Tags, defaultExcludeTags) {
			continue
		}
		result = append(result, p)
	}

	return result
}

func parseCSV(s string) map[string]bool {
	m := make(map[string]bool)
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			m[v] = true
		}
	}
	return m
}

func hasAnyTag(tags []string, set map[string]bool) bool {
	for _, t := range tags {
		if set[t] {
			return true
		}
	}
	return false
}
