package job

// Job context, version 2.
//
// The compatibility rules this file implements — optional within v2, forbidden
// at v1, lenient consumers and strict producers — are recorded with their
// rejected alternatives in doc/adr/0001-v2-optional-members.md.
//
// Version 1 tells a subprocess WHERE it runs and WHAT parameters it got, but
// names the work it is doing with two loose strings — project.name and
// job.name. Those strings are not the identity the orchestrator schedules,
// caches, and reports under, so a subprocess cannot attribute anything it emits
// to the task the CLI is tracking; every surface re-derives its own key.
//
// v2 adds two members and changes nothing else:
//
//   - IDENTITY IS TYPED. `identity` is protocols/cli's TaskIdentity — the same
//     type, not a copy — so the identity a subprocess reads is byte-identical
//     to the one the session record, the plan and the result envelope carry.
//     Reuse instead of duplication is deliberate: a second declaration of the
//     same five structs is a drift waiting to happen.
//   - STAGING IS TASK-OWNED. `staging` names the directories the task must
//     write its declared outputs into, one per declared-output root of the
//     extension task contract v3. Cold runs execute in that tree and declared
//     outputs are captured from it, so the paths must come from the
//     orchestrator instead of being derived by convention in each SDK — that
//     inference is exactly what the task contract's declaration replaced.
//
// Four more OPTIONAL v2 members are declared for lifecycle-specific work. A
// producer emits each only for work that declares the corresponding lifecycle,
// and later work wires this existing shape instead of reshaping the contract
// under consumers that already read it:
//
//   - `extension.runtimePath` — the resolved runtime executable;
//   - `extension.cacheRoot` — the extension's machine-global cache root;
//   - `project.metadata[extensionName]` — provider-owned probe metadata;
//   - `invocation` — the non-secret id and private artifact root supplied only
//     to a finalizes relation's producer, consumers, and finalizer.
//
// Two further members are the same kind of thing: a RESOLVED fact the
// orchestrator already owns, handed to the task that needs it instead of being
// re-derived on the other side of the process boundary.
//
//   - `project.type` — the project's resolved classification, after the
//     authored-over-probed merge core performs;
//   - `project.dependencyClosure` — the project plus its in-workspace
//     dependency closure, in canonical project-id order.
//
// Both exist because extension-owned infra aggregation replaced a core gate
// that walked the graph itself: the POLICY (what a workload declares, what its
// runtime defaults are) belongs to the language extension, while the GRAPH
// stays core's. Splitting it any other way would mean either core keeping
// language knowledge or every extension rebuilding the workspace graph.
//
// `selection` is the same kind of member for the same reason: the orchestrator
// has already resolved WHAT THE USER ASKED FOR into a mode, a baseline and a
// project set, and an extension that re-derived it would need the CLI's flag
// vocabulary, its impact resolver and a git diff — three things a subprocess
// must not own. It is optional within v2 because a producer older than the
// member emits none; a consumer that receives no selection must not assume the
// run was unscoped.
//
// `workspaceProjects[].config` carries one AUTHORED fact rather than a resolved
// one: each project's parsed putnami.json, kept raw because protocols/workspace
// owns its shape. It lets a workspace-reading task honor a sibling's reviewed
// declarations without scanning manifests and inventing a second loader. The
// producer omits it from the other ProjectRef carriers to avoid repeating the
// same document; it remains v2-only wherever a producer does supply it.

// `workspace.options` carries the workspace-level extension option blocks the
// orchestrator already decoded. It is optional within v2 and remains raw: the
// named extension owns each block's semantics, while this protocol guarantees
// only a non-null object envelope. A task that reads one of those options must
// also declare the workspace file in its cache inputs.
//
// Optional is not unconstrained: a v1 document may not carry them, an absolute
// path is required when a path IS present, metadata must be a namespaced JSON
// object, and project config must be a non-null JSON object. A producer cannot
// invent a second meaning later.
//
// A document without `protocolVersion` is version 1, exactly as before, and v1
// documents may not carry v2 members. Both versions are accepted; the Putnami
// orchestrator emits v2.

import (
	"path"
	"strings"

	cliproto "go.putnami.dev/protocol/cli"
)

// ProtocolVersion2 is the v2 job context protocol version, carried in the
// document's `protocolVersion` member. ProtocolVersion stays 1 because an
// absent member is the stable v1 wire marker, not because it is the producer's
// current version.
const ProtocolVersion2 = 2

