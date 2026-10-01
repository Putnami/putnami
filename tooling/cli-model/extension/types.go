// Package extension provides types and logic for extension manifest parsing,
// pipeline expansion, expression evaluation, and binding resolution.
//
// Manifest types are provided by the extension protocol module;
// resolved types (ExtensionDescription, JobDefinition) are CLI-specific.
package extension

import (
	proto "go.putnami.dev/protocol/extension"
)

// ManifestFilename is the extension manifest file name, owned by the protocol.
const ManifestFilename = proto.ManifestFilename

// --- Manifest type aliases (re-exported from protocol) ---

type Manifest = proto.Manifest
type Dependencies = proto.Dependencies
type CommandDefinition = proto.CommandDefinition
type SessionPrerequisiteDefinition = proto.SessionPrerequisiteDefinition
type SessionPrerequisiteParamBinding = proto.SessionPrerequisiteParamBinding
type CommandTraits = proto.CommandTraits
type ToolDefinition = proto.ToolDefinition
type ToolAnnotations = proto.ToolAnnotations

// Re-exported trait constants from the extension protocol.
const (
	SideEffectsRegistry = proto.SideEffectsRegistry
	SideEffectsCloud    = proto.SideEffectsCloud
)

// Re-exported task input sources. `task` is the typed producer/consumer edge
// the planner enforces (jobs/plan_producers.go); the rest are cache-key
// contributions.
const (
	TaskInputFromProject   = proto.TaskInputFromProject
	TaskInputFromWorkspace = proto.TaskInputFromWorkspace
	TaskInputFromTask      = proto.TaskInputFromTask
	TaskInputFromParams    = proto.TaskInputFromParams
	TaskInputFromEnv       = proto.TaskInputFromEnv
	TaskInputFromRuntime   = proto.TaskInputFromRuntime
)

type CommandGroupDefinition = proto.CommandGroupDefinition
type SubcommandDefinition = proto.SubcommandDefinition
type PositionalDefinition = proto.PositionalDefinition
type PipelineStep = proto.PipelineStep
type StepActivation = proto.StepActivation
type TaskDefinition = proto.TaskDefinition

type TaskBatchPolicy = proto.TaskBatchPolicy
type TaskInputPort = proto.TaskInputPort
type TaskOutputPort = proto.TaskOutputPort
type ResourceRef = proto.ResourceRef

// Task contract v3 (protocol/extension task_contract.go): a task's declared
// outputs, its effects beyond them, and the source-mutation flag. Re-exported
// so the planner and the store name one set of types. Recognition only — the
// CLI still infers capture for every task, v2 and v3 alike, until declared
// capture lands.
type TaskDeclaration = proto.TaskDeclaration
type DeclaredOutput = proto.DeclaredOutput
type OutputRef = proto.OutputRef
type TaskCachePolicy = proto.TaskCachePolicy
type TaskCacheKey = proto.TaskCacheKey
type TaskOutputArtifact = proto.TaskOutputArtifact
type FlagDefinition = proto.FlagDefinition
type InputBinding = proto.InputBinding
type OutputBinding = proto.OutputBinding
type StepCacheOverride = proto.StepCacheOverride
type ContractsDefinition = proto.ContractsDefinition
type ManifestHooks = proto.ManifestHooks
type HookDefinition = proto.HookDefinition
type HookCacheConfig = proto.HookCacheConfig
type RuntimeDefinition = proto.RuntimeDefinition
type RuntimePrepare = proto.RuntimePrepare

// WorkspaceAdapter is the manifest's `workspace` section: the markers that make
// a directory a project this extension owns, the metadata inputs whose content
// decides its probe answer, the directories its scan skips, and the task that
// performs its own workspace mutations.
type WorkspaceAdapter = proto.WorkspaceAdapter

// Resource scope values for ResourceRef.Scope.
const (
	ResourceScopeProject   = proto.ResourceScopeProject
	ResourceScopeWorkspace = proto.ResourceScopeWorkspace
)

// Declared-output kinds and roots, and the source-tree resource id a v3 task
// pairs with its mutatesSources flag.
const (
	OutputKindFile          = proto.OutputKindFile
	OutputKindDirectory     = proto.OutputKindDirectory
	OutputRootProject       = proto.OutputRootProject
	OutputRootWorkspace     = proto.OutputRootWorkspace
	OutputRootCommandOutput = proto.OutputRootCommandOutput
	ResourceIDSources       = proto.ResourceIDSources
)

