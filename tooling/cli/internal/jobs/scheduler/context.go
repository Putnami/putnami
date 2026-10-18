package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
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
	WorkspaceProjects []cachekey.JobContextProjectRef `json:"workspaceProjects"`
	Extension         JobContextExtension             `json:"extension"`
	Job               JobContextJob                   `json:"job"`
	Params            map[string]any                  `json:"params"`
	Version           *cachekey.JobContextVersion     `json:"version,omitempty"`
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
	Type              string                          `json:"type,omitempty"`
	DependencyClosure []cachekey.JobContextProjectRef `json:"dependencyClosure,omitempty"`
	// Tags are the project's resolved tags, in the form the task cache key
	// holds them (projectContextTags). Never nil in a built context: a project
	// that resolves to no tags emits `[]`, which a consumer reads as an answer
	// and tells apart from an absent member.
	Tags []string `json:"tags,omitzero"`
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

// treeBaseVersion is the base version of every line in what a cached task reads
// (ADR 0060): its job context and the capability packages of the build stamp.
const treeBaseVersion = "0.0.0"

// versionsSeenBy returns the version of each line that a job's context carries
// (ADR 0060).
//
// Git resolves a line's version from the tags and commit messages a checkout
// holds, not from the tree, so two lanes that build one tree resolve two
// versions. A task the cache can serve therefore sees a line's version only
// when its key carries that version: the task declares cache.versionAware, or
// it reads a version-var parameter that is set. Every other cached task sees
// base version 0.0.0 and no commit. A task the cache never serves sees every
// line as Git resolved it. The rule reads the task, not --no-cache, so one task
// reads one context whether or not the run uses the cache. versionVarSet tells
// whether the job's resolved cache-key parameters set version-var.
func versionsSeenBy(job *ScheduledJob, versions cachekey.RunVersions, versionVarSet bool) cachekey.RunVersions {
	if len(versions) == 0 || !CanUseCache(job) || cachekey.KeyEmbedsVersion(job, versionVarSet) {
		return versions
	}
	tree := make(cachekey.RunVersions, len(versions))
	for line := range versions {
		tree[line] = &cachekey.JobContextVersion{Base: treeBaseVersion, Line: line}
	}
	return tree
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
// (see task_capture.go). The staging root is the STORE's boundary, so
// naming it here would hand a task directories it must not write to.
func BuildJobContext(
	ws *workspace.Workspace,
	job *ScheduledJob,
	commandParams map[string]any,
	configDefaults map[string]any,
	versions cachekey.RunVersions,
) *JobCommandContext {
	cmdName := jobCommandName(job)
	identity := job.TypedIdentity()
	versions = versionsSeenBy(job, versions,
		cachekey.ParamHasVersionVar(cachekey.TaskCacheParamsWith(ws, job, commandParams, false)))
	ctx := &JobCommandContext{
		ProtocolVersion: protocoljob.ProtocolVersion2,
		Identity:        &identity,
		WorkspaceRoot:   ws.Root,
		Workspace: JobContextWorkspace{
			Name:     ws.Name,
			RootPath: ws.Root,
			Version:  cachekey.LineBaseVersion(versions, job.Project),
		},
		Extension: JobContextExtension{
			Name:        job.Extension.Name,
			Root:        cachekey.JobExtensionRoot(ws, job),
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
		Tags:              projectContextTags(job.Project),
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
	ctx.WorkspaceProjects = cachekey.WorkspaceProjectsContext(ws, versions)
	// An empty workspace writes "workspaceProjects": [] so a reader can tell
	// "no projects" from a CLI too old to write the member.
	if ctx.WorkspaceProjects == nil {
		ctx.WorkspaceProjects = []cachekey.JobContextProjectRef{}
	}

	// Output path — use Path (not Name or ID) for filesystem safety.
	// Name may contain slashes (@putnami/app), ID has "/" prefix.
	ctx.OutputPath = filepath.Join(ws.Root, ".putnami", "out", job.Project.Path, cmdName)

	ctx.Params = cachekey.DeliveredJobParams(job, commandParams, configDefaults)
	ctx.Version = cachekey.VersionInfoForProject(versions, job.Project)

	return ctx
}

func selectedProjectsContext(ws *workspace.Workspace, versions cachekey.RunVersions, projects []*workspace.Project, cmdName string) []JobContextSelectedProject {
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
			Version:      cachekey.LineBaseVersion(versions, project),
			Dependencies: cachekey.DirectDependencyIDs(ws, project),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
func dependencyClosureContext(ws *workspace.Workspace, versions cachekey.RunVersions, project *workspace.Project) []cachekey.JobContextProjectRef {
	members := cachekey.ProjectDependencyClosure(ws, project)
	if len(members) == 0 {
		return nil
	}
	closure := make([]cachekey.JobContextProjectRef, 0, len(members))
	for _, member := range members {
		closure = append(closure, cachekey.JobContextProjectRef{
			ID:           member.ID,
			Name:         member.Name,
			SourceName:   member.SourceName,
			Version:      cachekey.LineBaseVersion(versions, member),
			Path:         member.Path,
			FullPath:     filepath.Join(ws.Root, member.Path),
			Dependencies: cachekey.DirectDependencyIDs(ws, member),
		})
	}
	return closure
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

// projectContextTags returns the project's resolved tags as a sorted list of
// distinct, trimmed tags. It is never nil.
//
// The list is the tag set of the project's probe view, the view the cache key's
// project metadata digest hashes (workspace.Workspace.MetadataDigestFor). A
// task therefore reads the tags its key holds: the order of the authored list,
// a repeated tag and surrounding white space move neither.
func projectContextTags(project *workspace.Project) []string {
	tags := workspace.ProbeViewOf(project, nil).Tags
	if tags == nil {
		return []string{}
	}
	return tags
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

// addCamelCaseKeys adds camelCase variants for any kebab-case key in params.
// Both formats are kept so TS scripts (camelCase) and Python scripts (kebab-case) work.
func addCamelCaseKeys(params map[string]any) {
	projected, _ := cachekey.ProjectParamAliases(params)
	for name, value := range projected {
		params[name] = value
	}
}
