package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	extensionmodel "go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/useragent"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// JobCommandContext is the JSON structure written to a temp file and passed
// to extension subprocesses via --putnamiContext <path>. It is the producer
// side of go.putnami.dev/protocol/job, pinned by
// TestBuildJobContext_ConformsToProtocol.
//
// The orchestrator emits protocol version 2 unconditionally. There is no v1
// producer left: an extension that reaches this context is
// stamped at CLI contract 3, and contract 3 requires a consumer that reads v2.
// Emitting v1 to "be safe" would mean withholding the typed identity from every
// subprocess in order to accommodate an SDK the manifest loader already rejects.
type JobCommandContext struct {
	// ProtocolVersion is the job context contract version. Always
	// protocoljob.ProtocolVersion2: an absent member would mean v1, and v1
	// forbids the identity below.
	ProtocolVersion int `json:"protocolVersion"`
	// Identity is the typed identity of the task this subprocess runs — the
	// SAME value the plan, the session records and the result envelope carry,
	// so anything the subprocess emits joins to the orchestrator's view of the
	// task instead of being re-derived from project.name + job.name.
	Identity         *protocolcli.TaskIdentity   `json:"identity,omitempty"`
	WorkspaceRoot    string                      `json:"workspaceRoot"`
	Workspace        JobContextWorkspace         `json:"workspace"`
	Project          *JobContextProject          `json:"project,omitempty"`
	SelectedProjects []JobContextSelectedProject `json:"selectedProjects,omitempty"`
	// Selection is the invocation's RESOLVED project selection: the mode, whether
	// the run is narrowed, the baseline `--impacted` resolved to, and the project
	// ids in scope. It is the protocol's own type rather than a fourth local copy
	// (as `invocation` already is), because the whole point of the member is that
	// its bytes are the CLI's ResolvedSelection's bytes.
	//
	// SelectedProjects says which projects, for the workspace-scoped jobs that
	// receive it; this says what was asked for and how it resolved, for every
	// job. Absent only when the caller resolved nothing.
	Selection *protocoljob.Selection `json:"selection,omitempty"`
	// WorkspaceProjects is the COMPLETE resolved workspace membership with its
	// direct dependency edges, in canonical project-id order. It answers a
	// different question from SelectedProjects: that one is what the run ACTS
	// on, this one is what the workspace CONTAINS.
	//
	// A task cannot substitute one for the other. Under `--impacted` the
	// selection is a subset, so a member outside it reads as absent and an edge
	// into it is never walked — a false violation and a missed one, both
	// measured on `architecture validate`. The same measurement
	// on a per-PROJECT task showed the split is not about activation: SDD
	// feature relations resolve against every authored manifest in the
	// workspace, so a project-scoped validation without this member reported a
	// real cross-project relation as dangling.
	WorkspaceProjects []JobContextProjectRef `json:"workspaceProjects,omitempty"`
	Extension         JobContextExtension    `json:"extension"`
	Job               JobContextJob          `json:"job"`
	Params            map[string]any         `json:"params"`
	Version           *JobContextVersion     `json:"version,omitempty"`
	// Invocation is the non-secret locator (id + private artifact root) of the
	// `finalizes` relation this task participates in. It is present ONLY for that
	// relation's producer, its listed consumers, and its finalizer; every other
	// task omits the member entirely, which is what keeps the private tree
	// undiscoverable from a context document that has no business knowing it.
	Invocation *protocoljob.Invocation `json:"invocation,omitempty"`
	// UserScope is present exactly when the job runs outside any workspace, from
	// an extension pinned in the user scope. The workspace members then name the
	// user-scope directory, and CallerDir is where the command was invoked and
	// where the job runs. Inside a workspace the member is absent.
	UserScope  *protocoljob.UserScope `json:"userScope,omitempty"`
	OutputPath string                 `json:"outputPath"`
	CacheRoot  string                 `json:"cacheRoot"`
}

// JobContextWorkspace describes the workspace in the context file. Options
// carries the committed workspace-level extension configuration to
// workspace-scoped tasks. It is v2-only and omitted when the workspace has
// none; consumers own each named block's semantics.
type JobContextWorkspace struct {
	Name     string                    `json:"name"`
	RootPath string                    `json:"rootPath"`
	Version  string                    `json:"version,omitempty"`
	Options  map[string]map[string]any `json:"options,omitempty"`
}