// Declared-output drift policies and the diagnostic code the engine reports a
// difference under (protocol ADR 0004).
const (
	OutputDriftWarn           = proto.OutputDriftWarn
	OutputDriftFail           = proto.OutputDriftFail
	OutputDriftDiagnosticCode = proto.OutputDriftDiagnosticCode
)

// The closed task-effect vocabulary, so the planner names the same effects a
// manifest declares.
const (
	EffectWorkspaceFiles = proto.EffectWorkspaceFiles
	EffectToolchainCache = proto.EffectToolchainCache
	EffectNetwork        = proto.EffectNetwork
	EffectRegistry       = proto.EffectRegistry
	EffectCloud          = proto.EffectCloud
	EffectProcess        = proto.EffectProcess
)

// IsExternalTaskEffect reports whether an effect makes a task unreplayable from
// cache: restoring a stored result would skip it entirely.
var IsExternalTaskEffect = proto.IsExternalTaskEffect

// NormalizeOutputPath returns the canonical form of a declared output path, or
// an error explaining why the path cannot own a region of the tree.
var NormalizeOutputPath = proto.NormalizeOutputPath

// DeriveTaskCacheKey builds a TaskCacheKey from task input ports.
var DeriveTaskCacheKey = proto.DeriveTaskCacheKey

// ManifestProtocolVersion reports which task-contract version a loaded manifest
// exercises (v3 when any task carries a `declares` block, v2 otherwise).
var ManifestProtocolVersion = proto.ManifestProtocolVersion

// OutputsOverlap reports whether two declared outputs claim the same
// filesystem region — the "one owner per output" predicate. Callers that have
// resolved each root to an absolute directory get the exact cross-root answer.
var OutputsOverlap = proto.OutputsOverlap

// DecidableExcludes returns the subpaths a directory output cedes that an
// ownership comparison may honor: normalized, strictly inside the output's own
// path, and sorted. Undecidable entries are dropped, so a defective declaration
// never buys an ownership exemption.
var DecidableExcludes = proto.DecidableExcludes

// DecidablePreserves returns the output-relative subpaths a directory output
// leaves to the workspace: normalized and sorted. Undecidable entries are
// dropped, so the bytes stay captured.
var DecidablePreserves = proto.DecidablePreserves

// --- Resolved types (CLI-specific, used by the planner) ---

// ExtensionDescription is the resolved, in-memory representation of an extension.
type ExtensionDescription struct {
	Name    string
	Version string
	Path    string // absolute filesystem path
	RelPath string // workspace-relative path (e.g., "typescript/extension")

	// LocalSource identifies a mutable development source discovered from a
	// workspace project/ref or an explicit filesystem path. Package and
	// lock-pinned installed extensions deliberately leave it false: their
	// resolved version remains their cache identity.
	LocalSource bool

	// PinnedOver is the workspace-relative path of the project whose manifest
	// of the same name this pinned build replaces, or "" when it replaces none.
	// Impact selection keeps that project the extension's source for the
	// projects that name its path.
	PinnedOver string

	Commands          map[string]string                 // commandName → description
	CommandVisibility map[string]string                 // commandName → "public" or "internal"; empty means public
	CommandGroups     map[string]CommandGroupDefinition // groupName → group definition (for "putnami <group> <subcommand>")
	Jobs              map[string]*JobDefinition         // jobName → definition
	ExtDeps           Dependencies
	Hooks             *ManifestHooks
	AutoServe         bool
	Tasks             map[string]TaskDefinition
	Contracts         *ContractsDefinition
	Tools             map[string]ToolDefinition
	Runtime           *RuntimeDefinition
	// Workspace is the extension's workspace adapter, when it declares one. Nil
	// means the extension contributes nothing to project discovery and is never
	// probed — which is what keeps the probe's cost proportional to the number
	// of extensions that actually own project metadata.
	Workspace *WorkspaceAdapter
	// RuntimeExecutable is populated by the engine's extension-runtime
	// synchronization phase. Job execution only consumes this resolved path; it
	// never prepares or validates a runtime lazily under a task timeout.
	RuntimeExecutable string
	// RuntimeDigest is the content identity computed by synchronization for a
	// mutable local runtime. Cache keys consume this same identity instead of
	// maintaining a second direct-exec hash lifecycle in the planner.
	RuntimeDigest string
	// RuntimeToolchains are resolved before cache-key computation and job
	// execution. The map is keyed by the runtime declaration's local alias.
	RuntimeToolchains map[string]RuntimeToolchainResolution
	// Ecosystems are the ecosystem profiles this extension owns, and Uses the
	// ids it publishes to without owning. Together they are where the CLI
	// learns which ecosystems exist: it names none of them itself, so adding a
	// language is a manifest and a publish job rather than a CLI change.
	Ecosystems []proto.EcosystemProfile
	Uses       []string
}

