// Package job defines the job execution context contract: the JSON
// document the Putnami orchestrator writes for every job subprocess and
// passes via --putnamiContext <path>.
//
// The orchestrator (the CLI) is the producer; extension SDKs in every
// language are consumers. This package is the source of truth for the
// shape, the required fields, and the parameter coercion rules.
package job

import (
	"encoding/json"
	"strconv"
)

// ProtocolVersion is the current job context protocol version.
const ProtocolVersion = 1

// Context is the job execution context document.
type Context struct {
	// presence is populated only by ParseStrict. Go zero values cannot preserve
	// whether an optional JSON member was absent, empty, or null, but producer
	// conformance must distinguish those cases at the v1/v2 boundary.
	presence contextPresence
	// ProtocolVersion is the contract version of this document. It is omitted
	// from v1 documents — a document WITHOUT the member is version 1, and that
	// is the only way to tell the two apart. See context_v2.go.
	ProtocolVersion int `json:"protocolVersion,omitempty"`
	// WorkspaceRoot is the absolute path to the workspace root the job runs in.
	WorkspaceRoot string `json:"workspaceRoot"`
	// OutputPath is the absolute path the job must write its structured result
	// document (the job's `.result.json`) to.
	OutputPath string `json:"outputPath"`
	// CacheRoot is the absolute path to the workspace cache directory the job may
	// read and write task caches under.
	CacheRoot string `json:"cacheRoot"`
	// Workspace holds workspace-level identity and metadata.
	Workspace Workspace `json:"workspace"`
	// Project is the project this job runs for.
	Project Project `json:"project"`
	// SelectedProjects is the full resolved project selection for the command
	// invocation, in run order; present when the job needs cross-project context.
	SelectedProjects []ProjectRef `json:"selectedProjects,omitempty"`
	// WorkspaceProjects is the COMPLETE resolved workspace membership, in
	// canonical project-id order, regardless of what this run selected. v2 only,
	// and optional within v2 — an orchestrator older than the member omits it.
	//
	// The two members are not redundant, and a consumer that treated them as one
	// gets both possible wrong answers. SelectedProjects is what the run ACTS
	// on; this is what the workspace CONTAINS. A validator whose subject is the
	// workspace — "is every project this architecture manifest names a real
	// member?", "does any project depend across a domain boundary without a
	// declared binding?" — cannot answer from the selection: under `--impacted`
	// the selection is a subset, so a member outside it reads as absent (a false
	// violation) and an edge into it is never walked (a missed one). Both were
	// measured before this member existed.
	//
	// It reaches EVERY job, not only the workspace-scoped ones, because a
	// per-project task can own a workspace-wide READ. The SDD feature contract
	// resolves a relation target against every authored manifest in the
	// workspace, so a project-scoped validation shown only its own project
	// reported a real cross-project relation as dangling. What decides whether a
	// task may make a workspace-wide CLAIM is Selection; what this member
	// decides is whether it can see the workspace at all.
	//
	// A task that reads the workspace must also SAY so in its declared cache
	// inputs (`from: "workspace"`). Reading this member widens the read set, and
	// a key that does not follow it stores a verdict a sibling's file can
	// silently invalidate.
	//
	// It carries the membership AND the resolved direct edges between its
	// members (ProjectRef.Dependencies): a producer that publishes this member
	// has resolved the workspace graph, which is what lets a consumer read an
	// entry with no edge list as a project that declares no dependency instead
	// of as a fact nobody supplied.
	WorkspaceProjects []ProjectRef `json:"workspaceProjects,omitempty"`
	// Selection is HOW the command invocation chose the projects in scope: the
	// mode, whether the run is narrowed, the baseline `--impacted` resolved to,
	// and the resolved project ids. v2 only — forbidden at v1.
	//
	// SelectedProjects answers "which projects", and only for the jobs that get
	// it; this answers "what did the user ask for, and what did it resolve to",
	// for every job. A task that reports on a workspace needs both: a validator
	// told it ran over three projects cannot tell a deliberate `--projects`
	// narrowing from a whole-workspace run of a three-project tree, and the two
	// license entirely different conclusions.
	Selection *Selection `json:"selection,omitempty"`
	// Extension identifies the extension providing the running job.
	Extension Extension `json:"extension"`
	// Job identifies the command/task the subprocess is executing.
	Job Job `json:"job"`
	// Identity is the typed identity of the task this subprocess runs: the same
	// value the plan, the session records and the result envelope carry, so
	// anything the subprocess reports can be joined to the orchestrator's view
	// of the task. v2 only — required at v2, forbidden at v1.
	Identity *TaskIdentity `json:"identity,omitempty"`
	// Staging is the task-owned staging tree the job must write its declared
	// outputs into. v2 only, and optional within v2: it is present exactly when
	// the orchestrator runs the task in a staging tree. Forbidden at v1.
	Staging *Staging `json:"staging,omitempty"`
	// Invocation is the non-secret delivery channel for invocation-scoped
	// artifacts. v2 only and optional: the orchestrator supplies it only to the
	// declared producer, consumers, and finalizer of one finalizes relation.
	Invocation *Invocation `json:"invocation,omitempty"`
	// UserScope is present exactly when the command runs outside any
	// workspace, from an extension pinned in the user scope. v2 only and
	// optional: a job inside a workspace never carries it. When it is present,
	// WorkspaceRoot and Workspace.RootPath name the user-scope directory the
	// orchestrator runs the job in, not a workspace the user owns, and the job
	// acts on UserScope.CallerDir.
	UserScope *UserScope `json:"userScope,omitempty"`
	// Params are the merged, coerced job parameters (CLI flags over config
	// defaults); accessors define the shared coercion contract.
	Params Params `json:"params"`
	// FilePatterns are the glob patterns scoping the job to a file subset, when
	// the command was invoked with a pattern filter.
	FilePatterns []string `json:"filePatterns,omitempty"`
	// Version is the orchestrator-resolved git version info, present when the job
	// needs a version stamp.
	Version *Version `json:"version,omitempty"`
}

