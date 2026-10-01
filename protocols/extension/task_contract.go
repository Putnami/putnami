// Task contract v3: declared outputs, effects, and source mutation.
//
// Under v2 a task's filesystem footprint is INFERRED. The scheduler captures
// the shared per-command output directory and subtracts a pre-run baseline to
// guess which files the step itself produced, special-cases the project
// subtrees a task writes (.gen, generated clients), and carries source mutation
// as a cache-private result marker. Inference is why two machines with
// byte-identical sources can store different blobs under the same key, and why
// every new artifact shape needs another special case in the CLI.
//
// v3 replaces the guessing with a declaration. A task states exactly which
// files and subtrees it produces, which effects it has beyond those outputs,
// and whether it rewrites the sources it reads. The vocabulary is additive:
// a task without a `declares` block is a v2 task and keeps v2 behavior
// unchanged, so manifests migrate one task at a time.
//
// Three invariants make the declaration worth trusting:
//
//   - EXACTNESS — a declared output names a concrete file or subtree under a
//     named root, never a glob and never a root itself, so capture is a
//     function of the declaration instead of of run order.
//   - ONE OWNER PER OUTPUT — no two tasks may declare the same path, and
//     file-vs-subtree nesting counts as the same path (see OutputsOverlap).
//     Ownership is what makes a declared capture safe to run concurrently. A
//     directory output may CEDE a named subpath (DeclaredOutput.Excludes),
//     which moves that one subtree to the task that produces it without
//     splitting the rest of the tree; the carve-out is what keeps ownership
//     exclusive instead of forcing a late writer to go uncaptured.
//   - HONEST EFFECTS — a task whose consequences a cache hit cannot reproduce
//     (a registry push, a cloud mutation, a long-lived process) must say so,
//     and must not be cacheable.
//
// The task contract adds a fourth dimension to a declared output —
// its SCOPE. Everything above assumes an output the orchestrator may keep, and
// most are: `scope: "durable"` is the default and the behavior of every output
// declared before the vocabulary existed. An `invocation`-scoped output is the
// exception the three invariants still hold for: it is exact, it has one owner,
// and its effects are honest — it simply lives in the invocation's private
// scratch instead of a staging root, so it is never captured, never restored,
// and (when `sensitive`) never named in an event, a result, or cache traffic.
// See doc/07-lifecycles.md.
//
// Everything here is canonical by construction: outputs are keyed by id (Go
// marshals map keys sorted), effects normalize to sorted order, and paths
// normalize to cleaned slash form. A digest taken over a normalized manifest
// (the B0e task-contract digest) therefore does not depend on authoring order
// or map iteration order.

package extension

