package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
)

// providerCall is one request that passed every orchestrator check and is ready for
// its provider.
type providerCall struct {
	Resolution collabResolution
	Operation  collab.OperationSpec
	Provided   collab.ProvidedOperation
	// Input is the validated, normalized request document.
	Input any
	// Arguments is Input's encoding: exactly what the provider receives.
	Arguments json.RawMessage
}

// providerMember is the provider member of the tool request.
func (c *providerCall) providerMember() *proto.ToolProviderCall {
	binding := c.Resolution.Binding
	return &proto.ToolProviderCall{
		Contract:  c.Resolution.Contract,
		Version:   binding.Version,
		Operation: c.Operation.Name,
		Settings:  binding.Settings,
	}
}

// prepareCall decides whether a request reaches a provider. It returns the call
// to make, or the envelope that answers without running anything: the
// capabilities document, an unbound or invalid binding, an operation the
// provider does not offer, a request the contract refuses, or a precondition
// the provider cannot enforce. A precondition is never dropped: a request
// that carries one the provider cannot compare is refused as unsupported.
func prepareCall(document collabDocument, resolution collabResolution, operation string, arguments json.RawMessage) (*providerCall, *collab.Envelope) {
	if operation == collab.OperationCapabilities {
		capabilities := capabilitiesOf(resolution)
		result, err := json.Marshal(capabilities)
		if err != nil {
			return nil, failure(resolution, operation, collab.OutcomeUnavailable, collab.ReasonProviderInvalidResponse, false, "encode capabilities: %v", err)
		}
		envelope := base(resolution, operation)
		envelope.Outcome = collab.OutcomeOK
		envelope.Result = result
		return nil, &envelope
	}
	switch resolution.Status {
	case collab.BindingStatusUnbound:
		message := fmt.Sprintf("no %s provider is bound: add options.collaboration.%s to %s",
			resolution.Contract, resolution.Contract, "putnami.workspace.json")
		if names := candidateNames(resolution.Candidates); names != "" {
			message += " (installed providers: " + names + "; none is chosen without a binding)"
		}
		return nil, failure(resolution, operation, collab.OutcomeUnsupported, collab.ReasonBindingMissing, false, "%s", message)
	case collab.BindingStatusInvalid:
		issue := resolution.Issues[0]
		outcome := collab.OutcomeUnsupported
		if issue.Reason == collab.ReasonBindingProviderMissing {
			outcome = collab.OutcomeUnavailable
		}
		return nil, failure(resolution, operation, outcome, issue.Reason, false, "%s", joinIssues(resolution.Issues))
	}
	op, known := resolution.Spec.Operation(operation)
	if !known {
		return nil, failure(resolution, operation, collab.OutcomeInvalid, collab.ReasonRequestInvalid, false,
			"%s version %d has no operation %q (operations: capabilities, %s)",
			resolution.Contract, resolution.Spec.Version, operation, strings.Join(operationNames(resolution.Spec), ", "))
	}
	provided, offered := resolution.Operations[operation]
	if !offered {
		return nil, failure(resolution, operation, collab.OutcomeUnsupported, collab.ReasonOperationUnsupported, false,
			"%s does not offer the optional operation %s.%s", resolution.Provider.Name, resolution.Contract, operation)
	}
	input, diags := collab.ParseRequest(resolution.Contract, resolution.Spec.Version, operation, arguments)
	if diags != nil {
		return nil, failure(resolution, operation, collab.OutcomeInvalid, collab.ReasonRequestInvalid, false,
			"%s", collab.FormatDiagnostics(diags))
	}
	if precondition := requestPrecondition(input); precondition != "" && provided.Declaration.Preconditions == collab.PreconditionsNone {
		return nil, failure(resolution, operation, collab.OutcomeUnsupported, collab.ReasonPreconditionUnsupported, false,
			"%s cannot check preconditions on %s.%s, so the request's %s cannot be honored; nothing was sent",
			resolution.Provider.Name, resolution.Contract, operation, precondition)
	}
	normalize(input, document.Name)
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, failure(resolution, operation, collab.OutcomeInvalid, collab.ReasonRequestInvalid, false, "encode the request: %v", err)
	}
	return &providerCall{Resolution: resolution, Operation: op, Provided: provided, Input: input, Arguments: encoded}, nil
}