// Workspace holds workspace-level metadata.
type Workspace struct {
	// Name is the workspace name from putnami.workspace.json.
	Name string `json:"name"`
	// RootPath is the workspace root path, when the producer records it.
	RootPath string `json:"rootPath,omitempty"`
	// Version is the workspace version stamp, when set.
	Version string `json:"version,omitempty"`
	// Options are the committed workspace-level extension option blocks. v2
	// only and optional within v2: they let a workspace-scoped task resolve the
	// same policy the orchestrator loaded without reloading the workspace.
	Options map[string]json.RawMessage `json:"options,omitempty"`
}

// Project holds project-level metadata. Bin, Exports, and Compile stay
// raw because their shapes are language-specific (string or object).
type Project struct {
	// Name is the project name (its putnami.json "name").
	Name string `json:"name"`
	// Path is the project directory relative to the workspace root.
	Path string `json:"path"`
	// FullPath is the absolute project directory path.
	FullPath string `json:"fullPath"`
	// Main is the project entry point, when declared.
	//
	// SUPERSEDED by Metadata. The Putnami orchestrator no longer produces this
	// member: it was a package.json field core parsed and stamped into every
	// task's context regardless of language, which made a TypeScript fact part
	// of every job's contract. The field stays in the contract because it is
	// OPTIONAL and a producer outside this repository may still set it; a
	// consumer that wants a TypeScript entrypoint reads
	// `metadata["@putnami/typescript"]`.
	Main string `json:"main,omitempty"`
	// Bin is the project's bin declaration, kept raw because it is a string or an
	// object depending on language (see GetBinString). Superseded by Metadata —
	// see Main.
	Bin json.RawMessage `json:"bin,omitempty"`
	// Exports is the project's export map, kept raw because its shape is
	// language-specific. Superseded by Metadata — see Main.
	Exports json.RawMessage `json:"exports,omitempty"`
	// Compile is the project's compile configuration, kept raw because its shape
	// is language-specific.
	Compile json.RawMessage `json:"compile,omitempty"`
	// Publish is the project's publish-channels declaration, kept raw; parse it
	// with PublishChannels.
	Publish json.RawMessage `json:"publish,omitempty"`
	// Options are per-extension project option blocks, keyed by extension, kept
	// raw so each extension decodes its own block.
	Options map[string]json.RawMessage `json:"options,omitempty"`
	// Metadata is provider-owned project metadata, namespaced by the extension
	// that produced it. v2 only — forbidden at v1.
	//
	// It is the counterpart of Options, and the distinction is who wrote it:
	// Options is AUTHORED in the project's putnami.json, Metadata is DERIVED by
	// the owning extension's workspace probe. Namespacing lets several providers
	// contribute without a merge rule per field: a key identifies the extension
	// that owns and writes that block. The complete map is delivered on the
	// wire, so this is ownership rather than read isolation; consumers must
	// select only the namespace they own.
	//
	// Each block is kept raw so the owning extension decodes its own shape; the
	// orchestrator never interprets it.
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
	// Type is the project's RESOLVED classification: "application", "library",
	// or whatever vocabulary the workspace uses. v2 only — forbidden at v1.
	//
	// Resolved is the whole point. A project's classification is authored in
	// putnami.json OR derived by the owning provider's workspace probe, and the
	// merge rule between the two (authored wins, provider fills the gap) belongs
	// to the orchestrator. Handing the ANSWER to a task means a task that must
	// treat deployable workloads differently from consumed libraries reads one
	// member instead of re-implementing that merge — which is how two consumers
	// start disagreeing about what a project is.
	//
	// Empty means the workspace classified nothing; consumers read that as
	// "application", the same default the orchestrator applies.
	Type string `json:"type,omitempty"`
	// DependencyClosure is the project's in-workspace dependency closure: this
	// project PLUS every workspace project reachable through its dependency
	// edges, in canonical project-id order. v2 only — forbidden at v1.
	//
	// The seed is included on purpose. A task that aggregates something over a
	// project's whole graph (deployability requirements, license inventories)
	// must read the project's own contribution alongside its dependencies', so
	// the closure is delivered as the complete set rather than as "the others".
	//
	// The graph itself stays the orchestrator's: edges come from authored
	// dependencies and from provider probes, resolved once, and a task that
	// re-walked manifests to rebuild them would answer a different question
	// (scope includes, name-to-id translation and out-of-workspace edges are all
	// decided during resolution). This member is that resolved answer, so
	// extension-owned aggregation is a policy decision over core's graph rather
	// than a second graph.
	DependencyClosure []ProjectRef `json:"dependencyClosure,omitempty"`
}

