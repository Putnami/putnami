package workspace

// This file re-exports the pure workspace data model that
// go.putnami.dev/cli/model/workspace owns. Every symbol below is declared in
// the model package and named here under its exact original name, so the
// loading, probing, syncing and snapshotting code that stays in this package —
// and every CLI consumer of it — keeps a single spelling for one concept. It is
// the same shape internal/extension uses for the extension model.

import (
	model "go.putnami.dev/cli/model/workspace"
)

// --- Workspace and project ---

// Workspace represents a discovered workspace.
type Workspace = model.Workspace

// WarningCode is a stable machine-readable workspace warning reason.
type WarningCode = model.WarningCode

// WarningCodeProviderViewUnavailable marks an incomplete provider-derived
// project identity and dependency view.
const WarningCodeProviderViewUnavailable = model.WarningCodeProviderViewUnavailable

// Project represents a discovered project within a workspace.
type Project = model.Project

// ScopeContribution is what the breadcrumb chain contributes to a project.
type ScopeContribution = model.ScopeContribution

// GeneratedClientBinding is a generated client target's end of a contract edge.
type GeneratedClientBinding = model.GeneratedClientBinding

// VisibilityViolation is one import edge a project's declared boundary refuses.
type VisibilityViolation = model.VisibilityViolation

// VisibilityViolations returns every import edge of the resolved graph that
// crosses a scope boundary the imported project did not open.
var VisibilityViolations = model.VisibilityViolations

// ScopeKeyOf is the identity of the scope a project belongs to.
var ScopeKeyOf = model.ScopeKeyOf

// ScopeLabel renders a scope key for a human.
var ScopeLabel = model.ScopeLabel

// NewWorkspace assembles a Workspace from already-discovered projects.
var NewWorkspace = model.NewWorkspace

// CheckDuplicateNames returns an error when two or more projects resolve to the
// same name.
var CheckDuplicateNames = model.CheckDuplicateNames

// CanonicalRoot resolves a workspace root to its absolute, symlink-free form.
var CanonicalRoot = model.CanonicalRoot

// CleanWorkspacePath normalizes a workspace-relative path.
var CleanWorkspacePath = model.CleanWorkspacePath

// --- Dependency graph ---

// DependencyGraph represents the project dependency graph.
type DependencyGraph = model.DependencyGraph

// BuildGraph constructs a dependency graph from discovered projects.
var BuildGraph = model.BuildGraph

// --- Identity ---

// ProjectIDFromPath derives a project's canonical logical ID from its physical
// workspace-relative path.
var ProjectIDFromPath = model.ProjectIDFromPath

// ProbeViewOf projects a discovered project onto the probe protocol's project
// shape.
var ProbeViewOf = model.ProbeViewOf

// ProjectMetadataDigest is the per-project identity that enters every cache key
// affected by project metadata or dependency edges.
var ProjectMetadataDigest = model.ProjectMetadataDigest

// ProbePathOf translates the CLI's workspace-relative path spelling into the
// probe protocol's.
var ProbePathOf = model.ProbePathOf

// --- Membership (includes) ---

// ScopePaths returns configured autonomous scope entries.
var ScopePaths = model.ScopePaths

// RootProjectPaths returns direct project entries declared at workspace level.
var RootProjectPaths = model.RootProjectPaths

// --- Probe view application ---

// NameDivergences reports every project whose SOURCE identity disagrees with
// its resolved name.
var NameDivergences = model.NameDivergences

// ResolveProjectIdentity is the merge rule (explicit config > provider source
// identity > scope namePattern > directory basename), as one function.
var ResolveProjectIdentity = model.ResolveProjectIdentity

// --- Selection ---

// FilterOptions configures project selection.
type FilterOptions = model.FilterOptions

// FilterProjects selects projects based on filter options.
var FilterProjects = model.FilterProjects

// ResolveTarget resolves a target expression into a set of projects.
var ResolveTarget = model.ResolveTarget

// --- Change impact ---

// ChangeImpactOptions widens the shared change→project mapping.
type ChangeImpactOptions = model.ChangeImpactOptions

// ImpactTrace explains one change→project mapping: a seed per directly claimed
// project, the first edge into every propagated one.
type ImpactTrace = model.ImpactTrace