// requestPrecondition names the write precondition a request carries — an
// expectedRevision, or a mustNotExist creation guard — or "" when it carries
// none. A provider that declares preconditions none can check neither, so
// either one is refused before the provider runs.
func requestPrecondition(input any) string {
	revision := func(expected string) string {
		if expected != "" {
			return "expectedRevision"
		}
		return ""
	}
	switch in := input.(type) {
	case *collab.TaskUpdateInput:
		return revision(in.ExpectedRevision)
	case *collab.TaskTransitionInput:
		return revision(in.ExpectedRevision)
	case *collab.TaskAssignInput:
		return revision(in.ExpectedRevision)
	case *collab.ProposalUpsertInput:
		return revision(in.ExpectedRevision)
	case *collab.MemoryCheckpointInput:
		switch {
		case in.Precondition.ExpectedRevision != "":
			return "precondition.expectedRevision"
		case in.Precondition.MustNotExist:
			return "precondition.mustNotExist"
		}
	}
	return ""
}

// normalize applies the orchestrator's defaults, so every provider receives
// an explicit, bounded page and a memory identity naming the workspace.
func normalize(input any, workspaceName string) {
	page := func(p **collab.PageRequest) {
		if *p == nil {
			*p = &collab.PageRequest{}
		}
		if (*p).Size == 0 {
			(*p).Size = collab.DefaultPageSize
		}
	}
	identity := func(id *collab.MemoryIdentity) {
		if id.Workspace == "" && workspaceName != "" {
			id.Workspace = workspaceName
		}
	}
	switch in := input.(type) {
	case *collab.TaskFindInput:
		page(&in.Page)
	case *collab.ProposalFindInput:
		page(&in.Page)
	case *collab.MemoryContextInput:
		page(&in.Page)
		identity(&in.Identity)
	case *collab.MemorySearchInput:
		page(&in.Page)
		identity(&in.Identity)
	case *collab.MemoryMissionInput:
		identity(&in.Identity)
	case *collab.MemoryCheckpointInput:
		identity(&in.Identity)
	}
}

// transportFailure renders an invocation that produced no Response. A read
// that failed is unavailable. A mutation that started is unresolved: the
// provider may have written before it died, timed out or was canceled, and
// only a read can tell. A call its deadline or its caller stopped before the
// process started is unavailable and retryable: nothing ran.
func transportFailure(call *providerCall, stage invocationStage, message string) collab.Envelope {
	resolution, operation := call.Resolution, call.Operation.Name
	mutating := call.Operation.Access == collab.AccessMutating
	reason, retryable := collab.ReasonProviderFailed, false
	switch stage {
	case stagePrepare, stageStart:
		return *failure(resolution, operation, collab.OutcomeUnavailable, collab.ReasonProviderUnavailable, false, "%s", message)
	case stageTimeoutBeforeStart:
		return *failure(resolution, operation, collab.OutcomeUnavailable, collab.ReasonProviderTimeout, true, "%s", message)
	case stageCanceledBeforeStart:
		return *failure(resolution, operation, collab.OutcomeUnavailable, collab.ReasonProviderCanceled, true, "%s", message)
	case stageTimeout:
		reason, retryable = collab.ReasonProviderTimeout, true
	case stageCanceled:
		reason, retryable = collab.ReasonProviderCanceled, true
	case stageOutput:
		reason = collab.ReasonProviderInvalidResponse
	}
	if mutating {
		return unresolved(call, reason, "%s; the %s write may have happened", message, resolution.Contract+"."+operation)
	}
	return *failure(resolution, operation, collab.OutcomeUnavailable, reason, retryable, "%s", message)
}