import (
	"fmt"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Declared-output kinds. A declaration must say which one it is: capture
// restores a file as a file and a directory as its whole subtree, and the two
// are not interchangeable on a cache hit.
const (
	// OutputKindFile is a single regular file.
	OutputKindFile = "file"
	// OutputKindDirectory is a directory captured recursively (the whole
	// subtree, not just the directory entry).
	OutputKindDirectory = "directory"
	// OutputKindRuntimeFile is a file whose CONTENT only means anything inside
	// the invocation that produced it — a connection string for a container
	// that dies with the run, a socket path, a short-lived credential. Storing
	// it would be worse than useless: a cache hit would hand a later run a
	// pointer to something that no longer exists. It is therefore never
	// captured and never restored, and it requires OutputScopeInvocation.
	OutputKindRuntimeFile = "runtime-file"
)

// ValidOutputKinds is the closed declared-output kind vocabulary in canonical
// (sorted) order.
var ValidOutputKinds = []string{
	OutputKindDirectory,
	OutputKindFile,
	OutputKindRuntimeFile,
}

// Declared-output scopes: how long an output lives and who may see it.
//
// Scope is orthogonal to kind and root. Kind says what is on disk, root says
// where it resolves, and scope says whether the orchestrator may keep it.
const (
	// OutputScopeDurable is the default: the output outlives the invocation, is
	// captured into the task's cache entry, and is restored on a hit. Every
	// output declared before this vocabulary existed is durable.
	OutputScopeDurable = "durable"
	// OutputScopeInvocation confines the output to the current CLI invocation.
	// It is written into the invocation's private scratch (mode 0600 for a
	// sensitive one), never captured, never restored, and discarded when the
	// invocation ends — by its finalizer if one ran, by lease reaping if the
	// process was killed.
	OutputScopeInvocation = "invocation"
)

// ValidOutputScopes is the closed declared-output scope vocabulary in canonical
// (sorted) order.
var ValidOutputScopes = []string{
	OutputScopeDurable,
	OutputScopeInvocation,
}

// Declared-output roots. A declared path is always relative to one of these,
// so the same declaration resolves per project without the manifest knowing
// any absolute path.
const (
	// OutputRootProject resolves against the owning project's directory. This
	// is the default and covers in-tree artifacts such as .gen and generated
	// clients.
	OutputRootProject = "project"
	// OutputRootWorkspace resolves against the workspace root. Ownership of a
	// workspace-rooted path is workspace-global: two tasks that declare it
	// collide even when they run for different projects.
	OutputRootWorkspace = "workspace"
	// OutputRootCommandOutput resolves against the per-command output directory
	// (.putnami/out/<project>/<command>). That directory is SHARED by every
	// step of one command, which is exactly why the steps must name their own
	// files instead of the capture subtracting a baseline to tell them apart.
	OutputRootCommandOutput = "command-output"
)

// OutputRootInvocation is the resolution root of an invocation-scoped output:
// the invocation's private scratch directory.
//
// It is NOT a value `root` may be set to, which is why it sits outside the
// block above. The three roots an author writes are the STAGING roots a job
// context carries; the invocation scratch is not one of them, precisely because
// nothing there is ever captured. EffectiveRoot returns this name for an
// invocation-scoped output so ownership comparison keeps working — two
// invocation-scoped outputs can still collide with each other, and never with a
// staged one.
const OutputRootInvocation = "invocation"

// Declared-output drift policies (protocol ADR 0004).
//
// A generator that writes into the worktree — a client, a schema, a migration,
// a document — produces bytes the repository COMMITS. Whether the committed
// bytes still equal what the current inputs generate is a question only the
// writer can answer, and only at the moment it writes: a verifier that runs
// later in the session sees a tree the generator (or a cache restore) has
// already rewritten. The policy therefore rides on the declared output itself.
// The engine snapshots the output path immediately before the task writes it
// (or before a cache hit swaps the recorded tree in), compares it with the
// bytes present afterwards, and reports a difference as the typed diagnostic
// OutputDriftDiagnosticCode. Neither the snapshot nor the verdict is a cache
// key input; a warm and a cold run reach the same verdict.
const (
	// OutputDriftWarn reports the differing paths at warning severity. The
	// task keeps its own status.
	OutputDriftWarn = "warn"
	// OutputDriftFail reports the differing paths at error severity and fails
	// the task. Its outputs are still written or restored, so the worktree now
	// holds the regenerated bytes and the operator commits them.
	OutputDriftFail = "fail"
)

// ValidOutputDriftPolicies is the closed drift-policy vocabulary in canonical
// (sorted) order. The empty string is NOT a member: it is the absent field,
// which means no comparison.
var ValidOutputDriftPolicies = []string{
	OutputDriftFail,
	OutputDriftWarn,
}

// OutputDriftDiagnosticCode is the stable code of the diagnostic the engine
// emits when a drift-policed output differs from the bytes present before the
// task wrote it. It is an ordinary task diagnostic, so it reaches the session
// record, machine output and the terminal like any other; under
// OutputDriftFail it is also the failed task's error code.
const OutputDriftDiagnosticCode = "generated-output-drift"

// IsValidOutputDriftPolicy reports whether value is part of the closed drift
// vocabulary.
func IsValidOutputDriftPolicy(value string) bool {
	for _, candidate := range ValidOutputDriftPolicies {
		if candidate == value {
			return true
		}
	}
	return false
}

// Task effects: consequences a task has BEYOND writing its declared outputs.
// The vocabulary is closed so a planner can reason about it; a task that needs
// an effect this list cannot express is a protocol change, not a free-form
// string.
const (
	// EffectWorkspaceFiles rewrites shared workspace-root files that are not
	// its declared outputs — a lockfile, a workspace config (deps install).
	EffectWorkspaceFiles = "workspace-files"
	// EffectToolchainCache writes a machine-global toolchain cache outside the
	// workspace (the Go module cache, a bundler cache). Such writes are not
	// captured and not restored.
	EffectToolchainCache = "toolchain-cache"
	// EffectNetwork performs network I/O that contributes to the result. A
	// cache hit legitimately skips it — this effect documents the dependency,
	// it does not forbid caching.
	EffectNetwork = "network"
	// EffectRegistry publishes to an external package registry.
	EffectRegistry = "registry"
	// EffectCloud mutates remote cloud state (a deploy, a resource change).
	EffectCloud = "cloud"
	// EffectProcess starts a long-lived process or binds a port (serve, run).
	EffectProcess = "process"
)

// ValidTaskEffects is the closed task-effect vocabulary in canonical (sorted)
// order. Exported so conformance harnesses and planners enumerate the same set
// this package validates against.
var ValidTaskEffects = []string{
	EffectCloud,
	EffectNetwork,
	EffectProcess,
	EffectRegistry,
	EffectToolchainCache,
	EffectWorkspaceFiles,
}

// externalTaskEffects are the effects a cache hit CANNOT reproduce: replaying a
// stored result instead of running the task would silently skip a registry
// push, a cloud mutation, or a process the caller is waiting on. Declaring one
// of these while caching is enabled is a contradiction, not a preference.
var externalTaskEffects = []string{
	EffectCloud,
	EffectProcess,
	EffectRegistry,
}

// ResourceIDSources is the conventional write/read resource id for a project's
// source tree. A v3 task that sets MutatesSources must also declare it as a
// write, because that resource — not the new flag — is what the planner
// serializes conflicting jobs on.
const ResourceIDSources = "sources"

// TaskDeclaration is the v3 task contract block (`tasks.<name>.declares`).
//
// Its presence is what makes a task a v3 task: consumers switch from inferred
// capture to declared capture per task, and a task without it keeps v2
// behavior exactly. Absence is never an error — v3 is additive by design.
type TaskDeclaration struct {
	// Outputs are the exact files and subtrees the task produces, keyed by a
	// stable output id. This is the FILESYSTEM footprint used for capture and
	// restore; it is distinct from TaskDefinition.Outputs, which declares
	// data ports for pipeline wiring. A declared output may take its path from
	// such a port (see DeclaredOutput.PathFrom), which is how a runtime-chosen
	// directory stays declarable.
	Outputs map[string]DeclaredOutput `json:"outputs,omitempty"`
	// Effects are the task's consequences beyond its declared outputs, drawn
	// from ValidTaskEffects. Normalized to sorted order; duplicates are a
	// validation error rather than a silent de-duplication, so a manifest never
	// means two things.
	Effects []string `json:"effects,omitempty"`
	// MutatesSources declares that the task rewrites files in the source tree
	// it reads — a formatter, a lint --fix, a codemod. It replaces the
	// cache-private per-result marker the CLI used to infer this after the
	// fact, so the planner and the store know BEFORE the task runs that its
	// inputs may not survive it.
	MutatesSources bool `json:"mutatesSources,omitempty"`
}

// DeclaredOutput is one exact output of a task: a file or a subtree, at a
// path relative to a named root.
//
// Exactly one of Path and PathFrom is set. Path is the ordinary case — a
// literal, glob-free, root-relative path. PathFrom names a data output port of
// the same task whose runtime value supplies the path, which is how an output
// whose location is chosen by configuration (a generated-client directory) can
// still be declared instead of special-cased in the CLI.
type DeclaredOutput struct {
	// Kind is OutputKindFile or OutputKindDirectory. Required: capture and
	// restore treat the two differently.
	Kind string `json:"kind"`
	// Root is OutputRootProject (default), OutputRootWorkspace, or
	// OutputRootCommandOutput. It must be left unset on an invocation-scoped
	// output, whose root is the invocation scratch (OutputRootInvocation) and
	// is implied by the scope.
	Root string `json:"root,omitempty"`
	// Scope is OutputScopeDurable (default) or OutputScopeInvocation.
	Scope string `json:"scope,omitempty"`
	// Sensitive marks an output whose PATH and BYTES must never reach an event,
	// a result, a session record, telemetry, or cache traffic — a credential, a
	// connection string, a token file. It requires OutputScopeInvocation,
	// because a durable output is captured by definition, and it requires a
	// literal Path: a PathFrom value travels in the task's result document,
	// which would publish the path the flag exists to hide.
	Sensitive bool `json:"sensitive,omitempty"`
	// Path is the literal root-relative path. Cleaned slash form, no globs, no
	// template variables, no escape above the root, and never the root itself.
	Path string `json:"path,omitempty"`
	// PathFrom names a port in the task's `outputs` map whose reported value is
	// the root-relative path. Overlap involving an unresolved PathFrom output
	// is decided when the value is known (see OutputsOverlap).
	PathFrom string `json:"pathFrom,omitempty"`
	// OptionalEmpty marks an output that may legitimately be absent — or an
	// empty directory — after a SUCCESSFUL run: a coverage file when coverage
	// is off, a client directory when the project generates no client. Capture
	// records nothing for it and restore materializes nothing, and neither is
	// an error. Without this flag, a missing declared output means the
	// declaration and the task disagree.
	OptionalEmpty bool `json:"optionalEmpty,omitempty"`
	// Excludes are literal root-relative subpaths STRICTLY inside Path that a
	// directory output CEDES: they are not captured with it, not restored with
	// it, and another task may own them.
	//
	// The carve-out exists for a tree with one owner and one later writer. A
	// task that declares a subtree whole and runs FIRST otherwise makes
	// everything a later task writes into that tree uncapturable: the first
	// task's snapshot predates those bytes, and its restore replaces the tree
	// with that snapshot, so a run serving both tasks from cache loses them.
	// Ceding the subpath hands it to the task that produces it,
	// which is the only task whose cache hit can reproduce it.
	//
	// Each entry is a canonical concrete path (NormalizeOutputPath) that sits
	// strictly inside Path and never equals it — an output that cedes itself
	// declares nothing. Duplicates are a validation error rather than a silent
	// de-duplication, so a manifest never means two things, and entries
	// normalize and sort so a digest over the declaration does not depend on
	// authoring order. Only a DIRECTORY output with a literal Path may carry
	// them: a file has no subpath, and a PathFrom value is unknown until the
	// task runs, so "inside Path" would not be decidable from the manifest.
	//
	// A ceded subpath that no other task claims is captured by NOBODY. That is
	// what the declaration says, not an oversight: the cede states "these bytes
	// are not mine", and the region's owner — possibly in another extension —
	// states the rest.
	Excludes []string `json:"excludes,omitempty"`
	// Preserves are OUTPUT-relative subpaths of a directory output that the
	// WORKSPACE owns: a project document (putnami.json) or a module file
	// (go.mod, go.sum) an author commits inside a generated directory so the
	// directory can be a workspace project of its own. The task may scaffold
	// one when it is absent, but the bytes are never the task's: they are not
	// captured with the output, a restore never replaces or deletes them, and a
	// drift comparison never judges them (ADR 0005).
	//
	// Unlike Excludes, an entry is relative to the OUTPUT, not to the root, so
	// it is decidable from the declaration alone on a pathFrom output too:
	// "inside the output" holds by construction for any canonical relative
	// path. Nothing claims a preserved path, so it takes no part in ownership.
	// Entries normalize and sort like Excludes, duplicates are a validation
	// error, and only a durable DIRECTORY output may carry them.
	Preserves []string `json:"preserves,omitempty"`
	// Drift is OutputDriftWarn or OutputDriftFail, or empty for no comparison.
	// It asks the engine to compare the bytes this task produces (or a cache
	// hit restores) at the output path with the bytes present there before the
	// task ran, and to report a difference as OutputDriftDiagnosticCode.
	//
	// It is a property of a worktree output — bytes the repository commits and
	// a generator rewrites — so it requires a durable, non-sensitive file or
	// directory output under the project or workspace root. The
	// command-output root is never committed, and a runtime file or an
	// invocation-scoped output is never in the tree after the run, so none of
	// them has a committed state to drift from. A pathFrom output may carry the
	// policy under the PROJECT root only: its path is unknown before the task
	// runs, so the engine's reference is a digest of the project tree, and a
	// workspace-wide digest before every such task is not a cost this contract
	// asks anybody to pay.
	Drift string `json:"drift,omitempty"`
	// Description is human-readable documentation for the output.
	Description string `json:"description,omitempty"`
}

// DriftPolicy returns the output's drift policy, or "" when the output declares
// none.
func (o DeclaredOutput) DriftPolicy() string {
	return o.Drift
}

// EffectiveRoot returns the output's root, defaulting to the project root.
//
// An invocation-scoped output resolves against the invocation scratch
// (OutputRootInvocation) whatever the (necessarily unset) Root member says, so
// a consumer that resolves roots to directories never places an
// invocation-scoped file in a staged tree.
func (o DeclaredOutput) EffectiveRoot() string {
	if o.EffectiveScope() == OutputScopeInvocation {
		return OutputRootInvocation
	}
	if o.Root == "" {
		return OutputRootProject
	}
	return o.Root
}

// EffectiveScope returns the output's scope, defaulting to durable.
func (o DeclaredOutput) EffectiveScope() string {
	if o.Scope == "" {
		return OutputScopeDurable
	}
	return o.Scope
}

// IsInvocationScoped reports whether the output is confined to the current CLI
// invocation and therefore never captured or restored.
func (o DeclaredOutput) IsInvocationScoped() bool {
	return o.EffectiveScope() == OutputScopeInvocation
}

// Ref returns the output's ownership identity. The path is normalized when it
// can be; an unnormalizable path yields an empty Path, which OutputsOverlap
// treats as not comparable (validation reports the bad path separately).
func (o DeclaredOutput) Ref() OutputRef {
	ref := OutputRef{Root: o.EffectiveRoot(), FromPort: o.PathFrom}
	if o.Path != "" {
		if normalized, err := NormalizeOutputPath(o.Path); err == nil {
			ref.Path = normalized
		}
	}
	ref.Excludes = DecidableExcludes(ref.Path, o.Excludes)
	return ref
}

// DecidableExcludes returns the carve-outs an ownership comparison may honor:
// each entry canonicalized, kept only when it sits strictly inside parent, and
// sorted.
//
// An entry that cannot be normalized, or that does not sit inside the output's
// own path, is DROPPED rather than honored. Validation reports it separately,
// and a declaration that states an impossible carve-out must never buy an
// ownership exemption it did not earn: dropping it fails closed, which for a
// carve-out means reporting the overlap.
//
// The parent test is case-insensitive because ownership is: two spellings that
// alias on the default macOS or Windows filesystem cannot mean different
// things. The returned entries keep their authored case, which is the form
// every consumer resolves against a real directory.
func DecidableExcludes(parent string, excludes []string) []string {
	if parent == "" || len(excludes) == 0 {
		return nil
	}
	decidable := make([]string, 0, len(excludes))
	for _, entry := range excludes {
		normalized, err := NormalizeOutputPath(entry)
		if err != nil || !pathContainsFold(parent, normalized) {
			continue
		}
		decidable = append(decidable, normalized)
	}
	if len(decidable) == 0 {
		return nil
	}
	sort.Strings(decidable)
	return decidable
}

// OutputRef is the identity an output is owned by: a root plus either a
// concrete path or the port that will supply one.
//
// The same type serves both analysis stages. Statically, Root is a symbolic
// root name and paths are root-relative, so only same-root refs are comparable.
// At plan time a consumer resolves Root to an absolute base directory (the
// project dir, the workspace root, the command output dir for THIS command) and
// keeps the relative path; OutputsOverlap then compares fully resolved refs
// with exactly the same rule, which is how cross-root collisions — a
// workspace-rooted path that lands inside a project — are caught.
type OutputRef struct {
	Root     string
	Path     string
	FromPort string
	// Excludes are the subpaths this ref's declaration cedes, in the same
	// namespace as Path — root-relative for a static comparison, joined onto
	// the resolved base for a plan-time one — normalized and sorted by
	// DecidableExcludes. A ref that lands inside one of them is not a second
	// owner of this ref's region (see OutputsOverlap).
	Excludes []string
}

// Resolved reports whether the ref names a concrete path. An unresolved ref
// (one waiting on a task output port) cannot be compared against a literal
// path before the task runs.
func (r OutputRef) Resolved() bool {
	return r.FromPort == "" && r.Path != ""
}

// WithoutCarveOuts returns the ref with its ceded subpaths dropped.
//
// It is the comparison ownership uses INSIDE one task. A cede moves a region
// between TASKS, and it works because the pipeline orders them: the ceding
// task's restore swaps a tree over its destination and the owning task, which
// runs after it, puts the subtree back. Nothing orders one task's own outputs
// against each other on restore, so a task that ceded a subpath to ITSELF
// would restore its two outputs in an arbitrary order and sometimes delete one
// with the other. Such a declaration stays the overlap it was before carve-outs
// existed.
func (r OutputRef) WithoutCarveOuts() OutputRef {
	r.Excludes = nil
	return r
}

// OutputsOverlap reports whether two declared outputs claim the same
// filesystem region — the negation of "one owner per output".
//
// Overlap is containment, not string equality: a file declared inside a
// declared subtree overlaps that subtree, because capturing or restoring the
// subtree also captures or restores the file. Containment is tested at a path
// SEGMENT boundary, so "dist" does not contain "dist2".
//
// Two refs never overlap when their roots differ, because a root-relative path
// says nothing about another root's tree; resolved refs (absolute roots) make
// that comparison exact. A ref that still waits on a task output port is
// compared by port: the same port under the same root resolves to the same
// directory, so it is reported as overlap; a port and a literal path are not
// comparable and are left to the resolved-time check.
//
// A CEDED subpath is the one exemption: when one ref's declaration excludes a
// subtree, a ref at or inside that subtree is not a second owner of it, because
// the ceding ref neither captures nor restores those bytes. The test is applied
// in both directions, so the answer does not depend on argument order.
func OutputsOverlap(a, b OutputRef) bool {
	if !strings.EqualFold(a.Root, b.Root) {
		return false
	}
	if a.FromPort != "" || b.FromPort != "" {
		return a.FromPort != "" && a.FromPort == b.FromPort
	}
	if a.Path == "" || b.Path == "" {
		return false
	}
	// Ownership is compared case-insensitively on every host. A manifest must
	// have one portable meaning, and two spellings that alias on the default
	// Windows or macOS filesystem cannot safely have different owners.
	aPath := strings.ToLower(a.Path)
	bPath := strings.ToLower(b.Path)
	if aPath != bPath && !pathContains(aPath, bPath) && !pathContains(bPath, aPath) {
		return false
	}
	return !cedesRegion(a, bPath) && !cedesRegion(b, aPath)
}

// cedesRegion reports whether ref's declaration cedes the region other
// occupies: other is one of ref's excluded subpaths, or sits inside one.
//
// A region that merely CONTAINS an excluded subpath is not ceded: excluding
// .gen/migration-bundle says nothing about .gen itself, and treating it as a
// cede would drop a real collision between two owners of .gen.
func cedesRegion(ref OutputRef, other string) bool {
	for _, exclude := range ref.Excludes {
		ceded := strings.ToLower(exclude)
		if other == ceded || pathContains(ceded, other) {
			return true
		}
	}
	return false
}

// pathContains reports whether parent contains child as a subtree entry.
func pathContains(parent, child string) bool {
	return strings.HasPrefix(child, parent+"/")
}

// pathContainsFold is pathContains under the case-insensitive comparison
// ownership uses, so a carve-out means one thing on every host.
func pathContainsFold(parent, child string) bool {
	return pathContains(strings.ToLower(parent), strings.ToLower(child))
}

// NormalizeOutputPath returns the canonical form of a declared output path, or
// an error explaining why the path cannot own a region of the tree.
//
// Canonical means: slash-separated, cleaned (no "." or ".." segments, no
// duplicate or trailing separators), relative, and concrete. Globs and template
// variables are rejected because an output that matches a set of paths cannot
// have a single owner, and absolute paths and escapes are rejected because a
// declaration must stay inside its root.
//
// It is the declared-output spelling of NormalizeRelativePath (paths.go), which
// the runtime and workspace sections share: every contract surface that names a
// concrete file answers to one rule.
func NormalizeOutputPath(p string) (string, error) {
	return NormalizeRelativePath(p)
}

// IsValidTaskEffect reports whether name is part of the closed effect
// vocabulary.
func IsValidTaskEffect(name string) bool {
	for _, effect := range ValidTaskEffects {
		if effect == name {
			return true
		}
	}
	return false
}

// IsExternalTaskEffect reports whether an effect makes a task unreplayable from
// cache: restoring a stored result would skip the effect entirely.
func IsExternalTaskEffect(name string) bool {
	for _, effect := range externalTaskEffects {
		if effect == name {
			return true
		}
	}
	return false
}

// UsesTaskContractV3 reports whether the task carries a v3 declaration. A task
// that does not is a v2 task and keeps v2 semantics.
func (t TaskDefinition) UsesTaskContractV3() bool {
	return t.Declares != nil
}

// ManifestProtocolVersion reports which contract version a parsed manifest
// exercises: ProtocolVersionV3 when it uses any v3 vocabulary,
// ProtocolVersionV2 otherwise. This is how a loader recognizes v3 without an
// on-the-wire version field — v3 vocabulary is self-identifying, and its
// absence is indistinguishable from a v2 manifest because it IS a v2 manifest.
//
// The v3 vocabulary is a `declares` block on any task (the task contract) plus
// the surfaces classified by ManifestLifecyclePrimitives. All are additive: a
// manifest that uses none of them is a v2 manifest and behaves exactly as it
// did.
//
// The result does not depend on map iteration order: it is a disjunction.
func ManifestProtocolVersion(m *Manifest) int {
	if m == nil {
		return ProtocolVersionV2
	}
	if len(ManifestLifecyclePrimitives(m)) != 0 {
		return ProtocolVersionV3
	}
	for _, task := range m.Tasks {
		if task.UsesTaskContractV3() {
			return ProtocolVersionV3
		}
	}
	return ProtocolVersionV2
}

// declaresSourceWrite reports whether the task declares the project-scoped
// "sources" write resource the planner serializes source mutation on.
func declaresSourceWrite(writes []ResourceRef) bool {
	for _, ref := range writes {
		if ref.ID == ResourceIDSources && ref.EffectiveScope() == ResourceScopeProject {
			return true
		}
	}
	return false
}

// validateTaskDeclaration checks one task's v3 declaration. It returns nothing
// for a v2 task, so adding these checks cannot change any existing manifest's
// verdict. Diagnostics are emitted in sorted output-id order, then declaration
// order for effects, then the fixed conflict order below.
func validateTaskDeclaration(field string, task TaskDefinition) []diag.Diagnostic {
	declaration := task.Declares
	if declaration == nil {
		return nil
	}
	base := field + ".declares"
	var diags []diag.Diagnostic

	for _, id := range sortedKeys(declaration.Outputs) {
		output := declaration.Outputs[id]
		outField := fmt.Sprintf("%s.outputs.%s", base, id)
		if strings.TrimSpace(id) == "" {
			diags = append(diags, diag.Errorf("required-field", outField, "declared output id cannot be empty"))
		}
		diags = append(diags, validateDeclaredOutput(outField, output, task)...)
	}

	diags = append(diags, validateTaskEffects(base, declaration.Effects)...)
	diags = append(diags, validateDeclarationConflicts(base, task, declaration)...)
	return diags
}

func validateDeclaredOutput(field string, output DeclaredOutput, task TaskDefinition) []diag.Diagnostic {
	var diags []diag.Diagnostic

	switch output.Kind {
	case OutputKindFile, OutputKindDirectory, OutputKindRuntimeFile:
	case "":
		diags = append(diags, diag.Errorf("required-field", field+".kind",
			"declared output kind is required; must be one of: %s", strings.Join(ValidOutputKinds, ", ")))
	default:
		diags = append(diags, diag.Errorf("invalid-enum", field+".kind",
			"invalid declared output kind %q; must be one of: %s", output.Kind, strings.Join(ValidOutputKinds, ", ")))
	}

	switch output.Root {
	case "", OutputRootProject, OutputRootWorkspace, OutputRootCommandOutput:
	default:
		diags = append(diags, diag.Errorf("invalid-enum", field+".root",
			"invalid declared output root %q; must be one of: command-output, project, workspace", output.Root))
	}

	switch output.Scope {
	case "", OutputScopeDurable, OutputScopeInvocation:
	default:
		diags = append(diags, diag.Errorf("invalid-enum", field+".scope",
			"invalid declared output scope %q; must be one of: %s", output.Scope, strings.Join(ValidOutputScopes, ", ")))
	}

	diags = append(diags, validateInvocationScope(field, output)...)

	switch {
	case output.Path == "" && output.PathFrom == "":
		diags = append(diags, diag.Errorf("required-field", field+".path",
			"declared output must set exactly one of path or pathFrom"))
	case output.Path != "" && output.PathFrom != "":
		diags = append(diags, diag.Errorf("invalid-value", field+".path",
			"declared output must set exactly one of path or pathFrom, not both"))
	case output.Path != "":
		if _, err := NormalizeOutputPath(output.Path); err != nil {
			diags = append(diags, diag.Errorf("invalid-output-path", field+".path",
				"invalid declared output path %q: %v", output.Path, err))
		}
	default:
		if _, ok := task.Outputs[output.PathFrom]; !ok {
			diags = append(diags, diag.Errorf("unresolved-output-port", field+".pathFrom",
				"declared output pathFrom references output port %q, which the task does not declare", output.PathFrom))
		}
	}

	diags = append(diags, validateOutputExcludes(field, output)...)
	diags = append(diags, validateOutputPreserves(field, output)...)
	return append(diags, validateOutputDrift(field, output)...)
}

// validateOutputPreserves checks the workspace-owned subpaths one declared
// output states. Each rule is decidable from the declaration alone: an entry is
// output-relative, so no runtime path is needed to place it.
func validateOutputPreserves(field string, output DeclaredOutput) []diag.Diagnostic {
	if len(output.Preserves) == 0 {
		return nil
	}
	var diags []diag.Diagnostic
	// A file has no subpath, and an invocation-scoped output is never captured
	// or restored, so there is nothing a preserve could protect it from.
	if output.Kind != OutputKindDirectory {
		diags = append(diags, diag.Errorf("invalid-output-preserve", field+".preserves",
			"only a %q output may preserve a subpath; this output declares kind %q",
			OutputKindDirectory, output.Kind))
	}
	if output.EffectiveScope() != OutputScopeDurable {
		diags = append(diags, diag.Errorf("invalid-output-preserve", field+".preserves",
			"only a %q output may preserve a subpath: an invocation-scoped output is never captured or restored",
			OutputScopeDurable))
	}
	seen := make(map[string]string, len(output.Preserves))
	for i, entry := range output.Preserves {
		entryField := fmt.Sprintf("%s.preserves[%d]", field, i)
		normalized, err := NormalizeOutputPath(entry)
		if err != nil {
			diags = append(diags, diag.Errorf("invalid-output-path", entryField,
				"invalid declared output preserve %q: %v", entry, err))
			continue
		}
		if first, duplicate := seen[strings.ToLower(normalized)]; duplicate {
			diags = append(diags, diag.Errorf("duplicate-output-preserve", entryField,
				"declared output preserve %q is declared more than once (first as %q)", entry, first))
			continue
		}
		seen[strings.ToLower(normalized)] = entry
	}
	return diags
}

// DecidablePreserves returns the workspace-owned subpaths a directory output
// preserves, output-relative, each canonicalized and sorted. An entry that
// cannot be normalized is DROPPED: validation reports it, and keeping the bytes
// in the capture is the safe direction for a misspelled preserve.
func DecidablePreserves(output DeclaredOutput) []string {
	if output.Kind != OutputKindDirectory || len(output.Preserves) == 0 {
		return nil
	}
	decidable := make([]string, 0, len(output.Preserves))
	for _, entry := range output.Preserves {
		normalized, err := NormalizeOutputPath(entry)
		if err != nil {
			continue
		}
		decidable = append(decidable, normalized)
	}
	if len(decidable) == 0 {
		return nil
	}
	sort.Strings(decidable)
	return decidable
}

// validateOutputDrift checks that a drift policy is one the closed vocabulary
// names and that the output it rides on has a committed state to drift from.
// Every rule compares two statements the SAME output makes, so it is decidable
// from the declaration alone, and the reference the engine will take is
// decidable with it: a literal path names its own snapshot, and a pathFrom
// output under the project root names the project tree.
func validateOutputDrift(field string, output DeclaredOutput) []diag.Diagnostic {
	if output.Drift == "" {
		return nil
	}
	var diags []diag.Diagnostic
	if !IsValidOutputDriftPolicy(output.Drift) {
		diags = append(diags, diag.Errorf("invalid-enum", field+".drift",
			"invalid declared output drift policy %q; must be one of: %s", output.Drift, strings.Join(ValidOutputDriftPolicies, ", ")))
	}
	// A runtime file is never in the tree after the run; an invocation-scoped
	// output is discarded with the invocation. Neither has committed bytes.
	if output.Kind == OutputKindRuntimeFile {
		diags = append(diags, diag.Errorf("invalid-output-drift", field+".drift",
			"a drift policy compares committed bytes, and a %q output is never in the tree after the run", OutputKindRuntimeFile))
	}
	if output.EffectiveScope() == OutputScopeInvocation {
		diags = append(diags, diag.Errorf("invalid-output-drift", field+".drift",
			"a drift policy compares committed bytes, and an invocation-scoped output is discarded with the invocation"))
	}
	// The verdict names the output's relative paths in a diagnostic, which is
	// one of the surfaces a sensitive output may never reach. (A sensitive
	// output is invocation-scoped by another rule; this one names the reason
	// that would survive even if that rule moved.)
	if output.Sensitive {
		diags = append(diags, diag.Errorf("invalid-output-drift", field+".drift",
			"a drift policy reports the output's paths in a diagnostic, which a sensitive output must never reach"))
	}
	// .putnami/out is never committed, so there is nothing there to drift from.
	if output.Root == OutputRootCommandOutput {
		diags = append(diags, diag.Errorf("invalid-output-drift", field+".drift",
			"a drift policy compares committed bytes, and the %q root is never committed", OutputRootCommandOutput))
	}
	// The reference for a pathFrom output is a digest of the root it resolves
	// under, taken before the path is known. The project tree is bounded; the
	// workspace is not.
	if output.PathFrom != "" && output.Root != "" && output.Root != OutputRootProject {
		diags = append(diags, diag.Errorf("invalid-output-drift", field+".drift",
			"a drift policy on a pathFrom output requires the %q root: the reference is taken before the path is known, "+
				"so it is a digest of the root, and only the project tree is bounded enough to digest before every run", OutputRootProject))
	}
	return diags
}

// validateOutputExcludes checks the carve-outs one declared output states.
// Every rule compares two statements the SAME output makes, so a carve-out is
// decidable from the declaration alone — which is what lets the manifest-local
// check and the planner apply one predicate to it.
//
// Diagnostics come out in declaration order, so two runs over one manifest are
// byte-identical.
func validateOutputExcludes(field string, output DeclaredOutput) []diag.Diagnostic {
	if len(output.Excludes) == 0 {
		return nil
	}
	var diags []diag.Diagnostic

	// A carve-out names a subtree of a subtree. A file has none, and a runtime
	// file is never captured in the first place.
	if output.Kind != OutputKindDirectory {
		diags = append(diags, diag.Errorf("invalid-output-exclude", field+".excludes",
			"only a %q output may cede a subpath; this output declares kind %q",
			OutputKindDirectory, output.Kind))
	}
	// "Strictly inside path" is not decidable for a path the task reports at
	// runtime, and a carve-out whose meaning waits for a run cannot be checked
	// by the manifest-local half of ownership at all.
	if output.PathFrom != "" {
		diags = append(diags, diag.Errorf("invalid-output-exclude", field+".excludes",
			"a declared output that cedes a subpath must declare a literal path: a pathFrom value is "+
				"unknown until the task runs, so no manifest check can decide what the subpath is inside of"))
	}

	parent, parentErr := NormalizeOutputPath(output.Path)
	seen := make(map[string]string, len(output.Excludes))
	for i, entry := range output.Excludes {
		entryField := fmt.Sprintf("%s.excludes[%d]", field, i)
		normalized, err := NormalizeOutputPath(entry)
		if err != nil {
			diags = append(diags, diag.Errorf("invalid-output-path", entryField,
				"invalid declared output exclude %q: %v", entry, err))
			continue
		}
		if first, duplicate := seen[strings.ToLower(normalized)]; duplicate {
			diags = append(diags, diag.Errorf("duplicate-output-exclude", entryField,
				"declared output exclude %q is declared more than once (first as %q)", entry, first))
			continue
		}
		seen[strings.ToLower(normalized)] = entry
		if parentErr != nil || output.PathFrom != "" {
			// The parent is already reported as invalid (or is a port); saying
			// the exclude is not inside it would repeat one defect twice.
			continue
		}
		if !pathContainsFold(parent, normalized) {
			diags = append(diags, diag.Errorf("invalid-output-exclude", entryField,
				"declared output exclude %q must name a subpath strictly inside the output path %q",
				entry, output.Path))
		}
	}

	return diags
}

// validateInvocationScope checks the three ways an output can claim
// invocation-scoped or sensitive semantics without the rest of its declaration
// agreeing. Each rule compares two statements the SAME output makes, so it is
// decidable from the declaration alone.
func validateInvocationScope(field string, output DeclaredOutput) []diag.Diagnostic {
	var diags []diag.Diagnostic
	invocation := output.EffectiveScope() == OutputScopeInvocation

	// A runtime file is by definition meaningless outside the run that wrote
	// it. Declaring one as durable would ask the store to cache a pointer to a
	// resource that no longer exists.
	if output.Kind == OutputKindRuntimeFile && !invocation {
		diags = append(diags, diag.Errorf("invocation-scope-required", field+".scope",
			"declared output kind %q only means anything inside the invocation that wrote it; set scope to %q",
			OutputKindRuntimeFile, OutputScopeInvocation))
	}

	// A durable output is captured by definition, and capture is exactly the
	// path a sensitive value must never take.
	if output.Sensitive && !invocation {
		diags = append(diags, diag.Errorf("invocation-scope-required", field+".scope",
			"a sensitive output must not be captured; set scope to %q", OutputScopeInvocation))
	}

	// The invocation scratch is not a staging root, so naming one would make
	// two contradictory statements about where the file resolves.
	if invocation && output.Root != "" {
		diags = append(diags, diag.Errorf("invocation-scope-conflict", field+".root",
			"an invocation-scoped output resolves against the invocation scratch, not the %q root; leave root unset",
			output.Root))
	}

	// A pathFrom value is reported by the task and travels in its result
	// document, which is one of the surfaces a sensitive path may never reach.
	if output.Sensitive && output.PathFrom != "" {
		diags = append(diags, diag.Errorf("sensitive-path-leak", field+".pathFrom",
			"a sensitive output must declare a literal path: a pathFrom value is reported through the task "+
				"result, which would publish the path the sensitive flag exists to withhold"))
	}

	return diags
}

func validateTaskEffects(base string, effects []string) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seen := make(map[string]bool, len(effects))
	for i, effect := range effects {
		effectField := fmt.Sprintf("%s.effects[%d]", base, i)
		if !IsValidTaskEffect(effect) {
			diags = append(diags, diag.Errorf("invalid-enum", effectField,
				"invalid task effect %q; must be one of: %s", effect, strings.Join(ValidTaskEffects, ", ")))
			continue
		}
		if seen[effect] {
			diags = append(diags, diag.Errorf("duplicate-effect", effectField,
				"task effect %q is declared more than once", effect))
		}
		seen[effect] = true
	}
	return diags
}

