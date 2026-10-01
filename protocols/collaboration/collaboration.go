// Package collaboration defines the collaboration provider contracts: three
// independently bound, independently versioned operation contracts — tasks,
// change proposals and memory — that a workspace routes to the provider
// extensions it chooses.
//
// A contract is data, not code. The catalog below names every operation of
// every contract version, whether a provider must implement it, whether it
// reads or mutates, and which request and result documents it exchanges. The
// orchestrator routes a call to the provider the workspace bound, validates the
// request before the provider sees it and the result before the caller sees it,
// and answers every call with one Envelope whose Outcome is a closed
// vocabulary. A provider extension owns every backend call, credential,
// identifier and mapping behind an operation.
//
// The wire transport is the extension tool transport of
// go.putnami.dev/protocol/extension: a provider implements an operation as a
// manifest-declared tool marked with ProviderMetaKey, reads one
// ToolCallRequest and writes one ToolCallResult whose single text block is a
// Response document.
package collaboration

import (
	"slices"
	"sort"
)

// Contract names. Each names a contract that is bound, versioned and
// discovered independently of the other two.
const (
	// ContractTasks finds, reads, creates, updates and transitions work items.
	ContractTasks = "tasks"
	// ContractProposals finds or creates the change proposal for an exact
	// repository/base/head, reads its status and checks, and publishes reviews.
	ContractProposals = "proposals"
	// ContractMemory loads contextual records, reads a mission and saves a
	// checkpoint against an expected revision.
	ContractMemory = "memory"
)

// ContractNames is the closed contract vocabulary in canonical (sorted) order.
var ContractNames = []string{ContractMemory, ContractProposals, ContractTasks}

// IsContract reports whether name is a member of the contract vocabulary.
func IsContract(name string) bool {
	return slices.Contains(ContractNames, name)
}

// Operation names. A name is meaningful only inside one contract version: the
// catalog decides which contract declares it.
const (
	// OperationCapabilities is answered by the orchestrator itself, never by a
	// provider: it reports the binding, the provider and every operation's
	// support. Every contract reserves the name.
	OperationCapabilities = "capabilities"

	OperationFind       = "find"
	OperationGet        = "get"
	OperationCreate     = "create"
	OperationUpdate     = "update"
	OperationTransition = "transition"
	OperationAssign     = "assign"
	OperationLink       = "link"
	OperationClaim      = "claim"

	OperationUpsert = "upsert"
	OperationStatus = "status"
	OperationReview = "review"
	OperationMerge  = "merge"

	OperationContext    = "context"
	OperationMission    = "mission"
	OperationCheckpoint = "checkpoint"
	OperationSearch     = "search"
)

// Access classifies an operation as a read or a mutation. It is the contract's
// statement, and a provider's tool annotations must agree with it.
type Access string

// Access values.
const (
	AccessRead     Access = "read"
	AccessMutating Access = "mutating"
)

// Preconditions states how a provider enforces an expectedRevision (or an
// exclusive claim) on a mutating operation.
type Preconditions string

// Precondition enforcement levels.
const (
	// PreconditionsAtomic compares and writes in one step the backend
	// guarantees: a concurrent writer cannot interleave.
	PreconditionsAtomic Preconditions = "atomic"
	// PreconditionsChecked compares, then writes: the provider refuses a stale
	// revision it observes, but a concurrent writer can interleave between the
	// two steps. It never promises atomicity.
	PreconditionsChecked Preconditions = "checked"
	// PreconditionsNone cannot compare revisions. The orchestrator refuses a
	// request carrying expectedRevision as unsupported instead of dropping the
	// precondition.
	PreconditionsNone Preconditions = "none"
)

// ValidPreconditions is the closed enforcement vocabulary in canonical order.
var ValidPreconditions = []Preconditions{PreconditionsAtomic, PreconditionsChecked, PreconditionsNone}

// PreconditionRule states what an operation requires of its provider's
// precondition declaration.
type PreconditionRule int

// Precondition rules.
const (
	// PreconditionRuleNone: the operation takes no revision precondition and a
	// provider must not declare one.
	PreconditionRuleNone PreconditionRule = iota
	// PreconditionRuleDeclared: the operation accepts an optional
	// expectedRevision and the provider must declare how it enforces it.
	PreconditionRuleDeclared
	// PreconditionRuleAtomic: the operation is meaningless without an atomic
	// comparison, and the provider must declare PreconditionsAtomic.
	PreconditionRuleAtomic
)