// interpretResult checks a provider's tool result and renders the envelope. The
// result must hold exactly one Response the contract accepts, a read may not
// claim an unresolved write or a conflict, and an ok result must describe
// exactly the item or identity the request named. A mutation whose answer
// fails any check is unresolved.
func interpretResult(call *providerCall, result proto.ToolCallResult) collab.Envelope {
	resolution, op := call.Resolution, call.Operation
	if len(result.Content) != 1 {
		return invalidResponse(call, "the provider wrote %d content blocks; a collaboration result is exactly one Response", len(result.Content))
	}
	response, diags := collab.ParseResponse([]byte(result.Content[0].Text))
	if diags != nil {
		return invalidResponse(call, "the provider's response violates the contract: %s%s", collab.FormatDiagnostics(diags), smallerPageHint(call, diags))
	}
	if op.Access == collab.AccessRead && (response.Outcome == collab.OutcomeUnresolved || response.Outcome == collab.OutcomeConflict) {
		return invalidResponse(call, "a read cannot answer %s", response.Outcome)
	}
	if result.IsError != (response.Outcome != collab.OutcomeOK) {
		return invalidResponse(call, "the tool result's isError disagrees with outcome %s", response.Outcome)
	}
	envelope := base(resolution, op.Name)
	if response.Outcome != collab.OutcomeOK {
		errorValue := *response.Error
		if response.Outcome == collab.OutcomeUnresolved && errorValue.Reconcile == "" {
			errorValue.Reconcile = ReconcileHint(resolution.Contract, op.Name)
		}
		envelope.Outcome = response.Outcome
		envelope.Error = &errorValue
		return envelope
	}
	parsed, diags := collab.ParseResult(resolution.Contract, resolution.Spec.Version, op.Name, response.Result)
	if diags != nil {
		return invalidResponse(call, "the provider's %s result violates the contract: %s%s", op.Name, collab.FormatDiagnostics(diags), smallerPageHint(call, diags))
	}
	if mismatch := identityMismatch(call.Input, parsed); mismatch != "" {
		if op.Access == collab.AccessMutating {
			return unresolved(call, collab.ReasonProviderIdentity, "the provider answered for a different item: %s", mismatch)
		}
		return *failure(resolution, op.Name, collab.OutcomeUnavailable, collab.ReasonProviderIdentity, false,
			"the provider answered for a different item: %s", mismatch)
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return invalidResponse(call, "encode the result: %v", err)
	}
	envelope.Outcome = collab.OutcomeOK
	envelope.Result = encoded
	return envelope
}

// smallerPageHint tells the caller of a list operation whose answer exceeds
// the document bound how to get the items anyway: a provider that does not
// shorten its pages itself answers a smaller page.size in several documents.
// It is "" for any other refusal and any other operation.
func smallerPageHint(call *providerCall, diags []diag.Diagnostic) string {
	page := requestedPage(call.Input)
	// Only the whole document above its bound is a page too large; a member
	// above its own bound stays too large at any page size.
	if page == nil || !slices.ContainsFunc(diags, func(d diag.Diagnostic) bool { return d.Code == collab.ErrorCodeTooLarge && d.Field == "" }) {
		return ""
	}
	return fmt.Sprintf("; a page of %d items does not fit in one answer: ask for a smaller page.size", page.Size)
}

// requestedPage is the page a list request asks for, or nil for a request of
// any other operation. prepareCall gives every list request one.
func requestedPage(input any) *collab.PageRequest {
	switch in := input.(type) {
	case *collab.TaskFindInput:
		return in.Page
	case *collab.ProposalFindInput:
		return in.Page
	case *collab.MemoryContextInput:
		return in.Page
	case *collab.MemorySearchInput:
		return in.Page
	}
	return nil
}

func invalidResponse(call *providerCall, format string, args ...any) collab.Envelope {
	if call.Operation.Access == collab.AccessMutating {
		return unresolved(call, collab.ReasonProviderInvalidResponse, format, args...)
	}
	return *failure(call.Resolution, call.Operation.Name, collab.OutcomeUnavailable, collab.ReasonProviderInvalidResponse, false, format, args...)
}

