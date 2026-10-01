// Package workspace handles workspace detection, project discovery,
// dependency graph construction, and project filtering.
package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	diag "go.putnami.dev/protocol/diagnostic"
	wsproto "go.putnami.dev/protocol/workspace"
)

// loadCache memoizes Load results per absolute root path. A single CLI
// invocation calls Load at least twice (extension discovery + runJobCommands)
// and each call does a full multi-scope config merge plus O(N) project
// discovery; without this cache the cost is paid twice on every command.
//
// The workspace index is different from ordinary source input: another
// Putnami process may publish it while a long-lived MCP session is already
// running. Every entry therefore carries the atomic snapshot file's identity.
// The warm path is one stat; when WriteSnapshot replaces the file, os.SameFile
// changes even if size and timestamps happen to match, and the next Load
// rebuilds from the new provider view.
var loadCache sync.Map // map[string]loadCacheEntry

type loadCacheEntry struct {
	workspace *Workspace
	index     snapshotFileIdentity
}

type snapshotFileIdentity struct {
	info    os.FileInfo
	errText string
}

func snapshotIdentity(root string) snapshotFileIdentity {
	info, err := os.Stat(SnapshotPath(root))
	if err == nil {
		return snapshotFileIdentity{info: info}
	}
	if os.IsNotExist(err) {
		return snapshotFileIdentity{}
	}
	return snapshotFileIdentity{errText: err.Error()}
}

func (id snapshotFileIdentity) same(other snapshotFileIdentity) bool {
	if id.errText != other.errText || (id.info == nil) != (other.info == nil) {
		return false
	}
	if id.info == nil {
		return true
	}
	return id.info.Size() == other.info.Size() &&
		id.info.ModTime().Equal(other.info.ModTime()) &&
		os.SameFile(id.info, other.info)
}

// loadKey normalizes the cache key. Different relative spellings of the
// same root resolve to one entry, and a missing/inaccessible root falls
// back to the raw input so the slow path can surface the error. It is the
// root Load assigns (canonicalRoot), so a root reached through a link,
// a Windows junction included, shares the entry of the directory it names.
func loadKey(root string) string {
	return canonicalRoot(root)
}

// InvalidateLoadCache drops the cached *Workspace for root. Tests use
// this between scenarios; production callers should not need it because
// watch mode does not re-Load within a session.
func InvalidateLoadCache(root string) {
	loadCache.Delete(loadKey(root))
}