// ImpactSeed is one changed file's claim on a project.
type ImpactSeed = model.ImpactSeed

// ImpactEdge is the edge the impact walk first crossed into a project.
type ImpactEdge = model.ImpactEdge

// ImpactStep is one hop of a path through the impact union.
type ImpactStep = model.ImpactStep

// ImpactEdgeKind names the relation that put a project into the impacted set.
type ImpactEdgeKind = model.ImpactEdgeKind

// Re-exported impact seed and edge kinds.
const (
	ImpactSeedPathOwner         = model.ImpactSeedPathOwner
	ImpactSeedRootWatchedFile   = model.ImpactSeedRootWatchedFile
	ImpactSeedDeclaredInput     = model.ImpactSeedDeclaredInput
	ImpactSeedScopeConfig       = model.ImpactSeedScopeConfig
	ImpactSeedWorkspaceInput    = model.ImpactSeedWorkspaceInput
	ImpactEdgeDependency        = model.ImpactEdgeDependency
	ImpactEdgeExtensionConsumer = model.ImpactEdgeExtensionConsumer
	ImpactEdgeContract          = model.ImpactEdgeContract
)

// ProjectsForChangedFiles maps changed workspace-relative paths onto every
// project that must rebuild.
var ProjectsForChangedFiles = model.ProjectsForChangedFiles

// ImpactPath is the shortest path between two projects over the same edge union
// impact widening walks, with each hop's kind.
var ImpactPath = model.ImpactPath

// ImpactReach is the closure a change to one project reaches, with the scope
// of every project in it — what `why_impacted` answers from.
var ImpactReach = model.ImpactReach

// ImpactReachWithTasks is ImpactReach resolved onto the tasks a change to the
// project reaches.
var ImpactReachWithTasks = model.ImpactReachWithTasks

// TaskImpactIndex answers which tasks of a project read a path, and which tasks
// of a dependent one task reaches.
type TaskImpactIndex = model.TaskImpactIndex

// ChangeImpact is ProjectsForChangedFiles plus the unowned workspace-root paths
// the mapping reached nobody with.
var ChangeImpact = model.ChangeImpact

// ProjectOwnersForPath returns the projects that own a workspace-relative path:
// the nearest project whose directory holds the path, plus every project that
// claims it as an asset.
var ProjectOwnersForPath = model.ProjectOwnersForPath

// CollectProjectAssetPaths resolves cross-project asset source paths.
var CollectProjectAssetPaths = model.CollectProjectAssetPaths

// --- Auto selection ---

// AutoSelectionMode describes the project selector chosen for a bare job run.
type AutoSelectionMode = model.AutoSelectionMode

// AutoSelectionReason explains why a bare job run chose its mode.
type AutoSelectionReason = model.AutoSelectionReason

// LastBuildLookup returns the last fully successful build SHA for a branch and
// command set.
type LastBuildLookup = model.LastBuildLookup

// AutoSelection is the resolved project selection for a bare job command.
type AutoSelection = model.AutoSelection

// Re-exported auto-selection modes and reasons.
const (
	AutoSelectionAll      = model.AutoSelectionAll
	AutoSelectionImpacted = model.AutoSelectionImpacted

	AutoSelectionReasonFirstBuild = model.AutoSelectionReasonFirstBuild
	AutoSelectionReasonLastBuild  = model.AutoSelectionReasonLastBuild
	AutoSelectionReasonTrunk      = model.AutoSelectionReasonTrunk

	AutoSelectionReasonNoRepository = model.AutoSelectionReasonNoRepository
)

// --- Lowercase spellings kept for this package's own call sites ---
//
// These moved with the model and are still used by the loading, discovery,
// syncing and snapshotting halves that stay here. Aliasing them under their
// original lowercase names keeps those call sites untouched.

var (
	canonicalRoot          = model.CanonicalRoot
	cleanWorkspacePath     = model.CleanWorkspacePath
	probePathOf            = model.ProbePathOf
	checkDuplicateNames    = model.CheckDuplicateNames
	resolveProjectIdentity = model.ResolveProjectIdentity
	traceChangeImpact      = model.TraceChangeImpact
)