func unresolved(call *providerCall, reason, format string, args ...any) collab.Envelope {
	envelope := failure(call.Resolution, call.Operation.Name, collab.OutcomeUnresolved, reason, false, format, args...)
	envelope.Error.Reconcile = ReconcileHint(call.Resolution.Contract, call.Operation.Name)
	return *envelope
}

// ReconcileHint names the read that establishes whether an unresolved write
// happened, and when a repeat is safe. It is the reconcile member the route
// writes when the provider names none; a stand-in for the route's answers
// takes it from here, so it carries the text the route writes.
func ReconcileHint(contract, operation string) string {
	switch contract + "." + operation {
	case "tasks.create":
		return "repeat tasks.create with the same idempotencyKey: the provider returns the task that key created, or creates it once"
	case "proposals.upsert":
		return "run proposals.find for the same change: upsert never creates a second proposal for one repository/base/head"
	case "proposals.review":
		return "repeat proposals.review with the same idempotencyKey: the provider returns the review that key published, or publishes it once"
	case "proposals.merge":
		return "read the proposal with proposals.status before any retry"
	case "memory.checkpoint":
		return "read the mission with memory.mission; repeat the checkpoint with the same idempotencyKey and precondition, which replays a write that landed"
	}
	return "read the task with tasks.get and compare its revision before any retry"
}

func base(resolution collabResolution, operation string) collab.Envelope {
	envelope := collab.Envelope{Contract: resolution.Contract, Operation: operation}
	if resolution.Binding != nil && resolution.Spec != nil {
		envelope.Version = resolution.Binding.Version
	}
	if resolution.Provider != nil {
		envelope.Provider = &collab.ProviderIdentity{Name: resolution.Provider.Name, Version: resolution.Provider.Version}
	}
	return envelope
}

func failure(resolution collabResolution, operation string, outcome collab.Outcome, reason string, retryable bool, format string, args ...any) *collab.Envelope {
	envelope := base(resolution, operation)
	envelope.Outcome = outcome
	envelope.Error = &collab.Error{Message: fmt.Sprintf(format, args...), Reason: reason, Retryable: retryable}
	return &envelope
}

// identityMismatch compares an ok result with the request it answers and
// describes the first difference; "" means the result is about exactly what
// was asked.
func identityMismatch(input, result any) string {
	if mismatch, handled := taskMismatch(input, result); handled {
		return mismatch
	}
	if mismatch, handled := proposalMismatch(input, result); handled {
		return mismatch
	}
	return memoryMismatch(input, result)
}

// resultAs asserts a parsed result's type. ParseResult allocates the type the
// catalog names, so a mismatch is a routing defect, reported as a mismatch
// rather than a panic.
func resultAs[T any](result any) (T, string) {
	typed, ok := result.(T)
	if !ok {
		return typed, fmt.Sprintf("the result is a %T", result)
	}
	return typed, ""
}

func sameRef(what string, got, want collab.Ref) string {
	if got != want {
		return fmt.Sprintf("%s %s/%s, requested %s/%s", what, got.Source, got.ID, want.Source, want.ID)
	}
	return ""
}

func taskRefMismatch(result any, want collab.Ref) string {
	task, wrong := resultAs[*collab.TaskResult](result)
	if wrong != "" {
		return wrong
	}
	return sameRef("task", task.Task.Ref, want)
}

func taskMismatch(input, result any) (string, bool) {
	switch in := input.(type) {
	case *collab.TaskRefInput:
		return taskRefMismatch(result, in.Ref), true
	case *collab.TaskUpdateInput:
		return taskRefMismatch(result, in.Ref), true
	case *collab.TaskAssignInput:
		return taskRefMismatch(result, in.Ref), true
	case *collab.TaskLinkInput:
		return taskRefMismatch(result, in.Ref), true
	case *collab.TaskTransitionInput:
		if mismatch := taskRefMismatch(result, in.Ref); mismatch != "" {
			return mismatch, true
		}
		if task, _ := resultAs[*collab.TaskResult](result); task.Task.State != in.State {
			return fmt.Sprintf("task state %s, requested %s", task.Task.State, in.State), true
		}
		return "", true
	case *collab.TaskClaimInput:
		return claimMismatch(in, result), true
	case *collab.TaskFindInput:
		return taskListMismatch(in, result), true
	}
	return "", false
}