// validateDeclarationConflicts checks the ways a declaration can contradict the
// rest of the task it belongs to. Each rule compares two statements the task
// makes about itself, so it is decidable from the manifest alone.
func validateDeclarationConflicts(base string, task TaskDefinition, declaration *TaskDeclaration) []diag.Diagnostic {
	var diags []diag.Diagnostic

	if task.Cache != nil && task.Cache.NoOutput && len(declaration.Outputs) > 0 {
		diags = append(diags, diag.Errorf("effect-conflict", base+".outputs",
			"task declares %d output(s) but cache.noOutput says it never writes output files",
			len(declaration.Outputs)))
	}

	// An invocation-scoped output is never captured, so a cache hit would
	// replay a "success" whose artifact was never created and hand the
	// consumers a path to nothing. Such a task is pruned when its consumers all
	// hit — it is not itself replayable.
	if task.Cache.IsEnabled() {
		for _, id := range sortedKeys(declaration.Outputs) {
			if !declaration.Outputs[id].IsInvocationScoped() {
				continue
			}
			diags = append(diags, diag.Errorf("invocation-cache-conflict", base+".outputs."+id,
				"task declares an invocation-scoped output, which is never captured; a cache hit would "+
					"report success without it, so such a task must set cache.enabled to false"))
		}
	}

	if task.Cache.IsEnabled() {
		declared := make(map[string]bool, len(declaration.Effects))
		for _, effect := range declaration.Effects {
			declared[effect] = true
		}
		// Iterate the canonical vocabulary, not the declaration, so the
		// diagnostics are ordered and de-duplicated regardless of how the
		// effects were written.
		for _, effect := range externalTaskEffects {
			if !declared[effect] {
				continue
			}
			diags = append(diags, diag.Errorf("effect-conflict", base+".effects",
				"task declares the external effect %q, which a cache hit would skip; such a task must set cache.enabled to false",
				effect))
		}
	}

	writesSources := declaresSourceWrite(task.Writes)
	switch {
	case declaration.MutatesSources && !writesSources:
		diags = append(diags, diag.Errorf("effect-conflict", base+".mutatesSources",
			"task declares mutatesSources but not the project-scoped %q write resource the planner serializes source mutation on",
			ResourceIDSources))
	case !declaration.MutatesSources && writesSources:
		diags = append(diags, diag.Errorf("effect-conflict", base+".mutatesSources",
			"task declares the %q write resource but not mutatesSources; a v3 task contract states source mutation explicitly",
			ResourceIDSources))
	}

	return diags
}