// JobContextProject describes the project in the context file.
//
// The npm-shaped members (`main`, `bin`, `exports`) are GONE. They were
// package.json fields that core parsed, carried in its own
// Project struct and stamped into every task's context regardless of language —
// a Go test job received a `bin` member it could not have and a Python job
// received an `exports` map that meant nothing to it.
//
// They are replaced by Metadata: the same facts, produced by the extension that
// understands them, namespaced under that extension's name. The TypeScript
// extension reads its own `metadata["@putnami/typescript"]` block; every other
// provider is free to publish whatever its own tasks need without asking core to
// grow a member for it.
type JobContextProject struct {
	Name     string                    `json:"name"`
	Path     string                    `json:"path"`
	FullPath string                    `json:"fullPath"`
	Publish  []string                  `json:"publish,omitempty"`
	Options  map[string]map[string]any `json:"options,omitempty"`
	// Metadata is provider-owned project metadata, namespaced by the extension
	// whose workspace probe produced it. Core carries it verbatim and never
	// interprets it; the complete map travels, so a consumer selects the one
	// namespace it owns. Omitted entirely when no provider reported anything.
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
	// Type is the project's resolved classification, and DependencyClosure is
	// the project plus its in-workspace dependency closure in canonical
	// project-id order.
	//
	// Both are facts core has ALREADY resolved — the authored-over-probed type
	// merge, and the graph built from authored dependencies plus provider probe
	// edges — handed to the task that needs them. They replaced core's own infra
	// gate, which walked that graph and classified those projects in order to
	// emit an artifact whose CONTENT it had no business deciding. The graph
	// stays core's; what a workload declares and what it defaults to is the
	// language extension's.
	Type              string                 `json:"type,omitempty"`
	DependencyClosure []JobContextProjectRef `json:"dependencyClosure,omitempty"`
}

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
}

// JobContextSelectedProject is one project in the resolved command selection.
type JobContextSelectedProject struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	FullPath string `json:"fullPath"`
	// OutputPath mirrors the leader's OutputPath derivation for this project so
	// a batched extension process can write each project's outputs to its own
	// captured directory. Empty for non-batch selections.
	OutputPath string `json:"outputPath,omitempty"`
	// SourceName is the identity the project declared BEFORE a scope
	// namePattern override — workspace.Project.SourceName, the same value
	// NameDivergences reports on. It travels so a provider's workspace-sync task
	// renames exactly the manifests core reports as diverging, instead of
	// rewriting every manifest whose name merely differs from the resolved name.
	SourceName string `json:"sourceName,omitempty"`
	// Version is the base version of the project's line — LineBaseVersion, the
	// same helper the workspace block uses — because a package reference is a
	// pair. A selector that locates a contribution by package names both the
	// package and its version, and a task handed only the name resolves nothing
	// for the versioned half of the corpus while the versionless half keeps
	// working: a silent narrowing, not a failure.
	Version string `json:"version,omitempty"`
	// Dependencies are the resolved direct dependency ids, the same member the
	// other project-reference carriers publish.
	Dependencies []string `json:"dependencies,omitempty"`
}

// JobContextExtension describes the extension in the context file.
type JobContextExtension struct {
	Name        string `json:"name"`
	Root        string `json:"root"`
	RuntimePath string `json:"runtimePath,omitempty"`
	CacheRoot   string `json:"cacheRoot,omitempty"`
}