func taskListMismatch(in *collab.TaskFindInput, result any) string {
	list, wrong := resultAs[*collab.TaskListResult](result)
	if wrong != "" {
		return wrong
	}
	if len(list.Items) > in.Page.Size {
		return fmt.Sprintf("%d items for a page of %d", len(list.Items), in.Page.Size)
	}
	for _, task := range list.Items {
		if len(in.States) > 0 && !slices.Contains(in.States, task.State) {
			return fmt.Sprintf("task %s is %s, outside the requested states", task.Ref.ID, task.State)
		}
	}
	return ""
}

func claimMismatch(in *collab.TaskClaimInput, result any) string {
	claim, wrong := resultAs[*collab.TaskClaimResult](result)
	if wrong != "" {
		return wrong
	}
	if mismatch := sameRef("task", claim.Task.Ref, in.Ref); mismatch != "" {
		return mismatch
	}
	if claim.Held && claim.Task.Holder != in.Holder {
		return fmt.Sprintf("claim held by %q, requested for %q", claim.Task.Holder, in.Holder)
	}
	if in.Release && claim.Held {
		return "a release answered that the claim is still held"
	}
	return ""
}

func proposalMismatch(input, result any) (string, bool) {
	switch in := input.(type) {
	case *collab.ProposalFindInput:
		return proposalListMismatch(in, result), true
	case *collab.ProposalUpsertInput:
		return upsertMismatch(in, result), true
	case *collab.ProposalRefInput:
		status, wrong := resultAs[*collab.ProposalStatusResult](result)
		if wrong != "" {
			return wrong, true
		}
		return sameRef("proposal", status.Proposal.Ref, in.Ref), true
	case *collab.ProposalReviewInput:
		return reviewMismatch(in, result), true
	case *collab.ProposalMergeInput:
		merged, wrong := resultAs[*collab.ProposalResult](result)
		if wrong != "" {
			return wrong, true
		}
		if mismatch := sameRef("proposal", merged.Proposal.Ref, in.Ref); mismatch != "" {
			return mismatch, true
		}
		if merged.Proposal.State != collab.ProposalStateMerged {
			return fmt.Sprintf("a merge answered state %s", merged.Proposal.State), true
		}
		return "", true
	}
	return "", false
}

func upsertMismatch(in *collab.ProposalUpsertInput, result any) string {
	upsert, wrong := resultAs[*collab.ProposalUpsertResult](result)
	if wrong != "" {
		return wrong
	}
	proposal := upsert.Proposal
	if mismatch := sameChange(proposal.Change, in.Change, in.Change.Repository); mismatch != "" {
		return mismatch
	}
	if in.Change.HeadCommit != "" && proposal.Change.HeadCommit != "" && !sameCommit(proposal.Change.HeadCommit, in.Change.HeadCommit) {
		return fmt.Sprintf("head commit %s, requested %s", proposal.Change.HeadCommit, in.Change.HeadCommit)
	}
	return ""
}

func reviewMismatch(in *collab.ProposalReviewInput, result any) string {
	review, wrong := resultAs[*collab.ProposalReviewResult](result)
	if wrong != "" {
		return wrong
	}
	if review.Review.Verdict != in.Verdict {
		return fmt.Sprintf("review verdict %s, requested %s", review.Review.Verdict, in.Verdict)
	}
	if in.Commit != "" && review.Review.Commit != "" && !sameCommit(review.Review.Commit, in.Commit) {
		return fmt.Sprintf("review of commit %s, requested %s", review.Review.Commit, in.Commit)
	}
	return ""
}