// ProjectRef identifies one project the context references: a member of the
// resolved command selection (Context.SelectedProjects), the complete workspace
// membership (Context.WorkspaceProjects), or a project's dependency closure
// (Project.DependencyClosure). One shape, so a consumer reads a project
// reference the same way wherever it appears.
type ProjectRef struct {
	// ID is the project's stable identifier, when the producer assigns one.
	ID string `json:"id,omitempty"`
	// Name is the project name.
	Name string `json:"name"`
	// Path is the project directory relative to the workspace root.
	Path string `json:"path"`
	// FullPath is the absolute project directory path.
	FullPath string `json:"fullPath"`
	// OutputPath is the absolute directory this project's job must write its
	// structured outputs (coverage, JUnit, and any other captured artifacts)
	// to. It is the per-project analog of the top-level Context.OutputPath:
	// when several projects share one batched extension process, each writes to
	// its own OutputPath so the orchestrator captures each project's outputs
	// into that project's own cache entry. Additive and omitempty for forward
	// compatibility — a solo job leaves it empty and uses Context.OutputPath.
	OutputPath string `json:"outputPath,omitempty"`
	// SourceName is the identity the project DECLARED — its own putnami.json
	// name, or the name its provider read out of its native manifest — before a
	// scope `namePattern` override was applied. Name is the resolved answer
	// AFTER that override; the two differ only when nothing declared a name and
	// a scope supplied one.
	//
	// It exists so a provider's sync task can act on exactly the divergence set
	// the orchestrator reports. A task that aligns a native manifest's name MUST
	// NOT rename a manifest whose SourceName already equals Name: that manifest
	// is where the resolved identity CAME FROM, so it is not out of alignment,
	// and rewriting it orphans every sibling reference that still spells the old
	// name (a bun `workspace:*` dependency, say).
	//
	// v2 ONLY, and optional within v2: a v1 document carrying it is rejected
	// (job.unexpected_field), because a v1 consumer would ignore exactly the
	// member a v2 consumer would act on — and here the disagreement is a
	// workspace-wide rename. Optional within v2 because an orchestrator older
	// than the member omits it; a task must then decline the rename, since not
	// renaming is recoverable and renaming is not.
	SourceName string `json:"sourceName,omitempty"`
	// Version is the project's RESOLVED effective version: its own authored
	// version, else the nearest scope's, else the workspace's. It is the same
	// value the orchestrator's project model carries, not a second reading of
	// the manifest.
	//
	// It exists because a package reference is a PAIR. A selector that locates
	// a contribution by package names both the package and its version, and a
	// consumer handed only the name has to either widen the match or fail it —
	// and failing it is the quiet outcome: the reference degrades to "source
	// unavailable" while every versionless reference beside it still resolves,
	// so the narrowing shows up as a missing verdict rather than as an error.
	// Resolved for the same reason Project.Type is: the
	// authored-over-scope-over-workspace fallback belongs to the orchestrator,
	// and a task re-deriving it would need the scope chain this contract does
	// not carry.
	//
	// v2 ONLY, and optional within v2, for the same reasons as SourceName above
	// — with the opposite safe default. A task that receives no version must
	// match by name alone rather than assume a version: an unversioned match is
	// wider than intended, while a match against an INVENTED version is simply
	// wrong.
	Version string `json:"version,omitempty"`
	// Dependencies are the ids of the projects this one depends on DIRECTLY, in
	// the order the orchestrator resolved them. v2 only, and optional within v2.
	//
	// Direct is the whole point, and it is what Project.DependencyClosure is not.
	// The closure answers "everything this project transitively reaches", which
	// is what an aggregation over a graph needs; this answers "what does this
	// project itself declare", which is what a rule ABOUT the graph needs. An
	// architecture contract authorizes exact producer-to-consumer edges, so a
	// consumer handed only a closure cannot tell an edge it must justify from one
	// its dependency already justified — it would report a violation for every
	// pair in the closure, or, reading the closure as opaque, none at all. The
	// latter is what happened before this member existed: the detector observed
	// zero edges and reported every undeclared dependency as absent.
	//
	// Ids, not names, because an id is the workspace's own stable identity and a
	// name is resolvable to it only through the name→id table this contract does
	// not carry. Out-of-workspace edges are omitted: there is no member to point
	// at, and a rule about the workspace graph has nothing to say about them.
	//
	// Absence is read from the CONTAINING member, never from this one. Inside
	// WorkspaceProjects, whose producer has by definition resolved the graph, an
	// entry without `dependencies` declares none. Anywhere else — a lone
	// SelectedProjects from a producer older than this member — absence says
	// nothing, and a consumer whose verdict is about edges must decline rather
	// than read "I was told nothing" as "there is nothing". That is why the
	// architecture validator keys its fail-closed check on WorkspaceProjects
	// being present and not on any single reference's edge list.
	Dependencies []string `json:"dependencies,omitempty"`
	// Config is the project's authored putnami.json as the orchestrator parsed
	// it. It stays raw because the workspace protocol owns the shape; declaring
	// it again here would create a second schema for the same document.
	//
	// The orchestrator populates it on WorkspaceProjects, where a validator can
	// need an authored fact about a sibling (`featureAuthority`, for example).
	// SelectedProjects and DependencyClosure omit it to avoid repeating the same
	// manifest in multiple carriers. v2 only: a v1 consumer would ignore the
	// reviewed answer while a v2 consumer acted on it.
	Config json.RawMessage `json:"config,omitempty"`
	// Extensions are the extension references the project resolves to, as the
	// orchestrator resolved them: its putnami.json list, else the provider's
	// view, else its scope's. A reference is an extension name or a
	// workspace-relative path such as `/go/extension`.
	//
	// The orchestrator populates it on WorkspaceProjects, where a workspace task
	// can need to know which extension's options apply to a sibling: an option
	// block keyed by an extension applies only to that extension's projects.
	// v2 only, for the reason Config is.
	Extensions []string `json:"extensions,omitempty"`
}

