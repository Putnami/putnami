package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	wsproto "go.putnami.dev/protocol/workspace"
)

// WarningCode is a stable machine-readable reason attached to a workspace
// warning whose meaning must be consumed without matching human prose.
type WarningCode string

const (
	// WarningCodeProviderViewUnavailable means provider-derived project identity
	// or dependency edges could not be adopted and are therefore incomplete.
	WarningCodeProviderViewUnavailable WarningCode = "workspace.provider_view_unavailable"
)

// Workspace represents a discovered workspace.
type Workspace struct {
	Name         string
	Root         string
	Config       *wsproto.Config
	Projects     []*Project
	Graph        *DependencyGraph
	Warnings     []string            // non-fatal warnings detected during loading
	WarningCodes []WarningCode       // stable meanings for warnings consumed by policy
	ScopeIndex   *wsproto.ScopeIndex // aggregated aliases, groups and lines from all scopes
	// Registries is the workspace's registry endpoints, one raw entry per
	// ecosystem id. The entry's shape belongs to the ecosystem profile its
	// extension declares, so core carries it without reading inside it.
	Registries map[string]json.RawMessage
	// Lines maps each version line's scope path (slash-separated, relative to
	// the root) to its git tag pattern. A workspace where no scope declares a
	// line is one line: {"": "v{version}"}.
	Lines       map[string]string
	projectMap  map[string]*Project // name → project for O(1) lookup
	projectByID map[string]*Project // ID → project for O(1) lookup

	// Probe/identity digests. Computed on first use
	// and memoized: every scheduled job's cache key asks for its project's
	// metadata digest, so recomputing per job would hash the same normalized
	// view once per task. Guarded because a single *Workspace is shared across
	// the scheduler's goroutines.
	digestMu        sync.Mutex
	probeDigest     string
	identityDigest  string
	metadataDigests map[string]string // project ID → metadata digest

	// probe carries the merged provider answer this workspace resolved. It IS
	// the source of every project's language identity and
	// dependency edges, so it reaches every cache key that observes project
	// metadata — see probe_view.go for the equality that makes that safe.
	probe probeView
}

// AddWarning appends human prose and, when provided, its stable machine code.
func (ws *Workspace) AddWarning(code WarningCode, message string) {
	if ws == nil {
		return
	}
	ws.Warnings = append(ws.Warnings, message)
	if code != "" && !ws.HasWarningCode(code) {
		ws.WarningCodes = append(ws.WarningCodes, code)
	}
}

// HasWarningCode reports whether workspace loading emitted a stable warning
// reason. Callers must branch on this instead of Warnings message text.
func (ws *Workspace) HasWarningCode(code WarningCode) bool {
	if ws == nil {
		return false
	}
	for _, candidate := range ws.WarningCodes {
		if candidate == code {
			return true
		}
	}
	return false
}

// CanonicalRoot resolves a workspace root to its absolute, link-free form:
// symbolic links, and junctions on Windows (ResolveLinks).
func CanonicalRoot(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return root
	}
	real, err := ResolveLinks(abs)
	if err != nil {
		return abs
	}
	return real
}

// NewWorkspace assembles a Workspace from already-discovered projects,
// building the dependency graph and the lookup maps behind ProjectByName and
// ProjectByID. Load uses it after project discovery; tests use it to construct
// workspaces directly. Name is taken from cfg when present.
func NewWorkspace(root string, cfg *wsproto.Config, projects []*Project) *Workspace {
	ws := &Workspace{
		Root:        root,
		Config:      cfg,
		Projects:    projects,
		projectMap:  make(map[string]*Project, len(projects)),
		projectByID: make(map[string]*Project, len(projects)),
	}
	if cfg != nil {
		ws.Name = cfg.Name
	}
	ws.rebuildIndexes()
	return ws
}