// Typed task identity, reused verbatim from protocols/cli. These are ALIASES,
// not copies: the job context and the CLI's machine documents cannot drift
// apart on the identity's shape, because there is only one shape.
type (
	// TaskIdentity is the immutable typed identity of one planned task.
	TaskIdentity = cliproto.TaskIdentity
	// ProjectIdentity identifies the owning project structurally.
	ProjectIdentity = cliproto.ProjectIdentity
	// TaskRef identifies the task within its project structurally.
	TaskRef = cliproto.TaskRef
	// ProviderIdentity identifies the extension providing the task.
	ProviderIdentity = cliproto.ProviderIdentity
)

// Task identity scopes, re-exported from protocols/cli for the same reason.
const (
	// TaskScopeProject is a task that runs once per selected project.
	TaskScopeProject = cliproto.TaskScopeProject
	// TaskScopeWorkspace is a task that runs once for the whole workspace.
	TaskScopeWorkspace = cliproto.TaskScopeWorkspace
)

// Staging root names. They are the declared-output roots of the extension task
// contract v3 (go.putnami.dev/protocol/extension: OutputRootProject,
// OutputRootWorkspace, OutputRootCommandOutput) — the same vocabulary a task
// declares its outputs against, so resolving a declared output is exactly
// Staging.PathFor(output.EffectiveRoot()) joined with the declared path. The
// equality is pinned by a drift test rather than by an import, so consumers of
// this contract do not take on the manifest protocol.
const (
	// StagingRootProject stages what a task writes under its project directory.
	StagingRootProject = "project"
	// StagingRootWorkspace stages what a task writes under the workspace root.
	StagingRootWorkspace = "workspace"
	// StagingRootCommandOutput stages what a task writes under the per-command
	// output directory.
	StagingRootCommandOutput = "command-output"
)

// StagingRoots is the closed staging-root vocabulary in canonical (sorted)
// order.
var StagingRoots = []string{
	StagingRootCommandOutput,
	StagingRootProject,
	StagingRootWorkspace,
}

// Staging is the task-owned directory tree a v2 job writes its declared outputs
// into.
//
// Every path is absolute and lives under Root, so the orchestrator can capture
// or discard everything one task produced by acting on a single directory. The
// three roots are DISTINCT and non-nesting: the task contract's "one owner per
// output" rule only holds across roots because two different roots are two
// different directories, and staging that resolved two roots to the same place
// would silently break it.
type Staging struct {
	// Root is the staging directory this task owns. It is a strict ancestor of
	// the three root directories below and of nothing else the task writes.
	Root string `json:"root"`
	// Project is where the task writes outputs declared against the project
	// root (StagingRootProject).
	Project string `json:"project"`
	// Workspace is where the task writes outputs declared against the workspace
	// root (StagingRootWorkspace).
	Workspace string `json:"workspace"`
	// CommandOutput is where the task writes outputs declared against the
	// per-command output directory (StagingRootCommandOutput).
	CommandOutput string `json:"commandOutput"`
}

// Selection modes: the closed vocabulary of how one command invocation chose
// the projects in scope.
//
// The three values and the member names below are the CLI's own resolved
// selection type (tooling/cli/internal/commands/shared.ResolvedSelection),
// which cannot be imported here because it carries resolved *workspace.Project
// values and would drag the CLI's workspace loader into a protocol package. The
// equality is pinned by a drift test rather than by an import, the same way the
// staging roots above are pinned against the manifest protocol.
const (
	// SelectionModeAll is the unscoped whole-workspace projection.
	SelectionModeAll = "all"
	// SelectionModeProjects is an explicit selector, with or without filters.
	SelectionModeProjects = "projects"
	// SelectionModeImpacted is the `--impacted` projection.
	SelectionModeImpacted = "impacted"
)

// SelectionModes is the closed selection-mode vocabulary in canonical (sorted)
// order.
var SelectionModes = []string{
	SelectionModeAll,
	SelectionModeImpacted,
	SelectionModeProjects,
}

