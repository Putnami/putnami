// Package extension acquires extensions — discovery, install, integrity,
// registry and lockfile resolution — and re-exports the pure extension spec
// that go.putnami.dev/cli/model/extension owns.
//
// This file is that re-export layer. Every symbol below is declared in the
// model package and named here under its exact original name, so acquisition
// code and every CLI consumer keep a single spelling for one concept. It is
// the same shape internal/extension already used to re-export
// go.putnami.dev/protocol/extension.
package extension

import (
	model "go.putnami.dev/cli/model/extension"
)

// ManifestFilename is the extension manifest file name, owned by the protocol.
const ManifestFilename = model.ManifestFilename

// --- Manifest type aliases (re-exported from the protocol via the model) ---

type Manifest = model.Manifest
type Dependencies = model.Dependencies
type CommandDefinition = model.CommandDefinition
type SessionPrerequisiteDefinition = model.SessionPrerequisiteDefinition
type SessionPrerequisiteParamBinding = model.SessionPrerequisiteParamBinding
type CommandTraits = model.CommandTraits
type ToolDefinition = model.ToolDefinition
type ToolAnnotations = model.ToolAnnotations

// Re-exported trait constants from the extension protocol.
const (
	SideEffectsRegistry = model.SideEffectsRegistry
	SideEffectsCloud    = model.SideEffectsCloud
)

// Re-exported task input sources. `task` is the typed producer/consumer edge
// the planner enforces (jobs/plan_producers.go); the rest are cache-key
// contributions.
const (
	TaskInputFromProject   = model.TaskInputFromProject
	TaskInputFromWorkspace = model.TaskInputFromWorkspace
	TaskInputFromTask      = model.TaskInputFromTask
	TaskInputFromParams    = model.TaskInputFromParams
	TaskInputFromEnv       = model.TaskInputFromEnv
	TaskInputFromRuntime   = model.TaskInputFromRuntime
)

type CommandGroupDefinition = model.CommandGroupDefinition
type SubcommandDefinition = model.SubcommandDefinition
type PositionalDefinition = model.PositionalDefinition
type PipelineStep = model.PipelineStep
type StepActivation = model.StepActivation
type TaskDefinition = model.TaskDefinition

type TaskBatchPolicy = model.TaskBatchPolicy
type TaskInputPort = model.TaskInputPort
type TaskOutputPort = model.TaskOutputPort
type ResourceRef = model.ResourceRef

// Task contract v3 (protocol/extension task_contract.go): a task's declared
// outputs, its effects beyond them, and the source-mutation flag.
type TaskDeclaration = model.TaskDeclaration
type DeclaredOutput = model.DeclaredOutput
type OutputRef = model.OutputRef
type TaskCachePolicy = model.TaskCachePolicy
type TaskCacheKey = model.TaskCacheKey
type TaskOutputArtifact = model.TaskOutputArtifact
type FlagDefinition = model.FlagDefinition
type InputBinding = model.InputBinding
type OutputBinding = model.OutputBinding
type StepCacheOverride = model.StepCacheOverride
type ContractsDefinition = model.ContractsDefinition
type ManifestHooks = model.ManifestHooks
type HookDefinition = model.HookDefinition
type HookCacheConfig = model.HookCacheConfig
type RuntimeDefinition = model.RuntimeDefinition
type RuntimePrepare = model.RuntimePrepare

// WorkspaceAdapter is the manifest's `workspace` section.
type WorkspaceAdapter = model.WorkspaceAdapter

// Resource scope values for ResourceRef.Scope.
const (
	ResourceScopeProject   = model.ResourceScopeProject
	ResourceScopeWorkspace = model.ResourceScopeWorkspace
)

// Declared-output kinds and roots, and the source-tree resource id a v3 task
// pairs with its mutatesSources flag.
const (
	OutputKindFile          = model.OutputKindFile
	OutputKindDirectory     = model.OutputKindDirectory
	OutputRootProject       = model.OutputRootProject
	OutputRootWorkspace     = model.OutputRootWorkspace
	OutputRootCommandOutput = model.OutputRootCommandOutput
	ResourceIDSources       = model.ResourceIDSources
)

// Declared-output drift policies and the diagnostic code the engine reports a
// difference under (protocol ADR 0004).
const (
	OutputDriftWarn           = model.OutputDriftWarn
	OutputDriftFail           = model.OutputDriftFail
	OutputDriftDiagnosticCode = model.OutputDriftDiagnosticCode
)

// The closed task-effect vocabulary.
const (
	EffectWorkspaceFiles = model.EffectWorkspaceFiles
	EffectToolchainCache = model.EffectToolchainCache
	EffectNetwork        = model.EffectNetwork
	EffectRegistry       = model.EffectRegistry
	EffectCloud          = model.EffectCloud
	EffectProcess        = model.EffectProcess
)

// IsExternalTaskEffect reports whether an effect makes a task unreplayable from
// cache: restoring a stored result would skip it entirely.
var IsExternalTaskEffect = model.IsExternalTaskEffect

// NormalizeOutputPath returns the canonical form of a declared output path, or
// an error explaining why the path cannot own a region of the tree.
var NormalizeOutputPath = model.NormalizeOutputPath

// DeriveTaskCacheKey builds a TaskCacheKey from task input ports.
var DeriveTaskCacheKey = model.DeriveTaskCacheKey

// ManifestProtocolVersion reports which task-contract version a loaded manifest
// exercises (v3 when any task carries a `declares` block, v2 otherwise).
var ManifestProtocolVersion = model.ManifestProtocolVersion