// CheckDuplicateNames returns an error when two or more projects resolve to the
// same name. Empty names are skipped (an unnamed project is a separate concern).
// The message lists every colliding name with its declaring paths in
// deterministic order so the diagnostic is stable across runs.
func CheckDuplicateNames(projects []*Project) error {
	paths := make(map[string][]string, len(projects))
	for _, p := range projects {
		if p.Name == "" {
			continue
		}
		paths[p.Name] = append(paths[p.Name], p.Path)
	}

	dupNames := make([]string, 0)
	for name, ps := range paths {
		if len(ps) > 1 {
			dupNames = append(dupNames, name)
		}
	}
	if len(dupNames) == 0 {
		return nil
	}
	sort.Strings(dupNames)

	parts := make([]string, len(dupNames))
	for i, name := range dupNames {
		ps := append([]string(nil), paths[name]...)
		sort.Strings(ps)
		parts[i] = fmt.Sprintf("%q (%s)", name, strings.Join(ps, ", "))
	}
	return fmt.Errorf(
		"duplicate project name(s) %s; project names must be unique across the workspace — rename one of the colliding projects (or fix the scope config that resolves them to the same name)",
		strings.Join(parts, "; "),
	)
}

// ProbeDigest is the workspace's normalized aggregate probe digest:
// the digest of every resolved project's probe-shaped metadata view, folded
// order-independently. It is the workspace-level half of plan identity, and it
// moves when — and only when — some project's metadata or dependency edges
// move.
func (ws *Workspace) ProbeDigest() string {
	ws.digestMu.Lock()
	defer ws.digestMu.Unlock()
	if ws.probeDigest == "" {
		ws.probeDigest = wsproto.ProbeWorkspaceDigest([]wsproto.ProbeResult{ws.ProbeResultView()})
	}
	return ws.probeDigest
}

// IdentityDigest is the workspace/plan identity: the aggregate probe digest
// folded with the workspace's own name.
//
// No version is folded in. A workspace declares none since versions are derived
// from the git tags of each line, and a per-line version has no place in a
// whole-workspace identity: it would make a plan recorded for one workspace
// depend on which line happened to advance.
func (ws *Workspace) IdentityDigest() string {
	ws.digestMu.Lock()
	identity := ws.identityDigest
	ws.digestMu.Unlock()
	if identity != "" {
		return identity
	}

	probe := ws.ProbeDigest()
	h := sha256.New()
	writeIdentityField(h, identityDigestFormat)
	writeIdentityField(h, "workspace")
	writeIdentityField(h, ws.Name)
	writeIdentityField(h, probe)
	identity = identityDigestFormat + ":" + hex.EncodeToString(h.Sum(nil))

	ws.digestMu.Lock()
	ws.identityDigest = identity
	ws.digestMu.Unlock()
	return identity
}

// MetadataDigestFor is the per-project identity that enters every cache key
// affected by project metadata or dependency edges. Memoized per project ID.
func (ws *Workspace) MetadataDigestFor(project *Project) string {
	if project == nil {
		return ""
	}
	ws.digestMu.Lock()
	if digest, ok := ws.metadataDigests[project.ID]; ok {
		ws.digestMu.Unlock()
		return digest
	}
	ws.digestMu.Unlock()

	view := ProbeViewOf(project, ws.DependencyPathResolver())
	// Provenance is read by the graph checks at synchronization and by no
	// task. In a key it re-keys every dependent for an edit no task reads:
	// removing the last test-only import of a workspace module flips that edge
	// to `declared` and moves this digest, and with it every importer's
	// describe — the cascade the non-test source contract exists to remove.
	view.DependencySources = nil
	digest := ProjectMetadataDigest(view, project.Dependencies, project.Metadata, project.GeneratedClient)

	ws.digestMu.Lock()
	if ws.metadataDigests == nil {
		ws.metadataDigests = make(map[string]string, len(ws.Projects))
	}
	ws.metadataDigests[project.ID] = digest
	ws.digestMu.Unlock()
	return digest
}

// ProjectByName returns a project by name, or nil.
func (ws *Workspace) ProjectByName(name string) *Project {
	return ws.projectMap[name]
}

// ProjectByID returns a project by its canonical ID (e.g. "/typescript/frameworks/application"), or nil.
func (ws *Workspace) ProjectByID(id string) *Project {
	return ws.projectByID[id]
}