// ValidateTaskContracts is the conformance harness for the v3 task contract: it
// proves all three invariants over a whole parsed manifest and is what the
// strict validation path calls, so an SDK author, `putnami dev extension
// validate`, the package-time gate and a conformance fixture can never disagree
// about what conformance means.
//
//   - EXACTNESS and HONEST EFFECTS are per-task: a declaration's kinds, roots
//     and paths, its effects, and the ways those contradict the task's own
//     cache policy and write resources.
//   - ONE OWNER PER OUTPUT is manifest-wide and lives in
//     ValidateOutputOwnership.
//
// Diagnostics come out in sorted task order and then the ownership phase, so
// two runs over one manifest are byte-identical. The whole function is keyed on
// `declares`: a manifest with no v3 task produces nothing, which is what makes
// wiring it into the strict path additive for every manifest written so far.
//
// The cross-extension, cross-project half of ownership is deliberately absent —
// it is the planner's, and it applies the SAME predicates (OutputsOverlap,
// NormalizeOutputPath, IsExternalTaskEffect) to refs whose roots have been
// resolved to concrete directories. Both sides call this package rather than
// reimplementing the rules, which is why a manifest that passes here cannot
// surprise the planner with a rule it never heard of.
func ValidateTaskContracts(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	var diags []diag.Diagnostic
	for _, name := range sortedKeys(m.Tasks) {
		diags = append(diags, validateTaskDeclaration("tasks."+name, m.Tasks[name])...)
	}
	return append(diags, ValidateOutputOwnership(m)...)
}