// OutputsOverlap reports whether two declared outputs claim the same
// filesystem region — the "one owner per output" predicate.
var OutputsOverlap = model.OutputsOverlap

// DecidableExcludes returns the subpaths a directory output cedes that an
// ownership comparison may honor: normalized, strictly inside the output's own
// path, and sorted.
var DecidableExcludes = model.DecidableExcludes

// DecidablePreserves returns the output-relative subpaths a directory output
// leaves to the workspace (protocol ADR 0005): normalized and sorted.
var DecidablePreserves = model.DecidablePreserves

// --- Resolved types (the planner's in-memory extension) ---

// ExtensionDescription is the resolved, in-memory representation of an extension.
type ExtensionDescription = model.ExtensionDescription

// JobDefinition is the resolved definition of a schedulable job.
type JobDefinition = model.JobDefinition

// SkippedExtension records an extension that discovery found but could not
// load. Discovery produces it; the provider gates and the release-archives
// guard read it.
type SkippedExtension = model.SkippedExtension

// BuildJobMap creates a mapping from job name → list of job definitions.
var BuildJobMap = model.BuildJobMap

// ExpandAlsoRunCommands widens a requested command list with every `alsoRuns`
// companion a matching job definition declares (request-level, before any
// activation is evaluated).
var ExpandAlsoRunCommands = model.ExpandAlsoRunCommands

// FindExtensionByName returns the extension with the given name, or nil.
var FindExtensionByName = model.FindExtensionByName

// --- Manifest loading and resolution ---

// LoadManifest reads and parses a putnami.extension.json file, negotiating the
// manifest's declared cliContract against this CLI's contract version.
var LoadManifest = model.LoadManifest

// Resolve converts a raw manifest into an ExtensionDescription ready for use by
// the planner.
var Resolve = model.Resolve

// ExpandTemplateVars replaces template variables in a string.
var ExpandTemplateVars = model.ExpandTemplateVars

// BuildTemplateVars builds the standard template variable map.
var BuildTemplateVars = model.BuildTemplateVars

// --- Expressions ---

// ParamMap is the untyped parameter bag expressions resolve against.
type ParamMap = model.ParamMap

// WhenContext is the evaluation context for a step's `if` expression.
type WhenContext = model.WhenContext

// StepResult is a completed step's expression-visible outcome.
type StepResult = model.StepResult

// EvaluateExpression reports whether an expression holds in the given context.
var EvaluateExpression = model.EvaluateExpression

// EvaluateExpressionChecked evaluates an expression and reports whether every
// root it referenced was resolvable.
var EvaluateExpressionChecked = model.EvaluateExpressionChecked

// ValidateExpressionSyntax checks the manifest expression grammar without
// requiring plan-time values for otherwise valid paths.
var ValidateExpressionSyntax = model.ValidateExpressionSyntax

// --- Flags ---

// MergeCommandFlags merges the flag surfaces of every job serving a command.
var MergeCommandFlags = model.MergeCommandFlags

// FlagDefsCompatible reports whether two definitions of the same flag agree.
var FlagDefsCompatible = model.FlagDefsCompatible

// CollectCommandFlags gathers the union of the jobs' flag definitions.
var CollectCommandFlags = model.CollectCommandFlags

// MergeFlagLayers merges flag layers in increasing precedence.
var MergeFlagLayers = model.MergeFlagLayers

// ValidateCommandFlags reports the first incompatible flag redefinition.
var ValidateCommandFlags = model.ValidateCommandFlags

// --- Pipelines ---

// StepSeparator is used in step job names: build~transpile.
const StepSeparator = model.StepSeparator

// StepJobName builds a composite name: {commandName}~{stepId}.
var StepJobName = model.StepJobName

// NamespaceForExtension returns the stable namespace used in internal job names.
var NamespaceForExtension = model.NamespaceForExtension

// NamespacedCommandJobName builds an internal name for a non-pipeline command.
var NamespacedCommandJobName = model.NamespacedCommandJobName

// NamespacedStepJobName builds an internal name for a pipeline step.
var NamespacedStepJobName = model.NamespacedStepJobName

// IsExternalRef returns true if the dependency reference is cross-project.
var IsExternalRef = model.IsExternalRef

// ExpandedStep is the result of expanding a pipeline step into a job definition.
type ExpandedStep = model.ExpandedStep

// PipelineExpansionOptions controls planner-only details of pipeline expansion.
type PipelineExpansionOptions = model.PipelineExpansionOptions

// ExpandPipeline expands a command's pipeline steps into job definitions.
var ExpandPipeline = model.ExpandPipeline

// ExpandPipelineWithOptions expands a command pipeline with planner-specific
// options, such as internal namespacing for composable commands.
var ExpandPipelineWithOptions = model.ExpandPipelineWithOptions

// --- Contract validation ---

// ContractError describes a single contract validation failure.
type ContractError = model.ContractError

// ValidateContracts validates task input/output port wiring across all
// extension commands.
var ValidateContracts = model.ValidateContracts

// --- Reserved provider resolution ---

// ErrProviderAmbiguous is the sentinel for two loaded extensions both declaring
// the same reserved provider command.
var ErrProviderAmbiguous = model.ErrProviderAmbiguous

// ResolvedProvider identifies the extension that serves a reserved provider
// command.
type ResolvedProvider = model.ResolvedProvider

// ResolveReservedProvider finds the single loaded extension declaring command.
var ResolveReservedProvider = model.ResolveReservedProvider

// SkippedProviderCause returns a one-line explanation when a provider is absent
// only because discovery could not LOAD an extension.
var SkippedProviderCause = model.SkippedProviderCause