// GetBinString returns the bin field as a string if it's a simple string value.
func (p *Project) GetBinString() string {
	if p.Bin == nil {
		return ""
	}
	var s string
	if json.Unmarshal(p.Bin, &s) == nil {
		return s
	}
	return ""
}

// Extension identifies the Putnami extension running this job.
type Extension struct {
	// Name is the extension name (e.g. "/go/extension").
	Name string `json:"name"`
	// Root is the absolute path to the extension's installed root.
	Root string `json:"root"`
	// RuntimePath is the absolute path to the RESOLVED runtime executable the
	// orchestrator prepared for this extension. v2 only — forbidden at v1.
	//
	// It is the generic replacement for the language-specific wrapper paths a
	// subprocess used to reconstruct from Root: an extension that re-invokes
	// itself uses this value instead of knowing which of its own binaries the
	// CLI decided to run. Absent when the extension declares no runtime section
	// in its manifest.
	RuntimePath string `json:"runtimePath,omitempty"`
	// CacheRoot is the absolute path to the machine-global cache directory this
	// extension owns. v2 only — forbidden at v1.
	//
	// It is per-extension and generic: a language cache is the extension's
	// concern, so the orchestrator hands each extension one stable directory
	// instead of exporting a language-specific environment variable per
	// ecosystem. Distinct from Context.CacheRoot, which is the workspace's
	// mutable scratch and is shared by every extension.
	CacheRoot string `json:"cacheRoot,omitempty"`
}

