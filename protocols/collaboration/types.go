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
	// Key is the identifier people read and write for the task, such as
	// "#123" or "ENG-42", when the provider has one. It is for display and
	// for matching a mention in text; Ref, not Key, names the task in a
	// request.
	Key string `json:"key,omitempty"`
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
	// CreatedAt is when the task was created, in RFC 3339, when known.
	CreatedAt string `json:"createdAt,omitempty"`
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
	// A HeadCommit names the commit the caller pushed to the head: a provider
	// whose proposal head follows the pushed ref answers an open proposal
	// behind a head ref already at that commit once its head catches up, or
	// answers unavailable and retryable.
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

// ProposalCommentsInput is the proposals.comments request.
type ProposalCommentsInput struct {
	// Ref identifies the proposal whose comments to list.
	Ref Ref `json:"ref"`
	// Page asks for one bounded page of the result.
	Page *PageRequest `json:"page,omitempty"`
}

// ProposalComment is one text someone wrote on a change proposal: the body of
// a review, a comment on a file or on one line of it, or a comment on the
// proposal as a whole. A comment on no file has no Path. A comment on a whole
// file has a Path and no Line, and so does a comment whose line was deleted or
// is no longer in the change.
type ProposalComment struct {
	// Ref is the comment's source-qualified opaque reference.
	Ref Ref `json:"ref"`
	// URL is a display link. It is never an identity.
	URL string `json:"url,omitempty"`
	// Author identifies who wrote the comment, when known.
	Author string `json:"author,omitempty"`
	// SubmittedAt is when the comment was published, in RFC 3339, when known.
	SubmittedAt string `json:"submittedAt,omitempty"`
	// Review is the review the comment belongs to, when it belongs to one.
	Review *Ref `json:"review,omitempty"`
	// Path is the repository-relative path of the file the comment is
	// anchored to, absent for a comment on no file.
	Path string `json:"path,omitempty"`
	// Line is the line of Path, in the file at Commit, the comment is
	// anchored to. It is absent for a comment on the whole file, and for one
	// whose line was deleted or is no longer in the change. It is never set
	// without Path, and the first line is 1.
	Line *int `json:"line,omitempty"`
	// Commit is the commit whose file Line refers to, when known. It is not
	// necessarily the commit the comment was written on: a provider may report
	// where the comment stands now.
	Commit string `json:"commit,omitempty"`
	// Body is the comment's text.
	Body string `json:"body"`
}