// ProjectByPath returns a project by its physical workspace-relative path.
// Paths and IDs deliberately have separate lookup semantics because transparent
// group folders are present only in paths.
func (ws *Workspace) ProjectByPath(projectPath string) *Project {
	projectPath = CleanWorkspacePath(projectPath)
	for _, p := range ws.Projects {
		if CleanWorkspacePath(p.Path) == projectPath {
			return p
		}
	}
	return nil
}

// ProjectsByPattern returns projects matching a path pattern.
// Supports:
//   - "/path/to/project" — exact match by ID
//   - "/path/to/..." — prefix wildcard (all projects under /path/to/)
//   - ".../suffix" — suffix wildcard (all projects whose ID ends with /suffix)
//   - ".../infix/..." — infix wildcard (all projects whose ID contains /infix/)
func (ws *Workspace) ProjectsByPattern(pattern string) []*Project {
	hasPrefixWild := strings.HasPrefix(pattern, ".../")
	hasSuffixWild := strings.HasSuffix(pattern, "/...")

	switch {
	case hasPrefixWild && hasSuffixWild:
		// Infix wildcard: ".../frameworks/..." → match projects containing "/frameworks/"
		infix := "/" + pattern[4:len(pattern)-4] + "/"
		var result []*Project
		for _, p := range ws.Projects {
			if strings.Contains(p.ID, infix) {
				result = append(result, p)
			}
		}
		return result

	case hasPrefixWild:
		// Suffix wildcard: ".../web" → match projects whose ID ends with "/web"
		suffix := "/" + pattern[4:]
		var result []*Project
		for _, p := range ws.Projects {
			if strings.HasSuffix(p.ID, suffix) {
				result = append(result, p)
			}
		}
		return result

	case hasSuffixWild:
		// Prefix wildcard: "/path/..." → match all projects under "/path/"
		prefix := strings.TrimSuffix(pattern, "...") // keeps trailing "/"
		var result []*Project
		for _, p := range ws.Projects {
			if strings.HasPrefix(p.ID, prefix) || p.ID+"/" == prefix {
				result = append(result, p)
			}
		}
		return result
	}

	// Exact match by ID
	if p := ws.ProjectByID(pattern); p != nil {
		return []*Project{p}
	}
	return nil
}

// ProbeResultView is the core-owned provider's answer for this workspace: every
// RESOLVED project — after the merged provider view was applied — projected onto
// the probe protocol's shape.
//
// It is folded together with the real providers' answers into the aggregate
// probe digest, so the workspace identity is a function of the contract shape
// rather than of a private encoding, and it stays meaningful in a workspace with
// no adapter-declaring extension at all: core is then the only provider.
func (ws *Workspace) ProbeResultView() wsproto.ProbeResult {
	result := wsproto.ProbeResult{
		Version:   wsproto.ProbeProtocolVersion,
		Extension: coreProbeExtension,
		Projects:  make([]wsproto.ProbeProject, 0, len(ws.Projects)),
	}
	for _, project := range ws.Projects {
		result.Projects = append(result.Projects, ProbeViewOf(project, ws.DependencyPathResolver()))
	}
	wsproto.NormalizeProbeResult(&result)
	return result
}

// coreProbeExtension is the provider name core answers under. It is not an
// installable extension: it names core's own contribution so a merged view can
// attribute every claim.
const coreProbeExtension = "core"

// DependencyPathResolver maps a declared dependency name (or ID) to the
// depended-on project's workspace-relative path. Keying dependency edges on
// PATHS — never on a module path and never assuming a project root is a Go
// module root — is the accepted contract wording for this slice.
func (ws *Workspace) DependencyPathResolver() func(string) (string, bool) {
	return func(dependency string) (string, bool) {
		if project := ws.ProjectByName(dependency); project != nil {
			return CleanWorkspacePath(project.Path), true
		}
		if project := ws.ProjectByID(dependency); project != nil {
			return CleanWorkspacePath(project.Path), true
		}
		return "", false
	}
}