func proposalListMismatch(in *collab.ProposalFindInput, result any) string {
	list, wrong := resultAs[*collab.ProposalListResult](result)
	if wrong != "" {
		return wrong
	}
	if len(list.Items) > in.Page.Size {
		return fmt.Sprintf("%d items for a page of %d", len(list.Items), in.Page.Size)
	}
	repository := in.Change.Repository
	for _, proposal := range list.Items {
		if mismatch := sameChange(proposal.Change, in.Change, repository); mismatch != "" {
			return mismatch
		}
		repository = proposal.Change.Repository
		if len(in.States) > 0 && !slices.Contains(in.States, proposal.State) {
			return fmt.Sprintf("proposal %s is %s, outside the requested states", proposal.Ref.ID, proposal.State)
		}
	}
	return ""
}

func memoryMismatch(input, result any) string {
	switch in := input.(type) {
	case *collab.MemoryMissionInput:
		mission, wrong := resultAs[*collab.MemoryRecordResult](result)
		if wrong != "" {
			return wrong
		}
		if record := mission.Record; record.Kind != collab.MemoryKindMission || record.Identity.Mission != in.Mission {
			return fmt.Sprintf("record %s of mission %q, requested mission %q", record.Kind, record.Identity.Mission, in.Mission)
		}
	case *collab.MemoryCheckpointInput:
		return checkpointMismatch(in, result)
	case *collab.MemoryContextInput:
		return memoryListMismatch(result, in.Page.Size, in.Kinds)
	case *collab.MemorySearchInput:
		return memoryListMismatch(result, in.Page.Size, in.Kinds)
	}
	return ""
}

func checkpointMismatch(in *collab.MemoryCheckpointInput, result any) string {
	checkpoint, wrong := resultAs[*collab.MemoryCheckpointResult](result)
	if wrong != "" {
		return wrong
	}
	if checkpoint.Record.Identity.Mission != in.Mission {
		return fmt.Sprintf("checkpoint of mission %q, requested %q", checkpoint.Record.Identity.Mission, in.Mission)
	}
	if !checkpoint.Replayed && in.Precondition.ExpectedRevision != "" && checkpoint.Record.Revision == in.Precondition.ExpectedRevision {
		return "a new checkpoint kept the expected revision; a write always produces a new revision"
	}
	return ""
}

func memoryListMismatch(result any, size int, kinds []collab.MemoryKind) string {
	list, wrong := resultAs[*collab.MemoryListResult](result)
	if wrong != "" {
		return wrong
	}
	if len(list.Items) > size {
		return fmt.Sprintf("%d items for a page of %d", len(list.Items), size)
	}
	for _, record := range list.Items {
		if len(kinds) > 0 && !slices.Contains(kinds, record.Kind) {
			return fmt.Sprintf("record %s is a %s, outside the requested kinds", record.Ref.ID, record.Kind)
		}
	}
	return ""
}

// sameChange compares a returned proposal's identity with the requested one.
// repository is the repository every answer must name: the requested one, or
// the one an earlier item of the same answer named.
func sameChange(got, want collab.Change, repository string) string {
	if got.Base != want.Base || got.Head != want.Head {
		return fmt.Sprintf("proposal for %s...%s, requested %s...%s", got.Base, got.Head, want.Base, want.Head)
	}
	if repository != "" && got.Repository != repository {
		return fmt.Sprintf("proposal in repository %s, requested %s", got.Repository, repository)
	}
	return ""
}

// sameCommit compares two commits where either may be abbreviated.
func sameCommit(a, b string) bool {
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func candidateNames(candidates []collab.ProviderIdentity) string {
	names := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		names = append(names, candidate.Name)
	}
	return strings.Join(names, ", ")
}

func joinIssues(issues []collab.CapabilityIssue) string {
	messages := make([]string, 0, len(issues))
	for _, issue := range issues {
		messages = append(messages, issue.Message)
	}
	return strings.Join(messages, "; ")
}

func operationNames(spec *collab.ContractSpec) []string {
	names := make([]string, 0, len(spec.Operations))
	for _, op := range spec.Operations {
		names = append(names, op.Name)
	}
	return names
}
