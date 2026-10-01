// Package wsview is the extension's read-only view of a Putnami workspace.
//
// # Why it exists
//
// The SDD engines were written against the CLI's `internal/workspace`, whose
// *Workspace is the output of a full loader: filesystem discovery, scope
// breadcrumbs, provider probes, dependency graph. An extension may not have
// that. It gets workspace knowledge from the job context the orchestrator
// wrote, and nothing else — no directory scan, no `putnami` subprocess.
//
// So this package is the SHAPE the engines read, plus the two ways it can be
// filled: FromContext for a job invocation, and NewWorkspace for a caller that
// already resolved membership itself (the git-revision reader builds one out of
// an immutable commit's tree).
//
// It is deliberately NOT a loader. There is no Load, no discovery, no probe. A
// field this view cannot carry is a gap in the wire contract to be closed in
// protocols/job, never a fact to re-derive here — two derivations of the same
// project identity is exactly how a validator and the orchestrator start
// disagreeing about what the workspace contains.
//
// # What the wire supplies today
//
// FromContext fills Root and the membership, taking the most complete member
// the context offers:
//
//   - `workspaceProjects` — the COMPLETE resolved membership, with each
//     project's RESOLVED DIRECT dependency edges. It reaches every job, and it
//     is the only member a workspace-wide READ may be built on. Whether a task
//     may make a workspace-wide CLAIM is a different question, answered by
//     `selection`.
//   - `selectedProjects` — the run's selection, written by both command paths.
//     A view built from it is a SUBSET whenever the run was narrowed, so
//     FromContext attaches WarningCodeProviderViewUnavailable and the
//     architecture engine fails closed instead of reporting a subset as the
//     whole.
//   - `project` — the job's own project, for a project-scoped task, which
//     receives neither collection. Its `publish` and `options` travel with it,
//     so spec completeness sees a publishable project as publishable.
//   - `workspace.options` — the committed workspace-level extension option
//     blocks, decoded separately by WorkspaceOptionsFromContext for a
//     workspace-scoped policy. They never become a reason to reload the
//     workspace in this extension.
//
// One limit remains, and every caller must know it:
//
//   - The membership is what a view can SEE, not what it may report on. A
//     per-project task builds the full membership here and narrows its
//     PROJECTION with a Selection naming its own project, which is the engines'
//     own two-tier split: identities are minted workspace-wide, evaluation is
//     scoped. A task that widened its projection to match its membership would
//     report every project's findings under one project's key.
package wsview

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	workspaceproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
)

// WorkspaceOptionsFromContext decodes the committed workspace option blocks
// carried by job-context v2. It never reads putnami.workspace.json: the
// orchestrator is the one workspace loader, and the context is the task's
// bounded authority. Invalid block shapes fail closed in deterministic key
// order because these values decide whether automatic validation may block.
func WorkspaceOptionsFromContext(ctx *pctx.Context) (map[string]map[string]any, error) {
	if ctx == nil || len(ctx.Workspace.Options) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(ctx.Workspace.Options))
	for name := range ctx.Workspace.Options {
		names = append(names, name)
	}
	sort.Strings(names)
	options := make(map[string]map[string]any, len(names))
	for _, name := range names {
		var block map[string]any
		if err := json.Unmarshal(ctx.Workspace.Options[name], &block); err != nil || block == nil {
			return nil, fmt.Errorf("workspace.options.%s must be a non-null JSON object", name)
		}
		options[name] = block
	}
	return options, nil
}

// Workspace is the resolved membership one SDD evaluation runs over.
//
// Field-compatible with the members `go.putnami.dev/cli/model/workspace`.Workspace
// exposes that the SDD engines read, so the engines are the same code on both
// sides of the extraction.
type Workspace struct {
	Name     string
	Version  string
	Root     string
	Config   *workspaceproto.Config
	Projects []*Project
	// Graph is the resolved direct-dependency index over Projects, built by
	// NewWorkspace. Empty in a wire-built view — see the package doc.
	Graph *DependencyGraph
	// Warnings and WarningCodes are the non-fatal findings the party that
	// resolved this membership attached to it. The architecture engine fails
	// closed on WarningCodeProviderViewUnavailable rather than enforcing
	// dependency rules over a graph it knows is partial.
	Warnings     []string
	WarningCodes []WarningCode

	projectByID   map[string]*Project
	projectByName map[string]*Project
}