// ownedOutput is one declared output paired with the task that claims it.
type ownedOutput struct {
	task  string
	id    string
	field string
	ref   OutputRef
}

// ValidateOutputOwnership enforces ONE OWNER PER OUTPUT across the manifest: no
// two declared outputs may claim the same path, where "same path" includes
// file-vs-subtree nesting (see OutputsOverlap).
//
// A directory output that CEDES a subpath is not an owner of it, so a second
// task may declare that subpath — and only that subpath: a third claim on the
// ceded region still collides with the task that took it. The refs carry the
// carve-out (DeclaredOutput.Ref), so this check and the planner's resolved-ref
// check decide it with the same predicate instead of two spellings of it.
//
// Command-output paths are scoped to the command whose directory they live in:
// two tasks that never appear in the same command write to different
// directories and cannot collide, while two steps of ONE command share the
// directory — the ambiguity that baseline subtraction exists to paper over.
// Project- and workspace-rooted outputs are compared unconditionally, because
// any two tasks selected for the same project resolve them to the same tree.
//
// This is the manifest-local half of the invariant. The cross-extension half —
// two extensions claiming the same path in one workspace — is the planner's,
// and it applies OutputsOverlap to resolved refs.
func ValidateOutputOwnership(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return nil
	}

	owned := make([]ownedOutput, 0, len(m.Tasks))
	for _, taskName := range sortedKeys(m.Tasks) {
		task := m.Tasks[taskName]
		if task.Declares == nil {
			continue
		}
		for _, id := range sortedKeys(task.Declares.Outputs) {
			output := task.Declares.Outputs[id]
			owned = append(owned, ownedOutput{
				task:  taskName,
				id:    id,
				field: fmt.Sprintf("tasks.%s.declares.outputs.%s", taskName, id),
				ref:   output.Ref(),
			})
		}
	}
	if len(owned) < 2 {
		return nil
	}

	commands := commandsByTask(m)
	var diags []diag.Diagnostic
	for i := 1; i < len(owned); i++ {
		for j := 0; j < i; j++ {
			later, earlier := owned[i], owned[j]
			if !ownershipOverlap(later, earlier) {
				continue
			}
			if later.ref.Root == OutputRootCommandOutput &&
				later.task != earlier.task &&
				!sharesCommand(commands[later.task], commands[earlier.task]) {
				continue
			}
			if later.task == earlier.task {
				// Same wording for the invariant as the cross-task case, plus why
				// a carve-out does not excuse this one: every ownership
				// diagnostic names the rule it enforces, and a harness that
				// mutates a manifest looks for that name (scaffold's and
				// clientgen's manifest_contract_test.go both do).
				diags = append(diags, diag.Errorf("output-overlap", later.field,
					"declared output overlaps %s, declared by the same task; every output has exactly one "+
						"owner, and a carve-out cannot divide a task's own declaration — it cedes a subpath "+
						"to ANOTHER task, and nothing orders one task's outputs against each other on restore",
					earlier.field))
				continue
			}
			diags = append(diags, diag.Errorf("output-overlap", later.field,
				"declared output overlaps %s, declared by task %q; every output has exactly one owner",
				earlier.field, earlier.task))
		}
	}
	return diags
}