// OperationSpec is one operation of one contract version.
type OperationSpec struct {
	// Name is the operation name, unique inside its contract.
	Name string
	// Access is whether the operation reads or mutates.
	Access Access
	// Required operations must be implemented by every provider of the
	// contract version; the others are optional capabilities a provider
	// declares or omits.
	Required bool
	// Destructive operations may modify or remove existing state, so a
	// provider must annotate them destructiveHint: true. A mutating operation
	// that is not destructive only adds state.
	Destructive bool
	// Preconditions states what the provider must declare about revision
	// enforcement.
	Preconditions PreconditionRule
	// WorkspaceSelection operations take the canonical projects, impacted and
	// baseline arguments. The orchestrator resolves them through its own
	// selection projection, so the provider tool must declare
	// workspaceSelection and never resolve a selector itself.
	WorkspaceSelection bool
	// Summary is the provider-neutral, agent-facing description.
	Summary string
	// newInput and newResult allocate the request and result documents.
	newInput  func() any
	newResult func() any
}

// NewInput allocates a zero request document of this operation.
func (o OperationSpec) NewInput() any { return o.newInput() }

// NewResult allocates a zero result document of this operation.
func (o OperationSpec) NewResult() any { return o.newResult() }

// ContractSpec is one version of one contract.
type ContractSpec struct {
	// Name is the contract name.
	Name string
	// Version is the contract's integer major version. A version never changes
	// meaning once published: additions that an older reader could misread
	// need a new version.
	Version int
	// Operations lists the contract's operations in canonical (sorted) order.
	// OperationCapabilities is not listed: the orchestrator answers it.
	Operations []OperationSpec
}

// Operation returns the named operation of this contract version.
func (c ContractSpec) Operation(name string) (OperationSpec, bool) {
	for _, op := range c.Operations {
		if op.Name == name {
			return op, true
		}
	}
	return OperationSpec{}, false
}

// RequiredOperations returns the names of the operations every provider of
// this contract version implements, in canonical order.
func (c ContractSpec) RequiredOperations() []string {
	var names []string
	for _, op := range c.Operations {
		if op.Required {
			names = append(names, op.Name)
		}
	}
	return names
}

// Lookup returns one contract version from the catalog.
func Lookup(contract string, version int) (ContractSpec, bool) {
	for _, spec := range catalog {
		if spec.Name == contract && spec.Version == version {
			return spec, true
		}
	}
	return ContractSpec{}, false
}

// SupportedVersions returns the versions of a contract this package
// implements, ascending. An unknown contract has none.
func SupportedVersions(contract string) []int {
	var versions []int
	for _, spec := range catalog {
		if spec.Name == contract {
			versions = append(versions, spec.Version)
		}
	}
	sort.Ints(versions)
	return versions
}

// Catalog returns every contract version this package implements, in
// canonical (name, version) order.
func Catalog() []ContractSpec {
	out := make([]ContractSpec, len(catalog))
	copy(out, catalog)
	return out
}

func alloc[T any]() func() any { return func() any { return new(T) } }