// Project is one workspace member.
//
// Field-compatible with the members of `go.putnami.dev/cli/model/workspace`.Project
// the SDD engines read. Everything the loader computes and the engines ignore —
// the dependency graph edges, provider metadata, scope contributions,
// per-project config diagnostics — is absent on purpose.
type Project struct {
	// ID is the canonical logical identity, transparent group folders omitted.
	ID string
	// Name is the resolved project name.
	Name string
	// SourceName is the identity the project DECLARED, before a scope
	// namePattern override.
	SourceName string
	// Version is the version the orchestrator resolved for this project: the
	// base version of its line.
	// The orchestrator resolves it and puts the answer on the wire; empty means
	// nobody said, and a package selector must then match by name alone rather
	// than invent one.
	Version string
	// Type is the resolved classification ("library", "application").
	Type string
	// Path is the project directory relative to the workspace root.
	Path string
	// Tags, Dependencies, Publish, Extensions and RunsWith mirror the authored
	// putnami.json members. Only the revision reader populates them today,
	// except Extensions, which the job context carries as resolved.
	Tags         []string
	Dependencies []string
	Publish      []string
	Extensions   []string
	RunsWith     []string
	// ActivatedScope and ScopeIncludes carry a scope-self project's membership,
	// so BuildGraph can add the same implicit include→scope edges core adds.
	// Nothing on the wire supplies them.
	ActivatedScope bool
	ScopeIncludes  []string
	// Config is the parsed putnami.json, when the caller read one.
	Config *workspaceproto.ProjectConfig
}

// NewWorkspace assembles a Workspace from already-resolved projects, building
// the dependency graph and the name/id lookup indexes. Name comes from the
// workspace config when one is supplied.
//
// Same signature and same field derivation as the model constructor the CLI
// uses. What it does NOT do is discover anything: the projects are the caller's
// answer, and this only indexes them.
func NewWorkspace(root string, config *workspaceproto.Config, projects []*Project) *Workspace {
	ws := &Workspace{Root: root, Config: config, Projects: projects}
	if config != nil {
		ws.Name = config.Name
	}
	ws.rebuildIndexes()
	return ws
}

func (ws *Workspace) rebuildIndexes() {
	ws.projectByID = make(map[string]*Project, len(ws.Projects))
	ws.projectByName = make(map[string]*Project, len(ws.Projects))
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		if project.ID != "" {
			ws.projectByID[project.ID] = project
		}
		if project.Name != "" {
			ws.projectByName[project.Name] = project
		}
	}
	ws.Graph = BuildGraph(ws.Projects)
}

// ProjectByID returns the project with the given canonical id, or nil.
func (ws *Workspace) ProjectByID(id string) *Project {
	if ws == nil {
		return nil
	}
	return ws.projectByID[id]
}

// ProjectByName returns the project with the given resolved name, or nil.
func (ws *Workspace) ProjectByName(name string) *Project {
	if ws == nil {
		return nil
	}
	return ws.projectByName[name]
}

// AddWarning appends human prose and, when provided, its stable machine code.
// Copied from `go.putnami.dev/cli/model/workspace`.
func (ws *Workspace) AddWarning(code WarningCode, message string) {
	if ws == nil {
		return
	}
	ws.Warnings = append(ws.Warnings, message)
	if code != "" && !ws.HasWarningCode(code) {
		ws.WarningCodes = append(ws.WarningCodes, code)
	}
}

// HasWarningCode reports whether the party that resolved this membership
// attached a stable warning reason. Callers must branch on this instead of on
// Warnings message text.
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