// RuntimeToolchainResolution is the provider-neutral result of one exact
// workspace-lock resolution. Host paths affect the child environment while
// Identity remains portable and is the only value folded into cache keys.
//
// Executable is the symlink-resolved absolute path that answered the probe. It
// is what lets a child environment be built minimally: the CLI knows both the
// directory to put first on PATH and the exact executable name no other
// directory on that PATH may still offer.
type RuntimeToolchainResolution struct {
	Available   bool
	Executable  string
	Environment map[string]string
	Identity    string
}

// JobDefinition is the resolved definition of a schedulable job.
type JobDefinition struct {
	ExtensionName    string
	ExtensionPath    string // workspace-relative path of the extension project, or of the project a pinned build replaces (e.g., "typescript/extension")
	Name             string // display name (e.g., build or build~transpile)
	InternalName     string // scheduler/DAG name when namespacing is required
	CommandName      string // base command name for expanded/namespaced jobs
	StepID           string // pipeline step id for expanded steps
	Visibility       string // "public" or "internal"; empty means public for older manifests
	Kind             string
	Command          string
	Args             []string
	Cwd              string
	Env              map[string]string
	Toolchains       []string
	TimeoutMs        int
	DependsOn        []string
	CommandDependsOn []string // command-level deps (e.g., ["build"] same-project, ["!build"] session barrier)
	// SessionPrerequisites are dependent-command-owned, same-session command
	// invocations with optional selection, policy, gates, and local params.
	SessionPrerequisites []SessionPrerequisiteDefinition
	// AlsoRuns widens a request for this command to also plan the named
	// companion commands, each with its own activation (request-level
	// expansion, unlike CommandDependsOn and SessionPrerequisites which hang
	// off planned jobs).
	AlsoRuns     []string
	Defaults     map[string]string
	FilePatterns []string
	Writes       []ResourceRef // resources written; conflicting accesses are serialized
	Reads        []ResourceRef // resources read; serialized against conflicting writers
	// Resources are the task's named budget claims (manifest `resources`): units
	// of a scarce shared resource one execution holds while it runs. They gate
	// ADMISSION against the run's `--resource <name>=<units>` budgets and never
	// serialize anything, which is what separates them from Writes/Reads above.
	Resources       map[string]int
	Cache           bool
	Priority        int
	Quiet           bool
	ActivationFiles []string
	Activation      string
	Channel         string
	Flags           map[string]FlagDefinition
	PipelineSteps   []PipelineStep
	PipelineOutputs map[string]OutputBinding
	TaskCachePolicy *TaskCachePolicy
	Batchable       *TaskBatchPolicy
	Traits          CommandTraits // resolved orchestration traits (verb defaults or manifest override)
	Heavy           *bool         // per-step heavy override from the pipeline step
	CPUWeight       *float64      // per-step relative CPU-demand multiplier
	// BoundParams are literal `with: {value: ...}` bindings materialized for
	// this expanded pipeline step. They are task-local inputs, not command
	// defaults: execution overlays them after every shared command/config layer
	// so one step's binding cannot leak to, or be overridden by, a sibling.
	BoundParams map[string]any
	// ContractDigest is the canonical task-contract digest
	// (proto.TaskContractDigest) of the manifest task this job executes,
	// stamped at discovery and folded into cache key v5. Empty when the job has no manifest task.
	ContractDigest string
}
