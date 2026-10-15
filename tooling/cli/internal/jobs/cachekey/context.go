package cachekey

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	extensionmodel "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// JobContextProjectRef locates one project a context references — a member of
// the current project's dependency closure. It carries exactly what a consumer
// needs to read that project's files: its identity and both path forms.
type JobContextProjectRef struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	FullPath string `json:"fullPath"`
	// SourceName is the declared identity, the same member selectedProjects
	// carries. The protocol defines ONE project-reference shape so a consumer
	// reads a reference the same way wherever it appears; populating it in only
	// one of the two places would make that false in the direction that bites —
	// a consumer reading a closure member would see an empty declared identity
	// and take it for "nothing was declared" rather than "nobody said".
	SourceName string `json:"sourceName,omitempty"`
	// Version is the resolved effective version, the same member
	// selectedProjects carries, and populated here for the same one-shape
	// reason as SourceName.
	Version string `json:"version,omitempty"`
	// Dependencies are the ids of the projects this one depends on DIRECTLY,
	// from the resolved graph. The closure this reference lives in answers what
	// a project transitively reaches; this answers what it declared, which is
	// what a rule ABOUT the graph reads. Populated in every carrier for the same
	// one-shape reason as SourceName.
	Dependencies []string `json:"dependencies,omitempty"`
	// Config is the authored putnami.json, re-encoded from the loader's parsed
	// workspace-protocol value. It is populated only for the complete membership:
	// that is where a consumer answers questions about sibling projects.
	Config json.RawMessage `json:"config,omitempty"`
	// Extensions are the extension references the project resolves to: its
	// putnami.json list, else the provider's view, else its scope's. Set on
	// workspaceProjects only, as Config is.
	Extensions []string `json:"extensions,omitempty"`
	// Type is the resolved classification: the authored putnami.json type, else
	// the provider probe's, else "application". Set on workspaceProjects only,
	// as Config is, and always set there, so a workspace task tells a workload
	// from a library without reading Config, which holds only the authored half.
	Type string `json:"type,omitempty"`
}

// JobContextVersion describes the computed version metadata in the context file.
type JobContextVersion struct {
	Base string `json:"base"`
	Full string `json:"full"`
	SHA  string `json:"sha"`
	// Branch is the checked-out branch. It is provenance, not a channel: which
	// channels a publication advances is the release set's business.
	Branch string `json:"branch"`
	// Tag is the line tag HEAD carries, or "" when the commit is not a release
	// of this line. It is the tag itself — "ts/v0.4.0" — never a derived name.
	Tag    string `json:"tag"`
	Suffix string `json:"suffix"`
	// Tagged reports that HEAD carries this line's tag, so Base is the tag's
	// version and Full has no suffix.
	Tagged  bool `json:"tagged,omitempty"`
	IsDirty bool `json:"isDirty"`
	// Line is the scope path of the version line this version belongs to. ""
	// is the root line.
	Line string `json:"line,omitempty"`
}

// RunVersions is one run's version per version line, keyed by the line's scope
// path ("" is the root line).
//
// A repository releases its lines separately (D26), so there is no single "the
// version of this run": a TypeScript project and a Go module built by one
// command carry the versions of their own lines. Everything that used to take
// one *JobContextVersion takes this map and resolves it per project.
type RunVersions map[string]*JobContextVersion

// VersionInfoForProject returns the version of the line a project belongs to.
//
// A project whose line has no entry — a synthetic project in a test fixture, a
// line whose git state could not be read — falls back to the root line when the
// workspace declares one, and otherwise answers nil: a job then carries no
// version member, which is exactly what a run without git already did.
func VersionInfoForProject(versions RunVersions, project *workspace.Project) *JobContextVersion {
	if len(versions) == 0 {
		return nil
	}
	line := ""
	if project != nil {
		line = project.Line
	}
	if info, ok := versions[line]; ok {
		return info
	}
	return versions[""]
}

// Primary is the version a whole-run report is filed under: the root line's
// when the workspace declares one, else the first line in sorted order.
//
// A run spans several lines and there is no single version for it, so this is
// deliberately a REPORTING answer only — the revision and branch a session
// report records, which every line shares because they come from one tree.
// Nothing that stamps an artifact or keys a cache may read it.
func (v RunVersions) Primary() *JobContextVersion {
	if info, ok := v[""]; ok {
		return info
	}
	lines := make([]string, 0, len(v))
	for line := range v {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return nil
	}
	return v[lines[0]]
}