// FindRoot walks upward from startDir looking for a workspace root:
//   - putnami.workspace.json (always workspace-level)
//   - package.json with a "workspaces" field (monorepo root)
func FindRoot(startDir string) (string, error) {
	dir := startDir
	for {
		// Check for workspace-level config
		if wsproto.IsWorkspaceConfig(dir) {
			return canonicalRoot(dir), nil
		}
		// Check for package.json with "workspaces" field
		if isWorkspacePackageJSON(filepath.Join(dir, "package.json")) {
			return canonicalRoot(dir), nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", os.ErrNotExist
}

// isWorkspacePackageJSON returns true if the file exists and contains a "workspaces" field.
func isWorkspacePackageJSON(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var v struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return false
	}
	return len(v.Workspaces) > 0
}

// Load discovers the workspace from the given root path. Results are
// memoized per absolute root for the process lifetime; the first call
// pays the full multi-scope config merge + project discovery cost and
// subsequent calls reuse the same *Workspace.
func Load(root string) (*Workspace, error) {
	key := loadKey(root)
	for range 2 {
		currentIndex := snapshotIdentity(root)
		if cached, ok := loadCache.Load(key); ok {
			if entry, ok := cached.(loadCacheEntry); ok && entry.workspace != nil && entry.index.same(currentIndex) {
				return entry.workspace, nil
			}
		}
		ws, err := loadUncached(root)
		if err != nil {
			return nil, err
		}
		// Bind the workspace only when the index identity stayed fixed across the
		// read. If another process published between the two stats, retry so a
		// pre-index graph can never be cached under the new file's identity.
		if !currentIndex.same(snapshotIdentity(root)) {
			continue
		}
		candidate := loadCacheEntry{workspace: ws, index: currentIndex}
		if actual, loaded := loadCache.LoadOrStore(key, candidate); loaded {
			if entry, ok := actual.(loadCacheEntry); ok && entry.workspace != nil && entry.index.same(currentIndex) {
				return entry.workspace, nil
			}
			loadCache.Store(key, candidate)
		}
		return ws, nil
	}

	// A continuously replaced index is still safe to read because each publish
	// is atomic. Keep this complete point-in-time view under the identity from
	// before its read; the next Load will necessarily miss if the file moved.
	currentIndex := snapshotIdentity(root)
	ws, err := loadUncached(root)
	if err != nil {
		return nil, err
	}
	loadCache.Store(key, loadCacheEntry{workspace: ws, index: currentIndex})
	return ws, nil
}

func loadUncached(root string) (*Workspace, error) {
	root = canonicalRoot(root)
	cfg := wsproto.Load(root)

	projects := discoverProjects(root, cfg)
	resolveContractBindings(root, projects)
	if err := checkDuplicateIDs(projects); err != nil {
		return nil, err
	}
	if err := checkProjectConfigDiagnostics(root, projects); err != nil {
		return nil, err
	}
	ws := NewWorkspace(root, cfg, projects)

	// An authored `jobs` block is parsed and then dropped on the floor.
	// Project-level job definitions were never wired into planning, and they are
	// not going to be — a raw command/args override would bypass the declared
	// reads/writes and the contract digest that every scheduled task folds into
	// its cache key. Saying so at load time is the whole contract: the key is
	// accepted by the parser, so without this the operator's config silently does
	// nothing.
	if declaring := projectsDeclaringJobs(ws); len(declaring) > 0 {
		ws.Warnings = append(ws.Warnings, fmt.Sprintf(
			"%d project(s) (%s) declare `jobs` in %s, but project-level jobs are NOT implemented — "+
				"the key is parsed and ignored; use `disable.jobs` to opt out of an extension-provided job, "+
				"or `tasks` for per-project execution tuning",
			len(declaring), strings.Join(declaring, ", "), wsproto.ConfigFilename))
	}

	// Adopt the RECORDED provider view before any check runs.
	// Since the core parsers were deleted, a project's language
	// identity and its dependency edges come from the merged probe answer, and
	// `.putnami/workspace-index.json` is where the last one is stored.
	//
	// This costs one file read and NO extension process, which is the whole
	// point: every command that only READS the workspace — `projects list`,
	// `deps`, the MCP graph tools — gets the same identity a probing run
	// resolves, without paying for a probe. A run that plans or executes over
	// the graph re-validates the snapshot and re-probes what moved
	// (Synchronize), then adopts again; adoption is idempotent, so the second
	// application either reproduces the first exactly or replaces it wholesale.
	//
	// An unreadable snapshot is recorded as a warning and treated as absent:
	// the next `putnami projects sync` rebuilds it, and refusing to load a
	// workspace because its index is corrupt would make the repair command
	// unreachable.
	snapshot, snapshotErr := LoadSnapshot(root)
	if snapshotErr != nil {
		ws.Warnings = append(ws.Warnings, fmt.Sprintf(
			"workspace index could not be read and will be rebuilt on the next `putnami projects sync`: %v", snapshotErr))
	}
	merged := mergeRecordedResults(snapshot, ws)

	// The recorded view is VALIDATED before it is adopted.
	//
	// A recorded view that resolves two projects to one name, or that closes a
	// dependency cycle, fails the checks below — and because adoption used to
	// happen unconditionally and first, such an index made the workspace
	// permanently unloadable: every repair command loads the workspace, and
	// loading re-adopted the poisoned view before it could refuse it. Editing
	// the manifests did not help; only hand-deleting a file nothing named could.
	//
	// So a rejected recorded view degrades to the un-adopted resolution — a
	// strictly smaller identity, the same one a fresh checkout has — and says so,
	// naming the file to rebuild or remove.
	if len(merged) > 0 {
		if err := ws.AdoptValidatedProbeView(merged); err != nil {
			ws.AddWarning(WarningCodeProviderViewUnavailable, fmt.Sprintf(
				"workspace index %s records a provider view this workspace cannot adopt (%v); "+
					"continuing WITHOUT provider-derived identity — run `putnami projects sync` to rebuild it, "+
					"or delete %s", WorkspaceIndexFilename, err, SnapshotPath(root)))
			merged = nil
		}
	} else {
		ws.AdoptProbeViewUnvalidated(merged)
	}

	// A load that adopted NOTHING resolves every project from authored config
	// alone: no language identity, and no provider-derived dependency edges. That
	// is a legitimate state (a workspace with no adapter-declaring extension is
	// exactly this), but it is indistinguishable — from the outside — from a
	// workspace whose index is simply missing, and the read-only surfaces that
	// serve it (`projects list`, `deps`, the MCP graph tools, `--impacted`) would
	// silently under-report every provider-derived edge.
	//
	// The cheap, precise signal that this workspace EXPECTS a provider view is a
	// declared member core cannot describe at all: a directory with no
	// putnami.json is a member only a marker scan could have found, so only a
	// provider knows what it is called and what it depends on.
	if len(merged) == 0 {
		if awaiting, total := projectsAwaitingProbe(ws); total > 0 {
			ws.AddWarning(WarningCodeProviderViewUnavailable, fmt.Sprintf(
				"no workspace provider view (%s): %d project(s) (%s) resolve from authored config alone, so their "+
					"language identity and provider-derived dependency edges are missing — "+
					"run `putnami projects sync` to build the workspace index",
				WorkspaceIndexFilename, total, strings.Join(awaiting, ", ")))
		}
	}

	// Cycles surface here regardless of source (declared deps, provider-derived
	// edges, or implicit activated-scope edges). They're not recoverable
	// downstream so we fail the load. Since the recorded view is validated
	// before adoption, reaching this is an AUTHORED cycle — one an operator can
	// fix by editing a putnami.json, without deleting anything.
	if cycle := ws.Graph.FindCycle(); cycle != nil {
		return nil, fmt.Errorf("dependency cycle detected: %s", strings.Join(cycle, " → "))
	}

	// Detect name divergence: manifest identity vs scope-resolved name.
	//
	// ONLY when a provider view was adopted. Without one, every convention-named
	// project looks unaligned — its source identity has not been read yet — and
	// reporting that would tell an operator to `projects sync` a workspace whose
	// only problem is that it has never been probed. The run's probe phase
	// reports the real divergences once it has an answer.
	if len(merged) > 0 {
		ws.Warnings = append(ws.Warnings, NameDivergences(ws)...)
	}

	// Project names are identities: they key ProjectByName selection and the
	// config server's per-app storage prefix (the schema appName). A collision
	// silently makes one project unaddressable by name and clobbers another's
	// registered config, so a duplicate resolved name is a hard load error.
	//
	// As with the cycle check above, a recorded provider view can no longer
	// reach this: it was validated before adoption, so a collision here is one
	// the authored configuration itself declares.
	if err := checkDuplicateNames(ws.Projects); err != nil {
		return nil, err
	}

	// Build scope index (aggregated aliases and groups from all scopes).
	projectPaths := make([]string, len(ws.Projects))
	for i, p := range ws.Projects {
		projectPaths[i] = p.Path
	}
	scopeIndex, err := wsproto.ScanAllScopes(root, projectPaths)
	if err != nil {
		return nil, fmt.Errorf("scan scopes: %w", err)
	}
	ws.ScopeIndex = scopeIndex
	ws.Lines = scopeIndex.Lines

	resolveRegistries(ws)

	return ws, nil
}

// resolveRegistries settles every registry endpoint the run will need, once, at
// the workspace boundary: the workspace section, and each project's effective
// view of it.
//
// A project's override replaces an ecosystem's entry WHOLE. The keys inside an
// entry belong to the ecosystem profile the owning extension declares, so a
// deep merge here would synthesize a document no profile validates — a project
// that states `registries.npm` states all of it.
func resolveRegistries(ws *Workspace) {
	var declared map[string]json.RawMessage
	if ws.Config != nil {
		declared = ws.Config.Registries
	}
	ws.Registries = declared

	for _, project := range ws.Projects {
		var overrides map[string]json.RawMessage
		if project.Config != nil {
			overrides = project.Config.Registries
		}
		if len(overrides) == 0 {
			project.Registries = declared
			continue
		}
		effective := make(map[string]json.RawMessage, len(declared)+len(overrides))
		for ecosystem, entry := range declared {
			effective[ecosystem] = entry
		}
		for ecosystem, entry := range overrides {
			effective[ecosystem] = entry
		}
		project.Registries = effective
	}
}

// projectsDeclaringJobs lists the paths of every project whose putnami.json
// carries a top-level `jobs` object — the accepted-but-ignored surface the
// loader warns about.
//
// Presence, not size: `{"jobs": {}}` declares the key just as much as a populated
// one does, and the project-marker scan already treats it that way (it keys off
// `Jobs != nil`). Testing len() here would let an empty block mark a directory as
// a project and then say nothing about it — the same silent no-op this warning
// exists to end. `"jobs": null` and an absent key both leave the map nil and are
// correctly quiet.
//
// Paths (not names) because the path is what the operator has to open to remove
// the key, and because a project that has not been probed yet may not have a
// name. Sorted so one tree produces one warning string, run after run.
func projectsDeclaringJobs(ws *Workspace) []string {
	if ws == nil {
		return nil
	}
	paths := make([]string, 0)
	for _, project := range ws.Projects {
		if project.Config == nil || project.Config.Jobs == nil {
			continue
		}
		paths = append(paths, project.Path)
	}
	sort.Strings(paths)
	return paths
}

// projectsAwaitingProbe lists the members core cannot describe on its own: a
// declared project directory with no putnami.json. Such a member became one
// through a marker scan, so its name, its type and its dependency edges exist
// only in a provider's answer.
//
// The list is bounded for the diagnostic (three paths and a count) because a
// workspace that has never been probed has ALL of them, and a warning that
// prints two hundred paths is one nobody reads.
func projectsAwaitingProbe(ws *Workspace) (paths []string, total int) {
	const shown = 3
	paths = make([]string, 0, shown+1)
	for _, project := range ws.Projects {
		if project.Config != nil {
			continue
		}
		total++
		if len(paths) < shown {
			paths = append(paths, project.Path)
		}
	}
	if total > len(paths) {
		paths = append(paths, fmt.Sprintf("… and %d more", total-shown))
	}
	return paths, total
}

// checkDuplicateIDs returns an error when transparent group folders cause two
// physical project paths to collapse to the same logical ID. The diagnostic is
// sorted so workspace loading fails deterministically regardless of discovery
// order.
func checkDuplicateIDs(projects []*Project) error {
	paths := make(map[string][]string, len(projects))
	for _, p := range projects {
		paths[p.ID] = append(paths[p.ID], p.Path)
	}

	duplicateIDs := make([]string, 0)
	for id, physicalPaths := range paths {
		if len(physicalPaths) > 1 {
			duplicateIDs = append(duplicateIDs, id)
		}
	}
	if len(duplicateIDs) == 0 {
		return nil
	}
	sort.Strings(duplicateIDs)

	parts := make([]string, len(duplicateIDs))
	for i, id := range duplicateIDs {
		physicalPaths := append([]string(nil), paths[id]...)
		sort.Strings(physicalPaths)
		parts[i] = fmt.Sprintf("%q (%s)", id, strings.Join(physicalPaths, ", "))
	}
	return fmt.Errorf(
		"duplicate project ID(s) %s; transparent group folders must not collapse distinct project paths to the same logical ID",
		strings.Join(parts, "; "),
	)
}

// checkProjectConfigDiagnostics turns invalid task deadline tuning into a
// deterministic workspace-load error. The loader validates only the new tasks
// block, so existing partial project configs stay as loadable as before.
func checkProjectConfigDiagnostics(root string, projects []*Project) error {
	var offenders []*Project
	for _, project := range projects {
		if project != nil && diag.HasErrors(project.ConfigDiagnostics) {
			offenders = append(offenders, project)
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	sort.Slice(offenders, func(i, j int) bool { return offenders[i].Path < offenders[j].Path })

	var details []string
	for _, project := range offenders {
		configPath := filepath.Join(root, filepath.FromSlash(project.Path), wsproto.ConfigFilename)
		for _, finding := range diag.Errors(project.ConfigDiagnostics) {
			details = append(details, fmt.Sprintf("%s: %s: %s", configPath, finding.Field, finding.Message))
		}
	}
	return fmt.Errorf("invalid project configuration:\n  %s", strings.Join(details, "\n  "))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