// FromContext builds the view a job subprocess is entitled to from its context
// document. It reads only the context: no filesystem, no subprocess.
//
// A nil context yields nil, which every engine already treats as "workspace is
// required" rather than as an empty workspace.
//
// Membership comes from the most complete member the wire offers, in this
// order, and the order is the contract rather than a preference:
//
//  1. `workspaceProjects` — the COMPLETE resolved membership with its direct
//     dependency edges. A workspace-wide verdict is provable only from this.
//  2. `selectedProjects` — the run's selection, for an orchestrator that
//     predates the member. A workspace-wide verdict is NOT provable from it,
//     which is why falling back here also raises the incomplete-view warning
//     below.
//  3. `project` — the job's own project, for a project-scoped task, which
//     receives neither collection. Its `publish` and `options` travel too, so
//     spec completeness sees a publishable project as publishable.
//
// Scoped reports whether the invocation NARROWED the run: false means the
// projects below are the whole workspace, true means they are a subset and a
// workspace-wide claim would be a lie. It is false when the context carries no
// `selection` block at all — an orchestrator older than the member — which is
// the same reading every other consumer of an absent optional member takes.
func FromContext(ctx *pctx.Context) (ws *Workspace, scoped bool) {
	if ctx == nil {
		return nil, false
	}
	refs, complete := ctx.WorkspaceProjects, true
	if len(refs) == 0 {
		refs, complete = ctx.SelectedProjects, false
	}
	projects := make([]*Project, 0, len(refs))
	for _, ref := range refs {
		id := ref.ID
		if id == "" {
			id = ProjectIDFromPath(ref.Path)
		}
		projects = append(projects, &Project{
			ID:         id,
			Name:       ref.Name,
			SourceName: ref.SourceName,
			// The version the orchestrator resolved, not a re-read of any
			// manifest: a package-root source selector keys on name AND
			// version, and a view that dropped it would degrade every
			// version-qualified binding to "source unavailable" while the
			// versionless ones beside it kept resolving.
			Version: ref.Version,
			Path:    CleanWorkspacePath(ref.Path),
			// The RESOLVED direct edges, already translated from declared names
			// to workspace ids. BuildGraph keeps an id verbatim, so the graph
			// this view indexes is core's graph rather than a second reading of
			// anybody's manifest.
			Dependencies: append([]string(nil), ref.Dependencies...),
			Config:       decodeProjectConfig(ref.Config),
			// The extensions the orchestrator resolved, so a workspace step
			// applies an extension-keyed option block to that extension's
			// projects only.
			Extensions: append([]string(nil), ref.Extensions...),
		})
	}
	if len(projects) == 0 {
		if own := projectFromOwnContext(ctx); own != nil {
			projects = append(projects, own)
		}
	}
	adoptOwnProjectFacts(ctx, projects)

	view := NewWorkspace(ctx.WorkspaceRoot, nil, projects)
	view.Name = ctx.Workspace.Name
	view.Version = ctx.Workspace.Version
	if !complete && len(ctx.SelectedProjects) > 0 {
		// The orchestrator ran this task ONCE FOR THE WORKSPACE and handed it
		// the selection instead of the membership. Every workspace-wide claim is
		// then unprovable in BOTH directions — a member outside the selection
		// reads as absent, and an edge into it is never walked — so the view
		// says so rather than letting a validator report a subset as the whole.
		//
		// The guard is on SelectedProjects rather than on the membership alone
		// because a PROJECT-scoped job legitimately has neither: it falls back
		// to its own project below, and that is a narrow view, not a partial
		// workspace one.
		view.AddWarning(WarningCodeProviderViewUnavailable,
			"the job context carries the project selection but not the workspace membership, "+
				"so workspace-wide enforcement has an incomplete project view")
	}
	return view, ctx.Selection != nil && ctx.Selection.Scoped
}

// projectFromOwnContext builds the one-project membership a PROJECT-SCOPED task
// is entitled to. Such a task receives neither collection — it runs for one
// project and the orchestrator says which — so its own `project` block is the
// whole answer, and a view that ignored it would discover nothing at all.
//
// A project rooted at the workspace root yields nothing, and that is not a
// missed case: it is the SYNTHETIC project a workspace-once job runs for, and
// minting a member out of it would put a project in the view that no
// putnami.json declares. Discovery already treats the workspace root as a root,
// so the answer is the same either way.
func projectFromOwnContext(ctx *pctx.Context) *Project {
	path := CleanWorkspacePath(ctx.Project.Path)
	if path == "" {
		return nil
	}
	return &Project{
		ID:   ProjectIDFromPath(path),
		Name: ctx.Project.Name,
		Type: ctx.Project.Type,
		Path: path,
	}
}