// LineBaseVersion is the base version of a project's line, or "" when no
// version was computed for it. It is the value the wire's project references
// and the workspace block carry: a package reference is a pair, and the version
// half of the pair is the line's, never a field an author declared.
func LineBaseVersion(versions RunVersions, project *workspace.Project) string {
	if info := VersionInfoForProject(versions, project); info != nil {
		return info.Base
	}
	return ""
}

// ResolvedJobParams returns the exact resolved parameter bag delivered to a
// task. Keeping this projection shared by the job context and cache-key builder
// prevents the cache from hashing a different value than the subprocess reads.
func ResolvedJobParams(
	job *jobmodel.ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
) map[string]any {
	params := MergeParamLayers(
		configDefaults,
		job.JobDef,
		job.JobDef.Defaults,
		job.Project,
		job.Extension.Name,
		jobmodel.JobCommandName(job),
		nil,
		commandParams,
	)

	ApplyBoundParams(params, job.JobDef.BoundParams)
	return params
}

// ApplyBoundParams is the single precedence rule for planned task-local
// parameters. The planner uses it when a prerequisite command becomes the
// owner of another prerequisite, and BuildJobContext uses it at dispatch, so a
// synthetic value cannot activate a different plan than the task receives.
func ApplyBoundParams(params extensionmodel.ParamMap, bound extensionmodel.ParamMap) {
	// A literal step binding is an explicit task input, not another command
	// default. Apply it after every shared layer so a CLI/project value cannot
	// override the same exact spelling and so the value remains local to this
	// job. Alias projection happens exactly once for this layer: an authored
	// camelCase key wins over a derived alias in the same layer, while this
	// higher-precedence layer still overrides command and project defaults.
	ApplyProjectedParamLayer(params, bound)
}

// ProjectParamAliases is the one pure parameter projection shared by planning,
// invocation compatibility, and dispatch. It preserves every exact spelling,
// then adds deterministic kebab-to-camel aliases. An exact camelCase spelling
// always wins; when several legacy spellings collapse to the same alias, the
// lexicographically first source wins. Sources name the exact input spelling
// used for collision diagnostics.
func ProjectParamAliases(params extensionmodel.ParamMap) (extensionmodel.ParamMap, map[string]string) {
	projected := make(extensionmodel.ParamMap, len(params))
	sources := make(map[string]string, len(params))
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		projected[name] = params[name]
		sources[name] = name
	}
	for _, name := range names {
		if !strings.Contains(name, "-") {
			continue
		}
		alias := canonicalParamName(name)
		if _, exact := params[alias]; exact {
			continue
		}
		if _, applied := projected[alias]; applied {
			continue
		}
		projected[alias] = params[name]
		sources[alias] = name
	}
	return projected, sources
}

// ApplyProjectedParamLayer applies one precedence layer after deriving its
// aliases. A higher-precedence kebab spelling therefore overrides a lower
// camelCase default, while an exact camel spelling within the same layer still
// wins over that layer's derived alias.
func ApplyProjectedParamLayer(destination, layer extensionmodel.ParamMap) {
	projected, _ := ProjectParamAliases(layer)
	for name, value := range projected {
		destination[name] = value
	}
}