// ownershipOverlap answers OutputsOverlap for a pair of owned outputs, ignoring
// carve-outs when both belong to the SAME task (see OutputRef.WithoutCarveOuts).
func ownershipOverlap(later, earlier ownedOutput) bool {
	if later.task == earlier.task {
		return OutputsOverlap(later.ref.WithoutCarveOuts(), earlier.ref.WithoutCarveOuts())
	}
	return OutputsOverlap(later.ref, earlier.ref)
}

// commandsByTask maps each task to the sorted set of commands whose pipeline
// references it.
func commandsByTask(m *Manifest) map[string][]string {
	byTask := make(map[string]map[string]bool, len(m.Tasks))
	for _, cmdName := range sortedKeys(m.Commands) {
		for _, step := range m.Commands[cmdName].Run {
			if step.Task == "" {
				continue
			}
			if byTask[step.Task] == nil {
				byTask[step.Task] = make(map[string]bool, 1)
			}
			byTask[step.Task][cmdName] = true
		}
	}
	out := make(map[string][]string, len(byTask))
	for task, set := range byTask {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		out[task] = names
	}
	return out
}

func sharesCommand(a, b []string) bool {
	for _, left := range a {
		for _, right := range b {
			if left == right {
				return true
			}
		}
	}
	return false
}