// Invocation identifies one CLI invocation and the private artifact tree used
// by an invocation-scoped output lifecycle.
//
// Both members are non-secret locators. They are delivered only in job context
// documents for the relation's producer, declared consumers, and finalizer;
// they must not be copied into results, events, session records, telemetry, or
// cache traffic.
type Invocation struct {
	// ID is the opaque, non-secret invocation handle used by leases and cleanup.
	ID string `json:"id"`
	// ArtifactRoot is the absolute private directory against which
	// invocation-scoped declared-output paths resolve.
	ArtifactRoot string `json:"artifactRoot"`
}

// UserScope describes a job the orchestrator runs outside any workspace.
type UserScope struct {
	// CallerDir is the absolute directory the user ran the command from. The
	// job process starts in it, and it is the directory the job acts on. It
	// has no workspace manifest, and the orchestrator writes nothing in it.
	CallerDir string `json:"callerDir"`
}

// Job identifies the current job being executed.
type Job struct {
	// Name is the job/task name the subprocess must run (e.g. "build").
	Name string `json:"name"`
}

// Version holds git version info from the orchestrator.
type Version struct {
	// Base is the base version (the last release tag) without any suffix.
	Base string `json:"base"`
	// Full is the full computed version string, including any dirty/prerelease
	// suffix.
	Full string `json:"full"`
	// SHA is the current commit hash.
	SHA string `json:"sha"`
	// Branch is the current git branch name.
	Branch string `json:"branch"`
	// IsDirty reports whether the working tree had uncommitted changes when the
	// version was computed.
	IsDirty bool `json:"isDirty"`
	// Tag is the exact tag at the current commit, when the commit is tagged.
	Tag string `json:"tag"`
	// Suffix is the version suffix appended after Base (e.g. a prerelease or
	// dirty marker), when present.
	Suffix string `json:"suffix"`
}

// Params holds typed and untyped job parameters. The coercion rules in
// the accessors below are part of the contract: every SDK must resolve
// CLI flag strings and config-default JSON values identically.
type Params map[string]json.RawMessage

// String returns the string value of a parameter, or "" if not found.
// Fallback keys are tried in order when the primary key is absent.
func (p Params) String(key string, fallbacks ...string) string {
	keys := append([]string{key}, fallbacks...)
	for _, k := range keys {
		raw, ok := p[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		// Try unquoted (raw value that's not a JSON string)
		return string(raw)
	}
	return ""
}

// Bool returns the bool value of a parameter, or the default if not found.
// Fallback keys are tried in order when the primary key is absent.
func (p Params) Bool(key string, defaultVal bool, fallbacks ...string) bool {
	keys := append([]string{key}, fallbacks...)
	for _, k := range keys {
		raw, ok := p[k]
		if !ok {
			continue
		}
		var b bool
		if json.Unmarshal(raw, &b) == nil {
			return b
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch s {
			case "true", "1", "yes", "on":
				return true
			case "false", "0", "no", "off":
				return false
			}
		}
	}
	return defaultVal
}

// Int returns the int value of a parameter, or the default if not found.
// Fallback keys are tried in order when the primary key is absent. Numeric JSON
// values and numeric strings are both accepted so CLI flag values and config
// defaults resolve the same way.
func (p Params) Int(key string, defaultVal int, fallbacks ...string) int {
	keys := append([]string{key}, fallbacks...)
	for _, k := range keys {
		raw, ok := p[k]
		if !ok {
			continue
		}
		var n float64
		if json.Unmarshal(raw, &n) == nil {
			return int(n)
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return int(f)
			}
		}
	}
	return defaultVal
}

// Float returns the float64 value of a parameter, or the default if not found.
// Fallback keys are tried in order when the primary key is absent. Numeric JSON
// values and numeric strings (e.g. "80" or "85.5") are both accepted so the
// param resolves the same whether it arrives as a JSON number from config or as
// a string from a CLI flag.
func (p Params) Float(key string, defaultVal float64, fallbacks ...string) float64 {
	keys := append([]string{key}, fallbacks...)
	for _, k := range keys {
		raw, ok := p[k]
		if !ok {
			continue
		}
		var n float64
		if json.Unmarshal(raw, &n) == nil {
			return n
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	}
	return defaultVal
}

// PublishChannels parses the project.publish array into a string slice.
func (c *Context) PublishChannels() []string {
	if c.Project.Publish == nil {
		return nil
	}
	var channels []string
	if json.Unmarshal(c.Project.Publish, &channels) != nil {
		return nil
	}
	return channels
}

// HasPublishChannel returns true if the project publishes to the given channel.
func (c *Context) HasPublishChannel(channel string) bool {
	for _, ch := range c.PublishChannels() {
		if ch == channel {
			return true
		}
	}
	return false
}