// WorkspaceProjectsContext resolves the COMPLETE workspace membership into the
// context document's project references, in canonical project-id order.
//
// It is deliberately not a second view of the selection. The selection is what
// the run acts on; this is what the workspace contains, and a task whose subject
// is the workspace needs the second. The order is sorted by id — not
// ws.Projects order — because a membership answer must be the same bytes for the
// same workspace whichever run produced it, which is also what the contract
// validates.
func WorkspaceProjectsContext(ws *workspace.Workspace, versions RunVersions) []JobContextProjectRef {
	if ws == nil {
		return nil
	}
	members := make([]*workspace.Project, 0, len(ws.Projects))
	for _, project := range ws.Projects {
		if project != nil {
			members = append(members, project)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })
	out := make([]JobContextProjectRef, 0, len(members))
	for _, project := range members {
		out = append(out, JobContextProjectRef{
			ID:         project.ID,
			Name:       project.Name,
			SourceName: project.SourceName,
			Version:    LineBaseVersion(versions, project),
			Path:       project.Path,
			FullPath:   filepath.Join(ws.Root, project.Path),
			Config:     encodedProjectConfig(project),
			Extensions: slices.Clone(project.Extensions),
			Type:       workspaceMemberType(project),
			// The resolved DIRECT edges. Publishing them here is what makes the
			// member a graph rather than a listing: a consumer reading an entry
			// with no edge list inside this member knows the project declares
			// none, because a producer that publishes the membership has already
			// resolved the graph.
			Dependencies: DirectDependencyIDs(ws, project),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// defaultProjectType is the classification of a project that neither its
// putnami.json nor its provider classified.
const defaultProjectType = "application"

// workspaceMemberType returns the classification a workspaceProjects entry
// carries: the resolved type, else the workspace default. The member is never
// empty, so a consumer reads an entry without it as one from a producer that
// predates the member.
func workspaceMemberType(project *workspace.Project) string {
	if project.Type == "" {
		return defaultProjectType
	}
	return project.Type
}

// encodedProjectConfig re-encodes the authored putnami.json the loader parsed.
//
// A config that will not re-encode is omitted rather than failing every job in
// the workspace: the raw member is an optional refinement, while the loader's
// strict diagnostics remain the authority for invalid authored configuration.
func encodedProjectConfig(project *workspace.Project) json.RawMessage {
	if project == nil || project.Config == nil {
		return nil
	}
	encoded, err := json.Marshal(project.Config)
	if err != nil {
		return nil
	}
	return encoded
}

// DirectDependencyIDs returns a project's resolved direct in-workspace
// dependency ids, sorted.
//
// The graph is core's answer, not a re-read of the manifest: name→id
// translation, scope-include edges and provider-probed edges are all decided
// during resolution, and a consumer that walked putnami.json itself would build
// a different graph. Sorted because two runs over one workspace must produce
// byte-identical documents; the graph's own slice order is insertion order.
func DirectDependencyIDs(ws *workspace.Workspace, project *workspace.Project) []string {
	if ws == nil || project == nil || ws.Graph == nil {
		return nil
	}
	deps := ws.Graph.DependenciesOf(project.ID)
	if len(deps) == 0 {
		return nil
	}
	ids := append([]string(nil), deps...)
	sort.Strings(ids)
	return ids
}

// ProjectDependencyClosure returns the project's in-workspace dependency
// closure — itself plus every project reachable through its dependency edges —
// in the graph's canonical project-id order.
//
// It is core's SINGLE definition of "the closure", and three surfaces read it:
// the job context's project.dependencyClosure (what a task actually walks at
// runtime), the plan-time `closureFiles` activation gate, and the `closure`
// cache-key input. A gate, a key and a task that each computed their own answer
// is exactly how a step gets dropped for a project whose need is transitive, or
// gets a stored verdict keyed on a smaller set of inputs than it read.
func ProjectDependencyClosure(ws *workspace.Workspace, project *workspace.Project) []*workspace.Project {
	if ws == nil || project == nil {
		return nil
	}
	ids := []string{project.ID}
	if ws.Graph != nil {
		ids = ws.Graph.TransitiveDependenciesOf(ids)
	}
	members := make([]*workspace.Project, 0, len(ids))
	for _, id := range ids {
		if member := ws.ProjectByID(id); member != nil {
			members = append(members, member)
		}
	}
	return members
}

// ProjectClosureRoots returns the absolute directory of every member of the
// project's dependency closure, in the closure's canonical order.
func ProjectClosureRoots(ws *workspace.Workspace, project *workspace.Project) []string {
	members := ProjectDependencyClosure(ws, project)
	roots := make([]string, 0, len(members))
	for _, member := range members {
		roots = append(roots, filepath.Join(ws.Root, member.Path))
	}
	return roots
}

func JobExtensionRoot(ws *workspace.Workspace, job *jobmodel.ScheduledJob) string {
	if job == nil || job.Extension == nil {
		return ""
	}
	if ws == nil || ws.Root == "" || job.Extension.RelPath == "" {
		return job.Extension.Path
	}
	relPath := strings.TrimLeft(job.Extension.RelPath, `/\`)
	relPath = filepath.FromSlash(strings.ReplaceAll(relPath, `\`, `/`))
	relPath = filepath.Clean(relPath)
	if relPath == "." || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return job.Extension.Path
	}
	return filepath.Join(ws.Root, relPath)
}

// KebabToCamel converts a kebab-case string to camelCase.
// e.g., "config-path" → "configPath", "put-registry-url" → "putRegistryUrl"
func KebabToCamel(s string) string {
	parts := strings.Split(s, "-")
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > 0 {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// canonicalParamName is the shared spelling rule for task-local parameters.
// The runtime exposes kebab-case inputs through their camelCase alias, so any
// planner compatibility or collision check must use this exact projection too.
func canonicalParamName(name string) string {
	if !strings.Contains(name, "-") {
		return name
	}
	return KebabToCamel(name)
}
