package collaboration

// Ref is a source-qualified opaque reference. Source names the backend the
// identifier belongs to, as "<kind>:<locator>" ("github:acme/app",
// "local:3f9a0c1d"); ID is meaningful only to that source. A caller never
// parses either member: it stores the pair and passes it back. Two references
// are the same item exactly when both members are equal.
type Ref struct {
	// Source names the backend the identifier belongs to, as
	// "<kind>:<locator>" ("github:acme/app", "local:3f9a0c1d").
	Source string `json:"source"`
	// ID is the backend-specific identifier, meaningful only to Source.
	ID string `json:"id"`
}

// PageRequest asks for one bounded page. Size defaults to DefaultPageSize and
// never exceeds MaxPageSize; Cursor is the opaque Page.Next of the previous
// page, absent for the first one.
type PageRequest struct {
	// Size is the requested page size; 0 means DefaultPageSize.
	Size int `json:"size,omitempty"`
	// Cursor is the opaque Page.Next of the previous page, absent for the
	// first page of a traversal.
	Cursor string `json:"cursor,omitempty"`
}

// Page describes the page a list result carries. Next is absent on the last
// page. Items keep one stable, provider-defined order across the pages of one
// traversal.
type Page struct {
	// Next is the opaque cursor for the following page, absent on the last page.
	Next string `json:"next,omitempty"`
}

// Page bounds.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// TaskState is the provider-neutral state of a task. A provider maps its own
// statuses, labels or columns onto this vocabulary and may report its own
// label in Task.ProviderState for display.
type TaskState string

// Task states.
const (
	TaskStateOpen       TaskState = "open"
	TaskStateInProgress TaskState = "in_progress"
	TaskStateBlocked    TaskState = "blocked"
	TaskStateDone       TaskState = "done"
	TaskStateCanceled   TaskState = "canceled"
)

// ValidTaskStates is the closed task-state vocabulary in canonical order.
var ValidTaskStates = []TaskState{TaskStateBlocked, TaskStateCanceled, TaskStateDone, TaskStateInProgress, TaskStateOpen}

