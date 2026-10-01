package workspace

import (
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// Project discovery after the core parsers were deleted.
//
// This file used to parse package.json and go.mod, derive Go module edges, and
// read a pyproject name. All three are gone. What is left is the half core can
// own in a language-neutral orchestrator: Putnami membership (includes, scopes),
// explicitly authored putnami.json identity, canonical paths and IDs, and the
// scope conventions (namePattern, inherited tags/extensions/version).
//
// Everything a LANGUAGE knows — what a project is called in its own manifest,
// which other projects it depends on, what its provider-owned metadata is —
// arrives from the workspace probe as a merged provider view and is applied by
// applyProbeView. Discovery therefore produces a project whose identity is
// deliberately INCOMPLETE until a view is applied, and applying one is
// idempotent: it only ever reads the authored config and the scope chain
// captured here, never a value a previous application wrote. That is what makes
// "load from a valid snapshot" and "load after re-probing" produce the same
// project — and therefore the same cache key — for one tree.

// discoverProjects finds all projects in the workspace from Putnami membership:
// the workspace config's includes (scopes and direct project paths) and each
// scope's own includes.
//
// It returns projects and nothing else. It used to thread a `warnings *[]string`
// down to discoverProject so a malformed package.json / go.mod could be surfaced
// through Workspace.Warnings; a later change deleted the parsers that
// produced those warnings, and the finding now belongs to the provider that owns
// the manifest — it arrives as an `invalid-manifest` diagnostic on that
// provider's probe result and is replayed from the snapshot on every later run
// (sync.go, replayRecordedDiagnostics). The pointer that survived the deletion
// had no writer left anywhere in this package, so it was carrying the promise of
// a warning nothing could emit.
//
// There is deliberately no language-manifest discovery source left here. The
// root package.json `workspaces` array used to be one; it is npm membership, it
// is written by the TypeScript adapter's own sync task, and reading
// it back in a neutral core is the same layering mistake in the other direction.
// A directory that is not yet a member becomes one through `projects sync`,
// which walks the tree with the markers extensions declare.
func discoverProjects(root string, cfg *wsproto.Config) []*Project {
	projects := make([]*Project, 0, len(cfg.Includes))
	seen := make(map[string]bool) // keyed by relative path to dedup

	// An include can be either an autonomous scope with its own projects, or a
	// direct project path.
	for _, path := range ScopePaths(root, cfg) {
		projects = appendScopeProjects(root, path, seen, projects)
	}
	for _, path := range RootProjectPaths(root, cfg) {
		projects = appendDirectProject(root, path, seen, projects)
	}

	return projects
}

func appendScopeProjects(root, scopePath string, seen map[string]bool, projects []*Project) []*Project {
	scopeDir := filepath.Join(root, filepath.FromSlash(scopePath))
	sc := wsproto.ReadScopeConfig(scopeDir)
	scopeIncludes := sc.IncludePaths()
	if len(scopeIncludes) == 0 {
		return projects
	}
	// activate: true promotes the scope directory itself to an activation target.
	// The scope's putnami.json doubles as a project config — name, tags,
	// extensions, options apply to the scope-self project the same way they
	// would for a leaf.
	var scopeSelf *Project
	if sc.Activate && !seen[scopePath] && fileExists(scopeDir) {
		if proj := discoverProject(root, scopePath); proj != nil {
			proj.ActivatedScope = true
			seen[scopePath] = true
			projects = append(projects, proj)
			scopeSelf = proj
		}
	}
	for _, relProj := range scopeIncludes {
		fullRel := cleanWorkspacePath(filepath.Join(scopePath, relProj))
		if fullRel == "" {
			continue
		}
		projPath := filepath.Join(root, filepath.FromSlash(fullRel))
		if !fileExists(projPath) {
			continue
		}
		if scopeSelf != nil {
			scopeSelf.ScopeIncludes = append(scopeSelf.ScopeIncludes, ProjectIDFromPath(fullRel))
		}
		if seen[fullRel] {
			continue
		}
		proj := discoverProject(root, fullRel)
		if proj != nil {
			seen[fullRel] = true
			projects = append(projects, proj)
		}
	}
	return projects
}

func appendDirectProject(root, path string, seen map[string]bool, projects []*Project) []*Project {
	if path == "" || seen[path] {
		return projects
	}
	projPath := filepath.Join(root, filepath.FromSlash(path))
	if !fileExists(projPath) {
		return projects
	}
	proj := discoverProject(root, path)
	if proj != nil && !seen[path] {
		seen[path] = true
		projects = append(projects, proj)
	}
	return projects
}

// discoverProject reads a project's CORE-OWNED metadata from a directory: its
// putnami.json and the scope chain above it.
//
// The project's language identity is deliberately absent when this returns:
// applyProbeView fills it from the merged provider view. A project that no
// provider claims keeps exactly what is resolved here, which is why the
// directory-basename fallback lives in the identity resolution rather than
// here.
func discoverProject(workspaceRoot, relPath string) *Project {
	absPath := filepath.Join(workspaceRoot, relPath)
	proj := &Project{Path: relPath}

	projConfig, diags := wsproto.LoadProjectConfigWithDiagnostics(absPath)
	if projConfig != nil {
		proj.Config = projConfig
	}
	// Keep task-tuning diagnostics even when their config cannot be decoded;
	// otherwise a malformed timeout would disappear before workspace validation.
	proj.ConfigDiagnostics = diags

	if bc, sources := wsproto.LoadScopeChainWithSources(workspaceRoot, absPath); bc != nil {
		proj.Scope = ScopeContribution{
			Name:         bc.ResolveNamePattern(filepath.Base(relPath)),
			Tags:         bc.Tags,
			Extensions:   bc.Extensions,
			ConfigPaths:  sources,
			Distribution: bc.Distribution,
		}
	}
	proj.Line = nearestAncestorLine(workspaceRoot, absPath)

	proj.ID = ProjectIDFromPath(proj.Path)
	resolveProjectIdentity(proj, nil)
	return proj
}

// nearestAncestorLine returns the scope path of the version line a project
// belongs to: the DEEPEST ancestor scope that declares a `line` block, in
// slash form relative to the workspace root. The empty string is the implicit
// root line, which is also the answer for a workspace where no scope declares
// one.
//
// It walks the ancestors itself rather than reading the merged scope chain,
// because a line is deliberately not inherited: the merged chain carries no
// line block, and inheriting it would make every intermediate scope a line.
func nearestAncestorLine(workspaceRoot, projectDir string) string {
	dir := filepath.Dir(projectDir)
	for {
		rel, err := filepath.Rel(workspaceRoot, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return ""
		}
		if wsproto.ReadScopeConfig(dir).IsLine() {
			return filepath.ToSlash(rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