// JobContextJob describes the job in the context file.
type JobContextJob struct {
	Name string `json:"name"`
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

// BuildJobContext constructs a JobCommandContext for a scheduled job.
//
// The document is always job context v2: the version member and the typed
// identity are stamped here, once, from the plan node's own identity
// (ScheduledJob.TypedIdentity), so the value the subprocess reads cannot drift
// from the one the session record and the result envelope carry.
//
// `staging` is deliberately NOT emitted. v2 defines it as optional and present
// "exactly when the orchestrator runs the task in a staging tree", and B4b
// established that this workspace does not: declared outputs are captured FROM
// their real locations because sibling steps read each other's trees and
// extensions recompute the command output directory from the workspace root
// (see jobs/task_capture.go). The staging root is the STORE's boundary, so
// naming it here would hand a task directories it must not write to.
func BuildJobContext(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
	versions RunVersions,
) *JobCommandContext {
	cmdName := jobCommandName(job)
	identity := job.TypedIdentity()
	ctx := &JobCommandContext{
		ProtocolVersion: protocoljob.ProtocolVersion2,
		Identity:        &identity,
		WorkspaceRoot:   ws.Root,
		Workspace: JobContextWorkspace{
			Name:     ws.Name,
			RootPath: ws.Root,
			Version:  LineBaseVersion(versions, job.Project),
		},
		Extension: JobContextExtension{
			Name:        job.Extension.Name,
			Root:        jobExtensionRoot(ws, job),
			RuntimePath: job.Extension.RuntimeExecutable,
			// The machine-global directory THIS extension owns. It replaces
			// the per-ecosystem environment variables
			// core used to export into every job regardless of which extension
			// ran it: a Python task received PUTNAMI_GO_CACHE_DIR and
			// BUN_INSTALL_CACHE_DIR because core, not the extension, decided
			// where a language cache lives.
			CacheRoot: extensionproto.MachineCacheRoot(job.Extension.Name, ws.Root),
		},
		Job: JobContextJob{
			Name: cmdName,
		},
		// cacheRoot is per-WORKSPACE arbitrary mutable scratch (job-context temp
		// files, extension scratch) — deliberately NOT the content-addressed store.
		// Concurrent-safe caches such as Go's use a separate shared root.
		CacheRoot:  store.ResolveScratchRoot(ws.Root),
		Invocation: job.InvocationLocator(),
		// The plan node's resolved selection, shared by pointer: it is resolved
		// ONCE per invocation and never mutated afterwards, so every task's
		// document reports the same run. Copying it per task would only invite a
		// caller to think a task may edit its own view of the run's scope.
		Selection: job.Selection,
		UserScope: job.UserScope,
	}
	if ws.Config != nil {
		ctx.Workspace.Options = ws.Config.Options
	}

	// Project context
	projRoot := filepath.Join(ws.Root, job.Project.Path)
	ctx.Project = &JobContextProject{
		Name:              job.Project.Name,
		Path:              job.Project.Path,
		FullPath:          projRoot,
		Publish:           job.Project.Publish,
		Options:           projectContextOptions(job.Project),
		Metadata:          projectContextMetadata(job.Project),
		Type:              job.Project.Type,
		DependencyClosure: dependencyClosureContext(ws, versions, job.Project),
	}
	ctx.SelectedProjects = selectedProjectsContext(ws, versions, job.SelectedProjects, cmdName)
	// The complete membership travels on EVERY document, not only the
	// workspace-scoped ones.
	//
	// The narrower rule was tried first and measured wrong. A per-PROJECT task
	// can own a workspace-wide read: the SDD feature contract resolves a
	// relation target against every authored manifest in the workspace, so a
	// project-scoped validation that saw only its own project reported a real
	// cross-project relation as dangling — a false failure in the gate, on a
	// repository where nothing was wrong. What decides whether a task may make a
	// workspace-wide CLAIM is `selection`, which every document already carries;
	// what this member decides is whether it can SEE the workspace at all, and
	// that is not a property of the task's activation.
	ctx.WorkspaceProjects = workspaceProjectsContext(ws, versions)

	// Output path — use Path (not Name or ID) for filesystem safety.
	// Name may contain slashes (@putnami/app), ID has "/" prefix.
	ctx.OutputPath = filepath.Join(ws.Root, ".putnami", "out", job.Project.Path, cmdName)

	ctx.Params = deliveredJobParams(job, commandParams, configDefaults)
	ctx.Version = VersionInfoForProject(versions, job.Project)

	return ctx
}

// deliveredJobParams is the parameter bag BuildJobContext hands a task as
// ctx.Params. The batch key and the plan-time batch cap check read it through
// this one function, so neither can resolve a parameter differently from what
// the task receives.
func deliveredJobParams(
	job *ScheduledJob,
	commandParams extensionmodel.ParamMap,
	configDefaults extensionmodel.ParamMap,
) extensionmodel.ParamMap {
	params := resolvedJobParams(job, commandParams, configDefaults)
	// The project's effective registry endpoints, one raw entry per ecosystem,
	// travel to the task that publishes: an extension reads its own ecosystem's
	// entry and generates the native registry file from it, so no endpoint is
	// hard-coded in an extension or hand-written into a dotfile.
	//
	// Deliberately set AFTER resolvedJobParams and never through BoundParams:
	// resolvedJobParams is the exact projection the cache-key builder hashes, so
	// routing the endpoints through it would make a mirror change every task's
	// key and evict a store that holds the same bytes.
	if len(job.Project.Registries) > 0 {
		params["registries"] = job.Project.Registries
	}
	return params
}

// jobConfigDefaults returns the workspace option layers that apply to a job's
// command and extension: options["*"], options[command], options[extension]
// and options[extension:command], merged in that order. A workspace without a
// config contributes nothing.
func jobConfigDefaults(ws *workspace.Workspace, job *ScheduledJob) extensionmodel.ParamMap {
	if ws == nil || ws.Config == nil || job.Extension == nil {
		return nil
	}
	return ws.Config.GetCommandDefaults(jobCommandName(job), job.Extension.Name)
}

// resolvedJobParams returns the exact resolved parameter bag delivered to a
// task. Keeping this projection shared by the job context and cache-key builder
// prevents the cache from hashing a different value than the subprocess reads.
func resolvedJobParams(
	job *ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
) map[string]any {
	params := mergeParamLayers(
		configDefaults,
		job.JobDef,
		job.JobDef.Defaults,
		job.Project,
		job.Extension.Name,
		jobCommandName(job),
		nil,
		commandParams,
	)

	applyBoundParams(params, job.JobDef.BoundParams)
	return params
}

// applyBoundParams is the single precedence rule for planned task-local
// parameters. The planner uses it when a prerequisite command becomes the
// owner of another prerequisite, and BuildJobContext uses it at dispatch, so a
// synthetic value cannot activate a different plan than the task receives.
func applyBoundParams(params extensionmodel.ParamMap, bound extensionmodel.ParamMap) {
	// A literal step binding is an explicit task input, not another command
	// default. Apply it after every shared layer so a CLI/project value cannot
	// override the same exact spelling and so the value remains local to this
	// job. Alias projection happens exactly once for this layer: an authored
	// camelCase key wins over a derived alias in the same layer, while this
	// higher-precedence layer still overrides command and project defaults.
	applyProjectedParamLayer(params, bound)
}

// projectParamAliases is the one pure parameter projection shared by planning,
// invocation compatibility, and dispatch. It preserves every exact spelling,
// then adds deterministic kebab-to-camel aliases. An exact camelCase spelling
// always wins; when several legacy spellings collapse to the same alias, the
// lexicographically first source wins. Sources name the exact input spelling
// used for collision diagnostics.
func projectParamAliases(params extensionmodel.ParamMap) (extensionmodel.ParamMap, map[string]string) {
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

// applyProjectedParamLayer applies one precedence layer after deriving its
// aliases. A higher-precedence kebab spelling therefore overrides a lower
// camelCase default, while an exact camel spelling within the same layer still
// wins over that layer's derived alias.
func applyProjectedParamLayer(destination, layer extensionmodel.ParamMap) {
	projected, _ := projectParamAliases(layer)
	for name, value := range projected {
		destination[name] = value
	}
}

func selectedProjectsContext(ws *workspace.Workspace, versions RunVersions, projects []*workspace.Project, cmdName string) []JobContextSelectedProject {
	if len(projects) == 0 {
		return nil
	}
	out := make([]JobContextSelectedProject, 0, len(projects))
	for _, project := range projects {
		if project == nil {
			continue
		}
		out = append(out, JobContextSelectedProject{
			ID:       project.ID,
			Name:     project.Name,
			Path:     project.Path,
			FullPath: filepath.Join(ws.Root, project.Path),
			// Mirror BuildJobContext's leader OutputPath derivation so each
			// batched project writes to its own captured output directory.
			OutputPath: filepath.Join(ws.Root, ".putnami", "out", project.Path, cmdName),
			// The declared identity, so a workspace-sync task can tell "this
			// manifest is where the name came from" from "a scope renamed this
			// project and the manifest has not caught up". Without it the task
			// sees only the resolved name and rewrites both alike.
			SourceName: project.SourceName,
			// The line's version, through the same helper the workspace block
			// uses. A project no longer declares a version of its own, so the
			// pair's version half is the one its line is at — and routing every
			// reference through one helper is what keeps two members of one
			// document from naming two different versions of one package.
			Version:      LineBaseVersion(versions, project),
			Dependencies: DirectDependencyIDs(ws, project),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// workspaceProjectsContext resolves the COMPLETE workspace membership into the
// context document's project references, in canonical project-id order.
//
// It is deliberately not a second view of the selection. The selection is what
// the run acts on; this is what the workspace contains, and a task whose subject
// is the workspace needs the second. The order is sorted by id — not
// ws.Projects order — because a membership answer must be the same bytes for the
// same workspace whichever run produced it, which is also what the contract
// validates.
func workspaceProjectsContext(ws *workspace.Workspace, versions RunVersions) []JobContextProjectRef {
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

// dependencyClosureContext resolves the project's in-workspace dependency
// closure — itself plus every project reachable through its dependency edges —
// into the context document's project references.
//
// The seed is included, because the closure exists for tasks that aggregate
// over a project's WHOLE graph and must read the project's own contribution
// beside its dependencies'. Order is the graph's canonical project-id order, so
// two runs over one workspace produce byte-identical documents.
//
// A workspace with no graph (a single-project resolution, a test fixture)
// yields the project alone rather than nothing: "no edges" is a closure of one,
// not an absent answer, and a consumer must not have to tell those apart.
func dependencyClosureContext(ws *workspace.Workspace, versions RunVersions, project *workspace.Project) []JobContextProjectRef {
	members := projectDependencyClosure(ws, project)
	if len(members) == 0 {
		return nil
	}
	closure := make([]JobContextProjectRef, 0, len(members))
	for _, member := range members {
		closure = append(closure, JobContextProjectRef{
			ID:           member.ID,
			Name:         member.Name,
			SourceName:   member.SourceName,
			Version:      LineBaseVersion(versions, member),
			Path:         member.Path,
			FullPath:     filepath.Join(ws.Root, member.Path),
			Dependencies: DirectDependencyIDs(ws, member),
		})
	}
	return closure
}

// projectDependencyClosure returns the project's in-workspace dependency
// closure — itself plus every project reachable through its dependency edges —
// in the graph's canonical project-id order.
//
// It is core's SINGLE definition of "the closure", and three surfaces read it:
// the job context's project.dependencyClosure (what a task actually walks at
// runtime), the plan-time `closureFiles` activation gate, and the `closure`
// cache-key input. A gate, a key and a task that each computed their own answer
// is exactly how a step gets dropped for a project whose need is transitive, or
// gets a stored verdict keyed on a smaller set of inputs than it read.
func projectDependencyClosure(ws *workspace.Workspace, project *workspace.Project) []*workspace.Project {
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

// projectClosureRoots returns the absolute directory of every member of the
// project's dependency closure, in the closure's canonical order.
func projectClosureRoots(ws *workspace.Workspace, project *workspace.Project) []string {
	members := projectDependencyClosure(ws, project)
	roots := make([]string, 0, len(members))
	for _, member := range members {
		roots = append(roots, filepath.Join(ws.Root, member.Path))
	}
	return roots
}

func jobExtensionRoot(ws *workspace.Workspace, job *ScheduledJob) string {
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

// projectContextMetadata copies the project's provider-owned metadata into the
// context document.
//
// The map is copied rather than shared: the *workspace.Project is memoized for
// the process and read by every scheduled job, and handing the same map to a
// document that is then encoded per task would let one task's mutation reach
// another's context. An empty map is returned as nil so the member is omitted —
// an absent member and an empty object must not encode differently, because the
// context document's bytes are what a consumer validates.
func projectContextMetadata(project *workspace.Project) map[string]json.RawMessage {
	if project == nil || len(project.Metadata) == 0 {
		return nil
	}
	copied := make(map[string]json.RawMessage, len(project.Metadata))
	for extension, block := range project.Metadata {
		copied[extension] = block
	}
	return copied
}

func projectContextOptions(project *workspace.Project) map[string]map[string]any {
	if project == nil || project.Config == nil || len(project.Config.Options) == 0 {
		return nil
	}
	return project.Config.Options
}

// WriteContextFile serializes a JobCommandContext to a temporary JSON file
// and returns its path. The caller must delete the file when done.
func WriteContextFile(ctx *JobCommandContext, cacheRoot string) (string, error) {
	data, err := json.Marshal(ctx)
	if err != nil {
		return "", fmt.Errorf("marshal job context: %w", err)
	}

	// Write to a temp file in the cache root directory
	dir := cacheRoot
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create context dir %s: %w", dir, err)
	}

	f, err := os.CreateTemp(dir, "job-context-*.json")
	if err != nil {
		return "", fmt.Errorf("create context file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(data); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write context file: %w", err)
	}

	return f.Name(), nil
}

// CallerDirEnv mirrors userScope.callerDir. It is set only for a job that runs
// outside any workspace, and the runner strips an inherited value, so its
// presence tells a shell extension that no workspace exists.
const CallerDirEnv = "PUTNAMI_CALLER_DIR"

// ProvidersEnv names the invocation providers a CLI process enables when its
// command line has no --providers. The choice holds for one process: the
// runner strips an inherited value, so a nested CLI in a job starts with the
// credential provider off unless its own command line turns it on.
const ProvidersEnv = "PUTNAMI_PROVIDERS"

// BuildEnvVars returns PUTNAMI_* environment variables that mirror the JSON
// context file. Shell extensions read these to avoid parsing JSON in bash.
// Go extensions should prefer the context file via extension-sdk/context.
//
// The cache variables have distinct ownership: PUTNAMI_CACHE_ROOT is workspace
// scratch, PUTNAMI_EXTENSION_CACHE_ROOT is extension-owned machine state, and
// PUTNAMI_OCI_CACHE_ROOT is core-owned content shared by image extensions and
// worktrees of this repository.
// The three language-specific variables that used to be here —
// PUTNAMI_GO_CACHE_DIR, PUTNAMI_BUN_CACHE_DIR, BUN_INSTALL_CACHE_DIR — were
// exported into every job of every extension, so core told a Python task where
// the Bun cache was and a fourth ecosystem had no variable at all.
func BuildEnvVars(ctx *JobCommandContext) []string {
	vars := []string{
		"PUTNAMI_WORKSPACE_ROOT=" + ctx.WorkspaceRoot,
		"PUTNAMI_WORKSPACE_NAME=" + ctx.Workspace.Name,
		"PUTNAMI_EXTENSION_NAME=" + ctx.Extension.Name,
		"PUTNAMI_JOB_NAME=" + ctx.Job.Name,
		"PUTNAMI_OUTPUT_PATH=" + ctx.OutputPath,
		"PUTNAMI_CACHE_ROOT=" + ctx.CacheRoot,
		extensionproto.MachineCacheRootEnv + "=" + ctx.Extension.CacheRoot,
		extensionproto.SharedOCILayerCacheRootEnv + "=" + filepath.Join(store.ResolveStoreRoot(ctx.WorkspaceRoot), store.OCILayerCacheDirName),
		"PUTNAMI_CLI_USER_AGENT=" + useragent.String(),
	}
	if ctx.Project != nil {
		vars = append(vars,
			"PUTNAMI_PROJECT_ROOT="+ctx.Project.FullPath,
			"PUTNAMI_PROJECT_NAME="+ctx.Project.Name,
			"PUTNAMI_PROJECT_PATH="+ctx.Project.FullPath,
		)
	}
	if ctx.UserScope != nil {
		vars = append(vars, CallerDirEnv+"="+ctx.UserScope.CallerDir)
	}
	if len(ctx.SelectedProjects) > 0 {
		vars = append(vars,
			"PUTNAMI_SELECTED_PROJECTS="+strings.Join(selectedProjectField(ctx.SelectedProjects, "name"), ","),
			"PUTNAMI_SELECTED_PROJECT_IDS="+strings.Join(selectedProjectField(ctx.SelectedProjects, "id"), ","),
			"PUTNAMI_SELECTED_PROJECT_PATHS="+strings.Join(selectedProjectField(ctx.SelectedProjects, "path"), ","),
			"PUTNAMI_SELECTED_PROJECT_ROOTS="+strings.Join(selectedProjectField(ctx.SelectedProjects, "fullPath"), ","),
		)
	}
	return vars
}

func selectedProjectField(projects []JobContextSelectedProject, field string) []string {
	out := make([]string, 0, len(projects))
	for _, project := range projects {
		var value string
		switch field {
		case "id":
			value = project.ID
		case "path":
			value = project.Path
		case "fullPath":
			value = project.FullPath
		default:
			value = project.Name
		}
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// kebabToCamel converts a kebab-case string to camelCase.
// e.g., "config-path" → "configPath", "put-registry-url" → "putRegistryUrl"
func kebabToCamel(s string) string {
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
	return kebabToCamel(name)
}

// addCamelCaseKeys adds camelCase variants for any kebab-case key in params.
// Both formats are kept so TS scripts (camelCase) and Python scripts (kebab-case) work.
func addCamelCaseKeys(params map[string]any) {
	projected, _ := projectParamAliases(params)
	for name, value := range projected {
		params[name] = value
	}
}