// Task is one work item.
type Task struct {
	// Ref is the task's source-qualified opaque reference.
	Ref Ref `json:"ref"`
	// Revision changes whenever the task changes; it is the token an
	// expectedRevision precondition compares. Parent is the one member a
	// provider may leave outside it: such a provider says so in the
	// description of its link operation, which capabilities reports, and an
	// expectedRevision then does not detect a concurrent link.
	Revision string `json:"revision"`
	// URL is a display link. It is never an identity.
	URL string `json:"url,omitempty"`
	// Title is the task's short summary.
	Title string `json:"title"`
	// Body is the task's full description.
	Body string `json:"body,omitempty"`
	// State is the provider-neutral state; see the TaskState* constants.
	State TaskState `json:"state"`
	// ProviderState is the provider's own status label, for display.
	ProviderState string `json:"providerState,omitempty"`
	// Labels are the task's provider-defined labels or tags.
	Labels []string `json:"labels,omitempty"`
	// Assignees are informational. An assignee is never an exclusive lease;
	// exclusivity is the optional claim operation's, where a provider enforces
	// it.
	Assignees []string `json:"assignees,omitempty"`
	// Parent is the task's containing item, when the provider supports
	// hierarchy.
	Parent *Ref `json:"parent,omitempty"`
	// Holder is the current exclusive claimant, reported only by a provider
	// that offers the claim operation.
	Holder string `json:"holder,omitempty"`
	// UpdatedAt is when the task last changed, in RFC 3339, when known.
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// TaskFindInput is the tasks.find request.
type TaskFindInput struct {
	// Query is text matched against title and body.
	Query string `json:"query,omitempty"`
	// States restricts the result to these states; empty means every state.
	States []TaskState `json:"states,omitempty"`
	// Labels restricts the result to tasks carrying every named label.
	Labels []string `json:"labels,omitempty"`
	// Page asks for one bounded page of the result.
	Page *PageRequest `json:"page,omitempty"`
}

// TaskListResult is the tasks.find result.
type TaskListResult struct {
	// Items are the matched tasks, in the provider's stable order.
	Items []Task `json:"items"`
	// Page describes the returned page.
	Page Page `json:"page"`
}

// TaskRefInput is the tasks.get request.
type TaskRefInput struct {
	// Ref identifies the task to read.
	Ref Ref `json:"ref"`
}

// TaskResult carries one task.
type TaskResult struct {
	// Task is the requested task.
	Task Task `json:"task"`
}

// TaskCreateInput is the tasks.create request. IdempotencyKey is required: a
// provider returns the task an earlier call with the same key created, and
// refuses the same key with different content as a conflict.
type TaskCreateInput struct {
	// Title is the new task's short summary.
	Title string `json:"title"`
	// Body is the new task's full description.
	Body string `json:"body,omitempty"`
	// Labels are the new task's provider-defined labels or tags.
	Labels []string `json:"labels,omitempty"`
	// State is the initial state; defaults to open.
	State TaskState `json:"state,omitempty"`
	// IdempotencyKey deduplicates a repeated create call.
	IdempotencyKey string `json:"idempotencyKey"`
}

// TaskCreateResult is the tasks.create result. Created is false when the
// idempotency key matched an earlier creation.
type TaskCreateResult struct {
	// Task is the created (or previously created) task.
	Task Task `json:"task"`
	// Created is false when the idempotency key matched an earlier creation.
	Created bool `json:"created"`
}

// TaskUpdateInput is the tasks.update request. An absent member is unchanged;
// labels, when present, replace the whole set.
type TaskUpdateInput struct {
	// Ref identifies the task to update.
	Ref Ref `json:"ref"`
	// ExpectedRevision is the task's current revision; a mismatch is a conflict.
	ExpectedRevision string `json:"expectedRevision,omitempty"`
	// Title, when present, replaces the task's title.
	Title *string `json:"title,omitempty"`
	// Body, when present, replaces the task's body.
	Body *string `json:"body,omitempty"`
	// Labels, when present, replaces the whole label set.
	Labels *[]string `json:"labels,omitempty"`
}

// TaskTransitionInput is the tasks.transition request.
type TaskTransitionInput struct {
	// Ref identifies the task to transition.
	Ref Ref `json:"ref"`
	// State is the task's new state.
	State TaskState `json:"state"`
	// ExpectedRevision is the task's current revision; a mismatch is a conflict.
	ExpectedRevision string `json:"expectedRevision,omitempty"`
	// Reason explains the transition, for display or audit.
	Reason string `json:"reason,omitempty"`
}

// TaskAssignInput is the tasks.assign request.
type TaskAssignInput struct {
	// Ref identifies the task to assign.
	Ref Ref `json:"ref"`
	// Assignees replaces the task's whole assignee set.
	Assignees []string `json:"assignees"`
	// ExpectedRevision is the task's current revision; a mismatch is a conflict.
	ExpectedRevision string `json:"expectedRevision,omitempty"`
}

// TaskLinkInput is the tasks.link request: exactly one of Parent (set) and
// Unlink (remove).
type TaskLinkInput struct {
	// Ref identifies the task to link.
	Ref Ref `json:"ref"`
	// Parent is the containing item to set, exclusive with Unlink.
	Parent *Ref `json:"parent,omitempty"`
	// Unlink removes the current parent, exclusive with Parent.
	Unlink bool `json:"unlink,omitempty"`
}

// TaskClaimInput is the tasks.claim request. Release gives up a claim Holder
// holds.
type TaskClaimInput struct {
	// Ref identifies the task to claim or release.
	Ref Ref `json:"ref"`
	// Holder is the caller claiming, or releasing, exclusivity.
	Holder string `json:"holder"`
	// Release gives up a claim Holder holds, instead of taking one.
	Release bool `json:"release,omitempty"`
}

// TaskClaimResult is the tasks.claim result.
type TaskClaimResult struct {
	// Task is the task after the claim attempt.
	Task Task `json:"task"`
	// Held is true when the caller now holds the claim.
	Held bool `json:"held"`
}

// Change is the exact identity a change proposal is associated with. On a
// request Repository may be omitted, and the provider then applies its
// configured repository; every proposal a provider returns names all three.
type Change struct {
	// Repository is the provider-scoped repository identity; omitted on a
	// request applies the provider's configured repository.
	Repository string `json:"repository,omitempty"`
	// Base is the branch or ref the change merges into.
	Base string `json:"base"`
	// Head is the branch or ref carrying the change.
	Head string `json:"head"`
	// HeadCommit is the full or abbreviated commit the head ref points at,
	// when known.
	HeadCommit string `json:"headCommit,omitempty"`
}

// ProposalState is the provider-neutral state of a change proposal.
type ProposalState string

// Proposal states.
const (
	ProposalStateDraft  ProposalState = "draft"
	ProposalStateOpen   ProposalState = "open"
	ProposalStateMerged ProposalState = "merged"
	ProposalStateClosed ProposalState = "closed"
)

// ValidProposalStates is the closed proposal-state vocabulary in canonical order.
var ValidProposalStates = []ProposalState{ProposalStateClosed, ProposalStateDraft, ProposalStateMerged, ProposalStateOpen}

// Proposal is one change proposal.
type Proposal struct {
	// Ref is the proposal's source-qualified opaque reference.
	Ref Ref `json:"ref"`
	// Revision changes whenever the proposal changes; it is the token an
	// expectedRevision precondition compares.
	Revision string `json:"revision"`
	// URL is a display link. It is never an identity.
	URL string `json:"url,omitempty"`
	// Change is the repository/base/head identity the proposal is associated
	// with.
	Change Change `json:"change"`
	// Title is the proposal's short summary.
	Title string `json:"title"`
	// Body is the proposal's full description.
	Body string `json:"body,omitempty"`
	// State is the provider-neutral state; see the ProposalState* constants.
	State ProposalState `json:"state"`
	// ProviderState is the provider's own status label, for display.
	ProviderState string `json:"providerState,omitempty"`
	// Labels are the proposal's provider-defined labels, including those a
	// provider applies from its own settings. Absent means the provider does
	// not report labels; an empty list means the proposal carries none. A
	// provider reports them on every answer about a proposal, or on none.
	Labels []string `json:"labels,omitzero"`
	// Assignees are the accounts assigned to the proposal, including one a
	// provider assigns from its own settings. Absent and empty differ as for
	// Labels. An assignee is information, never an exclusive lease.
	Assignees []string `json:"assignees,omitzero"`
	// UpdatedAt is when the proposal last changed, in RFC 3339, when known.
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// ProposalFindInput is the proposals.find request.
type ProposalFindInput struct {
	// Change identifies the exact repository/base/head to find proposals for.
	Change Change `json:"change"`
	// States restricts the result to these states; empty means every state.
	States []ProposalState `json:"states,omitempty"`
	// Page asks for one bounded page of the result.
	Page *PageRequest `json:"page,omitempty"`
}

// ProposalListResult is the proposals.find result.
type ProposalListResult struct {
	// Items are the matched proposals, in the provider's stable order.
	Items []Proposal `json:"items"`
	// Page describes the returned page.
	Page Page `json:"page"`
}

// ProposalUpsertInput is the proposals.upsert request.
type ProposalUpsertInput struct {
	// Change identifies the exact repository/base/head to create or update a
	// proposal for.
	Change Change `json:"change"`
	// Title is the proposal's short summary.
	Title string `json:"title"`
	// Body is the proposal's full description.
	Body string `json:"body,omitempty"`
	// Draft, when present, sets the proposal's draft state.
	Draft *bool `json:"draft,omitempty"`
	// ExpectedRevision is the proposal's current revision on an update; a
	// mismatch is a conflict. Absent on a create.
	ExpectedRevision string `json:"expectedRevision,omitempty"`
}

// ProposalUpsertResult is the proposals.upsert result.
type ProposalUpsertResult struct {
	// Proposal is the created or updated proposal.
	Proposal Proposal `json:"proposal"`
	// Created is true when this call created the proposal.
	Created bool `json:"created"`
}

// ProposalRefInput is the proposals.status request.
type ProposalRefInput struct {
	// Ref identifies the proposal to read.
	Ref Ref `json:"ref"`
}

// ChecksState summarizes the checks reported for a proposal's head commit.
type ChecksState string

// Check summary states.
const (
	ChecksStatePending ChecksState = "pending"
	ChecksStatePassing ChecksState = "passing"
	ChecksStateFailing ChecksState = "failing"
	// ChecksStateNone: the provider supports checks and none are reported.
	ChecksStateNone ChecksState = "none"
	// ChecksStateUnsupported: the provider has no hosted checks at all.
	ChecksStateUnsupported ChecksState = "unsupported"
)

// ValidChecksStates is the closed check-summary vocabulary in canonical order.
var ValidChecksStates = []ChecksState{ChecksStateFailing, ChecksStateNone, ChecksStatePassing, ChecksStatePending, ChecksStateUnsupported}

// CheckState is the state of one reported check.
type CheckState string

// Check states.
const (
	CheckStatePending CheckState = "pending"
	CheckStatePassing CheckState = "passing"
	CheckStateFailing CheckState = "failing"
	CheckStateSkipped CheckState = "skipped"
)

// ValidCheckStates is the closed check vocabulary in canonical order.
var ValidCheckStates = []CheckState{CheckStateFailing, CheckStatePassing, CheckStatePending, CheckStateSkipped}

// Checks is the check summary of one head commit.
type Checks struct {
	// State summarizes every reported check; see the ChecksState* constants.
	State ChecksState `json:"state"`
	// Commit is the head commit the checks ran on.
	Commit string `json:"commit,omitempty"`
	// Items are the individually reported checks.
	Items []Check `json:"items,omitempty"`
	// Detail explains the summary, notably why checks are unsupported.
	Detail string `json:"detail,omitempty"`
}

// Check is one reported check.
type Check struct {
	// Name is the check's display name.
	Name string `json:"name"`
	// State is the check's own state; see the CheckState* constants.
	State CheckState `json:"state"`
	// URL is a display link to the check's detail, when known.
	URL string `json:"url,omitempty"`
}

// ReviewVerdict is the verdict of a published review.
type ReviewVerdict string

// Review verdicts.
const (
	ReviewVerdictApprove        ReviewVerdict = "approve"
	ReviewVerdictComment        ReviewVerdict = "comment"
	ReviewVerdictRequestChanges ReviewVerdict = "request_changes"
)

// ValidReviewVerdicts is the closed verdict vocabulary in canonical order.
var ValidReviewVerdicts = []ReviewVerdict{ReviewVerdictApprove, ReviewVerdictComment, ReviewVerdictRequestChanges}

// Review is one published review.
type Review struct {
	// Ref is the review's source-qualified opaque reference.
	Ref Ref `json:"ref"`
	// URL is a display link. It is never an identity.
	URL string `json:"url,omitempty"`
	// Verdict is the review's outcome; see the ReviewVerdict* constants.
	Verdict ReviewVerdict `json:"verdict"`
	// Commit is the reviewed head commit, when known.
	Commit string `json:"commit,omitempty"`
	// Author identifies who published the review, when known.
	Author string `json:"author,omitempty"`
	// SubmittedAt is when the review was published, in RFC 3339, when known.
	SubmittedAt string `json:"submittedAt,omitempty"`
}

// ProposalStatusResult is the proposals.status result.
type ProposalStatusResult struct {
	// Proposal is the proposal whose status is reported.
	Proposal Proposal `json:"proposal"`
	// Checks summarizes the checks on the proposal's head commit.
	Checks Checks `json:"checks"`
	// Reviews are the proposal's published reviews, when the provider offers
	// review status.
	Reviews []Review `json:"reviews,omitempty"`
}

// ProposalReviewInput is the proposals.review request.
type ProposalReviewInput struct {
	// Ref identifies the proposal to review.
	Ref Ref `json:"ref"`
	// Verdict is the review's outcome; see the ReviewVerdict* constants.
	Verdict ReviewVerdict `json:"verdict"`
	// Body is the review's message.
	Body string `json:"body"`
	// Commit is the commit being reviewed, when required by the provider.
	Commit string `json:"commit,omitempty"`
	// IdempotencyKey deduplicates a repeated review call.
	IdempotencyKey string `json:"idempotencyKey"`
}

// ProposalReviewResult is the proposals.review result. Created is false when
// the idempotency key matched a review already published.
type ProposalReviewResult struct {
	// Review is the published (or previously published) review.
	Review Review `json:"review"`
	// Created is false when the idempotency key matched a review already
	// published.
	Created bool `json:"created"`
}

// MergeMethod names how a merge combines the proposal's commits.
type MergeMethod string

// Merge methods.
const (
	MergeMethodMerge  MergeMethod = "merge"
	MergeMethodSquash MergeMethod = "squash"
	MergeMethodRebase MergeMethod = "rebase"
)

// ValidMergeMethods is the closed merge-method vocabulary in canonical order.
var ValidMergeMethods = []MergeMethod{MergeMethodMerge, MergeMethodRebase, MergeMethodSquash}

// ProposalMergeInput is the proposals.merge request. ExpectedHeadCommit is
// required: a head that moved is a conflict, never a merge.
type ProposalMergeInput struct {
	// Ref identifies the proposal to merge.
	Ref Ref `json:"ref"`
	// ExpectedHeadCommit is the head commit the caller expects; a head that
	// moved is a conflict, never a merge.
	ExpectedHeadCommit string `json:"expectedHeadCommit"`
	// Method names how the merge combines the proposal's commits; defaults to
	// the provider's own default.
	Method MergeMethod `json:"method,omitempty"`
}

// ProposalResult carries one proposal.
type ProposalResult struct {
	// Proposal is the merged (or otherwise resulting) proposal.
	Proposal Proposal `json:"proposal"`
}

// MemoryIdentity locates a memory record: the workspace, repository, native
// scope and mission it belongs to. A request member that is absent does not
// narrow; the orchestrator fills Workspace from the workspace it serves.
type MemoryIdentity struct {
	// Workspace is the workspace the record belongs to; the orchestrator
	// fills it from the workspace it serves when absent.
	Workspace string `json:"workspace,omitempty"`
	// Repository is the repository the record belongs to, when relevant.
	Repository string `json:"repository,omitempty"`
	// Scope is the native scope the record belongs to, when relevant.
	Scope string `json:"scope,omitempty"`
	// Mission is the mission the record belongs to, when relevant.
	Mission string `json:"mission,omitempty"`
}

// MemoryKind classifies a memory record.
type MemoryKind string

// Memory record kinds.
const (
	// MemoryKindMission is a mission's checkpointed state.
	MemoryKindMission MemoryKind = "mission"
	// MemoryKindNote is contextual information attached to an identity.
	MemoryKindNote MemoryKind = "note"
)

// ValidMemoryKinds is the closed memory-kind vocabulary in canonical order.
var ValidMemoryKinds = []MemoryKind{MemoryKindMission, MemoryKindNote}

// EvidenceKind classifies a referenced evidence record.
type EvidenceKind string

// Evidence kinds.
const (
	EvidenceKindGate    EvidenceKind = "gate"
	EvidenceKindReview  EvidenceKind = "review"
	EvidenceKindQualify EvidenceKind = "qualify"
	EvidenceKindSession EvidenceKind = "session"
	EvidenceKindOther   EvidenceKind = "other"
)

// ValidEvidenceKinds is the closed evidence-kind vocabulary in canonical order.
var ValidEvidenceKinds = []EvidenceKind{EvidenceKindGate, EvidenceKindOther, EvidenceKindQualify, EvidenceKindReview, EvidenceKindSession}

// Evidence references a gate, review or qualification record by locator. It
// is a reference only: memory never carries a verdict, and a checkpoint that
// names evidence proves nothing the referenced record does not.
type Evidence struct {
	// Kind classifies the referenced record; see the EvidenceKind* constants.
	Kind EvidenceKind `json:"kind"`
	// Locator finds the referenced record (a path, URL or other pointer the
	// producer defines).
	Locator string `json:"locator"`
	// Digest is the referenced record's content digest, when known.
	Digest string `json:"digest,omitempty"`
}

// Provenance records who produced a memory record and when.
type Provenance struct {
	// RecordedAt is when the record was produced, in RFC 3339.
	RecordedAt string `json:"recordedAt"`
	// RecordedBy identifies who or what produced the record, when known.
	RecordedBy string `json:"recordedBy,omitempty"`
}

// Freshness records when a memory record last changed and when the provider
// read it.
type Freshness struct {
	// UpdatedAt is when the record last changed, in RFC 3339.
	UpdatedAt string `json:"updatedAt"`
	// RetrievedAt is when the provider read the record, in RFC 3339.
	RetrievedAt string `json:"retrievedAt"`
}

// MemoryRecord is one memory record.
type MemoryRecord struct {
	// Ref is the record's source-qualified opaque reference.
	Ref Ref `json:"ref"`
	// Revision changes whenever the record changes; it is the token an
	// expectedRevision precondition compares.
	Revision string `json:"revision"`
	// Kind classifies the record; see the MemoryKind* constants.
	Kind MemoryKind `json:"kind"`
	// Identity locates the record's workspace, repository, scope and mission.
	Identity MemoryIdentity `json:"identity"`
	// Title is the record's short summary.
	Title string `json:"title,omitempty"`
	// Content is the record's body.
	Content string `json:"content"`
	// Sources are the references the record's content is drawn from.
	Sources []Ref `json:"sources,omitempty"`
	// Evidence are the gate/review/qualify records this checkpoint references.
	Evidence []Evidence `json:"evidence,omitempty"`
	// Provenance records who produced the record and when.
	Provenance Provenance `json:"provenance"`
	// Freshness records when the record last changed and when it was read.
	Freshness Freshness `json:"freshness"`
}

// MemoryContextInput is the memory.context request. Projects, Impacted and
// Baseline are the canonical selection arguments; the orchestrator resolves
// them and hands the provider the resolved selection.
type MemoryContextInput struct {
	// Identity locates the workspace, repository, scope and mission to load
	// context for.
	Identity MemoryIdentity `json:"identity"`
	// Kinds restricts the result to these record kinds; empty means every kind.
	Kinds []MemoryKind `json:"kinds,omitempty"`
	// Projects is the resolved project selection to load context for.
	Projects []string `json:"projects,omitempty"`
	// Impacted resolves to the dependency-impacted project selection instead
	// of Projects alone.
	Impacted bool `json:"impacted,omitempty"`
	// Baseline is the resolved comparison revision the selection is computed
	// against.
	Baseline string `json:"baseline,omitempty"`
	// Page asks for one bounded page of the result.
	Page *PageRequest `json:"page,omitempty"`
}

// MemorySearchInput is the memory.search request.
type MemorySearchInput struct {
	// Query is text matched against a record's title and content.
	Query string `json:"query"`
	// Identity locates the workspace, repository, scope and mission to
	// search within.
	Identity MemoryIdentity `json:"identity"`
	// Kinds restricts the result to these record kinds; empty means every kind.
	Kinds []MemoryKind `json:"kinds,omitempty"`
	// Projects is the resolved project selection to search within.
	Projects []string `json:"projects,omitempty"`
	// Impacted resolves to the dependency-impacted project selection instead
	// of Projects alone.
	Impacted bool `json:"impacted,omitempty"`
	// Baseline is the resolved comparison revision the selection is computed
	// against.
	Baseline string `json:"baseline,omitempty"`
	// Page asks for one bounded page of the result.
	Page *PageRequest `json:"page,omitempty"`
}

// MemoryListResult is the memory.context and memory.search result.
type MemoryListResult struct {
	// Items are the matched records, in the provider's stable order.
	Items []MemoryRecord `json:"items"`
	// Page describes the returned page.
	Page Page `json:"page"`
}

// MemoryMissionInput is the memory.mission request.
type MemoryMissionInput struct {
	// Mission is the mission whose checkpointed state to read.
	Mission string `json:"mission"`
	// Identity locates the workspace, repository and scope the mission
	// belongs to.
	Identity MemoryIdentity `json:"identity"`
}

// MemoryRecordResult carries one memory record.
type MemoryRecordResult struct {
	// Record is the requested memory record.
	Record MemoryRecord `json:"record"`
}

// Precondition is a write precondition: exactly one of ExpectedRevision (the
// record must currently have this revision) and MustNotExist (the record must
// not exist yet).
type Precondition struct {
	// ExpectedRevision is the revision the record must currently have.
	ExpectedRevision string `json:"expectedRevision,omitempty"`
	// MustNotExist requires that the record does not exist yet.
	MustNotExist bool `json:"mustNotExist,omitempty"`
}

// MemoryCheckpointInput is the memory.checkpoint request.
type MemoryCheckpointInput struct {
	// Mission is the mission the checkpoint belongs to.
	Mission string `json:"mission"`
	// Identity locates the workspace, repository and scope the checkpoint
	// belongs to.
	Identity MemoryIdentity `json:"identity"`
	// Precondition is the write precondition the checkpoint must satisfy.
	Precondition Precondition `json:"precondition"`
	// IdempotencyKey deduplicates a repeated checkpoint call.
	IdempotencyKey string `json:"idempotencyKey"`
	// Title is the checkpoint's short summary.
	Title string `json:"title,omitempty"`
	// Content is the checkpoint's body.
	Content string `json:"content"`
	// Sources are the references the checkpoint's content is drawn from.
	Sources []Ref `json:"sources,omitempty"`
	// Evidence are the gate/review/qualify records this checkpoint references.
	Evidence []Evidence `json:"evidence,omitempty"`
}

// MemoryCheckpointResult is the memory.checkpoint result. Replayed is true
// when the idempotency key matched the write that produced the current
// revision, so nothing was written again.
type MemoryCheckpointResult struct {
	// Record is the written (or previously written) checkpoint record.
	Record MemoryRecord `json:"record"`
	// Replayed is true when the idempotency key matched the write that
	// produced the current revision, so nothing was written again.
	Replayed bool `json:"replayed"`
}

// CapabilitiesInput is the capabilities request. It takes no member.
type CapabilitiesInput struct{}