// adoptOwnProjectFacts copies the authored facts the wire carries for the job's
// OWN project onto its entry in the view.
//
// `project.publish` and `project.options` are on every context document for the
// job's own project. WorkspaceProjects now also carries each parsed config, so
// this merge enriches that config instead of replacing it. Spec completeness
// reads both to decide whether a project is published, and a TypeScript package
// declares publication only through `options.publish.npm`, so without them a
// publishable project reads as unpublished and its completeness gap is never
// assessed.
func adoptOwnProjectFacts(ctx *pctx.Context, projects []*Project) {
	path := CleanWorkspacePath(ctx.Project.Path)
	publish := ctx.PublishChannels()
	options := decodeProjectOptions(ctx.Project.Options)
	if len(publish) == 0 && len(options) == 0 {
		return
	}
	for _, project := range projects {
		if project == nil || project.Path != path {
			continue
		}
		project.Publish = publish
		if len(options) > 0 {
			if project.Config == nil {
				project.Config = &workspaceproto.ProjectConfig{Name: project.Name}
			}
			project.Config.Publish = publish
			project.Config.Options = options
		}
		return
	}
}

// decodeProjectOptions decodes the per-extension option blocks the wire keeps
// raw. A block that is not an object is skipped rather than reported: the
// orchestrator never interprets these, and an option shape this view cannot
// read is the owning extension's business, not a reason to fail a spec verdict.
func decodeProjectOptions(raw map[string]json.RawMessage) map[string]map[string]any {
	if len(raw) == 0 {
		return nil
	}
	options := make(map[string]map[string]any, len(raw))
	for name, block := range raw {
		var decoded map[string]any
		if json.Unmarshal(block, &decoded) != nil || decoded == nil {
			continue
		}
		options[name] = decoded
	}
	if len(options) == 0 {
		return nil
	}
	return options
}

// SeedRoots returns the workspace-relative roots of the view's projects, in
// wire order. It is what a caller hands to the feature engine's scope builder.
func (ws *Workspace) SeedRoots() []string {
	if ws == nil {
		return nil
	}
	roots := make([]string, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project != nil {
			roots = append(roots, project.Path)
		}
	}
	return roots
}

// ProjectIDFromPath derives a project's canonical logical ID from its physical
// workspace-relative path, dropping transparent "(group)" folders.
//
// Copied from `go.putnami.dev/cli/model/workspace`.ProjectIDFromPath: the
// revision reader mints ids for projects it discovered in a historical commit,
// and an id that disagreed with core's would make two runs over the same tree
// report different features.
func ProjectIDFromPath(projectPath string) string {
	physicalPath := CleanWorkspacePath(projectPath)
	if physicalPath == "" {
		return "/"
	}

	segments := strings.Split(physicalPath, "/")
	logicalSegments := make([]string, 0, len(segments))
	for _, segment := range segments {
		if isTransparentGroupSegment(segment) {
			continue
		}
		logicalSegments = append(logicalSegments, segment)
	}
	return "/" + strings.Join(logicalSegments, "/")
}

func isTransparentGroupSegment(segment string) bool {
	return len(segment) > 2 && segment[0] == '(' && segment[len(segment)-1] == ')'
}

// CleanWorkspacePath normalizes a workspace-relative path to its canonical
// slash form. Copied from `go.putnami.dev/cli/model/workspace`.
func CleanWorkspacePath(path string) string {
	path = strings.TrimSpace(strings.TrimPrefix(path, "/"))
	if path == "" {
		return ""
	}
	path = filepath.Clean(filepath.FromSlash(path))
	if path == "." {
		return ""
	}
	return filepath.ToSlash(path)
}
