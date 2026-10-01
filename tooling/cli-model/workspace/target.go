package workspace

import (
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// ResolveTarget resolves a target expression into a set of projects.
// Supports:
//   - "/path/to/project" — exact match by ID
//   - "/path/to/..." — recursive wildcard
//   - "./relative" or "../relative" — resolve relative to cwd
//   - "." — current project (cwd)
//   - "alias" — resolve via projectAliases then groups
//   - "a,b" — union of multiple targets
//   - "-expr" — subtraction (within a comma-separated list)
//
// Returns nil if no projects match.
func ResolveTarget(ws *Workspace, expr string, cwd string, index *wsproto.ScopeIndex) []*Project {
	if expr == "" || expr == "." {
		return resolveCurrentProject(ws, cwd)
	}

	// Split by comma for union support.
	parts := strings.Split(expr, ",")
	included := make(map[string]*Project)
	var excluded map[string]bool

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Subtraction: "-/path/..." or "-alias"
		if strings.HasPrefix(part, "-") {
			if excluded == nil {
				excluded = make(map[string]bool)
			}
			subExpr := part[1:]
			for _, p := range resolveSingleTarget(ws, subExpr, cwd, index) {
				excluded[p.ID] = true
			}
			continue
		}

		for _, p := range resolveSingleTarget(ws, part, cwd, index) {
			if _, ok := included[p.ID]; !ok {
				included[p.ID] = p
			}
		}
	}

	// Build result, excluding subtracted projects, preserving ws.Projects order.
	result := make([]*Project, 0, len(included))
	for _, p := range ws.Projects {
		if _, ok := included[p.ID]; !ok {
			continue
		}
		if excluded != nil && excluded[p.ID] {
			continue
		}
		result = append(result, p)
	}
	return result
}

// resolveSingleTarget resolves a single target pattern (no commas, no subtraction).
func resolveSingleTarget(ws *Workspace, pattern string, cwd string, index *wsproto.ScopeIndex) []*Project {
	pattern = strings.TrimSpace(pattern)

	switch {
	case pattern == ".":
		return resolveCurrentProject(ws, cwd)

	case strings.HasPrefix(pattern, "/"):
		// Absolute workspace path: exact or wildcard
		return ws.ProjectsByPattern(pattern)

	case strings.HasPrefix(pattern, ".../"):
		// Suffix/infix wildcard: .../web, .../frameworks/...
		return ws.ProjectsByPattern(pattern)

	case strings.HasPrefix(pattern, "./") || strings.HasPrefix(pattern, "../"):
		// Relative filesystem paths resolve through physical Project.Path, then
		// return projects carrying their logical IDs.
		return resolveRelativeTarget(ws, pattern, cwd)

	default:
		// Bare word: try alias → group → legacy name match
		if index != nil {
			if target, ok := index.ProjectAliases[pattern]; ok {
				return ws.ProjectsByPattern(target)
			}
			if groupPattern, ok := index.Groups[pattern]; ok {
				return resolveGroupPattern(ws, groupPattern)
			}
		}

		// Also check workspace-level projectAliases and groups
		if ws.Config != nil {
			if target, ok := ws.Config.ProjectAliases[pattern]; ok {
				return ws.ProjectsByPattern(target)
			}
			if groupPattern, ok := ws.Config.Groups[pattern]; ok {
				return resolveGroupPattern(ws, groupPattern)
			}
		}

		// Bare names that are not an alias or group match by project Name,
		// Path, or path basename.
		return selectByNames(ws, []string{pattern})
	}
}

// resolveRelativeTarget resolves a filesystem-relative target through physical
// project paths. It must not treat a resolved filesystem path as a project ID,
// because transparent group folders intentionally make the two differ.
func resolveRelativeTarget(ws *Workspace, relPath string, cwd string) []*Project {
	if cwd == "" {
		cwd, _ = filepath.Abs(".")
	}

	slashPath := filepath.ToSlash(relPath)
	wildcard := strings.HasSuffix(slashPath, "/...")
	base := relPath
	if wildcard {
		base = strings.TrimSuffix(slashPath, "/...")
		base = filepath.FromSlash(base)
	}

	absPath := CanonicalRoot(filepath.Join(cwd, base))
	physicalPath, err := filepath.Rel(ws.Root, absPath)
	if err != nil || !isRelativeToWorkspace(physicalPath) {
		return nil
	}
	physicalPath = CleanWorkspacePath(physicalPath)

	if !wildcard {
		if p := ws.ProjectByPath(physicalPath); p != nil {
			return []*Project{p}
		}
		return nil
	}
	if physicalPath == "" {
		return append([]*Project(nil), ws.Projects...)
	}

	var result []*Project
	for _, p := range ws.Projects {
		path := CleanWorkspacePath(p.Path)
		if path == physicalPath || strings.HasPrefix(path, physicalPath+"/") {
			result = append(result, p)
		}
	}
	return result
}

// resolveGroupPattern resolves a comma-separated group pattern (e.g.
// "/go/framework/app,/go/framework/inject") into a set of projects.
func resolveGroupPattern(ws *Workspace, pattern string) []*Project {
	var result []*Project
	seen := make(map[string]bool)
	for _, part := range strings.Split(pattern, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, p := range ws.ProjectsByPattern(part) {
			if !seen[p.ID] {
				seen[p.ID] = true
				result = append(result, p)
			}
		}
	}
	return result
}

func resolveCurrentProject(ws *Workspace, cwd string) []*Project {
	if cwd == "" {
		cwd, _ = filepath.Abs(".")
	}
	// Match ws.Root's symlink canonicalization so the path comparisons
	// below hold when the workspace is reached through a symlinked path.
	cwd = CanonicalRoot(cwd)
	var matched *Project
	matchedPathLen := -1
	for _, p := range ws.Projects {
		absPath := filepath.Join(ws.Root, p.Path)
		if (absPath == cwd || strings.HasPrefix(cwd, absPath+string(filepath.Separator))) && len(absPath) > matchedPathLen {
			matched = p
			matchedPathLen = len(absPath)
		}
	}
	if matched != nil {
		return []*Project{matched}
	}
	return nil
}