// NormalizeTaskDeclarations rewrites every v3 declaration into its canonical
// form in place: effects sorted, output paths cleaned. It is what makes a
// digest over a parsed manifest independent of how the manifest was authored.
// Paths that cannot be normalized are left untouched for validation to report.
func NormalizeTaskDeclarations(m *Manifest) {
	if m == nil {
		return
	}
	for name, task := range m.Tasks {
		if task.Declares == nil {
			continue
		}
		normalizeTaskDeclaration(&task)
		m.Tasks[name] = task
	}
}

// NormalizeTask canonicalizes one task in place: derives the cache key from
// input ports when no explicit policy is set (matching NormalizeManifest's
// historical behavior), sorts declared effects, and cleans declared output
// paths. NormalizeManifest applies it to every task; TaskContractDigest
// applies it to its private copy, so a digest never depends on whether the
// manifest traveled through a normalizing load path.
func NormalizeTask(t *TaskDefinition) {
	if t == nil {
		return
	}
	if t.Cache == nil && len(t.Inputs) > 0 {
		key := DeriveTaskCacheKey(t.Inputs)
		t.Cache = &TaskCachePolicy{Key: &key}
	}
	normalizeTaskDeclaration(t)
}

// normalizeTaskDeclaration is the per-task core of NormalizeTaskDeclarations.
func normalizeTaskDeclaration(t *TaskDefinition) {
	if t == nil || t.Declares == nil {
		return
	}
	if len(t.Declares.Effects) > 1 {
		sort.Strings(t.Declares.Effects)
	}
	for id, output := range t.Declares.Outputs {
		changed := false
		if output.Path != "" {
			if normalized, err := NormalizeOutputPath(output.Path); err == nil && normalized != output.Path {
				output.Path = normalized
				changed = true
			}
		}
		if normalizeOutputExcludes(output.Excludes) {
			changed = true
		}
		// Preserves canonicalize exactly like Excludes: cleaned, then sorted,
		// duplicates left for validation.
		if normalizeOutputExcludes(output.Preserves) {
			changed = true
		}
		if changed {
			t.Declares.Outputs[id] = output
		}
	}
}

// normalizeOutputExcludes canonicalizes a declared output's carve-outs in place
// — each entry cleaned, then the list sorted — and reports whether anything
// moved. Entries that cannot be normalized are left untouched for validation to
// report, and duplicates are NOT collapsed: a manifest that cedes one subpath
// twice is invalid, and silently de-duplicating it would hide the defect while
// changing what the manifest says.
func normalizeOutputExcludes(excludes []string) bool {
	changed := false
	for i, entry := range excludes {
		normalized, err := NormalizeOutputPath(entry)
		if err != nil || normalized == entry {
			continue
		}
		excludes[i] = normalized
		changed = true
	}
	if len(excludes) > 1 && !sort.StringsAreSorted(excludes) {
		sort.Strings(excludes)
		changed = true
	}
	return changed
}