// ProposalCommentListResult is the proposals.comments result.
type ProposalCommentListResult struct {
	// Items are the proposal's comments, in the provider's stable order.
	Items []ProposalComment `json:"items"`
	// Page describes the returned page.
	Page Page `json:"page"`
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

// MissionName names a mission. Repository and Key are MemoryIdentity's
// Repository and Mission, so a mission's sessions and its memory records name
// it alike. Together they are the mission's identity, compared exactly.
type MissionName struct {
	// Repository is the repository the mission belongs to, in lowercase,
	// such as "owner/name". It is required.
	Repository string `json:"repository"`
	// Key names the mission inside its repository, such as "issue-596". The
	// contract treats it as opaque.
	Key string `json:"key"`
	// Title is the mission's title, for display. It is not part of the
	// mission's identity.
	Title string `json:"title,omitempty"`
}

// SessionRef names one agent session on its host.
type SessionRef struct {
	// Engine names the host that runs the agent, in lowercase letters,
	// digits and hyphens.
	Engine string `json:"engine"`
	// ID is the host's identifier of the session: 1 to 128 letters, digits,
	// dots, colons, underscores or hyphens.
	ID string `json:"id"`
}

// SessionState is the state an agent session reports.
type SessionState string

// Session states.
const (
	// SessionStateWorking: the agent is working.
	SessionStateWorking SessionState = "working"
	// SessionStateHandedBack: the agent stopped and waits for a person.
	SessionStateHandedBack SessionState = "handed_back"
	// SessionStateWaiting: the agent waits for an answer until WaitUntil,
	// which a waiting session always reports.
	SessionStateWaiting SessionState = "waiting"
	// SessionStateFailed: the session stopped on an error.
	SessionStateFailed SessionState = "failed"
	// SessionStateEnded: the session ended: its host closed it or the user
	// quit. It carries no error and no WaitUntil.
	SessionStateEnded SessionState = "ended"
)

// ValidSessionStates is the closed session-state vocabulary in canonical order.
var ValidSessionStates = []SessionState{SessionStateEnded, SessionStateFailed, SessionStateHandedBack, SessionStateWaiting, SessionStateWorking}

// LoopStage is the stage of the contribution loop a session's change is at.
// The loop runs talk, declare, show, code, check, prove, validate, merge.
type LoopStage string

// Loop stages, in the loop's order.
const (
	// LoopStageTalk: the owner and the agent agree on the outcome.
	LoopStageTalk LoopStage = "talk"
	// LoopStageDeclare: the change declares its intent: specs and decisions.
	LoopStageDeclare LoopStage = "declare"
	// LoopStageShow: the change shows what it will look like before code.
	LoopStageShow LoopStage = "show"
	// LoopStageCode: the change is written.
	LoopStageCode LoopStage = "code"
	// LoopStageCheck: the change runs its gate.
	LoopStageCheck LoopStage = "check"
	// LoopStageProve: the change proves its outcome where it runs.
	LoopStageProve LoopStage = "prove"
	// LoopStageValidate: the owner validates the change.
	LoopStageValidate LoopStage = "validate"
	// LoopStageMerge: the change merges.
	LoopStageMerge LoopStage = "merge"
)

// ValidLoopStages is the closed loop-stage vocabulary in canonical order.
var ValidLoopStages = []LoopStage{LoopStageCheck, LoopStageCode, LoopStageDeclare, LoopStageMerge, LoopStageProve, LoopStageShow, LoopStageTalk, LoopStageValidate}

// SessionUsage is the tokens a session used, cumulative for the whole
// session, its subagents included. Each count is 0 to MaxUsageTokens.
type SessionUsage struct {
	// InputTokens are the input tokens read neither from nor into the cache.
	InputTokens int64 `json:"inputTokens"`
	// OutputTokens are the output tokens.
	OutputTokens int64 `json:"outputTokens"`
	// CacheWriteTokens are the input tokens written to the prompt cache.
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	// CacheReadTokens are the input tokens read from the prompt cache.
	CacheReadTokens int64 `json:"cacheReadTokens"`
}

// PlanStepStatus is the status of one plan step.
type PlanStepStatus string

// Plan step statuses.
const (
	PlanStepTodo  PlanStepStatus = "todo"
	PlanStepDoing PlanStepStatus = "doing"
	PlanStepDone  PlanStepStatus = "done"
)

// ValidPlanStepStatuses is the closed plan-step vocabulary in canonical order.
var ValidPlanStepStatuses = []PlanStepStatus{PlanStepDoing, PlanStepDone, PlanStepTodo}

// PlanStep is one step of an agent's plan.
type PlanStep struct {
	// Key identifies the step inside its plan. It is a token.
	Key string `json:"key"`
	// Status is where the step stands; see the PlanStep* constants.
	Status PlanStepStatus `json:"status"`
	// Text describes the step on one line.
	Text string `json:"text"`
}

// MissionPlan is a whole plan. An empty list of steps is an empty plan.
type MissionPlan struct {
	// Steps are the plan's steps, in order.
	Steps []PlanStep `json:"steps"`
}

// MessageReply is an agent's reply to one message of the inbox.
type MessageReply struct {
	// Message is the id of the message replied to.
	Message string `json:"message"`
	// Reply is the reply's text.
	Reply string `json:"reply"`
}

// MissionsSyncInput is the missions.sync request. Its natural key is the
// session and ObservedAt: a provider applies State, Stage, Usage and Plan only
// from a sync observed later than the last one it applied for the session, so
// a repeat changes nothing.
type MissionsSyncInput struct {
	// Session is the session that publishes.
	Session SessionRef `json:"session"`
	// Mission is the mission the session reports to.
	Mission MissionName `json:"mission"`
	// Event names the host event that caused the sync, for display. A
	// provider never branches on it: State carries the meaning.
	Event string `json:"event"`
	// State is the session's state; see the SessionState* constants.
	State SessionState `json:"state"`
	// ObservedAt is when the host saw the event, in RFC 3339 UTC.
	ObservedAt string `json:"observedAt"`
	// Model names the model the session runs, when known.
	Model string `json:"model,omitempty"`
	// Branch is the branch the session works on, when known.
	Branch string `json:"branch,omitempty"`
	// LastMessage is the agent's last message, when the session stopped.
	LastMessage string `json:"lastMessage,omitempty"`
	// Error is the error that stopped a failed session.
	Error string `json:"error,omitempty"`
	// WaitUntil is when a waiting session stops waiting, in RFC 3339. A
	// waiting session always reports it, and no other session does.
	WaitUntil string `json:"waitUntil,omitempty"`
	// Plan replaces the mission's plan. Absent keeps the plan; an empty list
	// of steps clears it.
	Plan *MissionPlan `json:"plan,omitempty"`
	// Delivered are the ids of the inbox messages the host gave the agent.
	// A repeated id counts once. A provider ignores an id it does not know
	// or that is not the caller's.
	Delivered []string `json:"delivered,omitempty"`
	// Decisions are the ids of the decisions this session asked; sending
	// them binds them to the session. A repeated id counts once. A provider
	// ignores an id it does not know or that is not the caller's.
	Decisions []string `json:"decisions,omitempty"`
	// Replies are the agent's replies to inbox messages, one per message.
	Replies []MessageReply `json:"replies,omitempty"`
	// Stage is the loop stage the session's change is at; see the
	// LoopStage* constants. A provider keeps the stage of the last sync it
	// applied that carried one: absent keeps it.
	Stage LoopStage `json:"stage,omitempty"`
	// Usage is the tokens the session used so far. A provider replaces the
	// usage it holds with the one of the last sync it applied that carried
	// one: absent keeps it.
	Usage *SessionUsage `json:"usage,omitempty"`
}

// SessionStatus is the session as the provider holds it.
type SessionStatus struct {
	// State is the state the provider holds for the session.
	State SessionState `json:"state"`
}

// OpenDecision is a decision of the mission that no one answered yet.
type OpenDecision struct {
	// ID is the decision's id.
	ID string `json:"id"`
	// Question is the decision's question.
	Question string `json:"question"`
}

// MissionView is what a sync or a status reads about a mission. It always
// describes the mission the request named, whatever a sync's age. A provider that does
// not hold that mission answers its key, an empty title, an empty plan and no
// open decision.
type MissionView struct {
	// Key is the mission's key: the one the request named.
	Key string `json:"key"`
	// Title is the mission's title, empty when it has none.
	Title string `json:"title"`
	// URL is a display link to the mission. It is never an identity.
	URL string `json:"url,omitempty"`
	// Plan is the mission's current plan: the plan that the session that
	// published last sent. It is empty when none did.
	Plan MissionPlan `json:"plan"`
	// OpenDecisions are the mission's decisions that no one answered yet.
	OpenDecisions []OpenDecision `json:"openDecisions"`
}

// InboxKind classifies an inbox message.
type InboxKind string

// Inbox message kinds.
const (
	// InboxKindInfo is information for the agent.
	InboxKindInfo InboxKind = "info"
	// InboxKindPrompt asks the agent for a reply.
	InboxKindPrompt InboxKind = "prompt"
	// InboxKindAnswer carries the answer to one of the mission's decisions.
	InboxKindAnswer InboxKind = "answer"
)

// ValidInboxKinds is the closed inbox-kind vocabulary in canonical order.
var ValidInboxKinds = []InboxKind{InboxKindAnswer, InboxKindInfo, InboxKindPrompt}

// InboxDecision is the decision an answer message answers.
type InboxDecision struct {
	// ID is the decision's id.
	ID string `json:"id"`
	// Question is the decision's question.
	Question string `json:"question"`
	// Answer is the owner's answer.
	Answer string `json:"answer"`
	// Option is the id of the option the owner chose, when the answer chose
	// one.
	Option string `json:"option,omitempty"`
}

// InboxMessage is one message that no session received yet.
type InboxMessage struct {
	// ID is the message's id: the one a reply and a delivery name.
	ID string `json:"id"`
	// Kind classifies the message; see the InboxKind* constants.
	Kind InboxKind `json:"kind"`
	// Text is the message's text. On an answer, Decision.Answer is the
	// answer, and Text is the provider's display of it.
	Text string `json:"text"`
	// CreatedAt is when the message was written, in RFC 3339.
	CreatedAt string `json:"createdAt"`
	// Decision is the decision an answer message answers. Only an answer
	// carries one.
	Decision *InboxDecision `json:"decision,omitempty"`
}

// DecisionState is where a decision stands.
type DecisionState string

// Decision states.
const (
	// DecisionStateOpen: no one answered the decision yet.
	DecisionStateOpen DecisionState = "open"
	// DecisionStateAnswered: the owner answered, and no session received the
	// answer yet.
	DecisionStateAnswered DecisionState = "answered"
	// DecisionStateAcknowledged: a session received the answer: a sync
	// delivered the message that carries it.
	DecisionStateAcknowledged DecisionState = "acknowledged"
)

// ValidDecisionStates is the closed decision-state vocabulary in canonical order.
var ValidDecisionStates = []DecisionState{DecisionStateAcknowledged, DecisionStateAnswered, DecisionStateOpen}

// DecisionStatus is where one decision stands.
type DecisionStatus struct {
	// ID is the decision's id.
	ID string `json:"id"`
	// State is where the decision stands; see the DecisionState* constants.
	State DecisionState `json:"state"`
}

// ReplyState is what became of one reply.
type ReplyState string

// Reply states.
const (
	// ReplyStateReplied: the reply is recorded, now or by an earlier call.
	ReplyStateReplied ReplyState = "replied"
	// ReplyStateRefused: the provider refused the reply; Reason says why.
	ReplyStateRefused ReplyState = "refused"
)

// ValidReplyStates is the closed reply-state vocabulary in canonical order.
var ValidReplyStates = []ReplyState{ReplyStateRefused, ReplyStateReplied}

// ValidReplyRefusals is the closed vocabulary of a refused reply's reason, in
// canonical order.
var ValidReplyRefusals = []string{ReasonMessageClosed, ReasonMessageDenied, ReasonMessageNotFound}

// ReplyStatus is what became of one reply a sync carried. A refused reply is
// final: the host drops it.
type ReplyStatus struct {
	// Message is the id of the message the reply answers.
	Message string `json:"message"`
	// State is what became of the reply; see the ReplyState* constants.
	State ReplyState `json:"state"`
	// Reason is why a reply was refused: one of ValidReplyRefusals. Only a
	// refused reply carries one.
	Reason string `json:"reason,omitempty"`
}

// MissionsSyncResult is the missions.sync result.
type MissionsSyncResult struct {
	// Session is the session as the provider holds it.
	Session SessionStatus `json:"session"`
	// Mission is the mission the sync named.
	Mission MissionView `json:"mission"`
	// Inbox are the mission's messages that no session received yet,
	// oldest first.
	Inbox []InboxMessage `json:"inbox"`
	// Decisions say where the decisions stand that the sync named.
	Decisions []DecisionStatus `json:"decisions"`
	// Replies hold one entry for each reply the sync carried.
	Replies []ReplyStatus `json:"replies"`
	// MinIntervalMs is the pace of routine syncs, in milliseconds. A host
	// may sync sooner at session start, at stop and during a wait, and a
	// provider does not refuse those syncs.
	MinIntervalMs int `json:"minIntervalMs"`
	// Now is the provider's clock, in RFC 3339.
	Now string `json:"now"`
}

// DecisionOption is one answer the owner can choose.
type DecisionOption struct {
	// ID identifies the option inside its decision: 1 to 64 letters, digits,
	// dots, underscores or hyphens.
	ID string `json:"id"`
	// Label names the option on one line.
	Label string `json:"label"`
	// Consequences says what choosing the option implies.
	Consequences string `json:"consequences,omitempty"`
}

// DecisionRecommendation is the option the agent recommends.
type DecisionRecommendation struct {
	// Option is the id of the recommended option.
	Option string `json:"option"`
	// Reason says why.
	Reason string `json:"reason,omitempty"`
}

// MissionsAskInput is the missions.ask request. Its content is
// Mission.Repository, Mission.Key, Question, Context, Options,
// Recommendation and Blocks: the same IdempotencyKey with the same content
// replays, and with other content is a conflict. Mission.Title is not
// content. A session binds the decision through MissionsSyncInput.Decisions.
type MissionsAskInput struct {
	// Mission is the mission whose owner decides.
	Mission MissionName `json:"mission"`
	// Question is what the owner decides.
	Question string `json:"question"`
	// Context is what the owner needs to know to decide.
	Context string `json:"context,omitempty"`
	// Options are the answers the owner can choose, 2 to 5.
	Options []DecisionOption `json:"options"`
	// Recommendation is the option the agent recommends.
	Recommendation *DecisionRecommendation `json:"recommendation,omitempty"`
	// Blocks says what waits for the answer.
	Blocks string `json:"blocks,omitempty"`
	// IdempotencyKey deduplicates a repeated ask.
	IdempotencyKey string `json:"idempotencyKey"`
}

// MissionDecision is one decision, as missions.ask answers it.
type MissionDecision struct {
	// ID is the decision's id: the one a sync and an inbox answer name.
	ID string `json:"id"`
	// State is where the decision stands; see the DecisionState* constants.
	State DecisionState `json:"state"`
	// URL is a display link to the decision. It is never an identity.
	URL string `json:"url,omitempty"`
	// Answer is the owner's answer, once there is one.
	Answer string `json:"answer,omitempty"`
	// Option is the id of the option the owner chose, when the answer chose
	// one.
	Option string `json:"option,omitempty"`
}

// MissionsAskResult is the missions.ask result. Replayed is true when the
// idempotency key matched a decision already recorded, so nothing was asked
// again.
type MissionsAskResult struct {
	// Decision is the decision recorded now or by the earlier call.
	Decision MissionDecision `json:"decision"`
	// Replayed is true when the idempotency key matched a decision already
	// recorded.
	Replayed bool `json:"replayed"`
}

// MissionsReplyInput is the missions.reply request. Its natural key is the
// message: a message takes one reply. A session claims a message through
// MissionsSyncInput.Delivered and Replies.
type MissionsReplyInput struct {
	// Message is the id of the message replied to.
	Message string `json:"message"`
	// Reply is the reply's text.
	Reply string `json:"reply"`
}

// RepliedMessage is a message once a reply is recorded.
type RepliedMessage struct {
	// ID is the message's id.
	ID string `json:"id"`
	// State is ReplyStateReplied.
	State ReplyState `json:"state"`
}

// MissionsReplyResult is the missions.reply result. Replayed is true when the
// message already held this reply, so nothing was written again.
type MissionsReplyResult struct {
	// Message is the message replied to.
	Message RepliedMessage `json:"message"`
	// Replayed is true when the message already held this reply.
	Replayed bool `json:"replayed"`
}

// MissionsStatusInput is the missions.status request: the mission to read
// and the page of its sessions. Mission.Title is for display and changes
// nothing.
type MissionsStatusInput struct {
	// Mission is the mission whose view and sessions to read.
	Mission MissionName `json:"mission"`
	// Page selects the page of sessions.
	Page *PageRequest `json:"page,omitempty"`
}

// MissionSession is one session of a mission, as the last sync the provider
// applied for it reported it.
type MissionSession struct {
	// Session names the session on its host.
	Session SessionRef `json:"session"`
	// State is the state the provider holds for the session.
	State SessionState `json:"state"`
	// ObservedAt is the observation time of the last sync applied for the
	// session, in RFC 3339 UTC.
	ObservedAt string `json:"observedAt"`
	// Stage is the loop stage the provider holds for the session, when it
	// holds one.
	Stage LoopStage `json:"stage,omitempty"`
	// Usage is the tokens the provider holds for the session, when it holds
	// them.
	Usage *SessionUsage `json:"usage,omitempty"`
	// Branch is the branch the session works on, when known.
	Branch string `json:"branch,omitempty"`
	// Error is the error that stopped a failed session; only a failed
	// session carries one.
	Error string `json:"error,omitempty"`
	// WaitUntil is when a waiting session stops waiting, in RFC 3339. A
	// waiting session always carries it, and no other session does.
	WaitUntil string `json:"waitUntil,omitempty"`
}

// MissionsStatusResult is the missions.status result.
type MissionsStatusResult struct {
	// Mission is the mission the request named. A provider that does not
	// hold it answers its empty view.
	Mission MissionView `json:"mission"`
	// Sessions are a page of the sessions whose last applied sync named the
	// mission, newest observation first, then by engine and id, each once. A
	// traversal lists once every session that does not sync during it.
	Sessions []MissionSession `json:"sessions"`
	// Page carries the cursor of the next page of sessions.
	Page Page `json:"page"`
}

// CapabilitiesInput is the capabilities request. It takes no member.
type CapabilitiesInput struct{}