// catalog is the single definition of every operation. It is sorted by
// contract name and version, and each contract's operations by name.
var catalog = []ContractSpec{
	{
		Name:    ContractMemory,
		Version: 1,
		Operations: []OperationSpec{
			{
				Name: OperationCheckpoint, Access: AccessMutating, Required: true, Destructive: true,
				Preconditions: PreconditionRuleAtomic,
				Summary: "Save a compact mission checkpoint against an expected revision, or create it only if it does not exist. " +
					"Returns the new revision or a conflict carrying the current one; a retry with the same idempotencyKey " +
					"after a lost response replays instead of writing twice. Checkpoints reference gate and review records; " +
					"they are context, never evidence.",
				newInput: alloc[MemoryCheckpointInput](), newResult: alloc[MemoryCheckpointResult](),
			},
			{
				Name: OperationContext, Access: AccessRead, Required: true, WorkspaceSelection: true,
				Summary: "Load the contextual records relevant to a workspace, repository, scope or mission, with their " +
					"source references, provenance, revision and freshness. Narrow with projects or impacted; results " +
					"are bounded pages.",
				newInput: alloc[MemoryContextInput](), newResult: alloc[MemoryListResult](),
			},
			{
				Name: OperationMission, Access: AccessRead, Required: true,
				Summary:  "Read one mission record and its current revision.",
				newInput: alloc[MemoryMissionInput](), newResult: alloc[MemoryRecordResult](),
			},
			{
				Name: OperationSearch, Access: AccessRead, WorkspaceSelection: true,
				Summary:  "Search memory records by text, narrowed like context. Optional capability.",
				newInput: alloc[MemorySearchInput](), newResult: alloc[MemoryListResult](),
			},
		},
	},
	{
		Name:    ContractProposals,
		Version: 1,
		Operations: []OperationSpec{
			{
				Name: OperationFind, Access: AccessRead, Required: true,
				Summary:  "Find the change proposals for an exact repository, base and head. Results are bounded pages.",
				newInput: alloc[ProposalFindInput](), newResult: alloc[ProposalListResult](),
			},
			{
				Name: OperationMerge, Access: AccessMutating, Destructive: true,
				Summary: "Merge a proposal only when its head is still the expected commit. Optional capability; " +
					"binding a provider that supports it does not authorize anyone to merge.",
				newInput: alloc[ProposalMergeInput](), newResult: alloc[ProposalResult](),
			},
			{
				Name: OperationReview, Access: AccessMutating, Required: true,
				Summary: "Publish a review of a proposal at an exact head commit. A retry with the same idempotencyKey " +
					"returns the review already published instead of publishing a second one.",
				newInput: alloc[ProposalReviewInput](), newResult: alloc[ProposalReviewResult](),
			},
			{
				Name: OperationStatus, Access: AccessRead, Required: true,
				Summary: "Read a proposal's state, the checks reported for its head commit and its reviews. A provider " +
					"without hosted checks reports checks as unsupported.",
				newInput: alloc[ProposalRefInput](), newResult: alloc[ProposalStatusResult](),
			},
			{
				Name: OperationUpsert, Access: AccessMutating, Required: true, Destructive: true,
				Preconditions: PreconditionRuleDeclared,
				Summary: "Find the open proposal for an exact repository, base and head and update it, or create it. " +
					"Repeating the call never creates a second proposal for the same identity.",
				newInput: alloc[ProposalUpsertInput](), newResult: alloc[ProposalUpsertResult](),
			},
		},
	},
	{
		Name:    ContractTasks,
		Version: 1,
		Operations: []OperationSpec{
			{
				Name: OperationAssign, Access: AccessMutating, Destructive: true,
				Preconditions: PreconditionRuleDeclared,
				Summary: "Replace a task's assignees. Optional capability. An assignee is information, never an " +
					"exclusive lease.",
				newInput: alloc[TaskAssignInput](), newResult: alloc[TaskResult](),
			},
			{
				Name: OperationClaim, Access: AccessMutating, Destructive: true,
				Preconditions: PreconditionRuleAtomic,
				Summary: "Claim or release a task exclusively. Optional capability, offered only by a provider that " +
					"enforces exclusivity atomically; a task held by another holder is a conflict.",
				newInput: alloc[TaskClaimInput](), newResult: alloc[TaskClaimResult](),
			},
			{
				Name: OperationCreate, Access: AccessMutating, Required: true,
				Summary: "Create a task. A retry with the same idempotencyKey returns the task already created instead " +
					"of creating a second one.",
				newInput: alloc[TaskCreateInput](), newResult: alloc[TaskCreateResult](),
			},
			{
				Name: OperationFind, Access: AccessRead, Required: true,
				Summary:  "Find tasks by text, semantic state and labels. Results are bounded pages.",
				newInput: alloc[TaskFindInput](), newResult: alloc[TaskListResult](),
			},
			{
				Name: OperationGet, Access: AccessRead, Required: true,
				Summary:  "Read one task by its source-qualified reference.",
				newInput: alloc[TaskRefInput](), newResult: alloc[TaskResult](),
			},
			{
				Name: OperationLink, Access: AccessMutating, Destructive: true,
				Summary:  "Set or remove a task's parent. Optional capability.",
				newInput: alloc[TaskLinkInput](), newResult: alloc[TaskResult](),
			},
			{
				Name: OperationTransition, Access: AccessMutating, Required: true, Destructive: true,
				Preconditions: PreconditionRuleDeclared,
				Summary:       "Move a task to another semantic state, optionally against an expected revision.",
				newInput:      alloc[TaskTransitionInput](), newResult: alloc[TaskResult](),
			},
			{
				Name: OperationUpdate, Access: AccessMutating, Required: true, Destructive: true,
				Preconditions: PreconditionRuleDeclared,
				Summary:       "Change a task's title, body or labels, optionally against an expected revision.",
				newInput:      alloc[TaskUpdateInput](), newResult: alloc[TaskResult](),
			},
		},
	},
}