// Selection is the resolved project selection of the command invocation this
// job belongs to.
//
// Every collection is sorted, so the same flags over the same tree produce the
// same bytes. Member ORDER is part of the contract too: this type must marshal
// byte-identically to the CLI's ResolvedSelection, which is what lets one
// resolver answer both the interactive surfaces and the planned jobs.
type Selection struct {
	// Mode is how the projection was chosen: one of SelectionModes.
	Mode string `json:"mode"`
	// Scoped distinguishes a narrowed run from the whole-workspace default. It
	// is the member a consumer acts on: a task that reports a verdict for the
	// workspace may only claim to have covered it when Scoped is false.
	Scoped bool `json:"scoped"`
	// Baseline is the ref `--impacted` actually resolved to, empty otherwise.
	// Meaningful only at SelectionModeImpacted.
	Baseline string `json:"baseline,omitempty"`
	// BaselineSource is the resolution tier that produced Baseline, so a
	// consumer can distrust a stale fallback ref the same way the run can.
	BaselineSource string `json:"baselineSource,omitempty"`
	// ProjectIDs are the selected projects' canonical ids, sorted.
	ProjectIDs []string `json:"projects"`
	// ReleaseSetProjects are the canonical ids, sorted, of the projects whose
	// publish and package steps this session's release-set plan owns. Absent
	// when the session coordinates no release set, and when its plan selected
	// no member. Always a subset of ProjectIDs.
	//
	// A session that names publish beside other commands plans two things at
	// once: the publication the channel head decided, and the verification the
	// caller's own selection asked for. ProjectIDs is what the run plans and
	// executes — the union — so this member is how a consumer tells the half it
	// may not attribute to the gate from the half it may.
	ReleaseSetProjects []string `json:"releaseSetProjects,omitempty"`
	// EmptyImpact records the legitimate no-op: `--impacted` resolved cleanly
	// and nothing changed. It is a success, not a "no projects matched" error,
	// and a consumer must render it as "nothing to check" rather than as a
	// failure to find work.
	EmptyImpact bool `json:"emptyImpact,omitempty"`
}

// PathFor returns the staging directory a declared-output root resolves to, or
// "" for a root this contract does not know. The empty result is the honest
// answer: a consumer must not invent a path for a root the orchestrator did not
// name.
func (s *Staging) PathFor(root string) string {
	if s == nil {
		return ""
	}
	switch root {
	case StagingRootProject:
		return s.Project
	case StagingRootWorkspace:
		return s.Workspace
	case StagingRootCommandOutput:
		return s.CommandOutput
	default:
		return ""
	}
}

// ContextVersion returns the protocol version the document declares: an absent
// (zero) member means version 1, which is how every v1 document is read.
func (c *Context) ContextVersion() int {
	if c == nil {
		return 0
	}
	if c.ProtocolVersion == 0 {
		return ProtocolVersion
	}
	return c.ProtocolVersion
}

// IsV2 reports whether the document speaks the v2 contract.
func (c *Context) IsV2() bool {
	return c.ContextVersion() == ProtocolVersion2
}

// isAbsolutePath reports whether p is an absolute path, independently of the
// host operating system. filepath.IsAbs is deliberately NOT used: it answers
// differently on Windows and POSIX, and this contract is validated against one
// cross-language corpus that must get the same verdict everywhere.
func isAbsolutePath(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == '/' {
		return true
	}
	// A Windows UNC path begins with TWO separators. A single leading
	// backslash is rooted only on the process's current drive and therefore is
	// not an absolute, context-independent location.
	if len(p) >= 2 && p[0] == '\\' && p[1] == '\\' {
		return true
	}
	// Windows drive-absolute: C:\ or C:/
	if len(p) >= 3 && isDriveLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return false
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// normalizePath returns the canonical comparison form of a path: slash
// separators, cleaned, no trailing separator (except for a bare root).
func normalizePath(p string) string {
	return path.Clean(strings.ReplaceAll(p, "\\", "/"))
}

// pathUnder reports whether child is strictly inside parent, tested at a path
// SEGMENT boundary so "/a/b" does not contain "/a/bc".
func pathUnder(parent, child string) bool {
	p, c := normalizePath(parent), normalizePath(child)
	if p == c {
		return false
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return strings.HasPrefix(c, p)
}

// pathsOverlap reports whether two paths claim the same region of the tree —
// the same containment rule the task contract's OutputsOverlap uses.
func pathsOverlap(a, b string) bool {
	return normalizePath(a) == normalizePath(b) || pathUnder(a, b) || pathUnder(b, a)
}
